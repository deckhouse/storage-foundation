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

package dataimport

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func pvcRequesting(q resource.Quantity, opts ...func(*corev1.PersistentVolumeClaim)) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: q},
			},
		},
	}
	for _, o := range opts {
		o(pvc)
	}
	return pvc
}

// TestNeedsPVCSpecUpdate_QuantityIsComparedByAmount is the guard for the defect: the storage request was
// compared with reflect.DeepEqual, which looks at resource.Quantity's unexported state -- the parsed value,
// the suffix format and a cached string -- rather than the amount. The same request survives a round-trip
// through the API server in a different representation, so the comparison reported a difference that was
// not there. The consequence is not an extra write: nearly all of PersistentVolumeClaim.spec is immutable,
// so the update is rejected and the reconcile fails on every pass.
func TestNeedsPVCSpecUpdate_QuantityIsComparedByAmount(t *testing.T) {
	cases := []struct {
		name       string
		existing   string
		desired    string
		wantUpdate bool
	}{
		// Same amount, different spelling: this is what a round-trip produces.
		{name: "3Gi vs 3072Mi", existing: "3Gi", desired: "3072Mi", wantUpdate: false},
		{name: "1Gi vs 1024Mi", existing: "1Gi", desired: "1024Mi", wantUpdate: false},
		{name: "identical", existing: "3Gi", desired: "3Gi", wantUpdate: false},
		// Genuinely different amounts must still be reported.
		{name: "grown", existing: "1Gi", desired: "2Gi", wantUpdate: true},
		{name: "shrunk", existing: "2Gi", desired: "1Gi", wantUpdate: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			existing := pvcRequesting(resource.MustParse(tc.existing))
			desired := pvcRequesting(resource.MustParse(tc.desired))

			assert.Equal(t, tc.wantUpdate, needsPVCSpecUpdate(existing, desired))
		})
	}
}

// TestNeedsPVCSpecUpdate_ParsedAndRoundTrippedAreEqual reproduces the representation difference directly,
// rather than relying on two spellings of the same size: a Quantity that has had its cached string
// invalidated is not DeepEqual to a freshly parsed one carrying the same amount.
func TestNeedsPVCSpecUpdate_ParsedAndRoundTrippedAreEqual(t *testing.T) {
	fresh := resource.MustParse("3Gi")

	roundTripped := resource.MustParse("3Gi")
	// Reading the value the way a serializer does populates/clears internal caches.
	_ = roundTripped.Value()
	roundTripped.Add(resource.MustParse("1Gi"))
	roundTripped.Sub(resource.MustParse("1Gi"))

	require.Equal(t, 0, fresh.Cmp(roundTripped), "the two must hold the same amount")

	assert.False(t, needsPVCSpecUpdate(pvcRequesting(fresh), pvcRequesting(roundTripped)),
		"a Quantity that only differs in representation must not be reported as a spec change")
}

// TestNeedsPVCSpecUpdate_OtherFieldsStillCompared keeps the surrounding checks honest: only the storage
// request moved to Cmp, the rest of the spec must still be compared.
func TestNeedsPVCSpecUpdate_OtherFieldsStillCompared(t *testing.T) {
	q := resource.MustParse("1Gi")
	block := corev1.PersistentVolumeBlock
	sc := "fast"
	other := "slow"

	t.Run("access modes", func(t *testing.T) {
		desired := pvcRequesting(q, func(p *corev1.PersistentVolumeClaim) {
			p.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}
		})
		assert.True(t, needsPVCSpecUpdate(pvcRequesting(q), desired))
	})

	t.Run("storage class", func(t *testing.T) {
		existing := pvcRequesting(q, func(p *corev1.PersistentVolumeClaim) { p.Spec.StorageClassName = &sc })
		desired := pvcRequesting(q, func(p *corev1.PersistentVolumeClaim) { p.Spec.StorageClassName = &other })
		assert.True(t, needsPVCSpecUpdate(existing, desired))
	})

	t.Run("volume mode", func(t *testing.T) {
		desired := pvcRequesting(q, func(p *corev1.PersistentVolumeClaim) { p.Spec.VolumeMode = &block })
		assert.True(t, needsPVCSpecUpdate(pvcRequesting(q), desired))
	})
}
