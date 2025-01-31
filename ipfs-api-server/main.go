// Copyright 2019 Cartesi Pte. Ltd.

// Licensed under the Apache License, Version 2.0 (the "License"); you may not use
// this file except in compliance with the License. You may obtain a copy of the
// License at http://www.apache.org/licenses/LICENSE-2.0

// Unless required by applicable law or agreed to in writing, software distributed
// under the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR
// CONDITIONS OF ANY KIND, either express or implied. See the License for the
// specific language governing permissions and limitations under the License.

// Package main implements a test client for Ipfs service.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	//"google.golang.org/grpc/codes"

	pb "github.com/cartesi/ipfs-service/proto"
	shell "github.com/ipfs/go-ipfs-api"
)

type merkleRootHashRequest struct {
	path     string
	log2Size uint32
}

type merkleRootHashCache map[merkleRootHashRequest][]byte

type server struct {
	pb.UnimplementedIpfsServer

	mrhcMu sync.Mutex
	mrhc   merkleRootHashCache
}

func newServer() *server {
	return &server{
		mrhc: make(merkleRootHashCache, 0),
	}
}

// request is used to track status of each incoming request
type request struct {
	done    chan string
	err     chan error
	running bool
	result  string
}

// addParams is used submit AddFile request to IPFS handler routine
type addParams struct {
	done     chan string
	err      chan error
	filePath string
}

// getParams is used submit GetFile request to IPFS handler routine
type getParams struct {
	done       chan string
	err        chan error
	exportDAG  bool
	ipfsPath   string
	outputPath string
	timeout    uint64
}

// SafeMap is used to track status of each request
type SafeMap struct {
	status map[string]*request
	mux    sync.Mutex
	addCh  chan addParams
	getCh  chan getParams
}

var safeMap = SafeMap{
	status: make(map[string]*request),
	addCh:  make(chan addParams),
	getCh:  make(chan getParams),
}

// Calculate the merkle tree root hash of the path given the tree log2 size
func (s *server) getMerkleRootHash(path string, log2Size uint32) ([]byte, error) {
	// check if cache already has requested merkle root hash
	hash, ok := s.getMerkleRootHashFromCache(path, log2Size)
	if ok {
		log.Printf("merkle root hash for path=%s and log2Size=%d found in cache", path, log2Size)
		return hash, nil
	}

	log.Printf("computing merkle root hash for path=%s and log2Size=%d", path, log2Size)

	// don't calculate merkle root hash if log2 size is 0
	if log2Size == 0 {
		return make([]byte, 32), nil
	}
	// log2 size must be greater than 3
	if log2Size < 3 {
		return nil, fmt.Errorf("invalid log2 size: %d, must be greater than or equal to 3", log2Size)
	}

	out, err := exec.Command(
		"/opt/cartesi/bin/merkle-tree-hash",
		fmt.Sprintf("--page-log2-size=%d", 3),
		fmt.Sprintf("--tree-log2-size=%d", log2Size),
		fmt.Sprintf("--input=%s", path),
	).CombinedOutput()

	if err != nil {
		return nil, fmt.Errorf("%s, %s", err, out)
	}

	outString := strings.TrimSpace(fmt.Sprintf("%s", out))

	if len(outString) != 64 {
		return nil, fmt.Errorf("failed to calculate merkle tree root hash: %s", outString)
	}

	return hex.DecodeString(outString)
}

func (s *server) getMerkleRootHashFromCache(path string, log2Size uint32) ([]byte, bool) {
	s.mrhcMu.Lock()
	defer s.mrhcMu.Unlock()

	req := merkleRootHashRequest{
		path:     path,
		log2Size: log2Size,
	}
	hash, ok := s.mrhc[req]
	if ok {
		return hash, true
	}

	return hash, false
}

func (s *server) putMerkleRootHashToCache(path string, log2Size uint32, hash []byte) {
	s.mrhcMu.Lock()
	defer s.mrhcMu.Unlock()

	req := merkleRootHashRequest{
		path:     path,
		log2Size: log2Size,
	}
	s.mrhc[req] = hash
}

