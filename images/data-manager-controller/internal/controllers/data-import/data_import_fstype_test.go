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
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/deckhouse/storage-foundation/common"
)

// scratchVolumeEvents records the two calls whose ORDER is the invariant under test: reading the scratch
// PersistentVolume and deleting the scratch claim. It also makes the fake client behave like a real cluster
// on that delete — a Delete-policy CSI volume goes away with its claim — so an observation attempted after
// the delete finds nothing, exactly as in production.
type scratchVolumeEvents struct {
	calls []string
}

const (
	eventGetPV     = "get-pv"
	eventDeletePVC = "delete-pvc"
)

func (e *scratchVolumeEvents) interceptor() interceptor.Funcs {
	return interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*corev1.PersistentVolume); ok {
				e.calls = append(e.calls, eventGetPV)
			}
			return cl.Get(ctx, key, obj, opts...)
		},
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			claim, ok := obj.(*corev1.PersistentVolumeClaim)
			if !ok || claim.Name != captureTestImportName {
				return cl.Delete(ctx, obj, opts...)
			}
			e.calls = append(e.calls, eventDeletePVC)
			// Reclaim the bound volume together with the claim: past this point spec.csi.fsType is gone.
			pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: captureTestScratchPVName}}
			if err := cl.Delete(ctx, pv); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			return cl.Delete(ctx, obj, opts...)
		},
	}
}

// TestEnsureDataArtifact_ObservesScratchVolumeFSTypeBeforeItIsDestroyed is the load-bearing test of the
// fix. status.data.fsType is the only record of the filesystem the imported bytes were written onto: the
// scratch volume is destroyed right after capture, the produced VolumeSnapshotContent carries no filesystem
// metadata, and the import-side consumer only attaches once status.data.artifactRef exists. So the value has
// to be observed on the live PersistentVolume, and observed BEFORE the delete — after it, nothing in the
// cluster knows it any more.
//
// The fixture makes both ways of getting it wrong visible: the StorageClass advertises
// captureTestClassFSType (predicting from the class yields that), and the volume disappears with the claim
// (observing too late yields the empty string).
func TestEnsureDataArtifact_ObservesScratchVolumeFSTypeBeforeItIsDestroyed(t *testing.T) {
	t.Parallel()

	events := &scratchVolumeEvents{}
	r, pvc := newArtifactCaptureReconcilerWith(t, artifactCaptureOptions{
		volumeMode:   corev1.PersistentVolumeFilesystem,
		pvFSType:     captureTestPVFSType,
		interceptors: []interceptor.Funcs{events.interceptor()},
	})

	res, err := r.ensureDataArtifact(context.Background(), pvc)
	require.NoError(t, err)
	assert.Zero(t, res.RequeueAfter)

	// (a) The published value is the one the volume itself recorded, not the class prediction.
	require.NotNil(t, r.dataImport.Status.Data)
	assert.Equal(t, captureTestPVFSType, r.dataImport.Status.Data.FsType,
		"status.data.fsType must be observed on the scratch PersistentVolume, not derived from the StorageClass (%q)",
		captureTestClassFSType)

	// (b) The observation happened while the volume still existed. Exact sequence, so neither reordering nor
	// a second read after the delete passes.
	assert.Equal(t, []string{eventGetPV, eventDeletePVC}, events.calls,
		"the scratch volume must be read before the claim is deleted; afterwards its filesystem type is unrecoverable")

	// (c) The delete really did take the volume with it — otherwise (b) would be guarding a fake client that
	// keeps the PV readable forever, and a late observation would still find it.
	getErr := r.Client.Get(context.Background(), types.NamespacedName{Name: captureTestScratchPVName}, &corev1.PersistentVolume{})
	require.Error(t, getErr)
	assert.True(t, apierrors.IsNotFound(getErr), "the scratch PersistentVolume must be gone once the claim is deleted")

	// (d) The rest of the published data leg is unaffected.
	require.NotNil(t, r.dataImport.Status.Data.ArtifactRef)
	assert.Equal(t, captureTestVSCName, r.dataImport.Status.Data.ArtifactRef.Name)
	assert.True(t, meta.IsStatusConditionTrue(r.dataImport.Status.Conditions, string(common.ConditionCompleted)))
}

// TestEnsureDataArtifact_BlockScratchVolumePublishesNoFSType pins the Block contract: a raw block volume
// carries no filesystem, so status.data.fsType stays empty and the restore path keeps ignoring it. The
// fixture is adversarial on purpose — the PV records a filesystem type anyway (some drivers copy the class
// parameter onto the volume regardless of volumeMode), so the emptiness has to come from the volumeMode gate
// rather than from the field happening to be unset.
func TestEnsureDataArtifact_BlockScratchVolumePublishesNoFSType(t *testing.T) {
	t.Parallel()

	r, pvc := newArtifactCaptureReconcilerWith(t, artifactCaptureOptions{
		volumeMode: corev1.PersistentVolumeBlock,
		pvFSType:   captureTestPVFSType,
	})

	res, err := r.ensureDataArtifact(context.Background(), pvc)
	require.NoError(t, err)
	assert.Zero(t, res.RequeueAfter)

	require.NotNil(t, r.dataImport.Status.Data)
	assert.Empty(t, r.dataImport.Status.Data.FsType,
		"a Block import has no filesystem; publishing the type recorded on the PV (%q) would assert one it does not have",
		captureTestPVFSType)

	// The Block import is otherwise a normal, completed import.
	require.NotNil(t, r.dataImport.Status.Data.ArtifactRef)
	assert.Equal(t, captureTestVSCName, r.dataImport.Status.Data.ArtifactRef.Name)
	assert.True(t, meta.IsStatusConditionTrue(r.dataImport.Status.Conditions, string(common.ConditionCompleted)))
}

