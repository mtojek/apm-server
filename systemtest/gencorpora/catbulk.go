// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package gencorpora

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/elastic/go-elasticsearch/v8/esutil"
)

// CatBulkServer wraps http server and a listener to listen
// for ES requests on any available port
type CatBulkServer struct {
	listener net.Listener
	server   *http.Server
	Addr     string

	writer io.WriteCloser

	metaUpdateChan chan docsStat
	metaWriteDone  chan struct{}
}

// docsStat represents statistics of ES docs generated by a request
type docsStat struct {
	count int
	bytes int
}

// NewCatBulkServer returns a HTTP Server which can serve as a
// fake ES server writing the response of the bulk request to the
// provided writer. Writes to the provided writer must be thread safe.
func NewCatBulkServer() (*CatBulkServer, error) {
	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, err
	}

	writer, err := os.Create(gencorporaConfig.CorporaPath)
	if err != nil {
		return nil, err
	}

	addr := listener.Addr().String()
	metaUpdateChan := make(chan docsStat)
	return &CatBulkServer{
		listener: listener,
		Addr:     addr,
		server: &http.Server{
			Addr:    addr,
			Handler: handleReq(metaUpdateChan, writer),
		},
		writer:         writer,
		metaUpdateChan: metaUpdateChan,
		metaWriteDone:  make(chan struct{}),
	}, nil
}

// Serve starts the fake ES server on a listener.
func (s *CatBulkServer) Serve() error {
	go func() {
		if err := s.metaWriter(); err != nil {
			log.Println("failed to write metadata", err)
		}
	}()

	if err := s.server.Serve(s.listener); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Stop initiates graceful shutdown the underlying HTTP server and writes
// generated corpus metadata on successful shutdown.
func (s *CatBulkServer) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer s.writer.Close()

	if err := s.server.Shutdown(ctx); err != nil {
		return fmt.Errorf("failed to shutdown cat bulk server no metadata written: %w", err)
	}

	close(s.metaUpdateChan)
	<-s.metaWriteDone

	return nil
}

func (s *CatBulkServer) metaWriter() error {
	defer close(s.metaWriteDone)

	metadata := struct {
		SourceFile                 string `json:"source-file"`
		DocumentCount              int    `json:"document-count"`
		UncompressedBytes          int    `json:"uncompressed-bytes"`
		IncludedsActionAndMetadata bool   `json:"includes-action-and-meta-data"`
	}{
		SourceFile:                 gencorporaConfig.CorporaPath,
		IncludedsActionAndMetadata: true,
	}

	// update metadata as request is received by the server
	for stat := range s.metaUpdateChan {
		metadata.DocumentCount += stat.count
		metadata.UncompressedBytes += stat.bytes
	}

	// write metadata to a file
	metadataBytes, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}

	writer, err := os.Create(gencorporaConfig.MetadataPath)
	defer writer.Close()

	if _, err := writer.Write(metadataBytes); err != nil {
		return err
	}

	return nil
}

func handleReq(metaUpdateChan chan docsStat, writer io.Writer) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		switch req.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"cluster_uuid": "cat_bulk"}`))
		case http.MethodPost:
			reader := req.Body
			defer req.Body.Close()

			if encoding := req.Header.Get("Content-Encoding"); encoding == "gzip" {
				var err error
				reader, err = gzip.NewReader(reader)
				if err != nil {
					log.Println("failed to read request body", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
			}

			mockResp := esutil.BulkIndexerResponse{}
			scanner := bufio.NewScanner(reader)
			scanner.Split(splitMetadataAndSource)

			var stat docsStat
			for scanner.Scan() {
				n, err := writer.Write(scanner.Bytes())
				if err != nil {
					// Discard the request without processing further
					log.Println("failed to write ES corpora to a file", err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}

				stat.count++
				stat.bytes += n

				item := map[string]esutil.BulkIndexerResponseItem{
					"action": {Status: http.StatusOK},
				}
				mockResp.Items = append(mockResp.Items, item)
			}

			if err := scanner.Err(); err != nil {
				log.Println("failed to read ES corpora", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			// Update metadata with the ES document statistics generated by this request
			metaUpdateChan <- stat

			resp, err := json.Marshal(mockResp)
			if err != nil {
				log.Println("failed to encode response to JSON", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
			w.Write(resp)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// splitMetadataAndSource splits the input ES corpora expecting each corpus to have
// action-and-metdata line followed by source document in an ndjson format. The EOL
// markers are preserved and included in the token.
func splitMetadataAndSource(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}

	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		// This represents metadata EOL marker
		// Try to find the source EOL marker
		if len(data) > i+1 {
			if j := bytes.IndexByte(data[i+1:], '\n'); j >= 0 {
				// This represents source EOL marker
				return i + j + 2, data[:i+j+2], nil
			}
		}
	}

	// At EOF the scanner will be in one of the following state:
	// 1. We don't have both action and metadata for atleast one document
	// 2. We have a final non-terminated line
	// We can return the data as is in both cases. Case 1 may represents input doc
	// to not be as metadata and action but is left to be handled by the consumer of
	// the generated corpus.
	if atEOF {
		return len(data), data, nil
	}

	// Request more data.
	return 0, nil, nil
}
