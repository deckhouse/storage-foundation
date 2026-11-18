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

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/images/controller/pkg/config"
)

var _ = Describe("VolumeRestoreRequest source namespace", func() {
	const (
		requestNamespace = "team-a"
		foreignNamespace = "team-b"
		moduleNamespace  = "d8-storage-foundation"
	)

	var (
		ctx context.Context
		cl  client.Client
		r   *VolumeRestoreRequestController
		cfg *config.Options
		rec *events.FakeRecorder
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme := runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(storagev1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(snapshotv1.AddToScheme(scheme)).To(Succeed())
		Expect(deckhousev1alpha1.AddToScheme(scheme)).To(Succeed())
		cfg = &config.Options{ControllerNamespace: moduleNamespace}
		cl = fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&snapshotv1.VolumeSnapshotContent{}).
			WithStatusSubresource(&storagev1alpha1.VolumeRestoreRequest{}).
			Build()
		rec = events.NewFakeRecorder(10)
		r = &VolumeRestoreRequestController{Client: cl, APIReader: cl, Scheme: scheme, Config: cfg, Recorder: rec}
	})

	drainEvents := func() []string {
		var got []string
		for {
			select {
			case e := <-rec.Events:
				got = append(got, e)
			default:
				return got
			}
		}
	}

	controllerRef := func(apiVersion, kind, name string, uid types.UID) []metav1.OwnerReference {
		return []metav1.OwnerReference{{APIVersion: apiVersion, Kind: kind, Name: name, UID: uid, Controller: ptr.To(true)}}
	}

	createVSC := func(name, refNamespace string, owners []metav1.OwnerReference) {
		vsc := &snapshotv1.VolumeSnapshotContent{
			ObjectMeta: metav1.ObjectMeta{Name: name, OwnerReferences: owners},
			Spec: snapshotv1.VolumeSnapshotContentSpec{
				Driver:            "test-driver",
				Source:            snapshotv1.VolumeSnapshotContentSource{VolumeHandle: ptr.To("handle")},
				VolumeSnapshotRef: corev1.ObjectReference{Namespace: refNamespace},
			},
			Status: &snapshotv1.VolumeSnapshotContentStatus{ReadyToUse: ptr.To(true)},
		}
		Expect(cl.Create(ctx, vsc)).To(Succeed())
	}

	createPV := func(name, claimNamespace string, owners []metav1.OwnerReference) {
		pv := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: name, OwnerReferences: owners},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{Driver: "test-driver", VolumeHandle: "handle"},
				},
			},
		}
		if claimNamespace != "" {
			pv.Spec.ClaimRef = &corev1.ObjectReference{Namespace: claimNamespace, Name: "claim"}
		}
		Expect(cl.Create(ctx, pv)).To(Succeed())
	}

	ownerRef := func(apiVersion, kind, name string, uid types.UID) metav1.OwnerReference {
		return metav1.OwnerReference{APIVersion: apiVersion, Kind: kind, Name: name, UID: uid}
	}

	createObjectKeeper := func(name, followNamespace string) types.UID {
		keeper := &deckhousev1alpha1.ObjectKeeper{
			ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name + "-uid")},
			Spec: deckhousev1alpha1.ObjectKeeperSpec{
				Mode: "FollowObject",
				FollowObjectRef: &deckhousev1alpha1.FollowObjectRef{
					APIVersion: APIGroupStorageDeckhouse,
					Kind:       "VolumeCaptureRequest",
					Namespace:  followNamespace,
					Name:       "vcr",
					UID:        "vcr-uid",
				},
			},
		}
		Expect(cl.Create(ctx, keeper)).To(Succeed())
		return keeper.UID
	}

	createSnapshotContent := func(name, snapshotNamespace, sourceNamespace string) types.UID {
		content := &unstructured.Unstructured{}
		content.SetGroupVersionKind(snapshotContentGVK)
		content.SetName(name)
		content.SetUID(types.UID(name + "-uid"))
		if snapshotNamespace != "" {
			Expect(unstructured.SetNestedField(content.Object, snapshotNamespace, "spec", "snapshotRef", "namespace")).To(Succeed())
		}
		if sourceNamespace != "" {
			Expect(unstructured.SetNestedField(content.Object, sourceNamespace, "status", "data", "sourceRef", "namespace")).To(Succeed())
		}
		Expect(cl.Create(ctx, content)).To(Succeed())
		return content.GetUID()
	}

	createAnnotatedVRR := func(namespace, kind, source string, annotations map[string]string) *storagev1alpha1.VolumeRestoreRequest {
		vrr := &storagev1alpha1.VolumeRestoreRequest{
			ObjectMeta: metav1.ObjectMeta{Name: "vrr", Namespace: namespace, UID: "vrr-uid", Generation: 3, Annotations: annotations},
			Spec: storagev1alpha1.VolumeRestoreRequestSpec{
				SourceRef: storagev1alpha1.ObjectReference{Kind: kind, Name: source},
				PvcTemplate: storagev1alpha1.PersistentVolumeClaimTemplateSpec{
					PersistentVolumeClaimTemplateMetadata: storagev1alpha1.PersistentVolumeClaimTemplateMetadata{Name: "restored"},
				},
			},
		}
		Expect(cl.Create(ctx, vrr)).To(Succeed())
		return vrr
	}

	createVRR := func(namespace, kind, source string) *storagev1alpha1.VolumeRestoreRequest {
		return createAnnotatedVRR(namespace, kind, source, nil)
	}

	exportOf := func(namespace string) map[string]string {
		return map[string]string{storagev1alpha1.AnnotationStorageManagerNamespaceKey: namespace}
	}

	reconcileVRR := func(namespace string) *storagev1alpha1.VolumeRestoreRequest {
		key := types.NamespacedName{Namespace: namespace, Name: "vrr"}
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).ToNot(HaveOccurred())
		out := &storagev1alpha1.VolumeRestoreRequest{}
		Expect(cl.Get(ctx, key, out)).To(Succeed())
		return out
	}

	expectFailedForeign := func(vrr *storagev1alpha1.VolumeRestoreRequest) {
		ready := meta.FindStatusCondition(vrr.Status.Conditions, storagev1alpha1.ConditionTypeReady)
		Expect(ready).ToNot(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal(storagev1alpha1.ConditionReasonForeignSource))
		Expect(meta.FindStatusCondition(vrr.Status.Conditions, storagev1alpha1.ConditionTypeSourceVerified)).To(BeNil())
	}

	expectRejected := func(vrr *storagev1alpha1.VolumeRestoreRequest) {
		expectFailedForeign(vrr)
		got := drainEvents()
		Expect(got).To(HaveLen(1))
		Expect(got[0]).To(HavePrefix("Warning " + storagev1alpha1.ConditionReasonForeignSource + " "))
		err := cl.Get(ctx, client.ObjectKey{Name: NamePrefixRetainerPVC + string(vrr.UID)}, &deckhousev1alpha1.ObjectKeeper{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "no ObjectKeeper may be created for a rejected request")
	}

	expectVerified := func(vrr *storagev1alpha1.VolumeRestoreRequest, reason string) {
		Expect(meta.FindStatusCondition(vrr.Status.Conditions, storagev1alpha1.ConditionTypeReady)).To(BeNil())
		verified := meta.FindStatusCondition(vrr.Status.Conditions, storagev1alpha1.ConditionTypeSourceVerified)
		Expect(verified).ToNot(BeNil())
		Expect(verified.Status).To(Equal(metav1.ConditionTrue))
		Expect(verified.Reason).To(Equal(reason))
		Expect(verified.ObservedGeneration).To(Equal(vrr.Generation))
		Expect(drainEvents()).To(BeEmpty())
	}

	It("rejects a VolumeSnapshotContent bound to a VolumeSnapshot of another namespace", func() {
		createVSC("vsc", foreignNamespace, nil)
		createVRR(requestNamespace, SourceKindVolumeSnapshotContent, "vsc")
		expectRejected(reconcileVRR(requestNamespace))
	})

	It("accepts a VolumeSnapshotContent bound to a VolumeSnapshot of the request namespace", func() {
		createVSC("vsc", requestNamespace, nil)
		createVRR(requestNamespace, SourceKindVolumeSnapshotContent, "vsc")
		expectVerified(reconcileVRR(requestNamespace), storagev1alpha1.ConditionReasonSourceInNamespace)
	})

	It("rejects a PersistentVolume claimed from another namespace", func() {
		createPV("pv", foreignNamespace, nil)
		createVRR(requestNamespace, SourceKindPersistentVolume, "pv")
		expectRejected(reconcileVRR(requestNamespace))
	})

	It("accepts a PersistentVolume claimed from the request namespace", func() {
		createPV("pv", requestNamespace, nil)
		createVRR(requestNamespace, SourceKindPersistentVolume, "pv")
		expectVerified(reconcileVRR(requestNamespace), storagev1alpha1.ConditionReasonSourceInNamespace)
	})

	It("accepts an unbound VolumeSnapshotContent captured by a request of the same namespace", func() {
		uid := createObjectKeeper("ret-vcr", requestNamespace)
		createVSC("vsc", "", controllerRef(APIGroupDeckhouse, KindObjectKeeper, "ret-vcr", uid))
		createVRR(requestNamespace, SourceKindVolumeSnapshotContent, "vsc")
		expectVerified(reconcileVRR(requestNamespace), storagev1alpha1.ConditionReasonSourceInNamespace)
	})

	It("rejects an unbound VolumeSnapshotContent captured by a request of another namespace", func() {
		uid := createObjectKeeper("ret-vcr", foreignNamespace)
		createVSC("vsc", "", controllerRef(APIGroupDeckhouse, KindObjectKeeper, "ret-vcr", uid))
		createVRR(requestNamespace, SourceKindVolumeSnapshotContent, "vsc")
		expectRejected(reconcileVRR(requestNamespace))
	})

	It("rejects a bound VolumeSnapshotContent of another namespace whatever owns it", func() {
		uid := createObjectKeeper("ret-vcr", requestNamespace)
		createVSC("vsc", foreignNamespace, controllerRef(APIGroupDeckhouse, KindObjectKeeper, "ret-vcr", uid))
		createVRR(requestNamespace, SourceKindVolumeSnapshotContent, "vsc")
		expectRejected(reconcileVRR(requestNamespace))
	})

	It("rejects a source whose ObjectKeeper owner reference does not match the keeper UID", func() {
		createObjectKeeper("ret-vcr", requestNamespace)
		createVSC("vsc", "", controllerRef(APIGroupDeckhouse, KindObjectKeeper, "ret-vcr", "other-uid"))
		createVRR(requestNamespace, SourceKindVolumeSnapshotContent, "vsc")
		expectRejected(reconcileVRR(requestNamespace))
	})

	It("accepts a detached PersistentVolume handed over to a SnapshotContent of the same namespace", func() {
		uid := createSnapshotContent("content", requestNamespace, foreignNamespace)
		createPV("pv", "", controllerRef("state-snapshotter.deckhouse.io/v1alpha1", "SnapshotContent", "content", uid))
		createVRR(requestNamespace, SourceKindPersistentVolume, "pv")
		expectVerified(reconcileVRR(requestNamespace), storagev1alpha1.ConditionReasonSourceInNamespace)
	})

	It("falls back to the captured claim namespace of a SnapshotContent without a snapshot reference", func() {
		uid := createSnapshotContent("content", "", requestNamespace)
		createPV("pv", "", controllerRef("state-snapshotter.deckhouse.io/v1alpha1", "SnapshotContent", "content", uid))
		createVRR(requestNamespace, SourceKindPersistentVolume, "pv")
		expectVerified(reconcileVRR(requestNamespace), storagev1alpha1.ConditionReasonSourceInNamespace)
	})

	It("rejects a detached PersistentVolume handed over to a SnapshotContent of another namespace", func() {
		uid := createSnapshotContent("content", foreignNamespace, requestNamespace)
		createPV("pv", "", controllerRef("state-snapshotter.deckhouse.io/v1alpha1", "SnapshotContent", "content", uid))
		createVRR(requestNamespace, SourceKindPersistentVolume, "pv")
		expectRejected(reconcileVRR(requestNamespace))
	})

	It("rejects a source whose namespace cannot be determined", func() {
		createPV("pv", "", nil)
		createVRR(requestNamespace, SourceKindPersistentVolume, "pv")
		expectRejected(reconcileVRR(requestNamespace))
	})

	It("rejects a source owned by an object of an unknown kind", func() {
		createVSC("vsc", "", controllerRef("v1", "ConfigMap", "cm", "cm-uid"))
		createVRR(requestNamespace, SourceKindVolumeSnapshotContent, "vsc")
		expectRejected(reconcileVRR(requestNamespace))
	})

	It("accepts a source of the exported namespace for a request in the module namespace", func() {
		createVSC("vsc", foreignNamespace, nil)
		createAnnotatedVRR(moduleNamespace, SourceKindVolumeSnapshotContent, "vsc", exportOf(foreignNamespace))
		expectVerified(reconcileVRR(moduleNamespace), storagev1alpha1.ConditionReasonModuleNamespace)
	})

	It("accepts an artifact of the exported namespace for a request in the module namespace", func() {
		uid := createSnapshotContent("content", requestNamespace, "")
		createVSC("vsc", "", controllerRef("state-snapshotter.deckhouse.io/v1alpha1", "SnapshotContent", "content", uid))
		createAnnotatedVRR(moduleNamespace, SourceKindVolumeSnapshotContent, "vsc", exportOf(requestNamespace))
		expectVerified(reconcileVRR(moduleNamespace), storagev1alpha1.ConditionReasonModuleNamespace)
	})

	It("rejects a source of another namespace than the exported one for a request in the module namespace", func() {
		createVSC("vsc", foreignNamespace, nil)
		createAnnotatedVRR(moduleNamespace, SourceKindVolumeSnapshotContent, "vsc", exportOf(requestNamespace))
		expectRejected(reconcileVRR(moduleNamespace))
	})

	It("rejects an artifact of another namespace than the exported one for a request in the module namespace", func() {
		uid := createObjectKeeper("ret-vcr", foreignNamespace)
		createVSC("vsc", "", controllerRef(APIGroupDeckhouse, KindObjectKeeper, "ret-vcr", uid))
		createAnnotatedVRR(moduleNamespace, SourceKindVolumeSnapshotContent, "vsc", exportOf(requestNamespace))
		expectRejected(reconcileVRR(moduleNamespace))
	})

	It("rejects a request in the module namespace that does not name the exported namespace", func() {
		createVSC("vsc", moduleNamespace, nil)
		createVRR(moduleNamespace, SourceKindVolumeSnapshotContent, "vsc")
		expectRejected(reconcileVRR(moduleNamespace))
	})

	It("rejects a request in the module namespace without the exported namespace whose source has none either", func() {
		createVSC("vsc", "", nil)
		createVRR(moduleNamespace, SourceKindVolumeSnapshotContent, "vsc")
		expectRejected(reconcileVRR(moduleNamespace))
	})

	It("does not exempt any namespace when the module namespace is unknown", func() {
		cfg.ControllerNamespace = ""
		createVSC("vsc", foreignNamespace, nil)
		createAnnotatedVRR(moduleNamespace, SourceKindVolumeSnapshotContent, "vsc", exportOf(foreignNamespace))
		expectRejected(reconcileVRR(moduleNamespace))
	})

	It("rejects a source whose SnapshotContent owner reference does not match the content UID", func() {
		createSnapshotContent("content", requestNamespace, "")
		createPV("pv", "", controllerRef("state-snapshotter.deckhouse.io/v1alpha1", "SnapshotContent", "content", "other-uid"))
		createVRR(requestNamespace, SourceKindPersistentVolume, "pv")
		expectRejected(reconcileVRR(requestNamespace))
	})

	It("traces the controller owner and not another owner of the request namespace", func() {
		own := createObjectKeeper("ret-own", requestNamespace)
		foreign := createObjectKeeper("ret-foreign", foreignNamespace)
		owners := controllerRef(APIGroupDeckhouse, KindObjectKeeper, "ret-foreign", foreign)
		owners = append(owners, ownerRef(APIGroupDeckhouse, KindObjectKeeper, "ret-own", own))
		createVSC("vsc", "", owners)
		createVRR(requestNamespace, SourceKindVolumeSnapshotContent, "vsc")
		expectRejected(reconcileVRR(requestNamespace))
	})

	It("accepts the controller owner of the request namespace next to another owner", func() {
		own := createObjectKeeper("ret-own", requestNamespace)
		foreign := createObjectKeeper("ret-foreign", foreignNamespace)
		owners := []metav1.OwnerReference{ownerRef(APIGroupDeckhouse, KindObjectKeeper, "ret-foreign", foreign)}
		owners = append(owners, controllerRef(APIGroupDeckhouse, KindObjectKeeper, "ret-own", own)...)
		createVSC("vsc", "", owners)
		createVRR(requestNamespace, SourceKindVolumeSnapshotContent, "vsc")
		expectVerified(reconcileVRR(requestNamespace), storagev1alpha1.ConditionReasonSourceInNamespace)
	})

	It("rejects an ObjectKeeper owner of another API group", func() {
		uid := createObjectKeeper("ret-vcr", requestNamespace)
		createVSC("vsc", "", controllerRef("other.io/v1", KindObjectKeeper, "ret-vcr", uid))
		createVRR(requestNamespace, SourceKindVolumeSnapshotContent, "vsc")
		expectRejected(reconcileVRR(requestNamespace))
	})

	It("rejects a SnapshotContent owner of another API group", func() {
		uid := createSnapshotContent("content", requestNamespace, "")
		createPV("pv", "", controllerRef("other.io/v1", "SnapshotContent", "content", uid))
		createVRR(requestNamespace, SourceKindPersistentVolume, "pv")
		expectRejected(reconcileVRR(requestNamespace))
	})

	It("records the new generation when the source changes to another one of the namespace", func() {
		createVSC("vsc", requestNamespace, nil)
		createVSC("other", requestNamespace, nil)
		createVRR(requestNamespace, SourceKindVolumeSnapshotContent, "vsc")
		vrr := reconcileVRR(requestNamespace)
		expectVerified(vrr, storagev1alpha1.ConditionReasonSourceInNamespace)

		vrr.Spec.SourceRef.Name = "other"
		vrr.Generation++
		Expect(cl.Update(ctx, vrr)).To(Succeed())
		expectVerified(reconcileVRR(requestNamespace), storagev1alpha1.ConditionReasonSourceInNamespace)
	})

	It("verifies the source again after the spec changes", func() {
		createVSC("vsc", requestNamespace, nil)
		createVSC("foreign", foreignNamespace, nil)
		createVRR(requestNamespace, SourceKindVolumeSnapshotContent, "vsc")
		vrr := reconcileVRR(requestNamespace)
		expectVerified(vrr, storagev1alpha1.ConditionReasonSourceInNamespace)

		vrr.Spec.SourceRef.Name = "foreign"
		vrr.Generation++
		Expect(cl.Update(ctx, vrr)).To(Succeed())
		expectFailedForeign(reconcileVRR(requestNamespace))
	})
})