// TestEnsureDataArtifact_LostRaceWithVolumeStillPublishesArtifactRef covers the race the other way round:
// the scratch volume is already gone when the observation is attempted (force-deleted, or reclaimed by a
// partially-completed earlier pass). The filesystem type is then lost — nothing can bring it back — but it
// must NOT cost the import its artifact: status.data.artifactRef and Completed are what make the produced
// VolumeSnapshotContent reachable at all, and the artifact is already durable by this point.
func TestEnsureDataArtifact_LostRaceWithVolumeStillPublishesArtifactRef(t *testing.T) {
	t.Parallel()

	r, pvc := newArtifactCaptureReconcilerWith(t, artifactCaptureOptions{
		volumeMode: corev1.PersistentVolumeFilesystem,
		pvFSType:   captureTestPVFSType,
		withoutPV:  true,
	})

	res, err := r.ensureDataArtifact(context.Background(), pvc)
	require.NoError(t, err, "a missing scratch volume must not fail the import: the artifact is already durable")
	assert.Zero(t, res.RequeueAfter, "a missing scratch volume must not delay publication either")

	require.NotNil(t, r.dataImport.Status.Data)
	require.NotNil(t, r.dataImport.Status.Data.ArtifactRef)
	assert.Equal(t, captureTestVSCName, r.dataImport.Status.Data.ArtifactRef.Name)
	assert.True(t, meta.IsStatusConditionTrue(r.dataImport.Status.Conditions, string(common.ConditionCompleted)))

	// Empty means "not known". Anything else here would be a guess — the class prediction most of all.
	assert.Empty(t, r.dataImport.Status.Data.FsType,
		"with no volume to observe the field must stay empty, not fall back to the StorageClass (%q)",
		captureTestClassFSType)
}

// TestScratchVolumeFSType covers the observation itself, case by case. Its whole contract is "return what
// the volume records, and the empty string for everything else" — the caller publishes the result next to an
// already-durable artifact, so there is no failure it may report.
//
// Limit: these cases pin what the function returns, not WHEN it is called. The ordering invariant (observe
// before the claim is deleted) is only checked by
// TestEnsureDataArtifact_ObservesScratchVolumeFSTypeBeforeItIsDestroyed.
func TestScratchVolumeFSType(t *testing.T) {
	t.Parallel()

	block := corev1.PersistentVolumeBlock
	filesystem := corev1.PersistentVolumeFilesystem

	csiPV := func(fsType string) *corev1.PersistentVolume {
		return &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: captureTestScratchPVName},
			Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{Driver: captureTestDriver, VolumeHandle: "vh-1", FSType: fsType},
			}},
		}
	}
	claim := func(mode *corev1.PersistentVolumeMode, volumeName string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: captureTestImportName, Namespace: captureTestNamespace},
			Spec:       corev1.PersistentVolumeClaimSpec{VolumeMode: mode, VolumeName: volumeName},
			Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
		}
	}

	tests := []struct {
		name string
		pvc  *corev1.PersistentVolumeClaim
		pv   *corev1.PersistentVolume
		want string
	}{
		{
			name: "filesystem volume reports what the driver recorded",
			pvc:  claim(&filesystem, captureTestScratchPVName),
			pv:   csiPV(captureTestPVFSType),
			want: captureTestPVFSType,
		},
		{
			// volumeMode is defaulted by the apiserver, so nil only reaches here on a hand-built object; nil
			// means Filesystem, which does carry a filesystem.
			name: "unset volumeMode is treated as filesystem",
			pvc:  claim(nil, captureTestScratchPVName),
			pv:   csiPV(captureTestPVFSType),
			want: captureTestPVFSType,
		},
		{
			name: "block volume reports nothing even when the PV records a type",
			pvc:  claim(&block, captureTestScratchPVName),
			pv:   csiPV(captureTestPVFSType),
			want: "",
		},
		{
			name: "driver recorded no filesystem type",
			pvc:  claim(&filesystem, captureTestScratchPVName),
			pv:   csiPV(""),
			want: "",
		},
		{
			name: "volume is already gone",
			pvc:  claim(&filesystem, captureTestScratchPVName),
			pv:   nil,
			want: "",
		},
		{
			name: "claim is not bound to any volume",
			pvc:  claim(&filesystem, ""),
			pv:   csiPV(captureTestPVFSType),
			want: "",
		},
		{
			// A non-CSI volume (hostPath, NFS, ...) has no fsType field to read at all.
			name: "volume is not a CSI volume",
			pvc:  claim(&filesystem, captureTestScratchPVName),
			pv: &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{Name: captureTestScratchPVName},
				Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
					HostPath: &corev1.HostPathVolumeSource{Path: "/tmp/scratch"},
				}},
			},
			want: "",
		},
	}

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			objects := []client.Object{tt.pvc}
			if tt.pv != nil {
				objects = append(objects, tt.pv)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

			assert.Equal(t, tt.want, scratchVolumeFSType(context.Background(), c, tt.pvc))
		})
	}
	t.Logf("scratch volume filesystem-type cases checked: %d", len(tests))
	require.NotEmpty(t, tests)
}
