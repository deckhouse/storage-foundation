#!/bin/bash


# Copyright 2025 Flant JSC
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

# Runs fuzzing test and restarts it if it fails
# Saves logs (stdout and stderr) to file
# Usage: ./hack/run_fuzzer.sh [additional arguments for go test]

set -e

SCRIPT_DIR="$(realpath $(dirname "$0"))"
WORK_DIR="$(realpath "$SCRIPT_DIR/../images/data-exporter")"

# Configuration
LOG_FILE="fuzzer.log"

# Cleanup function
cleanup() {
    echo "Received signal, stopping fuzzer..."
    if [[ -n $FUZZER_PID ]]; then
        # Forward signal to fuzzer process
        kill -INT $FUZZER_PID 2>/dev/null || true
        
        # Wait for the fuzzer process to complete
        wait $FUZZER_PID
        EXIT_CODE=$?

        echo "[$(date '+%Y-%m-%d %H:%M:%S')] Fuzzer exited with code $EXIT_CODE" | tee -a "$LOG_FILE"
    fi
    echo "Fuzzer interrupted." | tee -a "$LOG_FILE"
    exit 0
}

# Set up signal handlers
trap cleanup SIGINT SIGTERM

echo "Starting fuzzer runner..."
echo "Working directory: $WORK_DIR"
echo "Logs: $LOG_FILE"
echo "Press Ctrl+C to stop"
echo "================================"

RESTART_COUNT=0

cd "$WORK_DIR"

while true; do
    RESTART_COUNT=$((RESTART_COUNT + 1))
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] Starting fuzzer (attempt #$RESTART_COUNT)..." | tee -a "$LOG_FILE"

    # Start fuzzer in background and capture its PID
    go test -fuzz 'FuzzHandler' -coverpkg=./... -v ./cmd $@ 2>&1 | tee -a "$LOG_FILE" &
    FUZZER_PID=$!

    # Wait for the fuzzer process to complete
    wait $FUZZER_PID
    EXIT_CODE=$?

    echo "[$(date '+%Y-%m-%d %H:%M:%S')] Fuzzer exited with code $EXIT_CODE" | tee -a "$LOG_FILE"
    echo "Fuzzer failed, restarting in 2 seconds..." | tee -a "$LOG_FILE"
    sleep 2
done

echo "Fuzzer runner finished." | tee -a "$LOG_FILE"
