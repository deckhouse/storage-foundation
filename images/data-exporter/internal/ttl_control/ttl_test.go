/*
Copyright 2025 Flant JSC

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

package ttl_control

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/deckhouse/storage-foundation/common"
)

// TestNewTTLControl_TTLParsing covers the --ttl (spec.ttl) parsing, including the empty-ttl fallback: the
// CRD pattern for spec.ttl also matches the empty string, so a producer that sets ttl:"" would otherwise
// crash the server pod on ParseDuration(""). The fallback keeps the pod serving with the documented
// default idle window instead of crash-looping.
func TestNewTTLControl_TTLParsing(t *testing.T) {
	logger := slog.Default()

	tests := []struct {
		name    string
		ttlStr  string
		wantTTL time.Duration
		wantErr bool
	}{
		{name: "explicit duration", ttlStr: "30m", wantTTL: 30 * time.Minute},
		{name: "empty falls back to default", ttlStr: "", wantTTL: defaultServerTTL},
		{name: "whitespace-only falls back to default", ttlStr: "   ", wantTTL: defaultServerTTL},
		{name: "invalid is still rejected", ttlStr: "not-a-duration", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc, err := NewTTLControl(common.OperationImport, tt.ttlStr, "ns", "name", nil, nil, logger)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, tc)
			assert.Equal(t, tt.wantTTL, tc.ttl)
		})
	}
}
