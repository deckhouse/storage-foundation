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
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk"
	sfsnapv1 "github.com/deckhouse/storage-foundation/api/snapshot/v1"
)

func newVolumeSnapshotDomainLifecycleReconciler(
	t *testing.T,
	vs *sfsnapv1.VolumeSnapshot,
	coreObjects ...client.Object,
) *VolumeSnapshotDomainReconciler {
	t.Helper()

	coreScheme := apiruntime.NewScheme()
	if err := corev1.AddToScheme(coreScheme); err != nil {
		t.Fatalf("add corev1: %v", err)
	}
	coreClient := fake.NewClientBuilder().WithScheme(coreScheme).WithObjects(coreObjects...).Build()

	snapScheme := apiruntime.NewScheme()
	if err := sfsnapv1.AddToScheme(snapScheme); err != nil {
		t.Fatalf("add sfsnapv1: %v", err)
	}
	if err := ssv1alpha1.AddToScheme(snapScheme); err != nil {
		t.Fatalf("add state-snapshotter v1alpha1: %v", err)
	}
	snapClient := fake.NewClientBuilder().
		WithScheme(snapScheme).
		WithStatusSubresource(&sfsnapv1.VolumeSnapshot{}).
		WithObjects(vs).
		Build()

	return &VolumeSnapshotDomainReconciler{
		Client:     coreClient,
		APIReader:  coreClient,
		SnapClient: snapClient,
	}
}

// rootSnapshotOwner is the controller ownerReference the unified snapshot write-path stamps on a CSI
// VolumeSnapshot it creates, in the same payload as the provenance label. The adoption gate demands it, so
// every fixture that is supposed to be adopted carries it.
func rootSnapshotOwner() metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{
		APIVersion: storagev1alpha1.SchemeGroupVersion.String(),
		Kind:       storagev1alpha1.SnapshotKind,
		Name:       "root",
		UID:        "root-uid",
		Controller: &controller,
	}
}

// provenanceUserInCluster is the origin value the module's admission stamps into the CREATE payload of a
// VolumeSnapshot an ordinary client of the cluster API creates. It is transcribed rather than imported
// because it belongs to the state-snapshotter label vocabulary; the reconciler reads the KEY and never the
// value, so the fixtures carry the real value only to stay faithful to what a live cluster produces.
const provenanceUserInCluster = "user-in-cluster"

// preexistingVolumeSnapshot is a CSI VolumeSnapshot that carries NO origin label at all — the shape of a
// snapshot that was already in the cluster before the module was installed, since nothing was there to
// state where it came from. It deliberately carries the label the patched external-snapshotter fork stamps
// on every snapshot it reconciles — that label is what the reconciler used to adopt on, and pinning it here
// keeps it from becoming a discriminator again.
func preexistingVolumeSnapshot(name, pvcName string) *sfsnapv1.VolumeSnapshot {
	return &sfsnapv1.VolumeSnapshot{
		TypeMeta: metav1.TypeMeta{
			APIVersion: sfsnapv1.SchemeGroupVersion.String(),
			Kind:       sfsnapv1.Kind,
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "test",
			UID:       types.UID(name + "-uid"),
			Labels:    map[string]string{"storage-foundation.deckhouse.io/processed": "true"},
		},
		Spec: sfsnapv1.VolumeSnapshotSpec{
			Source: sfsnapv1.VolumeSnapshotSource{PersistentVolumeClaimName: &pvcName},
		},
	}
}

// userVolumeSnapshot is a CSI VolumeSnapshot this module did not create but DID see created: a client made
// it while the module was running, so admission stamped the user origin into its CREATE payload. That is
// the ordinary user snapshot of a live cluster, and it is captured like any other.
func userVolumeSnapshot(name, pvcName string) *sfsnapv1.VolumeSnapshot {
	vs := preexistingVolumeSnapshot(name, pvcName)
	vs.Labels[storagev1alpha1.LabelProvenance] = provenanceUserInCluster
	return vs
}

func managedCaptureVolumeSnapshot(name, pvcName string) *sfsnapv1.VolumeSnapshot {
	vs := userVolumeSnapshot(name, pvcName)
	vs.OwnerReferences = []metav1.OwnerReference{rootSnapshotOwner()}
	vs.Labels[labelSnapshotManaged] = managedValueTrue
	storagev1alpha1.StampProvenance(vs)
	return vs
}

