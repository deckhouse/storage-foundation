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

package publish

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMakeIngressVerifiesTheBackendCertificate(t *testing.T) {
	cfg := testIngressCfg()
	cfg.BackendCASecretName = "ca-for-pvc-abc"
	cfg.BackendServerName = "service-for-pvc-abc.d8-storage-foundation.svc"

	annotations := makeTestIngress(t, cfg).Annotations

	assert.Equal(t, "on", annotations[AnnotationProxySSLVerify])
	assert.Equal(t, "d8-storage-foundation/ca-for-pvc-abc", annotations[AnnotationProxySSLSecret])
	assert.Equal(t, "service-for-pvc-abc.d8-storage-foundation.svc", annotations[AnnotationProxySSLName])
	assert.Equal(t, "on", annotations[AnnotationProxySSLServerName])
	assert.Equal(t, "HTTPS", annotations[AnnotationBackendProtocol])
}

func TestMakeIngressWithoutBackendCASecret(t *testing.T) {
	annotations := makeTestIngress(t, testIngressCfg()).Annotations

	for _, key := range []string{AnnotationProxySSLVerify, AnnotationProxySSLSecret, AnnotationProxySSLName, AnnotationProxySSLServerName} {
		assert.NotContains(t, annotations, key)
	}
}
