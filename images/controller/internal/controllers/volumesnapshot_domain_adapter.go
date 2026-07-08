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

package controllers

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk"
	sfsnapv1 "github.com/deckhouse/storage-foundation/api/snapshot/v1"
)

// volumeSnapshotAdapter maps the Deckhouse-extended CSI VolumeSnapshot to the generic capture protocol.
// The VolumeSnapshot is a data leaf (no child snapshots); its data leg is the native CSI
// VolumeSnapshotContent (NOT a storage-foundation VolumeCaptureRequest), so the reconciler never calls
// EnsureVolumeCapture and this adapter never publishes a VolumeCaptureRequestName.
//
// Writer discipline (identical to the reference demo adapter): the SDK writes ONLY
// status.captureState.domainSpecificController (Get/SetDomainCaptureState), status.childrenSnapshotRefs
// (via the same — always empty for a leaf), and status.snapshotSource (Get/SetSnapshotSource). It NEVER
// writes the Ready condition and NEVER writes the core-owned captureState.commonController — it only reads
// them (CoreCaptureState, ReadyReason/ReadyMessage).
type volumeSnapshotAdapter struct {
	snap *sfsnapv1.VolumeSnapshot
}

func (a volumeSnapshotAdapter) Object() client.Object { return a.snap }

// SourceRef is empty: a VolumeSnapshot carries no generic spec.sourceRef. Its source is the PVC, resolved
// directly by the reconciler from spec.source.persistentVolumeClaimName.
func (a volumeSnapshotAdapter) SourceRef() snapshotsdk.SourceRef {
	return snapshotsdk.SourceRef{}
}

func (a volumeSnapshotAdapter) GetDomainCaptureState() snapshotsdk.DomainCaptureState {
	st := snapshotsdk.DomainCaptureState{ChildrenSnapshotRefs: a.snap.Status.ChildrenSnapshotRefs}
	if cs := a.snap.Status.CaptureState; cs != nil && cs.DomainSpecificController != nil {
		d := cs.DomainSpecificController
		st.ManifestCaptureRequestName = d.ManifestCaptureRequestName
		st.VolumeCaptureRequestName = d.VolumeCaptureRequestName
		st.ExcludedRefs = d.ExcludedRefs
		st.Phase = d.Phase
		st.Reason = d.Reason
		st.Message = d.Message
	}
	return st
}

func (a volumeSnapshotAdapter) SetDomainCaptureState(st snapshotsdk.DomainCaptureState) {
	a.snap.Status.ChildrenSnapshotRefs = st.ChildrenSnapshotRefs
	ensureVSDomainSpecificController(&a.snap.Status.CaptureState)
	d := a.snap.Status.CaptureState.DomainSpecificController
	d.ManifestCaptureRequestName = st.ManifestCaptureRequestName
	d.VolumeCaptureRequestName = st.VolumeCaptureRequestName
	d.ExcludedRefs = vsNonNilExcludedRefs(st.ExcludedRefs)
	d.Phase = st.Phase
	d.Reason = st.Reason
	d.Message = st.Message
}

func (a volumeSnapshotAdapter) GetSnapshotSource() *snapshotsdk.SnapshotSource {
	return a.snap.Status.SnapshotSource
}

func (a volumeSnapshotAdapter) SetSnapshotSource(src *snapshotsdk.SnapshotSource) {
	a.snap.Status.SnapshotSource = src
}

func (a volumeSnapshotAdapter) CoreCaptureState() snapshotsdk.CoreCaptureState {
	return vsCoreCaptureStateFrom(a.snap.Status.CaptureState)
}

func (a volumeSnapshotAdapter) ReadyReason() string {
	return vsReadyReason(a.snap.Status.Conditions)
}

func (a volumeSnapshotAdapter) ReadyMessage() string {
	return vsReadyMessage(a.snap.Status.Conditions)
}

// vsNonNilExcludedRefs guarantees a non-nil slice: excludedRefs is written WITHOUT omitempty, so a nil
// slice would marshal to JSON null and be rejected by the non-nullable CRD array. An empty [] is the
// correct "domain planned, nothing excluded" wire value, which a data leaf always writes.
func vsNonNilExcludedRefs(refs []storagev1alpha1.ExcludedObjectRef) []storagev1alpha1.ExcludedObjectRef {
	if refs == nil {
		return []storagev1alpha1.ExcludedObjectRef{}
	}
	return refs
}

// ensureVSDomainSpecificController lazily allocates captureState + its domain-written half. The
// core-written commonController half is never touched here.
func ensureVSDomainSpecificController(cs **storagev1alpha1.CaptureStateStatus) {
	if *cs == nil {
		*cs = &storagev1alpha1.CaptureStateStatus{}
	}
	if (*cs).DomainSpecificController == nil {
		(*cs).DomainSpecificController = &storagev1alpha1.DomainSpecificControllerCaptureState{}
	}
}

// vsCoreCaptureStateFrom reads the core-written capture-leg latches (captureState.commonController) into
// the SDK's read-only view. Absent sub-structure => both legs nil (no leg declared yet).
func vsCoreCaptureStateFrom(cs *storagev1alpha1.CaptureStateStatus) snapshotsdk.CoreCaptureState {
	if cs == nil || cs.CommonController == nil {
		return snapshotsdk.CoreCaptureState{}
	}
	return snapshotsdk.CoreCaptureState{
		ManifestCaptured: cs.CommonController.ManifestCaptured,
		DataCaptured:     cs.CommonController.DataCaptured,
	}
}

func vsReadyReason(conditions []metav1.Condition) string {
	if c := meta.FindStatusCondition(conditions, storagev1alpha1.ConditionReady); c != nil {
		return c.Reason
	}
	return ""
}

func vsReadyMessage(conditions []metav1.Condition) string {
	if c := meta.FindStatusCondition(conditions, storagev1alpha1.ConditionReady); c != nil {
		return c.Message
	}
	return ""
}
