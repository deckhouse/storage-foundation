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

package controllers

import (
	"context"
	"fmt"
	"testing"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	ssstoragev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/images/controller/pkg/config"
)

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "VolumeCaptureRequest Controller Suite")
}

var _ = Describe("VolumeCaptureRequest Controller", func() {
	var (
		ctx    context.Context
		client client.Client
		ctrl   *VolumeCaptureRequestController
		scheme *runtime.Scheme
		cfg    *config.Options
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		Expect(corev1.AddToScheme(scheme)).To(Succeed())
		Expect(storagev1.AddToScheme(scheme)).To(Succeed())
		Expect(storagev1alpha1.AddToScheme(scheme)).To(Succeed())
		Expect(snapshotv1.AddToScheme(scheme)).To(Succeed())
		Expect(deckhousev1alpha1.AddToScheme(scheme)).To(Succeed())

		cfg = &config.Options{
			RequestTTL:    10 * time.Minute,
			RequestTTLStr: "10m",
			Retention: config.RetentionConfig{
				SnapshotTTL: 24 * time.Hour,
			},
		}

		client = fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&snapshotv1.VolumeSnapshotContent{}).
			WithStatusSubresource(&storagev1alpha1.VolumeCaptureRequest{}).
			Build()

		ctrl = &VolumeCaptureRequestController{
			Client:    client,
			APIReader: client,
			Scheme:    scheme,
			Config:    cfg,
		}
	})

	// Helper factories for test objects
	newBoundPVC := func(name, namespace, storageClass, volumeName string) *corev1.PersistentVolumeClaim {
		var storageClassName *string
		if storageClass != "" {
			storageClassName = ptr.To(storageClass)
		}
		return &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				StorageClassName: storageClassName,
				VolumeName:       volumeName,
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("10Gi"),
					},
				},
			},
			Status: corev1.PersistentVolumeClaimStatus{
				Phase: corev1.ClaimBound,
			},
		}
	}

	newCSIPV := func(name, driver, volumeHandle string) *corev1.PersistentVolume {
		return &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{
						Driver:       driver,
						VolumeHandle: volumeHandle,
					},
				},
			},
		}
	}

	newStorageClassWithVSC := func(name, provisioner, vscClassName string) *storagev1.StorageClass {
		return &storagev1.StorageClass{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Annotations: map[string]string{
					"storage.deckhouse.io/volumesnapshotclass": vscClassName,
				},
			},
			Provisioner: provisioner,
		}
	}

	newVolumeSnapshotClass := func(name, driver string) *snapshotv1.VolumeSnapshotClass {
		return &snapshotv1.VolumeSnapshotClass{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
			Driver:         driver,
			DeletionPolicy: snapshotv1.VolumeSnapshotContentDelete,
		}
	}

	pvcTarget := func(namespace, name, uid string) storagev1alpha1.VolumeCaptureTarget {
		return storagev1alpha1.VolumeCaptureTarget{
			UID:        uid,
			APIVersion: "v1",
			Kind:       "PersistentVolumeClaim",
			Namespace:  namespace,
			Name:       name,
		}
	}

	newVCR := func(name, namespace string, mode string, target storagev1alpha1.VolumeCaptureTarget) *storagev1alpha1.VolumeCaptureRequest {
		return &storagev1alpha1.VolumeCaptureRequest{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				UID:       types.UID(fmt.Sprintf("vcr-uid-%s", name)),
			},
			Spec: storagev1alpha1.VolumeCaptureRequestSpec{
				Mode:   mode,
				Target: &target,
			},
		}
	}

	ensureSnapshotObjectKeeperUID := func(vcrUID types.UID) {
		retainerName := objectKeeperNameForVCR(vcrUID)
		ok := &deckhousev1alpha1.ObjectKeeper{}
		if err := client.Get(ctx, types.NamespacedName{Name: retainerName}, ok); err == nil && ok.UID == "" {
			ok.UID = types.UID("ok-uid-" + string(vcrUID))
			_ = client.Update(ctx, ok)
		}
	}

	fixSnapshotVSCOwnerRefs := func(vcr *storagev1alpha1.VolumeCaptureRequest) {
		retainerName := objectKeeperNameForVCR(vcr.UID)
		ok := &deckhousev1alpha1.ObjectKeeper{}
		if err := client.Get(ctx, types.NamespacedName{Name: retainerName}, ok); err != nil {
			return
		}
		if ok.UID == "" {
			ok.UID = types.UID("ok-uid-" + string(vcr.UID))
			_ = client.Update(ctx, ok)
		}
		if vcr.Spec.Target == nil {
			return
		}
		vscName := snapshotVSCName(vcr.UID, vcr.Spec.Target.UID)
		vsc := &snapshotv1.VolumeSnapshotContent{}
		if err := client.Get(ctx, types.NamespacedName{Name: vscName}, vsc); err != nil {
			return
		}
		needsUpdate := false
		for i := range vsc.OwnerReferences {
			if vsc.OwnerReferences[i].Kind == KindObjectKeeper && vsc.OwnerReferences[i].Name == retainerName {
				if vsc.OwnerReferences[i].UID == "" {
					vsc.OwnerReferences[i].UID = ok.UID
					needsUpdate = true
				}
			}
		}
		if needsUpdate {
			_ = client.Update(ctx, vsc)
		}
	}

	newReadyVSC := func(name string, readyToUse bool, errorMsg *string) *snapshotv1.VolumeSnapshotContent {
		vsc := &snapshotv1.VolumeSnapshotContent{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
			},
			Spec: snapshotv1.VolumeSnapshotContentSpec{
				Driver:                  "test-driver",
				VolumeSnapshotClassName: ptr.To("test-vsc-class"),
				DeletionPolicy:          snapshotv1.VolumeSnapshotContentDelete,
				Source: snapshotv1.VolumeSnapshotContentSource{
					VolumeHandle: ptr.To("test-volume-handle"),
				},
			},
		}
		if readyToUse || errorMsg != nil {
			vsc.Status = &snapshotv1.VolumeSnapshotContentStatus{
				ReadyToUse: ptr.To(readyToUse),
			}
			if errorMsg != nil {
				vsc.Status.Error = &snapshotv1.VolumeSnapshotError{
					Message: errorMsg,
					Time:    &metav1.Time{Time: time.Now()},
				}
			}
		}
		return vsc
	}

	// Helper to reconcile until terminal state or max iterations
	// Handles fake client UID workaround for ObjectKeeper
	reconcileUntilTerminal := func(vcr *storagev1alpha1.VolumeCaptureRequest, maxIterations int) error {
		for i := 0; i < maxIterations; i++ {
			// Re-read VCR to get latest state
			currentVCR := &storagev1alpha1.VolumeCaptureRequest{}
			if err := client.Get(ctx, types.NamespacedName{Name: vcr.Name, Namespace: vcr.Namespace}, currentVCR); err != nil {
				if apierrors.IsNotFound(err) {
					return nil // VCR deleted (by TTL scanner)
				}
				return err
			}

			// Check if terminal
			if isVolumeCaptureTerminal(currentVCR.Status.Conditions) {
				return nil // Terminal state reached
			}

			switch currentVCR.Spec.Mode {
			case ModeSnapshot:
				ensureSnapshotObjectKeeperUID(currentVCR.UID)
			case ModeDetach:
				retainerName := NamePrefixRetainerPV + string(currentVCR.UID)
				ok := &deckhousev1alpha1.ObjectKeeper{}
				if err := client.Get(ctx, types.NamespacedName{Name: retainerName}, ok); err == nil && ok.UID == "" {
					ok.UID = types.UID("ok-uid-" + string(currentVCR.UID))
					_ = client.Update(ctx, ok) // Ignore errors
				}
			}

			// Reconcile
			result, err := ctrl.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: currentVCR.Name, Namespace: currentVCR.Namespace}})
			if err != nil {
				return err
			}

			// Workaround for fake client: if RequeueAfter is set due to empty UID, set UID and continue
			if result.RequeueAfter > 0 && result.RequeueAfter == time.Second {
				if currentVCR.Spec.Mode == ModeSnapshot {
					ensureSnapshotObjectKeeperUID(currentVCR.UID)
					continue
				} else if currentVCR.Spec.Mode == ModeDetach {
					retainerName := NamePrefixRetainerPV + string(currentVCR.UID)
					ok := &deckhousev1alpha1.ObjectKeeper{}
					if err := client.Get(ctx, types.NamespacedName{Name: retainerName}, ok); err == nil && ok.UID == "" {
						ok.UID = types.UID("ok-uid-" + string(currentVCR.UID))
						_ = client.Update(ctx, ok) // Ignore errors
						// Continue loop to reconcile again with UID set
						continue
					}
				}
			}

			// Workaround for fake client: ensure ObjectKeeper has UID AFTER reconcile
			// ObjectKeeper might have been created during reconcile, so set UID if needed
			// If UID was set, update VSC/PV ownerRef to use correct UID
			switch currentVCR.Spec.Mode {
			case ModeSnapshot:
				fixSnapshotVSCOwnerRefs(currentVCR)
			case ModeDetach:
				retainerName := NamePrefixRetainerPV + string(currentVCR.UID)
				ok := &deckhousev1alpha1.ObjectKeeper{}
				if err := client.Get(ctx, types.NamespacedName{Name: retainerName}, ok); err == nil {
					if ok.UID == "" {
						ok.UID = types.UID("ok-uid-" + string(currentVCR.UID))
						_ = client.Update(ctx, ok) // Ignore errors
					}
					// Update PV ownerRef if it has empty UID (PV name is stored in VCR annotation)
					if currentVCR.Annotations != nil {
						if pvName, hasPVName := currentVCR.Annotations["storage-foundation.deckhouse.io/detach-pv-name"]; hasPVName {
							pv := &corev1.PersistentVolume{}
							if err := client.Get(ctx, types.NamespacedName{Name: pvName}, pv); err == nil {
								needsUpdate := false
								for i := range pv.OwnerReferences {
									if pv.OwnerReferences[i].Kind == KindObjectKeeper && pv.OwnerReferences[i].Name == retainerName {
										if pv.OwnerReferences[i].UID == "" {
											pv.OwnerReferences[i].UID = ok.UID
											needsUpdate = true
										}
									}
								}
								if needsUpdate {
									_ = client.Update(ctx, pv) // Ignore errors
								}
							}
						}
					}
				}
			}
		}
		return fmt.Errorf("did not reach terminal state for VCR %s/%s after %d iterations", vcr.Namespace, vcr.Name, maxIterations)
	}

	Describe("LEVEL 1: Unit invariants (Snapshot mode)", func() {
		Describe("Happy path", func() {
			It("should create ObjectKeeper and VSC with correct ownership", func() {
				// Given
				storageClass := newStorageClassWithVSC("test-sc", "test-driver", "test-vsc-class")
				Expect(client.Create(ctx, storageClass)).To(Succeed())

				vscClass := newVolumeSnapshotClass("test-vsc-class", "test-driver")
				Expect(client.Create(ctx, vscClass)).To(Succeed())

				pv := newCSIPV("test-pv", "test-driver", "test-volume-handle")
				Expect(client.Create(ctx, pv)).To(Succeed())

				pvc := newBoundPVC("test-pvc", "default", "test-sc", "test-pv")
				Expect(client.Create(ctx, pvc)).To(Succeed())

				vcr := newVCR("test-vcr", "default", ModeSnapshot, pvcTarget("default", "test-pvc", "uid-test-pvc"))
				Expect(client.Create(ctx, vcr)).To(Succeed())

				// When: reconcile until VSC is created (but not terminal yet - waiting for ReadyToUse)
				targetUID := "uid-test-pvc"
				csiVSCName := snapshotVSCName(vcr.UID, targetUID)
				retainerName := objectKeeperNameForVCR(vcr.UID)
				for i := 0; i < 5; i++ {
					ensureSnapshotObjectKeeperUID(vcr.UID)
					result, err := ctrl.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: vcr.Name, Namespace: vcr.Namespace}})
					Expect(err).ToNot(HaveOccurred())
					if result.RequeueAfter > 0 && result.RequeueAfter == time.Second {
						ensureSnapshotObjectKeeperUID(vcr.UID)
					}
				}

				// Simulate external-snapshotter setting ReadyToUse=true
				vsc := &snapshotv1.VolumeSnapshotContent{}
				Expect(client.Get(ctx, types.NamespacedName{Name: csiVSCName}, vsc)).To(Succeed())
				// The managed VSC must appear in the API already delete-protected: the marker is part of the
				// CREATE payload (delete-protection-contract.md §6.2), never a post-create patch. We assert it
				// on the very first read, before any status/ownerRef workaround touches the object.
				Expect(ssstoragev1alpha1.IsDeleteProtected(vsc)).To(BeTrue(),
					"created managed VSC must carry %s=%s in the CREATE payload",
					ssstoragev1alpha1.LabelDeleteProtected, ssstoragev1alpha1.LabelDeleteProtectedValue)
				// The fake client does not assign a UID on Create; set one so we can assert it propagates
				// into status.dataRef.artifact.uid (a real apiserver always assigns a UID).
				if vsc.UID == "" {
					vsc.UID = types.UID("vsc-uid-happy")
					Expect(client.Update(ctx, vsc)).To(Succeed())
					Expect(client.Get(ctx, types.NamespacedName{Name: csiVSCName}, vsc)).To(Succeed())
				}
				vsc.Status = &snapshotv1.VolumeSnapshotContentStatus{
					ReadyToUse: ptr.To(true),
				}
				Expect(client.Status().Update(ctx, vsc)).To(Succeed())

				// Reconcile again to process ReadyToUse=true and reach terminal state
				Expect(reconcileUntilTerminal(vcr, 5)).To(Succeed())

				// Then: verify invariants
				// ObjectKeeper exists
				objectKeeper := &deckhousev1alpha1.ObjectKeeper{}
				Expect(client.Get(ctx, types.NamespacedName{Name: retainerName}, objectKeeper)).To(Succeed())

				// Workaround for fake client: ensure ObjectKeeper has UID
				if objectKeeper.UID == "" {
					objectKeeper.UID = types.UID("ok-uid-" + string(vcr.UID))
					Expect(client.Update(ctx, objectKeeper)).To(Succeed())
				}

				Expect(objectKeeper.Spec.Mode).To(Equal("FollowObject"))
				Expect(objectKeeper.Spec.FollowObjectRef).ToNot(BeNil())
				Expect(objectKeeper.Spec.FollowObjectRef.UID).To(Equal(string(vcr.UID)))
				Expect(objectKeeper.Spec.FollowObjectRef.Name).To(Equal("test-vcr"))
				Expect(objectKeeper.Spec.FollowObjectRef.Namespace).To(Equal("default"))

				// VSC exists - re-read to get latest state
				Expect(client.Get(ctx, types.NamespacedName{Name: csiVSCName}, vsc)).To(Succeed())

				// Workaround for fake client: update VSC ownerRef if it has empty UID
				needsUpdate := false
				for i := range vsc.OwnerReferences {
					if vsc.OwnerReferences[i].Kind == KindObjectKeeper && vsc.OwnerReferences[i].Name == retainerName {
						if vsc.OwnerReferences[i].UID == "" {
							vsc.OwnerReferences[i].UID = objectKeeper.UID
							needsUpdate = true
						}
					}
				}
				if needsUpdate {
					Expect(client.Update(ctx, vsc)).To(Succeed())
					// Re-read after update
					Expect(client.Get(ctx, types.NamespacedName{Name: csiVSCName}, vsc)).To(Succeed())
				}

				// VSC ownership: ObjectKeeper is controller owner
				objectKeeperOwnerRef := false
				vcrOwnerRef := false
				for _, ref := range vsc.OwnerReferences {
					if ref.Kind == KindObjectKeeper && ref.Name == retainerName && ref.UID == objectKeeper.UID {
						Expect(ref.Controller).ToNot(BeNil())
						Expect(*ref.Controller).To(BeTrue())
						objectKeeperOwnerRef = true
					}
					if ref.Kind == KindVolumeCaptureRequest {
						vcrOwnerRef = true
					}
				}
				Expect(objectKeeperOwnerRef).To(BeTrue(), "ObjectKeeper should be controller owner of VSC")
				Expect(vcrOwnerRef).To(BeFalse(), "VCR should NOT be owner of VSC")

				// VSC spec invariants
				Expect(vsc.Spec.VolumeSnapshotRef.Name).To(BeEmpty(), "VSC should not have VolumeSnapshotRef")
				Expect(vsc.Spec.VolumeSnapshotRef.Namespace).To(BeEmpty())
				Expect(vsc.Spec.Source.VolumeHandle).ToNot(BeNil())
				Expect(*vsc.Spec.Source.VolumeHandle).To(Equal("test-volume-handle"))

				// VCR status - verify finalization after ReadyToUse=true
				updatedVCR := &storagev1alpha1.VolumeCaptureRequest{}
				Expect(client.Get(ctx, types.NamespacedName{Name: vcr.Name, Namespace: vcr.Namespace}, updatedVCR)).To(Succeed())

				readyCondition := getReadyCondition(updatedVCR.Status.Conditions)
				Expect(readyCondition).ToNot(BeNil())
				Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))

				Expect(updatedVCR.Status.Data).ToNot(BeNil())
				Expect(updatedVCR.Status.Data.Artifact.Kind).To(Equal("VolumeSnapshotContent"))
				Expect(updatedVCR.Status.Data.Artifact.Name).To(Equal(csiVSCName))
				Expect(updatedVCR.Status.Data.Artifact.UID).To(Equal("vsc-uid-happy"))
			})
		})

		Describe("CSI error (terminal)", func() {
			It("should mark VCR as failed when VSC has terminal error", func() {
				// Given
				storageClass := newStorageClassWithVSC("test-sc", "test-driver", "test-vsc-class")
				Expect(client.Create(ctx, storageClass)).To(Succeed())

				vscClass := newVolumeSnapshotClass("test-vsc-class", "test-driver")
				Expect(client.Create(ctx, vscClass)).To(Succeed())

				pv := newCSIPV("test-pv", "test-driver", "test-volume-handle")
				Expect(client.Create(ctx, pv)).To(Succeed())

				pvc := newBoundPVC("test-pvc", "default", "test-sc", "test-pv")
				Expect(client.Create(ctx, pvc)).To(Succeed())

				vcr := newVCR("test-vcr-error", "default", ModeSnapshot, pvcTarget("default", "test-pvc", "uid-test-pvc"))
				Expect(client.Create(ctx, vcr)).To(Succeed())

				// Create VSC with error (simulating CSI driver failure)
				targetUID := "uid-test-pvc"
				csiVSCName := snapshotVSCName(vcr.UID, targetUID)
				errorMsg := "provided secret is empty"
				vsc := newReadyVSC(csiVSCName, false, nil)
				vsc.UID = types.UID("vsc-uid-error")

				retainerName := objectKeeperNameForVCR(vcr.UID)
				objectKeeper := &deckhousev1alpha1.ObjectKeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name: retainerName,
						UID:  types.UID("ok-uid-test"),
					},
					Spec: deckhousev1alpha1.ObjectKeeperSpec{
						Mode: "FollowObject",
						FollowObjectRef: &deckhousev1alpha1.FollowObjectRef{
							APIVersion: APIGroupStorageDeckhouse,
							Kind:       KindVolumeCaptureRequest,
							Namespace:  "default",
							Name:       "test-vcr-error",
							UID:        string(vcr.UID),
						},
					},
				}
				Expect(client.Create(ctx, objectKeeper)).To(Succeed())

				// Set ownerRef on VSC
				vsc.OwnerReferences = []metav1.OwnerReference{
					{
						APIVersion: APIGroupDeckhouse,
						Kind:       KindObjectKeeper,
						Name:       retainerName,
						UID:        objectKeeper.UID,
						Controller: ptr.To(true),
					},
				}
				Expect(client.Create(ctx, vsc)).To(Succeed())

				// Update Status via subresource (correct way to set Status)
				vsc.Status = &snapshotv1.VolumeSnapshotContentStatus{
					Error: &snapshotv1.VolumeSnapshotError{
						Message: &errorMsg,
						Time:    &metav1.Time{Time: time.Now()},
					},
				}
				Expect(client.Status().Update(ctx, vsc)).To(Succeed())

				// When: reconcile
				Expect(reconcileUntilTerminal(vcr, 5)).To(Succeed())

				// Then: VCR is terminal Failed
				updatedVCR := &storagev1alpha1.VolumeCaptureRequest{}
				Expect(client.Get(ctx, types.NamespacedName{Name: vcr.Name, Namespace: vcr.Namespace}, updatedVCR)).To(Succeed())

				readyCondition := getReadyCondition(updatedVCR.Status.Conditions)
				Expect(readyCondition).ToNot(BeNil())
				Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
				Expect(readyCondition.Reason).To(Equal(storagev1alpha1.ConditionReasonSnapshotCreationFailed))
				Expect(readyCondition.Message).To(ContainSubstring(errorMsg))

				Expect(updatedVCR.Status.Data).ToNot(BeNil())
				Expect(updatedVCR.Status.Data.Artifact.Kind).To(Equal("VolumeSnapshotContent"))
				Expect(updatedVCR.Status.Data.Artifact.Name).To(Equal(csiVSCName))
				Expect(updatedVCR.Status.Data.Artifact.UID).To(Equal("vsc-uid-error"))

				// VSC still exists (not deleted)
				existingVSC := &snapshotv1.VolumeSnapshotContent{}
				Expect(client.Get(ctx, types.NamespacedName{Name: csiVSCName}, existingVSC)).To(Succeed())

				// No new VSC created
				vscList := &snapshotv1.VolumeSnapshotContentList{}
				Expect(client.List(ctx, vscList)).To(Succeed())
				Expect(vscList.Items).To(HaveLen(1))
			})
		})

		Describe("Misconfiguration scenarios", func() {
			DescribeTable("should mark VCR as failed for misconfiguration",
				func(setupFunc func() *storagev1alpha1.VolumeCaptureRequest, expectedReason string) {
					// Given
					vcr := setupFunc()
					Expect(client.Create(ctx, vcr)).To(Succeed())

					// When: reconcile
					Expect(reconcileUntilTerminal(vcr, 5)).To(Succeed())

					// Then: VCR is terminal Failed
					updatedVCR := &storagev1alpha1.VolumeCaptureRequest{}
					Expect(client.Get(ctx, types.NamespacedName{Name: vcr.Name, Namespace: vcr.Namespace}, updatedVCR)).To(Succeed())

					readyCondition := getReadyCondition(updatedVCR.Status.Conditions)
					Expect(readyCondition).ToNot(BeNil())
					Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
					Expect(readyCondition.Reason).To(Equal(expectedReason))
				},
				Entry("PVC not found",
					func() *storagev1alpha1.VolumeCaptureRequest {
						return newVCR("test-vcr", "default", ModeSnapshot, pvcTarget("default", "non-existent-pvc", "uid-non-existent-pvc"))
					},
					storagev1alpha1.ConditionReasonNotFound,
				),
				Entry("PVC not bound",
					func() *storagev1alpha1.VolumeCaptureRequest {
						pvc := &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name:      "test-pvc-unbound",
								Namespace: "default",
							},
							Spec: corev1.PersistentVolumeClaimSpec{
								StorageClassName: ptr.To("test-sc"),
							},
						}
						Expect(client.Create(ctx, pvc)).To(Succeed())

						return newVCR("test-vcr", "default", ModeSnapshot, pvcTarget("default", "test-pvc-unbound", "uid-test-pvc-unbound"))
					},
					storagev1alpha1.ConditionReasonInternalError,
				),
				Entry("PV without CSI",
					func() *storagev1alpha1.VolumeCaptureRequest {
						pv := &corev1.PersistentVolume{
							ObjectMeta: metav1.ObjectMeta{
								Name: "test-pv-no-csi",
							},
							Spec: corev1.PersistentVolumeSpec{
								PersistentVolumeSource: corev1.PersistentVolumeSource{
									HostPath: &corev1.HostPathVolumeSource{
										Path: "/tmp",
									},
								},
							},
						}
						Expect(client.Create(ctx, pv)).To(Succeed())

						pvc := newBoundPVC("test-pvc-no-csi", "default", "test-sc", "test-pv-no-csi")
						Expect(client.Create(ctx, pvc)).To(Succeed())

						return newVCR("test-vcr", "default", ModeSnapshot, pvcTarget("default", "test-pvc-no-csi", "uid-test-pvc-no-csi"))
					},
					storagev1alpha1.ConditionReasonInternalError,
				),
				Entry("StorageClass without VSC annotation",
					func() *storagev1alpha1.VolumeCaptureRequest {
						sc := &storagev1.StorageClass{
							ObjectMeta: metav1.ObjectMeta{
								Name: "test-sc-no-annotation",
							},
							Provisioner: "test-driver",
						}
						Expect(client.Create(ctx, sc)).To(Succeed())

						pv := newCSIPV("test-pv-no-annotation", "test-driver", "test-volume-handle")
						Expect(client.Create(ctx, pv)).To(Succeed())

						pvc := newBoundPVC("test-pvc-no-annotation", "default", "test-sc-no-annotation", "test-pv-no-annotation")
						Expect(client.Create(ctx, pvc)).To(Succeed())

						return newVCR("test-vcr", "default", ModeSnapshot, pvcTarget("default", "test-pvc-no-annotation", "uid-test-pvc-no-annotation"))
					},
					storagev1alpha1.ConditionReasonNotFound,
				),
				Entry("VolumeSnapshotClass not found",
					func() *storagev1alpha1.VolumeCaptureRequest {
						sc := newStorageClassWithVSC("test-sc-bad-vsc", "test-driver", "non-existent-vsc")
						Expect(client.Create(ctx, sc)).To(Succeed())

						pv := newCSIPV("test-pv-bad-vsc", "test-driver", "test-volume-handle")
						Expect(client.Create(ctx, pv)).To(Succeed())

						pvc := newBoundPVC("test-pvc-bad-vsc", "default", "test-sc-bad-vsc", "test-pv-bad-vsc")
						Expect(client.Create(ctx, pvc)).To(Succeed())

						return newVCR("test-vcr", "default", ModeSnapshot, pvcTarget("default", "test-pvc-bad-vsc", "uid-test-pvc-bad-vsc"))
					},
					storagev1alpha1.ConditionReasonNotFound,
				),
				Entry("Driver mismatch",
					func() *storagev1alpha1.VolumeCaptureRequest {
						sc := newStorageClassWithVSC("test-sc-mismatch", "test-driver", "test-vsc-class-mismatch")
						Expect(client.Create(ctx, sc)).To(Succeed())

						vscClass := newVolumeSnapshotClass("test-vsc-class-mismatch", "other-driver")
						Expect(client.Create(ctx, vscClass)).To(Succeed())

						pv := newCSIPV("test-pv-mismatch", "test-driver", "test-volume-handle")
						Expect(client.Create(ctx, pv)).To(Succeed())

						pvc := newBoundPVC("test-pvc-mismatch", "default", "test-sc-mismatch", "test-pv-mismatch")
						Expect(client.Create(ctx, pvc)).To(Succeed())

						return newVCR("test-vcr", "default", ModeSnapshot, pvcTarget("default", "test-pvc-mismatch", "uid-test-pvc-mismatch"))
					},
					storagev1alpha1.ConditionReasonInternalError,
				),
			)
		})
	})

	Describe("LEVEL 2: Detach mode", func() {
		Describe("Happy path", func() {
			It("should detach PV and set correct ownership", func() {
				// Given
				pv := newCSIPV("test-pv-detach", "test-driver", "test-volume-handle")
				pv.UID = types.UID("pv-uid-detach")
				pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain
				pv.Spec.ClaimRef = &corev1.ObjectReference{
					Namespace: "default",
					Name:      "test-pvc-detach",
				}
				Expect(client.Create(ctx, pv)).To(Succeed())

				pvc := newBoundPVC("test-pvc-detach", "default", "", "test-pv-detach")
				Expect(client.Create(ctx, pvc)).To(Succeed())

				vcr := newVCR("test-vcr-detach", "default", ModeDetach, pvcTarget("default", "test-pvc-detach", "uid-test-pvc-detach"))
				Expect(client.Create(ctx, vcr)).To(Succeed())

				// When: reconcile until terminal
				Expect(reconcileUntilTerminal(vcr, 10)).To(Succeed())

				// Then: verify invariants
				// PVC is deleted
				deletedPVC := &corev1.PersistentVolumeClaim{}
				err := client.Get(ctx, types.NamespacedName{Name: "test-pvc-detach", Namespace: "default"}, deletedPVC)
				Expect(apierrors.IsNotFound(err)).To(BeTrue())

				// PV is detached
				updatedPV := &corev1.PersistentVolume{}
				Expect(client.Get(ctx, types.NamespacedName{Name: "test-pv-detach"}, updatedPV)).To(Succeed())
				Expect(updatedPV.Spec.ClaimRef).To(BeNil())
				Expect(updatedPV.Annotations).To(HaveKey("storage-foundation.deckhouse.io/detached"))
				Expect(updatedPV.Annotations["storage-foundation.deckhouse.io/detached"]).To(Equal("true"))

				// ObjectKeeper exists
				retainerName := NamePrefixRetainerPV + string(vcr.UID)
				objectKeeper := &deckhousev1alpha1.ObjectKeeper{}
				Expect(client.Get(ctx, types.NamespacedName{Name: retainerName}, objectKeeper)).To(Succeed())

				// PV ownership: ObjectKeeper is controller owner
				objectKeeperOwnerRef := false
				for _, ref := range updatedPV.OwnerReferences {
					if ref.Kind == KindObjectKeeper && ref.Name == retainerName && ref.UID == objectKeeper.UID {
						Expect(ref.Controller).ToNot(BeNil())
						Expect(*ref.Controller).To(BeTrue())
						objectKeeperOwnerRef = true
					}
				}
				Expect(objectKeeperOwnerRef).To(BeTrue(), "ObjectKeeper should be controller owner of PV")

				// VCR status
				updatedVCR := &storagev1alpha1.VolumeCaptureRequest{}
				Expect(client.Get(ctx, types.NamespacedName{Name: vcr.Name, Namespace: vcr.Namespace}, updatedVCR)).To(Succeed())
				Expect(updatedVCR.Status.Data).ToNot(BeNil())
				Expect(updatedVCR.Status.Data.Artifact.Kind).To(Equal("PersistentVolume"))
				Expect(updatedVCR.Status.Data.Artifact.Name).To(Equal("test-pv-detach"))
				Expect(updatedVCR.Status.Data.Artifact.UID).To(Equal("pv-uid-detach"))

				readyCondition := getReadyCondition(updatedVCR.Status.Conditions)
				Expect(readyCondition).ToNot(BeNil())
				Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
			})
		})

		Describe("Idempotency", func() {
			It("should not recreate resources on repeated reconcile", func() {
				// Given: PVC already deleted, PV already detached, ObjectKeeper exists
				pv := newCSIPV("test-pv-idempotent", "test-driver", "test-volume-handle")
				pv.Spec.ClaimRef = nil
				if pv.Annotations == nil {
					pv.Annotations = make(map[string]string)
				}
				pv.Annotations["storage-foundation.deckhouse.io/detached"] = "true"
				Expect(client.Create(ctx, pv)).To(Succeed())

				vcr := newVCR("test-vcr-idempotent", "default", ModeDetach, pvcTarget("default", "test-pvc-idempotent", "uid-test-pvc-idempotent"))
				// Set annotation with PV name (controller sets this during first reconcile)
				if vcr.Annotations == nil {
					vcr.Annotations = make(map[string]string)
				}
				vcr.Annotations["storage-foundation.deckhouse.io/detach-pv-name"] = "test-pv-idempotent"
				Expect(client.Create(ctx, vcr)).To(Succeed())

				retainerName := NamePrefixRetainerPV + string(vcr.UID)
				objectKeeper := &deckhousev1alpha1.ObjectKeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name: retainerName,
						UID:  types.UID("ok-uid-idempotent"),
					},
					Spec: deckhousev1alpha1.ObjectKeeperSpec{
						Mode: "FollowObject",
						FollowObjectRef: &deckhousev1alpha1.FollowObjectRef{
							APIVersion: APIGroupStorageDeckhouse,
							Kind:       KindVolumeCaptureRequest,
							Namespace:  "default",
							Name:       "test-vcr-idempotent",
							UID:        string(vcr.UID),
						},
					},
				}
				Expect(client.Create(ctx, objectKeeper)).To(Succeed())

				// Set ownerRef
				pv.OwnerReferences = []metav1.OwnerReference{
					{
						APIVersion: APIGroupDeckhouse,
						Kind:       KindObjectKeeper,
						Name:       retainerName,
						UID:        objectKeeper.UID,
						Controller: ptr.To(true),
					},
				}
				Expect(client.Update(ctx, pv)).To(Succeed())

				originalOKUID := objectKeeper.UID
				originalPVUID := pv.UID

				// When: reconcile multiple times
				for i := 0; i < 3; i++ {
					_, err := ctrl.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: vcr.Name, Namespace: vcr.Namespace}})
					Expect(err).ToNot(HaveOccurred())
				}

				// Then: nothing recreated
				existingOK := &deckhousev1alpha1.ObjectKeeper{}
				Expect(client.Get(ctx, types.NamespacedName{Name: retainerName}, existingOK)).To(Succeed())
				Expect(existingOK.UID).To(Equal(originalOKUID), "ObjectKeeper should not be recreated")

				existingPV := &corev1.PersistentVolume{}
				Expect(client.Get(ctx, types.NamespacedName{Name: "test-pv-idempotent"}, existingPV)).To(Succeed())
				Expect(existingPV.UID).To(Equal(originalPVUID), "PV should not be recreated")
			})
		})
	})

	Describe("LEVEL 3: Lifecycle / GC semantics", func() {
		It("should maintain correct ownership chain", func() {
			// Given: VCR → ObjectKeeper → VSC
			storageClass := newStorageClassWithVSC("test-sc", "test-driver", "test-vsc-class")
			Expect(client.Create(ctx, storageClass)).To(Succeed())

			vscClass := newVolumeSnapshotClass("test-vsc-class", "test-driver")
			Expect(client.Create(ctx, vscClass)).To(Succeed())

			pv := newCSIPV("test-pv", "test-driver", "test-volume-handle")
			Expect(client.Create(ctx, pv)).To(Succeed())

			pvc := newBoundPVC("test-pvc", "default", "test-sc", "test-pv")
			Expect(client.Create(ctx, pvc)).To(Succeed())

			vcr := newVCR("test-vcr", "default", ModeSnapshot, pvcTarget("default", "test-pvc", "uid-test-pvc"))
			Expect(client.Create(ctx, vcr)).To(Succeed())

			// Reconcile until VSC is created
			targetUID := "uid-test-pvc"
			csiVSCName := snapshotVSCName(vcr.UID, targetUID)
			retainerName := objectKeeperNameForVCR(vcr.UID)
			for i := 0; i < 5; i++ {
				ensureSnapshotObjectKeeperUID(vcr.UID)
				result, err := ctrl.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: vcr.Name, Namespace: vcr.Namespace}})
				Expect(err).ToNot(HaveOccurred())
				if result.RequeueAfter > 0 && result.RequeueAfter == time.Second {
					ensureSnapshotObjectKeeperUID(vcr.UID)
				}
			}

			// Set ReadyToUse=true to allow terminal state
			vsc := &snapshotv1.VolumeSnapshotContent{}
			Expect(client.Get(ctx, types.NamespacedName{Name: csiVSCName}, vsc)).To(Succeed())
			vsc.Status = &snapshotv1.VolumeSnapshotContentStatus{
				ReadyToUse: ptr.To(true),
			}
			Expect(client.Status().Update(ctx, vsc)).To(Succeed())

			// Reconcile to terminal state
			Expect(reconcileUntilTerminal(vcr, 5)).To(Succeed())

			// Then: verify ownership chain
			objectKeeper := &deckhousev1alpha1.ObjectKeeper{}
			Expect(client.Get(ctx, types.NamespacedName{Name: retainerName}, objectKeeper)).To(Succeed())

			// Re-read VSC to get latest state
			Expect(client.Get(ctx, types.NamespacedName{Name: csiVSCName}, vsc)).To(Succeed())

			// VCR is NOT owner
			vcrOwnerRef := false
			for _, ref := range vsc.OwnerReferences {
				if ref.Kind == KindVolumeCaptureRequest {
					vcrOwnerRef = true
				}
			}
			Expect(vcrOwnerRef).To(BeFalse(), "VCR should NOT be owner")

			// ObjectKeeper is controller owner
			okControllerRef := false
			controllerCount := 0
			for _, ref := range vsc.OwnerReferences {
				if ref.Controller != nil && *ref.Controller {
					controllerCount++
					if ref.Kind == KindObjectKeeper && ref.UID == objectKeeper.UID {
						okControllerRef = true
					}
				}
			}
			Expect(okControllerRef).To(BeTrue(), "ObjectKeeper should be controller owner")
			Expect(controllerCount).To(Equal(1), "Should have exactly one controller owner")
		})

		It("should delete expired VCR via TTL scanner", func() {
			// Given: terminal VCR with expired TTL
			now := time.Now()
			completionTime := metav1.NewTime(now.Add(-15 * time.Minute)) // 15 minutes ago, TTL=10m

			vcr := &storagev1alpha1.VolumeCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vcr-expired",
					Namespace: "default",
					UID:       types.UID("vcr-uid-expired"),
				},
				Spec: storagev1alpha1.VolumeCaptureRequestSpec{
					Mode: ModeSnapshot,
					Target: &storagev1alpha1.VolumeCaptureTarget{
						UID:        "uid-test-pvc",
						APIVersion: "v1",
						Kind:       "PersistentVolumeClaim",
						Namespace:  "default",
						Name:       "test-pvc",
					},
				},
				Status: storagev1alpha1.VolumeCaptureRequestStatus{
					CompletionTimestamp: &completionTime,
					Conditions: []metav1.Condition{
						{
							Type:               storagev1alpha1.ConditionTypeReady,
							Status:             metav1.ConditionTrue,
							Reason:             storagev1alpha1.ConditionReasonCompleted,
							LastTransitionTime: completionTime,
						},
					},
				},
			}
			Expect(client.Create(ctx, vcr)).To(Succeed())

			// When: run TTL scanner
			ctrl.scanAndDeleteExpiredVCRs(ctx, client)

			// Then: VCR is deleted
			deletedVCR := &storagev1alpha1.VolumeCaptureRequest{}
			err := client.Get(ctx, types.NamespacedName{Name: vcr.Name, Namespace: vcr.Namespace}, deletedVCR)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "VCR should be deleted by TTL scanner")
		})
	})

	Describe("LEVEL 2: Single-target Snapshot mode edge cases", func() {
		It("should report TargetsPending while the single target VSC is not yet ReadyToUse", func() {
			storageClass := newStorageClassWithVSC("test-sc", "test-driver", "test-vsc-class")
			Expect(client.Create(ctx, storageClass)).To(Succeed())
			vscClass := newVolumeSnapshotClass("test-vsc-class", "test-driver")
			Expect(client.Create(ctx, vscClass)).To(Succeed())
			Expect(client.Create(ctx, newCSIPV("test-pv-pending", "test-driver", "handle-pending"))).To(Succeed())
			Expect(client.Create(ctx, newBoundPVC("pvc-pending", "default", "test-sc", "test-pv-pending"))).To(Succeed())

			vcr := newVCR("test-vcr-pending", "default", ModeSnapshot, pvcTarget("default", "pvc-pending", "uid-pvc-pending"))
			Expect(client.Create(ctx, vcr)).To(Succeed())

			// Reconcile several times: the VSC is created but external-snapshotter never sets ReadyToUse.
			for i := 0; i < 5; i++ {
				ensureSnapshotObjectKeeperUID(vcr.UID)
				_, err := ctrl.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: vcr.Name, Namespace: vcr.Namespace}})
				Expect(err).ToNot(HaveOccurred())
			}

			updated := &storagev1alpha1.VolumeCaptureRequest{}
			Expect(client.Get(ctx, types.NamespacedName{Name: vcr.Name, Namespace: vcr.Namespace}, updated)).To(Succeed())
			ready := getReadyCondition(updated.Status.Conditions)
			Expect(ready).ToNot(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal(storagev1alpha1.ConditionReasonTargetsPending))
			// dataRef is only set on success.
			Expect(updated.Status.Data).To(BeNil())
		})

		It("should requeue without creating VSC when ObjectKeeper UID is empty", func() {
			storageClass := newStorageClassWithVSC("test-sc", "test-driver", "test-vsc-class")
			Expect(client.Create(ctx, storageClass)).To(Succeed())
			vscClass := newVolumeSnapshotClass("test-vsc-class", "test-driver")
			Expect(client.Create(ctx, vscClass)).To(Succeed())
			Expect(client.Create(ctx, newCSIPV("test-pv-guard", "test-driver", "handle-guard"))).To(Succeed())
			Expect(client.Create(ctx, newBoundPVC("pvc-guard", "default", "test-sc", "test-pv-guard"))).To(Succeed())

			targetUID := "uid-pvc-guard"
			vcr := newVCR("test-vcr-ok-uid-guard", "default", ModeSnapshot, pvcTarget("default", "pvc-guard", targetUID))
			Expect(client.Create(ctx, vcr)).To(Succeed())

			retainerName := objectKeeperNameForVCR(vcr.UID)
			Expect(client.Create(ctx, &deckhousev1alpha1.ObjectKeeper{
				ObjectMeta: metav1.ObjectMeta{Name: retainerName},
				Spec: deckhousev1alpha1.ObjectKeeperSpec{
					Mode: "FollowObject",
					FollowObjectRef: &deckhousev1alpha1.FollowObjectRef{
						APIVersion: APIGroupStorageDeckhouse,
						Kind:       KindVolumeCaptureRequest,
						Namespace:  vcr.Namespace,
						Name:       vcr.Name,
						UID:        string(vcr.UID),
					},
				},
			})).To(Succeed())

			result, err := ctrl.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: vcr.Name, Namespace: vcr.Namespace}})
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(time.Second))

			vscName := snapshotVSCName(vcr.UID, targetUID)
			vsc := &snapshotv1.VolumeSnapshotContent{}
			err = client.Get(ctx, types.NamespacedName{Name: vscName}, vsc)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("should be idempotent across repeated reconciles (single VSC)", func() {
			storageClass := newStorageClassWithVSC("test-sc", "test-driver", "test-vsc-class")
			Expect(client.Create(ctx, storageClass)).To(Succeed())
			vscClass := newVolumeSnapshotClass("test-vsc-class", "test-driver")
			Expect(client.Create(ctx, vscClass)).To(Succeed())
			Expect(client.Create(ctx, newCSIPV("test-pv-idem", "test-driver", "handle-idem"))).To(Succeed())
			Expect(client.Create(ctx, newBoundPVC("pvc-idem", "default", "test-sc", "test-pv-idem"))).To(Succeed())

			vcr := newVCR("test-vcr-idem-single", "default", ModeSnapshot, pvcTarget("default", "pvc-idem", "uid-pvc-idem"))
			Expect(client.Create(ctx, vcr)).To(Succeed())

			vscName := snapshotVSCName(vcr.UID, "uid-pvc-idem")
			for i := 0; i < 4; i++ {
				ensureSnapshotObjectKeeperUID(vcr.UID)
				_, err := ctrl.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: vcr.Name, Namespace: vcr.Namespace}})
				Expect(err).ToNot(HaveOccurred())
			}
			vscList := &snapshotv1.VolumeSnapshotContentList{}
			Expect(client.List(ctx, vscList)).To(Succeed())
			names := map[string]struct{}{}
			for _, item := range vscList.Items {
				names[item.Name] = struct{}{}
			}
			Expect(names).To(HaveKey(vscName))
			Expect(len(names)).To(Equal(1))
		})
	})
})

// Helper function to get condition by type
func getReadyCondition(conditions []metav1.Condition) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == storagev1alpha1.ConditionTypeReady {
			return &conditions[i]
		}
	}
	return nil
}