func reconcileVolumeSnapshot(t *testing.T, r *VolumeSnapshotDomainReconciler, name string) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "test", Name: name},
	})
	if err != nil {
		t.Fatalf("reconcile VolumeSnapshot: %v", err)
	}
	return result
}

func declareCoreLegs(t *testing.T, r *VolumeSnapshotDomainReconciler, name string) {
	t.Helper()
	open := false
	got := getDomainVolumeSnapshot(t, r, name)
	if got.Status.CaptureState == nil {
		got.Status.CaptureState = &storagev1alpha1.CaptureStateStatus{}
	}
	got.Status.CaptureState.CommonController = &storagev1alpha1.CommonControllerCaptureState{
		ManifestCaptured: &open,
		DataCaptured:     &open,
	}
	if err := r.SnapClient.Status().Update(context.Background(), got); err != nil {
		t.Fatalf("declare core capture legs: %v", err)
	}
}

func getDomainVolumeSnapshot(t *testing.T, r *VolumeSnapshotDomainReconciler, name string) *sfsnapv1.VolumeSnapshot {
	t.Helper()
	vs := &sfsnapv1.VolumeSnapshot{}
	if err := r.SnapClient.Get(
		context.Background(),
		types.NamespacedName{Namespace: "test", Name: name},
		vs,
	); err != nil {
		t.Fatalf("get VolumeSnapshot: %v", err)
	}
	return vs
}

// assertVolumeSnapshotUnwritten asserts the reconcile left the object exactly as it found it and created
// nothing around it. resourceVersion is the assertion that generalises: it moves on ANY write, so it also
// catches a marker this test does not know to look for, a status patch, or a negative managed=false latch —
// all of which are the same error, a foreign object taken into this module's lifecycle.
func assertVolumeSnapshotUnwritten(t *testing.T, r *VolumeSnapshotDomainReconciler, before *sfsnapv1.VolumeSnapshot) {
	t.Helper()
	after := getDomainVolumeSnapshot(t, r, before.Name)
	if after.ResourceVersion != before.ResourceVersion {
		t.Fatalf("VolumeSnapshot was written: resourceVersion %q -> %q, labels %#v -> %#v",
			before.ResourceVersion, after.ResourceVersion, before.Labels, after.Labels)
	}
	requests := &ssv1alpha1.ManifestCaptureRequestList{}
	if err := r.SnapClient.List(context.Background(), requests, client.InNamespace("test")); err != nil {
		t.Fatalf("list ManifestCaptureRequests: %v", err)
	}
	if len(requests.Items) != 0 {
		t.Fatalf("ManifestCaptureRequests = %d, want 0: no capture may be planned for a snapshot we do not own", len(requests.Items))
	}
}

// TestVolumeSnapshotDomainCapturesAUserSnapshot pins the half of the model that is easy to lose while
// fixing the other one: a snapshot nobody owns, created while the module was there to see it, is a user's
// own snapshot, and it gets the full domain capture — the managed latch, the manifest request, the
// published source. Being exportable is not a reward for having been created by this module.
//
// Two things it must NOT get, and they are separate. Protection: the original bug was not "the module
// looked at a user's snapshot", it was "the module made a user's snapshot undeletable". And our own origin:
// the object arrives stating that a client created it, and capture must leave that statement alone rather
// than upgrade it into authorship — the data of this snapshot belongs to whoever made it, and the core
// decides that by exactly this value.
func TestVolumeSnapshotDomainCapturesAUserSnapshot(t *testing.T) {
	vs := userVolumeSnapshot("snapshot-user", "source-pvc")
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "source-pvc", Namespace: "test", UID: "pvc-uid"},
	}
	r := newVolumeSnapshotDomainLifecycleReconciler(t, vs, pvc)

	reconcileVolumeSnapshot(t, r, vs.Name)
	declareCoreLegs(t, r, vs.Name)
	reconcileVolumeSnapshot(t, r, vs.Name)

	got := getDomainVolumeSnapshot(t, r, vs.Name)
	if got.Labels[labelSnapshotManaged] != managedValueTrue {
		t.Fatalf("managed label = %q, want %q", got.Labels[labelSnapshotManaged], managedValueTrue)
	}
	if storagev1alpha1.IsDeleteProtected(got) {
		t.Fatalf("a user's snapshot must stay deletable, labels=%#v", got.Labels)
	}
	if storagev1alpha1.HasProvenance(got) {
		t.Fatalf("capture must not claim authorship of a snapshot it did not create, labels=%#v", got.Labels)
	}
	if got.Labels[storagev1alpha1.LabelProvenance] != provenanceUserInCluster {
		t.Fatalf("the origin stated at creation must survive capture untouched, labels=%#v", got.Labels)
	}
	if state := (volumeSnapshotAdapter{snap: got}).GetDomainCaptureState(); state.ManifestCaptureRequestName == "" {
		t.Fatalf("manifestCaptureRequestName must be published for a user snapshot too, state=%#v", state)
	}
	if got.Status.SourceRef == nil || got.Status.SourceRef.UID != pvc.UID {
		t.Fatalf("sourceRef = %#v, want the source PVC identity", got.Status.SourceRef)
	}
}

