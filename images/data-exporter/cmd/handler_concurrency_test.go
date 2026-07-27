/*
Copyright 2026 Flant JSC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/authorization"
	"github.com/deckhouse/storage-foundation/images/data-exporter/internal/config"
)

// TestExportHandlersConcurrent drives one exporter from many goroutines at once, replaying the fuzz
// seed corpus.
//
// It exists for `go test -race`: the fuzz targets serve a single request per iteration, so they cannot
// observe a data race between concurrent requests, which is a failure mode this exporter has been
// fixed for before. In production the handler is shared by every in-flight request, so it must be safe
// to call concurrently. Without -race this still checks that concurrent requests do not panic,
// deadlock or break the response invariants.
//
// Failure injection stays off here: mockfs carries no synchronisation at all and ProbabilityFailer
// holds an unsynchronised *rand.Rand, so injecting failures would report a race inside the mock rather
// than inside the handler. Concurrent reads of the immutable fixture tree are safe.
func TestExportHandlersConcurrent(t *testing.T) {
	const (
		workers    = 16
		iterations = 32
	)

	tests := []struct {
		name      string
		mode      config.VolumeMode
		root      string
		forbidden []string
	}{
		{
			name:      "filesystem",
			mode:      config.VolumeModeFilesystem,
			root:      "/sharedir",
			forbidden: []string{markerOuterDirFile, markerOuterRootFile, markerBlockDevice},
		},
		{
			name:      "block",
			mode:      config.VolumeModeBlock,
			root:      "/block",
			forbidden: []string{markerOuterDirFile, markerOuterRootFile, markerShareFile, markerShareNestedFile},
		},
	}

	seeds := seedCorpus()
	opt := fuzzURLOpt()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := newFuzzFS(t)
			fixture := snapshotFixture(t, fsys)

			exporter, err := NewExporterHandler(fsys, tt.root, tt.mode, logger)
			require.NoError(t, err)

			authorized := authorization.Authorize(
				exporter,
				allowAllAuthorizer{},
				common.OperationExport,
				opt.DataManagerNamespace,
			)

			mux := http.NewServeMux()
			switch tt.mode {
			case config.VolumeModeFilesystem:
				MuxAddFSHandler(mux, opt, authorized, logger)
			case config.VolumeModeBlock:
				MuxAddBlockHandler(mux, opt, authorized, logger)
			default:
				t.Fatalf("unsupported volume mode %v", tt.mode)
			}

			// Workers report through a channel: t.Fatalf must not be called from a goroutine other than
			// the one running the test.
			failures := make(chan error, workers*iterations)

			var wg sync.WaitGroup
			wg.Add(workers)

			for w := range workers {
				go func(worker int) {
					defer wg.Done()

					for i := range iterations {
						// Offset the starting seed per worker so the goroutines hit different routes at the
						// same moment rather than marching through the corpus in lockstep.
						s := seeds[(worker+i)%len(seeds)]

						// failProb is forced to 0 together with the disabled failer, so that checkResponse
						// applies its no-5xx invariant.
						req, ok := buildRequest(s.authType, s.basicCreds, s.bearerToken, s.wrongAuth, s.rawURL, s.methodIdx, s.body)
						if !ok {
							continue
						}

						rr := httptest.NewRecorder()
						mux.ServeHTTP(rr, req)

						if err := checkResponse(rr, req, tt.forbidden, 0); err != nil {
							failures <- err
						}
					}
				}(w)
			}

			wg.Wait()
			close(failures)

			for err := range failures {
				t.Error(err)
			}

			if err := checkFixtureUnchanged(fsys, fixture); err != nil {
				t.Error(err)
			}
		})
	}
}
