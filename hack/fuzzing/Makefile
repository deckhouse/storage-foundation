# Copyright 2026 Flant JSC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Fuzzing pipeline for the data-exporter HTTP handlers: run the fuzzer, promote the
# generated corpus into testdata, measure coverage over that corpus, archive a report.
#
# Every path is derived from this Makefile's own location, so the targets work from
# any working directory:
#
#   make -C hack/fuzzing all
#   make -C hack/fuzzing fuzz FUZZ_TIME=48h DRY_TIME=2h PARALLEL=4
#
# See README.md in this directory for the full runbook.

DIR := $(patsubst %/,%,$(dir $(abspath $(lastword $(MAKEFILE_LIST)))))
ROOT := $(abspath $(DIR)/../..)

# Directory of the Go module to fuzz.
MODULE_DIR := $(ROOT)/images/data-exporter

# Fuzz targets to run, one exporter each. The scripts assume every target lives in
# $(MODULE_DIR)/cmd, because coverage has to be collected for the whole module while the
# tests sit in one package. Revisit the scripts if we ever add targets elsewhere.
#
# Override to fuzz a single target, e.g.
#   make -C hack/fuzzing fuzz TARGETS=FuzzExportBlockHandler
TARGETS ?= FuzzExportFilesystemHandler FuzzExportBlockHandler

# Fuzzing timeout PER TARGET: `go test -fuzz` takes a pattern matching exactly one target,
# so the targets are fuzzed one after another and a full run takes
# FUZZ_TIME x $(words $(TARGETS)).
FUZZ_TIME ?= 48h

# Stop early once this much time passes without a new interesting input.
DRY_TIME ?= 2h

# Number of parallel fuzzing workers.
PARALLEL ?= 4

.PHONY: all fuzz race promote coverage archive clean clean-corpus help

# The stages feed each other, so never let -jN interleave them.
.NOTPARALLEL:

help:
	@echo "Targets ($(words $(TARGETS)) fuzz targets: $(TARGETS)):"
	@echo "  fuzz         run the fuzzer, FUZZ_TIME=$(FUZZ_TIME) per target (DRY_TIME=$(DRY_TIME), PARALLEL=$(PARALLEL))"
	@echo "  race         replay the seed corpus concurrently under the race detector"
	@echo "  promote      copy the generated corpora from .fuzzcache into cmd/testdata"
	@echo "  coverage     measure coverage over the promoted corpora (implies promote)"
	@echo "  archive      pack corpora, log and coverage report into a tarball"
	@echo "  all          fuzz -> promote -> coverage -> archive"
	@echo "  clean        remove generated artifacts, keeping the corpora in testdata"
	@echo "  clean-corpus additionally remove the promoted corpora"

all: fuzz promote coverage archive

fuzz:
	@bash "$(DIR)/fuzz.sh" "$(MODULE_DIR)" "$(FUZZ_TIME)" "$(DRY_TIME)" "$(PARALLEL)" "$(TARGETS)"

# The fuzz targets serve one request per iteration, so they cannot observe a race between
# concurrent requests. TestExportHandlersConcurrent does, and only reports races when the
# detector is on, which CI's plain `go test ./...` does not enable.
race:
	@cd "$(MODULE_DIR)" && go test -race -run TestExportHandlersConcurrent -count=1 ./cmd

promote:
	@bash "$(DIR)/promote_corpus.sh" "$(MODULE_DIR)" "$(TARGETS)"

coverage: promote
	@bash "$(DIR)/coverage.sh" "$(MODULE_DIR)" "$(TARGETS)"

archive:
	@bash "$(DIR)/archive.sh" "$(MODULE_DIR)"

# Every path is quoted: an unquoted `rm -rf` over a variable that picks up stray
# whitespace deletes the module source tree instead of the artifact underneath it.
clean:
	rm -rf "$(DIR)/out"
	rm -f "$(DIR)/runner/runner"
	rm -f "$(DIR)"/fuzz_report-*.tar.gz
	rm -rf "$(MODULE_DIR)/cmd/.fuzzcache"
	rm -f "$(MODULE_DIR)/cmd/coverage.txt"
	rm -f "$(MODULE_DIR)/cmd/coverage_total.txt"

# Split out from `clean` on purpose: testdata holds the promoted corpora, which may be
# committed and are the point of a completed run. Only wipe them deliberately.
clean-corpus: clean
	@for target in $(TARGETS); do \
		echo rm -rf "$(MODULE_DIR)/cmd/testdata/fuzz/$$target"; \
		rm -rf "$(MODULE_DIR)/cmd/testdata/fuzz/$$target"; \
	done