// TestVolumeSnapshotDomainVetoesForeignControllerOwner is the regression for the reported bug, and the one
// gate that survived: a snapshot another controller owns — a virtualization VirtualDiskSnapshot here — is
// left completely alone. The source PVC exists, so every later stage of the reconcile would have something
// to do; the object survives untouched only because the veto stops the reconcile before any write.
//
// The fixture also carries our provenance label, which is deliberate: provenance no longer buys anything at
// this gate, so a forged one cannot talk the reconcile past a foreign owner.
func TestVolumeSnapshotDomainVetoesForeignControllerOwner(t *testing.T) {
	foreignController := true
	vs := userVolumeSnapshot("snapshot-foreign-owner", "source-pvc")
	storagev1alpha1.StampProvenance(vs)
	vs.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "virtualization.deckhouse.io/v1alpha2",
		Kind:       "VirtualDiskSnapshot",
		Name:       "vds",
		UID:        "vds-uid",
		Controller: &foreignController,
	}}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "source-pvc", Namespace: "test", UID: "pvc-uid"},
	}
	r := newVolumeSnapshotDomainLifecycleReconciler(t, vs, pvc)
	before := getDomainVolumeSnapshot(t, r, vs.Name)

	if result := reconcileVolumeSnapshot(t, r, vs.Name); result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("result = %#v, want no requeue", result)
	}
	assertVolumeSnapshotUnwritten(t, r, before)
}

// TestVolumeSnapshotDomainAdoptsItsOwnSnapshot is the positive counterpart: a snapshot created by the write
// -path — our provenance, controlled by our Snapshot — is still adopted, latched and planned. It also pins
// that adoption does not manufacture protection: the fixture arrives without the protection label (its
// creator normally puts it in the CREATE payload) and must come out of the reconcile without one, because a
// reconciler that adds it here is a reconciler that would add it to anything it observes.
func TestVolumeSnapshotDomainAdoptsItsOwnSnapshot(t *testing.T) {
	vs := userVolumeSnapshot("snapshot-ours", "source-pvc")
	vs.OwnerReferences = []metav1.OwnerReference{rootSnapshotOwner()}
	storagev1alpha1.StampProvenance(vs)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "source-pvc", Namespace: "test", UID: "pvc-uid"},
	}
	r := newVolumeSnapshotDomainLifecycleReconciler(t, vs, pvc)

	reconcileVolumeSnapshot(t, r, vs.Name)
	declareCoreLegs(t, r, vs.Name)
	reconcileVolumeSnapshot(t, r, vs.Name)

	got := getDomainVolumeSnapshot(t, r, vs.Name)
	if got.Labels[labelSnapshotManaged] != managedValueTrue {
		t.Fatalf("managed label = %q, want %q", got.Labels[labelSnapshotManaged], managedValueTrue)
	}
	if storagev1alpha1.IsDeleteProtected(got) {
		t.Fatalf("protection must come from the creator, not from adoption, labels=%#v", got.Labels)
	}
	if state := (volumeSnapshotAdapter{snap: got}).GetDomainCaptureState(); state.ManifestCaptureRequestName == "" {
		t.Fatalf("manifestCaptureRequestName must be published for an adopted snapshot, state=%#v", state)
	}
}