// AddFile implements ipfs.IpfsServer
func (s *server) AddFile(ctx context.Context, in *pb.AddFileRequest) (*pb.AddFileResponse, error) {
	log.Printf("Received AddFileRequest: %+v", *in)

	safeMap.mux.Lock()
	defer safeMap.mux.Unlock()

	var response *pb.AddFileResponse = nil
	var err error = nil
	key := in.GetFilePath()
	status := safeMap.status[key]

	if status != nil {
		// Request being processed already
		if !status.running {
			// Return result as request is done
			response = &pb.AddFileResponse{
				AddOneof: &pb.AddFileResponse_Result{
					Result: &pb.AddFileResult{
						IpfsPath: status.result,
					}}}
		} else {
			// Pull progress or result as still running
			select {
			case status.result = <-status.done:
				// Return result
				response = &pb.AddFileResponse{
					AddOneof: &pb.AddFileResponse_Result{
						Result: &pb.AddFileResult{
							IpfsPath: status.result,
						}}}
				status.running = false
			case retErr := <-status.err:
				// Return error
				err = grpc.Errorf(codes.Unknown, retErr.Error())
				safeMap.status[key] = nil
			default:
				// Return progress
				response = &pb.AddFileResponse{
					AddOneof: &pb.AddFileResponse_Progress{
						Progress: &pb.Progress{
							// TODO: Calculate progress
							Progress:  0,
							UpdatedAt: uint64(time.Now().Unix()),
						}}}
			}
		}
	} else {
		// First time receive request
		safeMap.status[key] = &request{
			done:    make(chan string),
			err:     make(chan error),
			running: true,
		}

		go func() {
			// Submit AddFile job to IPFS
			safeMap.addCh <- addParams{
				done:     safeMap.status[key].done,
				err:      safeMap.status[key].err,
				filePath: key,
			}
		}()

		response = &pb.AddFileResponse{
			AddOneof: &pb.AddFileResponse_Progress{
				Progress: &pb.Progress{
					Progress:  0,
					UpdatedAt: uint64(time.Now().Unix()),
				}}}
	}

	return response, err
}

func (s *server) CacheMerkleRootHash(ctx context.Context, in *pb.CacheMerkleRootHashRequest) (*pb.CacheMerkleRootHashResponse, error) {
	log.Printf("Received CacheMerkleRootHash: %+v", *in)
	s.putMerkleRootHashToCache(in.GetIpfsPath(), in.GetLog2Size(), in.GetMerkleRootHash().GetData())

	return &pb.CacheMerkleRootHashResponse{}, nil
}

// GetFile implements ipfs.IpfsServer
func (s *server) GetFile(ctx context.Context, in *pb.GetFileRequest) (*pb.GetFileResponse, error) {
	log.Printf("Received GetFileRequest: %+v", *in)

	safeMap.mux.Lock()
	defer safeMap.mux.Unlock()

	var response *pb.GetFileResponse = nil
	var err error = nil

	key := in.GetIpfsPath() + "_" + in.GetOutputPath()
	status := safeMap.status[key]

	if status != nil {
		// Request being processed already
		if !status.running {
			rootHash, merkleErr := s.getMerkleRootHash(status.result, in.GetLog2Size())

			if merkleErr != nil {
				// Return error from get merkle root hash
				safeMap.status[key] = nil
				err = merkleErr
			} else {
				// Return result as request is done
				response = &pb.GetFileResponse{
					GetOneof: &pb.GetFileResponse_Result{
						Result: &pb.GetFileResult{
							OutputPath: status.result,
							RootHash: &pb.Hash{
								Data: rootHash,
							}}}}
			}
		} else {
			// Pull progress or result as still running
			select {
			case status.result = <-status.done:
				rootHash, merkleErr := s.getMerkleRootHash(status.result, in.GetLog2Size())

				if merkleErr != nil {
					// Return error from get merkle root hash
					safeMap.status[key] = nil
					err = merkleErr
				} else {
					// Return result
					response = &pb.GetFileResponse{
						GetOneof: &pb.GetFileResponse_Result{
							Result: &pb.GetFileResult{
								OutputPath: status.result,
								RootHash: &pb.Hash{
									Data: rootHash,
								}}}}
					status.running = false
				}
			case retErr := <-status.err:
				// Return error
				err = grpc.Errorf(codes.Unknown, retErr.Error())
				safeMap.status[key] = nil
			default:
				// Return progress
				response = &pb.GetFileResponse{
					GetOneof: &pb.GetFileResponse_Progress{
						Progress: &pb.Progress{
							// TODO: Calculate progress
							Progress:  0,
							UpdatedAt: uint64(time.Now().Unix()),
						}}}
			}
		}
	} else {
		// First time receive request
		safeMap.status[key] = &request{
			done:    make(chan string),
			err:     make(chan error),
			running: true,
		}

		go func() {
			// Submit GetFile job to IPFS
			safeMap.getCh <- getParams{
				done:       safeMap.status[key].done,
				err:        safeMap.status[key].err,
				exportDAG:  in.GetExportDag(),
				ipfsPath:   in.GetIpfsPath(),
				outputPath: in.GetOutputPath(),
				timeout:    in.GetTimeout(),
			}
		}()

		response = &pb.GetFileResponse{
			GetOneof: &pb.GetFileResponse_Progress{
				Progress: &pb.Progress{
					Progress:  0,
					UpdatedAt: uint64(time.Now().Unix()),
				}}}
	}

	return response, err
}

