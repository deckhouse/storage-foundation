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

package certutil

import (
	"crypto/x509"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/deckhouse/storage-foundation/common"
)

// TestServerCertificatePassesTheIngressCheck verifies the serving certificate the way the Ingress does:
// against the CA the pod publishes, under the name common.BackendServerName gives the Ingress.
func TestServerCertificatePassesTheIngressCheck(t *testing.T) {
	bundle, err := GenerateTLSBundle(slog.Default(), "10.0.0.1", "service-for-pvc-abc", "d8-storage-foundation", time.Hour)
	require.NoError(t, err)

	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(bundle.CACertPEM))
	leaf, err := x509.ParseCertificate(bundle.ServerCert.Certificate[0])
	require.NoError(t, err)

	_, err = leaf.Verify(x509.VerifyOptions{
		Roots:   roots,
		DNSName: common.BackendServerName("service-for-pvc-abc", "d8-storage-foundation"),
	})
	require.NoError(t, err)
}