func TestVolumeSnapshotDomainWaitsInPlanningForMissingPVC(t *testing.T) {
	vs := managedCaptureVolumeSnapshot("snapshot-waiting", "source-pvc")
	r := newVolumeSnapshotDomainLifecycleReconciler(t, vs)

	result := reconcileVolumeSnapshot(t, r, vs.Name)
	if result.RequeueAfter != volumeSnapshotDomainArtifactRequeueAfter {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, volumeSnapshotDomainArtifactRequeueAfter)
	}

	got := getDomainVolumeSnapshot(t, r, vs.Name)
	state := volumeSnapshotAdapter{snap: got}.GetDomainCaptureState()
	if state.Phase != snapshotsdk.PhasePlanning {
		t.Fatalf("phase = %q, want %q", state.Phase, snapshotsdk.PhasePlanning)
	}
	if !strings.Contains(state.Message, `PersistentVolumeClaim "source-pvc"`) {
		t.Fatalf("message = %q, want missing PVC diagnostic", state.Message)
	}

	requests := &ssv1alpha1.ManifestCaptureRequestList{}
	if err := r.SnapClient.List(context.Background(), requests, client.InNamespace("test")); err != nil {
		t.Fatalf("list ManifestCaptureRequests: %v", err)
	}
	if len(requests.Items) != 0 {
		t.Fatalf("ManifestCaptureRequests = %d, want 0", len(requests.Items))
	}
}

func TestVolumeSnapshotDomainPublishesFinishedWhenCoreCaptureCompletes(t *testing.T) {
	captured := false
	vs := managedCaptureVolumeSnapshot("snapshot-finished", "source-pvc")
	vs.Status.CaptureState = &storagev1alpha1.CaptureStateStatus{
		CommonController: &storagev1alpha1.CommonControllerCaptureState{
			ManifestCaptured: &captured,
			DataCaptured:     &captured,
		},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "source-pvc", Namespace: "test", UID: "pvc-uid"},
	}
	r := newVolumeSnapshotDomainLifecycleReconciler(t, vs, pvc)

	result := reconcileVolumeSnapshot(t, r, vs.Name)
	if result.RequeueAfter != volumeSnapshotDomainRequeueAfter {
		t.Fatalf("initial RequeueAfter = %s, want %s", result.RequeueAfter, volumeSnapshotDomainRequeueAfter)
	}

	got := getDomainVolumeSnapshot(t, r, vs.Name)
	state := volumeSnapshotAdapter{snap: got}.GetDomainCaptureState()
	if state.Phase != snapshotsdk.PhasePlanned {
		t.Fatalf("initial phase = %q, want %q", state.Phase, snapshotsdk.PhasePlanned)
	}
	if state.ManifestCaptureRequestName == "" {
		t.Fatal("manifestCaptureRequestName must be published")
	}
	if got.Status.SourceRef == nil || got.Status.SourceRef.Name != pvc.Name || got.Status.SourceRef.UID != pvc.UID {
		t.Fatalf("sourceRef = %#v, want PVC identity", got.Status.SourceRef)
	}

	captured = true
	got.Status.CaptureState.CommonController.ManifestCaptured = &captured
	got.Status.CaptureState.CommonController.DataCaptured = &captured
	if err := r.SnapClient.Status().Update(context.Background(), got); err != nil {
		t.Fatalf("update core capture latches: %v", err)
	}

	result = reconcileVolumeSnapshot(t, r, vs.Name)
	if result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("completed result = %#v, want no requeue", result)
	}
	got = getDomainVolumeSnapshot(t, r, vs.Name)
	state = volumeSnapshotAdapter{snap: got}.GetDomainCaptureState()
	if state.Phase != snapshotsdk.PhaseFinished {
		t.Fatalf("completed phase = %q, want %q", state.Phase, snapshotsdk.PhaseFinished)
	}
}