func exportDAGToCAR(gateway, cid, outputPath string) error {
	// Remove CAR file if it already exists.
	if _, err := os.Stat(outputPath); err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("failed to stat file %s - %s", outputPath, err)
		}
	} else {
		if err := os.Remove(outputPath); err != nil {
			return fmt.Errorf("failed to remove file %s - %s", outputPath, err)
		}
	}

	// Create CAR file.
	carFile, err := os.OpenFile(outputPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to create file %s - %s", outputPath, err)
	}
	defer carFile.Close()

	// Export DAG via IPFS RPC API method - https://docs.ipfs.tech/reference/kubo/rpc/#api-v0-dag-export
	url := fmt.Sprintf("http://%s/api/v0/dag/export?arg=%s", gateway, cid)
	resp, err := http.Post(url, "", bytes.NewBuffer([]byte{}))
	if err != nil {
		return fmt.Errorf("failed to export dag for cid %s - %s", cid, err)
	}
	defer resp.Body.Close()

	// Read streamed HTTP response from IPFS and wrute it to file chunk by chunk.
	// In theory read loop should be terminated by io.EOF -
	// https://stackoverflow.com/questions/22108519/how-do-i-read-a-streaming-response-body-using-golangs-net-http-package
	// But it seems, that IPFS server doesn't send io.EOF in response.
	reader := bufio.NewReader(resp.Body)
	for {
		chunk, _ := reader.ReadBytes('\n')
		if len(chunk) == 0 {
			break
		}
		if _, err := carFile.Write(chunk); err != nil {
			return fmt.Errorf("failed to write chunk to car file %s - %s", outputPath, err)
		}
		chunk = nil
	}

	return nil
}

func main() {
	port := flag.Int("p", 50051, "gRPC server port")
	gateway := flag.String("g", "localhost:5001", "IPFS API Address")

	flag.Parse()

	sh := shell.NewShell(*gateway)

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	go func() {
		for {
			// Start listening incoming requests from gRPC client
			select {
			case add := <-safeMap.addCh:
				cid, err := sh.AddDir(add.filePath)
				if err != nil {
					add.err <- fmt.Errorf("Could not add file: %s", err)
					break
				}

				log.Printf("Added file to IPFS with CID %s", cid)
				add.done <- cid

			case get := <-safeMap.getCh:
				sh.SetTimeout(time.Duration(get.timeout) * time.Second)
				hash := sha256.Sum256([]byte(get.ipfsPath))

				safeOutputPath := get.outputPath + "_" + hex.EncodeToString(hash[:])

				if get.exportDAG {
					cid := get.ipfsPath
					if err := exportDAGToCAR(*gateway, cid, safeOutputPath); err != nil {
						get.err <- err
						break
					}
					log.Printf("Exported DAG of file %s to CAR file %s", cid, safeOutputPath)
					get.done <- safeOutputPath
				} else {
					err := sh.Get(get.ipfsPath, safeOutputPath)
					if err != nil {
						get.err <- fmt.Errorf("Could not get file: %s", err)
						break
					}
					log.Printf("Fetched file from IPFS: %s -> %s, safe name %s", get.ipfsPath, get.outputPath, safeOutputPath)
					get.done <- safeOutputPath
				}
			}
		}
	}()

	s := grpc.NewServer()
	pb.RegisterIpfsServer(s, newServer())

	if err := s.Serve(listener); err != nil {
		log.Fatalf("gRPC server failed to start: %v", err)
	}
}