// TestVolumeSnapshotDomainRepublishesLostSourceRef pins the repair of a status field this reconciler is the
// only writer of but not the only writer of the object's status: external-snapshotter replaces the status
// from its own model, which has no notion of sourceRef, so a published reference can disappear. The cost of
// treating that as permanent is not cosmetic — the core withholds the native-CSI data leg while the source
// is unnamed — so an absent reference reopens the write on its own, without redoing planning: the MCR name
// already published must stay exactly as it is.
//
// The second reconcile pins the other half: once the value is back, the repair writes nothing at all, so the
// steady state of a planned snapshot stays write-free.
func TestVolumeSnapshotDomainRepublishesLostSourceRef(t *testing.T) {
	captured := false
	vs := managedCaptureVolumeSnapshot("snapshot-lost-source", "source-pvc")
	vs.Status.CaptureState = &storagev1alpha1.CaptureStateStatus{
		CommonController: &storagev1alpha1.CommonControllerCaptureState{
			ManifestCaptured: &captured,
			DataCaptured:     &captured,
		},
		DomainSpecificController: &storagev1alpha1.DomainSpecificControllerCaptureState{
			ManifestCaptureRequestName: "existing-mcr",
			Phase:                      snapshotsdk.PhasePlanned,
		},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "source-pvc", Namespace: "test", UID: "pvc-uid"},
	}
	r := newVolumeSnapshotDomainLifecycleReconciler(t, vs, pvc)

	reconcileVolumeSnapshot(t, r, vs.Name)

	got := getDomainVolumeSnapshot(t, r, vs.Name)
	if got.Status.SourceRef == nil || got.Status.SourceRef.Name != pvc.Name || got.Status.SourceRef.UID != pvc.UID {
		t.Fatalf("sourceRef = %#v, want the source PVC identity republished", got.Status.SourceRef)
	}
	if state := (volumeSnapshotAdapter{snap: got}).GetDomainCaptureState(); state.ManifestCaptureRequestName != "existing-mcr" {
		t.Fatalf("manifestCaptureRequestName = %q, want the published name untouched", state.ManifestCaptureRequestName)
	}
	requests := &ssv1alpha1.ManifestCaptureRequestList{}
	if err := r.SnapClient.List(context.Background(), requests, client.InNamespace("test")); err != nil {
		t.Fatalf("list ManifestCaptureRequests: %v", err)
	}
	if len(requests.Items) != 0 {
		t.Fatalf("ManifestCaptureRequests = %d, want 0: repairing the source must not replan the manifest leg", len(requests.Items))
	}

	reconcileVolumeSnapshot(t, r, vs.Name)

	after := getDomainVolumeSnapshot(t, r, vs.Name)
	if after.ResourceVersion != got.ResourceVersion {
		t.Fatalf("second reconcile wrote the object: resourceVersion %q -> %q, want the repair to be a no-op once equal",
			got.ResourceVersion, after.ResourceVersion)
	}
}

func TestVolumeSnapshotDomainStopsOnCoreFailureAfterPlannedWithoutLivePVC(t *testing.T) {
	captured := false
	vs := managedCaptureVolumeSnapshot("snapshot-core-failed", "deleted-pvc")
	vs.Status.CaptureState = &storagev1alpha1.CaptureStateStatus{
		CommonController: &storagev1alpha1.CommonControllerCaptureState{
			ManifestCaptured: &captured,
			DataCaptured:     &captured,
		},
		DomainSpecificController: &storagev1alpha1.DomainSpecificControllerCaptureState{
			ManifestCaptureRequestName: "existing-mcr",
			Phase:                      snapshotsdk.PhasePlanned,
			ExcludedRefs:               []storagev1alpha1.ExcludedObjectRef{},
		},
	}
	vs.Status.Conditions = []metav1.Condition{{
		Type:    storagev1alpha1.ConditionReady,
		Status:  metav1.ConditionFalse,
		Reason:  "VolumeCaptureFailed",
		Message: "native CSI capture failed",
	}}
	r := newVolumeSnapshotDomainLifecycleReconciler(t, vs)

	result := reconcileVolumeSnapshot(t, r, vs.Name)
	if result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("result = %#v, want no requeue", result)
	}

	got := getDomainVolumeSnapshot(t, r, vs.Name)
	state := volumeSnapshotAdapter{snap: got}.GetDomainCaptureState()
	if state.Phase != snapshotsdk.PhasePlanned {
		t.Fatalf("phase = %q, want core-owned failure to leave %q", state.Phase, snapshotsdk.PhasePlanned)
	}
	if state.Reason != "" || state.Message != "" {
		t.Fatalf("domain reason/message = %q/%q, want core failure left solely in Ready", state.Reason, state.Message)
	}
}

// TestVolumeSnapshotDomainSkipsASnapshotItNeverSawCreated is the gate for snapshots a cluster already had.
// Origin is stated once, into the CREATE payload, by whoever is present at the birth — the write-path for a
// tree node, the module's admission for a client's own snapshot — so an object with no origin key at all is
// one that was created before any of that existed. Managing it would mean the module taking over every
// snapshot found in the cluster the day it was installed.
//
// The fixture gives the reconcile every reason to act: the source PVC exists, nothing owns the snapshot,
// nothing vetoes it. Only the missing origin stops it, and the assertion is that NOTHING was written —
// not even the managed latch, which is the first write the reconcile would make.
func TestVolumeSnapshotDomainSkipsASnapshotItNeverSawCreated(t *testing.T) {
	vs := preexistingVolumeSnapshot("snapshot-preexisting", "source-pvc")
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "source-pvc", Namespace: "test", UID: "pvc-uid"},
	}
	r := newVolumeSnapshotDomainLifecycleReconciler(t, vs, pvc)
	before := getDomainVolumeSnapshot(t, r, vs.Name)

	if result := reconcileVolumeSnapshot(t, r, vs.Name); result.Requeue || result.RequeueAfter != 0 {
		t.Fatalf("result = %#v, want no requeue", result)
	}
	assertVolumeSnapshotUnwritten(t, r, before)
}

// TestVolumeSnapshotDomainSkipsImportModeAndPreProvisioned covers the two gates that answer a snapshot on
// WHAT IT IS rather than on where it came from: an import-mode snapshot has no live source to capture (the
// core materializes its content from uploaded manifests), and a pre-provisioned one names no source claim at
// all. Both fixtures carry an origin, so the gate under test is the only thing that can stop them —
// otherwise the reconcile would run the managed latch and plan a capture over a source that is not there.
//
// These two are also why the origin gate is placed BELOW them. The mutating admission that states an origin
// is scoped to VolumeSnapshots with a PVC source, so neither of these shapes can ever carry the key, however
// recently it was created; judged by origin they would every one of them report as predating the module.
// The outcome would be the same skip, which is exactly why the placement cannot be pinned by a test: what
// changes is the reason the module gives for it, and a reason that is false is what makes the next change to
// this function reason from the wrong premise.
func TestVolumeSnapshotDomainSkipsImportModeAndPreProvisioned(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "source-pvc", Namespace: "test", UID: "pvc-uid"},
	}

	cases := []struct {
		name    string
		prepare func(vs *sfsnapv1.VolumeSnapshot)
	}{
		{
			name: "import mode",
			prepare: func(vs *sfsnapv1.VolumeSnapshot) {
				vs.Spec.Mode = sfsnapv1.VolumeSnapshotModeImport
			},
		},
		{
			name: "pre-provisioned (no source claim)",
			prepare: func(vs *sfsnapv1.VolumeSnapshot) {
				empty := ""
				vs.Spec.Source.PersistentVolumeClaimName = &empty
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vs := userVolumeSnapshot("snapshot-"+strings.ReplaceAll(tc.name, " ", "-"), "source-pvc")
			tc.prepare(vs)
			r := newVolumeSnapshotDomainLifecycleReconciler(t, vs, pvc)
			before := getDomainVolumeSnapshot(t, r, vs.Name)

			if result := reconcileVolumeSnapshot(t, r, vs.Name); result.Requeue || result.RequeueAfter != 0 {
				t.Fatalf("result = %#v, want no requeue", result)
			}
			assertVolumeSnapshotUnwritten(t, r, before)
		})
	}
	t.Logf("checked %d snapshot shapes that carry no live source", len(cases))
}
