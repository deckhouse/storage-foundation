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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	storagev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/images/controller/pkg/config"
	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
)

// TestVolumeRestoreRequest is part of the unified test suite
// All controller tests are run together via TestControllers to avoid Ginkgo "Rerunning Suite" error

var _ = Describe("VolumeRestoreRequest", func() {
	var (
		ctx    context.Context
		client client.Client
		ctrl   *VolumeRestoreRequestController
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
		}

		client = fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&snapshotv1.VolumeSnapshotContent{}).
			WithStatusSubresource(&storagev1alpha1.VolumeRestoreRequest{}).
			WithStatusSubresource(&corev1.PersistentVolumeClaim{}).
			Build()

		ctrl = &VolumeRestoreRequestController{
			Client:    client,
			APIReader: client, // Use same client for tests
			Scheme:    scheme,
			Config:    cfg,
		}
	})

	// Helper function to get condition by type
	getCondition := func(conditions []metav1.Condition, conditionType string) *metav1.Condition {
		for i := range conditions {
			if conditions[i].Type == conditionType {
				return &conditions[i]
			}
		}
		return nil
	}

	// Helper to create VSC with ReadyToUse=true
	createReadyVSC := func(name string) *snapshotv1.VolumeSnapshotContent {
		vsc := &snapshotv1.VolumeSnapshotContent{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				UID:  types.UID(fmt.Sprintf("vsc-uid-%s", name)),
			},
			Spec: snapshotv1.VolumeSnapshotContentSpec{
				Driver: "test-driver",
				Source: snapshotv1.VolumeSnapshotContentSource{
					VolumeHandle: pointer.String("test-volume-handle"),
				},
			},
			Status: &snapshotv1.VolumeSnapshotContentStatus{
				ReadyToUse: pointer.Bool(true),
			},
		}
		Expect(client.Create(ctx, vsc)).To(Succeed())
		return vsc
	}

	// Helper to create PV
	createPV := func(name string) *corev1.PersistentVolume {
		pv := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				UID:  types.UID(fmt.Sprintf("pv-uid-%s", name)),
			},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{
						Driver:       "test-driver",
						VolumeHandle: "test-volume-handle",
					},
				},
			},
		}
		Expect(client.Create(ctx, pv)).To(Succeed())
		return pv
	}

	// Helper to create Bound PVC (simulating external-provisioner)
	createBoundPVC := func(namespace, name string) *corev1.PersistentVolumeClaim {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				UID:       types.UID(fmt.Sprintf("pvc-uid-%s", name)),
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			},
			Status: corev1.PersistentVolumeClaimStatus{
				Phase: corev1.ClaimBound,
			},
		}
		Expect(client.Create(ctx, pvc)).To(Succeed())
		return pvc
	}

	// Helper to create Pending PVC
	createPendingPVC := func(namespace, name string) *corev1.PersistentVolumeClaim {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				UID:       types.UID(fmt.Sprintf("pvc-uid-%s", name)),
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			},
			Status: corev1.PersistentVolumeClaimStatus{
				Phase: corev1.ClaimPending,
			},
		}
		Expect(client.Create(ctx, pvc)).To(Succeed())
		return pvc
	}

	// Helper to verify no VolumeSnapshot objects exist
	verifyNoVolumeSnapshots := func() {
		vsList := &snapshotv1.VolumeSnapshotList{}
		Expect(client.List(ctx, vsList)).To(Succeed())
		Expect(vsList.Items).To(BeEmpty(), "No VolumeSnapshot objects should exist")
	}

	// Helper to verify no PV/PVC created by controller
	// This is a critical architectural invariant: VRR controller MUST NOT create PVC
	verifyNoPVCCreatedByController := func(namespace, name string) {
		pvc := &corev1.PersistentVolumeClaim{}
		err := client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, pvc)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "PVC should not exist until created by external-provisioner")
	}

	// reconcileUntilObjectKeeperReady handles fake client workaround for ObjectKeeper UID
	// In real apiserver, UID is always populated immediately after Create.
	// Fake client may not populate UID, so we need to set it manually and reconcile again.
	// This helper encapsulates this test-specific workaround so tests don't need to know
	// about controller internals.
	reconcileUntilObjectKeeperReady := func(req reconcile.Request, vrrUID types.UID) (reconcile.Result, error) {
		for i := 0; i < 3; i++ {
			result, err := ctrl.Reconcile(ctx, req)
			if err != nil {
				return result, err
			}
			// If RequeueAfter is set (due to empty UID), ObjectKeeper was created but UID not populated by fake client
			if result.RequeueAfter > 0 && result.RequeueAfter == time.Second {
				retainerName := NamePrefixRetainerPVC + string(vrrUID)
				objectKeeper := &deckhousev1alpha1.ObjectKeeper{}
				if err := client.Get(ctx, types.NamespacedName{Name: retainerName}, objectKeeper); err != nil {
					return result, err
				}
				if objectKeeper.UID == "" {
					// Set UID manually to simulate apiserver behavior
					objectKeeper.UID = types.UID("ok-uid-" + string(vrrUID))
					if err := client.Update(ctx, objectKeeper); err != nil {
						return result, err
					}
					// Continue loop to reconcile again
					continue
				}
			}
			// ObjectKeeper is ready (either existed already or UID is now populated)
			return result, nil
		}
		// Fallback: return last result even if ObjectKeeper UID still not set
		// This should not happen in practice, but prevents infinite loop
		result, err := ctrl.Reconcile(ctx, req)
		return result, err
	}

	Describe("Restore from VolumeSnapshotContent - Happy Path", func() {
		It("should finalize VRR when PVC becomes Bound", func() {
			// Given: VSC exists and is ReadyToUse
			createReadyVSC("test-vsc")

			// Given: VRR exists
			vrr := &storagev1alpha1.VolumeRestoreRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vrr",
					Namespace: "default",
					UID:       types.UID("vrr-uid-123"),
				},
				Spec: storagev1alpha1.VolumeRestoreRequestSpec{
					SourceRef: storagev1alpha1.ObjectReference{
						Kind: SourceKindVolumeSnapshotContent,
						Name: "test-vsc",
					},
					TargetNamespace: "default",
					TargetPVCName:   "restored-pvc",
				},
			}
			Expect(client.Create(ctx, vrr)).To(Succeed())

			// When: Reconcile called (PVC doesn't exist yet)
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-vrr", Namespace: "default"}}
			result, err := reconcileUntilObjectKeeperReady(req, vrr.UID)
			Expect(err).ToNot(HaveOccurred())
			// Should requeue to wait for PVC (polling interval)
			Expect(result.RequeueAfter).To(BeNumerically(">", 0), "Should requeue when PVC not found")

			// Verify ObjectKeeper created
			retainerName := NamePrefixRetainerPVC + string(vrr.UID)
			objectKeeper := &deckhousev1alpha1.ObjectKeeper{}
			Expect(client.Get(ctx, types.NamespacedName{Name: retainerName}, objectKeeper)).To(Succeed())
			Expect(objectKeeper.Spec.Mode).To(Equal("FollowObject"))
			Expect(objectKeeper.Spec.FollowObjectRef.UID).To(Equal(string(vrr.UID)))

			// When: Test creates PVC (simulating external-provisioner)
			createBoundPVC("default", "restored-pvc")

			// When: Reconcile called again
			result, err = ctrl.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())
			Expect(result.RequeueAfter).To(BeZero())

			// Then: VRR finalized
			finalVRR := &storagev1alpha1.VolumeRestoreRequest{}
			Expect(client.Get(ctx, types.NamespacedName{Name: "test-vrr", Namespace: "default"}, finalVRR)).To(Succeed())

			readyCondition := getCondition(finalVRR.Status.Conditions, storagev1alpha1.ConditionTypeReady)
			Expect(readyCondition).ToNot(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(readyCondition.Reason).To(Equal(storagev1alpha1.ConditionReasonCompleted))
			Expect(finalVRR.Status.CompletionTimestamp).ToNot(BeNil())
			Expect(finalVRR.Status.TargetPVCRef).ToNot(BeNil())
			Expect(finalVRR.Status.TargetPVCRef.Name).To(Equal("restored-pvc"))
			Expect(finalVRR.Status.TargetPVCRef.Namespace).To(Equal("default"))

			// Then: No VolumeSnapshot objects created
			verifyNoVolumeSnapshots()

			// Then: Terminal VRR not reconciled again
			result, err = ctrl.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())
			Expect(result.RequeueAfter).To(BeZero())

			// Verify status unchanged
			unchangedVRR := &storagev1alpha1.VolumeRestoreRequest{}
			Expect(client.Get(ctx, types.NamespacedName{Name: "test-vrr", Namespace: "default"}, unchangedVRR)).To(Succeed())
			Expect(unchangedVRR.Status.CompletionTimestamp).To(Equal(finalVRR.Status.CompletionTimestamp))
		})
	})

	Describe("Restore from PersistentVolume - Happy Path", func() {
		It("should finalize VRR when PVC becomes Bound", func() {
			// Given: PV exists
			createPV("test-pv")

			// Given: VRR exists
			vrr := &storagev1alpha1.VolumeRestoreRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vrr-pv",
					Namespace: "default",
					UID:       types.UID("vrr-uid-pv-123"),
				},
				Spec: storagev1alpha1.VolumeRestoreRequestSpec{
					SourceRef: storagev1alpha1.ObjectReference{
						Kind: SourceKindPersistentVolume,
						Name: "test-pv",
					},
					TargetNamespace: "default",
					TargetPVCName:   "restored-pvc-pv",
				},
			}
			Expect(client.Create(ctx, vrr)).To(Succeed())

			// When: Reconcile called (PVC doesn't exist yet)
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-vrr-pv", Namespace: "default"}}
			result, err := reconcileUntilObjectKeeperReady(req, vrr.UID)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0), "Should requeue when PVC not found")

			// Verify ObjectKeeper created
			retainerName := NamePrefixRetainerPVC + string(vrr.UID)
			objectKeeper := &deckhousev1alpha1.ObjectKeeper{}
			Expect(client.Get(ctx, types.NamespacedName{Name: retainerName}, objectKeeper)).To(Succeed())

			// When: Test creates PVC (simulating external-provisioner)
			createBoundPVC("default", "restored-pvc-pv")

			// When: Reconcile called again
			result, err = ctrl.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())

			// Then: VRR finalized
			finalVRR := &storagev1alpha1.VolumeRestoreRequest{}
			Expect(client.Get(ctx, types.NamespacedName{Name: "test-vrr-pv", Namespace: "default"}, finalVRR)).To(Succeed())

			readyCondition := getCondition(finalVRR.Status.Conditions, storagev1alpha1.ConditionTypeReady)
			Expect(readyCondition).ToNot(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(readyCondition.Reason).To(Equal(storagev1alpha1.ConditionReasonCompleted))
			Expect(finalVRR.Status.CompletionTimestamp).ToNot(BeNil())

			// Then: No VolumeSnapshot objects created
			verifyNoVolumeSnapshots()
		})
	})

	Describe("Polling Scenarios", func() {
		It("should requeue when PVC is not found", func() {
			// Given: VSC exists and is ReadyToUse
			createReadyVSC("test-vsc-poll")

			// Given: VRR exists
			vrr := &storagev1alpha1.VolumeRestoreRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vrr-poll",
					Namespace: "default",
					UID:       types.UID("vrr-uid-poll"),
				},
				Spec: storagev1alpha1.VolumeRestoreRequestSpec{
					SourceRef: storagev1alpha1.ObjectReference{
						Kind: SourceKindVolumeSnapshotContent,
						Name: "test-vsc-poll",
					},
					TargetNamespace: "default",
					TargetPVCName:   "restored-pvc-poll",
				},
			}
			Expect(client.Create(ctx, vrr)).To(Succeed())

			// When: Reconcile called
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-vrr-poll", Namespace: "default"}}
			result, err := reconcileUntilObjectKeeperReady(req, vrr.UID)
			Expect(err).ToNot(HaveOccurred())

			// Then: RequeueAfter set (for PVC polling)
			Expect(result.RequeueAfter).To(BeNumerically(">", 0), "Should requeue when PVC not found")

			// Then: VRR status not finalized (key invariant check)
			updatedVRR := &storagev1alpha1.VolumeRestoreRequest{}
			Expect(client.Get(ctx, types.NamespacedName{Name: "test-vrr-poll", Namespace: "default"}, updatedVRR)).To(Succeed())
			Expect(updatedVRR.Status.CompletionTimestamp).To(BeNil())

			// Then: ObjectKeeper created
			retainerName := NamePrefixRetainerPVC + string(vrr.UID)
			objectKeeper := &deckhousev1alpha1.ObjectKeeper{}
			Expect(client.Get(ctx, types.NamespacedName{Name: retainerName}, objectKeeper)).To(Succeed())

			// Then: PVC not created by controller (critical architectural invariant)
			verifyNoPVCCreatedByController("default", "restored-pvc-poll")
		})

		It("should requeue when PVC exists but not Bound", func() {
			// Given: VSC exists and is ReadyToUse
			createReadyVSC("test-vsc-pending")

			// Given: VRR exists
			vrr := &storagev1alpha1.VolumeRestoreRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vrr-pending",
					Namespace: "default",
					UID:       types.UID("vrr-uid-pending"),
				},
				Spec: storagev1alpha1.VolumeRestoreRequestSpec{
					SourceRef: storagev1alpha1.ObjectReference{
						Kind: SourceKindVolumeSnapshotContent,
						Name: "test-vsc-pending",
					},
					TargetNamespace: "default",
					TargetPVCName:   "restored-pvc-pending",
				},
			}
			Expect(client.Create(ctx, vrr)).To(Succeed())

			// Given: PVC exists but is Pending (simulating external-provisioner still working)
			createPendingPVC("default", "restored-pvc-pending")

			// When: Reconcile called
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-vrr-pending", Namespace: "default"}}
			result, err := reconcileUntilObjectKeeperReady(req, vrr.UID)
			Expect(err).ToNot(HaveOccurred())

			// Then: RequeueAfter set (for PVC polling)
			Expect(result.RequeueAfter).To(BeNumerically(">", 0), "Should requeue when PVC not Bound")
		})
	})

	Describe("Error Scenarios - Terminal", func() {
		It("should mark VRR failed when VSC is not found", func() {
			// Given: VRR exists but VSC doesn't
			vrr := &storagev1alpha1.VolumeRestoreRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vrr-not-found",
					Namespace: "default",
					UID:       types.UID("vrr-uid-not-found"),
				},
				Spec: storagev1alpha1.VolumeRestoreRequestSpec{
					SourceRef: storagev1alpha1.ObjectReference{
						Kind: SourceKindVolumeSnapshotContent,
						Name: "non-existent-vsc",
					},
					TargetNamespace: "default",
					TargetPVCName:   "restored-pvc-not-found",
				},
			}
			Expect(client.Create(ctx, vrr)).To(Succeed())

			// When: Reconcile called
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-vrr-not-found", Namespace: "default"}}
			result, err := ctrl.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())

			// Then: VRR marked as failed
			updatedVRR := &storagev1alpha1.VolumeRestoreRequest{}
			Expect(client.Get(ctx, types.NamespacedName{Name: "test-vrr-not-found", Namespace: "default"}, updatedVRR)).To(Succeed())

			readyCondition := getCondition(updatedVRR.Status.Conditions, storagev1alpha1.ConditionTypeReady)
			Expect(readyCondition).ToNot(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCondition.Reason).To(Equal(storagev1alpha1.ConditionReasonNotFound))
			Expect(updatedVRR.Status.CompletionTimestamp).ToNot(BeNil())

			// Then: ObjectKeeper not created
			retainerName := NamePrefixRetainerPVC + string(vrr.UID)
			objectKeeper := &deckhousev1alpha1.ObjectKeeper{}
			err = client.Get(ctx, types.NamespacedName{Name: retainerName}, objectKeeper)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())

			// Then: Repeated reconcile is no-op
			result, err = ctrl.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())
		})

		It("should mark VRR failed when PV is not found", func() {
			// Given: VRR exists but PV doesn't
			vrr := &storagev1alpha1.VolumeRestoreRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vrr-pv-not-found",
					Namespace: "default",
					UID:       types.UID("vrr-uid-pv-not-found"),
				},
				Spec: storagev1alpha1.VolumeRestoreRequestSpec{
					SourceRef: storagev1alpha1.ObjectReference{
						Kind: SourceKindPersistentVolume,
						Name: "non-existent-pv",
					},
					TargetNamespace: "default",
					TargetPVCName:   "restored-pvc-pv-not-found",
				},
			}
			Expect(client.Create(ctx, vrr)).To(Succeed())

			// When: Reconcile called
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-vrr-pv-not-found", Namespace: "default"}}
			result, err := ctrl.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())

			// Then: VRR marked as failed
			updatedVRR := &storagev1alpha1.VolumeRestoreRequest{}
			Expect(client.Get(ctx, types.NamespacedName{Name: "test-vrr-pv-not-found", Namespace: "default"}, updatedVRR)).To(Succeed())

			readyCondition := getCondition(updatedVRR.Status.Conditions, storagev1alpha1.ConditionTypeReady)
			Expect(readyCondition).ToNot(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCondition.Reason).To(Equal(storagev1alpha1.ConditionReasonNotFound))
		})

		It("should requeue when VSC exists but ReadyToUse is false", func() {
			// Given: VSC exists but not ready
			vsc := &snapshotv1.VolumeSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-vsc-not-ready",
					UID:  types.UID("vsc-uid-not-ready"),
				},
				Spec: snapshotv1.VolumeSnapshotContentSpec{
					Driver: "test-driver",
				},
				Status: &snapshotv1.VolumeSnapshotContentStatus{
					ReadyToUse: pointer.Bool(false),
				},
			}
			Expect(client.Create(ctx, vsc)).To(Succeed())

			// Given: VRR exists
			vrr := &storagev1alpha1.VolumeRestoreRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vrr-vsc-not-ready",
					Namespace: "default",
					UID:       types.UID("vrr-uid-vsc-not-ready"),
				},
				Spec: storagev1alpha1.VolumeRestoreRequestSpec{
					SourceRef: storagev1alpha1.ObjectReference{
						Kind: SourceKindVolumeSnapshotContent,
						Name: "test-vsc-not-ready",
					},
					TargetNamespace: "default",
					TargetPVCName:   "restored-pvc-not-ready",
				},
			}
			Expect(client.Create(ctx, vrr)).To(Succeed())

			// When: Reconcile called
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-vrr-vsc-not-ready", Namespace: "default"}}
			result, err := reconcileUntilObjectKeeperReady(req, vrr.UID)
			Expect(err).ToNot(HaveOccurred())

			// Then: RequeueAfter set (waiting for VSC to become ReadyToUse)
			Expect(result.RequeueAfter).To(BeNumerically(">", 0), "Should requeue when VSC not ReadyToUse")
		})

		It("should mark VRR failed when VSC has terminal error", func() {
			// Given: VSC exists with terminal error
			errorMsg := "CSI snapshot creation failed"
			vsc := &snapshotv1.VolumeSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-vsc-error",
					UID:  types.UID("vsc-uid-error"),
				},
				Spec: snapshotv1.VolumeSnapshotContentSpec{
					Driver: "test-driver",
				},
				Status: &snapshotv1.VolumeSnapshotContentStatus{
					ReadyToUse: pointer.Bool(false),
					Error: &snapshotv1.VolumeSnapshotError{
						Message: &errorMsg,
						Time:    &metav1.Time{Time: time.Now()},
					},
				},
			}
			Expect(client.Create(ctx, vsc)).To(Succeed())

			// Given: VRR exists
			vrr := &storagev1alpha1.VolumeRestoreRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vrr-vsc-error",
					Namespace: "default",
					UID:       types.UID("vrr-uid-vsc-error"),
				},
				Spec: storagev1alpha1.VolumeRestoreRequestSpec{
					SourceRef: storagev1alpha1.ObjectReference{
						Kind: SourceKindVolumeSnapshotContent,
						Name: "test-vsc-error",
					},
					TargetNamespace: "default",
					TargetPVCName:   "restored-pvc-error",
				},
			}
			Expect(client.Create(ctx, vrr)).To(Succeed())

			// When: Reconcile called
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-vrr-vsc-error", Namespace: "default"}}
			result, err := ctrl.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())

			// Then: VRR marked as failed
			updatedVRR := &storagev1alpha1.VolumeRestoreRequest{}
			Expect(client.Get(ctx, types.NamespacedName{Name: "test-vrr-vsc-error", Namespace: "default"}, updatedVRR)).To(Succeed())

			readyCondition := getCondition(updatedVRR.Status.Conditions, storagev1alpha1.ConditionTypeReady)
			Expect(readyCondition).ToNot(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
			Expect(readyCondition.Reason).To(Equal(storagev1alpha1.ConditionReasonInternalError))
			Expect(readyCondition.Message).To(ContainSubstring(errorMsg))
		})
	})

	Describe("ObjectKeeper Behavior", func() {
		It("should create ObjectKeeper only once", func() {
			// Given: VSC exists
			createReadyVSC("test-vsc-ok")

			// Given: VRR exists
			vrr := &storagev1alpha1.VolumeRestoreRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vrr-ok",
					Namespace: "default",
					UID:       types.UID("vrr-uid-ok"),
				},
				Spec: storagev1alpha1.VolumeRestoreRequestSpec{
					SourceRef: storagev1alpha1.ObjectReference{
						Kind: SourceKindVolumeSnapshotContent,
						Name: "test-vsc-ok",
					},
					TargetNamespace: "default",
					TargetPVCName:   "restored-pvc-ok",
				},
			}
			Expect(client.Create(ctx, vrr)).To(Succeed())

			// When: First reconcile
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-vrr-ok", Namespace: "default"}}
			_, err := reconcileUntilObjectKeeperReady(req, vrr.UID)
			Expect(err).ToNot(HaveOccurred())

			// Get ObjectKeeper UID
			retainerName := NamePrefixRetainerPVC + string(vrr.UID)
			objectKeeper := &deckhousev1alpha1.ObjectKeeper{}
			Expect(client.Get(ctx, types.NamespacedName{Name: retainerName}, objectKeeper)).To(Succeed())
			originalUID := objectKeeper.UID

			// When: Second reconcile
			_, err = ctrl.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())

			// Then: ObjectKeeper still exists with same UID
			updatedObjectKeeper := &deckhousev1alpha1.ObjectKeeper{}
			Expect(client.Get(ctx, types.NamespacedName{Name: retainerName}, updatedObjectKeeper)).To(Succeed())
			Expect(updatedObjectKeeper.UID).To(Equal(originalUID), "ObjectKeeper should not be recreated")
		})

		It("should return error when ObjectKeeper UID mismatch", func() {
			// CRITICAL: This tests a hard invariant violation.
			// ObjectKeeper UID mismatch indicates corrupted system state:
			// - VRR was deleted and recreated with same name but different UID
			// - ObjectKeeper from previous VRR still exists
			// This should never happen in normal operation, but controller must detect and fail fast.
			// Controller returns error (not markFailed) because this is a programming/system error, not a user error.

			// Given: VSC exists
			createReadyVSC("test-vsc-uid-mismatch")

			// Given: VRR exists
			vrr := &storagev1alpha1.VolumeRestoreRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vrr-uid-mismatch",
					Namespace: "default",
					UID:       types.UID("vrr-uid-correct"),
				},
				Spec: storagev1alpha1.VolumeRestoreRequestSpec{
					SourceRef: storagev1alpha1.ObjectReference{
						Kind: SourceKindVolumeSnapshotContent,
						Name: "test-vsc-uid-mismatch",
					},
					TargetNamespace: "default",
					TargetPVCName:   "restored-pvc-uid-mismatch",
				},
			}
			Expect(client.Create(ctx, vrr)).To(Succeed())

			// Given: ObjectKeeper exists with wrong UID (simulating corrupted state)
			retainerName := NamePrefixRetainerPVC + string(vrr.UID)
			objectKeeper := &deckhousev1alpha1.ObjectKeeper{
				ObjectMeta: metav1.ObjectMeta{
					Name: retainerName,
					UID:  types.UID("ok-uid-wrong"),
				},
				Spec: deckhousev1alpha1.ObjectKeeperSpec{
					Mode: "FollowObject",
					FollowObjectRef: &deckhousev1alpha1.FollowObjectRef{
						APIVersion: APIGroupStorageDeckhouse,
						Kind:       KindVolumeRestoreRequest,
						Namespace:  "default",
						Name:       "test-vrr-uid-mismatch",
						UID:        "wrong-uid", // Wrong UID - invariant violation
					},
				},
			}
			Expect(client.Create(ctx, objectKeeper)).To(Succeed())

			// When: Reconcile called
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-vrr-uid-mismatch", Namespace: "default"}}
			_, err := ctrl.Reconcile(ctx, req)

			// Then: Error returned (hard invariant violation)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("UID mismatch"))

			// Then: VRR status not updated (controller failed before status update)
			updatedVRR := &storagev1alpha1.VolumeRestoreRequest{}
			Expect(client.Get(ctx, types.NamespacedName{Name: "test-vrr-uid-mismatch", Namespace: "default"}, updatedVRR)).To(Succeed())
			Expect(updatedVRR.Status.CompletionTimestamp).To(BeNil())
		})

		It("should requeue when ObjectKeeper UID not populated", func() {
			// Given: VSC exists
			createReadyVSC("test-vsc-no-uid")

			// Given: VRR exists
			vrr := &storagev1alpha1.VolumeRestoreRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vrr-no-uid",
					Namespace: "default",
					UID:       types.UID("vrr-uid-no-uid"),
				},
				Spec: storagev1alpha1.VolumeRestoreRequestSpec{
					SourceRef: storagev1alpha1.ObjectReference{
						Kind: SourceKindVolumeSnapshotContent,
						Name: "test-vsc-no-uid",
					},
					TargetNamespace: "default",
					TargetPVCName:   "restored-pvc-no-uid",
				},
			}
			Expect(client.Create(ctx, vrr)).To(Succeed())

			// When: Reconcile called (may create ObjectKeeper)
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-vrr-no-uid", Namespace: "default"}}
			_, err := reconcileUntilObjectKeeperReady(req, vrr.UID)
			Expect(err).ToNot(HaveOccurred())

			// Then: VRR not finalized (waiting for PVC or ObjectKeeper UID)
			updatedVRR := &storagev1alpha1.VolumeRestoreRequest{}
			Expect(client.Get(ctx, types.NamespacedName{Name: "test-vrr-no-uid", Namespace: "default"}, updatedVRR)).To(Succeed())
			Expect(updatedVRR.Status.CompletionTimestamp).To(BeNil())
		})
	})

	Describe("Idempotency", func() {
		It("should not process terminal VRR again", func() {
			// Given: Terminal VRR (Ready=True)
			completionTime := metav1.Now()
			vrr := &storagev1alpha1.VolumeRestoreRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vrr-terminal",
					Namespace: "default",
					UID:       types.UID("vrr-uid-terminal"),
				},
				Spec: storagev1alpha1.VolumeRestoreRequestSpec{
					SourceRef: storagev1alpha1.ObjectReference{
						Kind: SourceKindVolumeSnapshotContent,
						Name: "test-vsc-terminal",
					},
					TargetNamespace: "default",
					TargetPVCName:   "restored-pvc-terminal",
				},
				Status: storagev1alpha1.VolumeRestoreRequestStatus{
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
			Expect(client.Create(ctx, vrr)).To(Succeed())

			// When: Reconcile called
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-vrr-terminal", Namespace: "default"}}
			result, err := ctrl.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())

			// Then: No requeue, no processing
			Expect(result.Requeue).To(BeFalse())
			Expect(result.RequeueAfter).To(BeZero())

			// Then: Status unchanged (time may differ slightly, so check that it exists and Ready condition unchanged)
			updatedVRR := &storagev1alpha1.VolumeRestoreRequest{}
			Expect(client.Get(ctx, types.NamespacedName{Name: "test-vrr-terminal", Namespace: "default"}, updatedVRR)).To(Succeed())
			Expect(updatedVRR.Status.CompletionTimestamp).ToNot(BeNil())
			readyCondition := getCondition(updatedVRR.Status.Conditions, storagev1alpha1.ConditionTypeReady)
			Expect(readyCondition).ToNot(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))

			// Then: ObjectKeeper not created
			retainerName := NamePrefixRetainerPVC + string(vrr.UID)
			objectKeeper := &deckhousev1alpha1.ObjectKeeper{}
			err = client.Get(ctx, types.NamespacedName{Name: retainerName}, objectKeeper)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("should ignore spec changes after Ready", func() {
			// Given: Terminal VRR (Ready=True)
			completionTime := metav1.Now()
			vrr := &storagev1alpha1.VolumeRestoreRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vrr-spec-change",
					Namespace: "default",
					UID:       types.UID("vrr-uid-spec-change"),
				},
				Spec: storagev1alpha1.VolumeRestoreRequestSpec{
					SourceRef: storagev1alpha1.ObjectReference{
						Kind: SourceKindVolumeSnapshotContent,
						Name: "test-vsc-spec-change",
					},
					TargetNamespace: "default",
					TargetPVCName:   "restored-pvc-spec-change",
				},
				Status: storagev1alpha1.VolumeRestoreRequestStatus{
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
			Expect(client.Create(ctx, vrr)).To(Succeed())

			// When: Spec changed manually
			updatedVRR := &storagev1alpha1.VolumeRestoreRequest{}
			Expect(client.Get(ctx, types.NamespacedName{Name: "test-vrr-spec-change", Namespace: "default"}, updatedVRR)).To(Succeed())
			updatedVRR.Spec.TargetPVCName = "changed-pvc-name"
			Expect(client.Update(ctx, updatedVRR)).To(Succeed())

			// When: Reconcile called
			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-vrr-spec-change", Namespace: "default"}}
			result, err := ctrl.Reconcile(ctx, req)
			Expect(err).ToNot(HaveOccurred())

			// Then: No-op (terminal VRR ignored)
			Expect(result.Requeue).To(BeFalse())
			Expect(result.RequeueAfter).To(BeZero())

			// Then: Status unchanged (time may differ slightly, so check that it exists and Ready condition unchanged)
			finalVRR := &storagev1alpha1.VolumeRestoreRequest{}
			Expect(client.Get(ctx, types.NamespacedName{Name: "test-vrr-spec-change", Namespace: "default"}, finalVRR)).To(Succeed())
			Expect(finalVRR.Status.CompletionTimestamp).ToNot(BeNil())
			readyCondition := getCondition(finalVRR.Status.Conditions, storagev1alpha1.ConditionTypeReady)
			Expect(readyCondition).ToNot(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
		})
	})

	Describe("TTL Scanner", func() {
		// NOTE: These tests use white-box testing approach - they call private method scanAndDeleteExpiredVRRs directly.
		// This is acceptable for unit testing TTL logic, but note that in production TTL scanner runs
		// as a background goroutine via StartTTLScanner (which is tested separately via integration tests).
		// Alternative approach would be to test via StartTTLScanner + time manipulation, but that's more complex.

		It("should delete VRR after TTL expired", func() {
			// Given: Terminal VRR with TTL expired
			now := time.Now()
			completionTime := metav1.NewTime(now.Add(-15 * time.Minute)) // 15 minutes ago, TTL=10m, so expired

			vrr := &storagev1alpha1.VolumeRestoreRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vrr-ttl-expire",
					Namespace: "default",
					UID:       types.UID("vrr-uid-ttl"),
				},
				Status: storagev1alpha1.VolumeRestoreRequestStatus{
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
			Expect(client.Create(ctx, vrr)).To(Succeed())

			// When: TTL scanner runs
			ctrl.scanAndDeleteExpiredVRRs(ctx, client)

			// Then: VRR deleted
			deletedVRR := &storagev1alpha1.VolumeRestoreRequest{}
			err := client.Get(ctx, types.NamespacedName{Name: "test-vrr-ttl-expire", Namespace: "default"}, deletedVRR)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "VRR should be deleted because config TTL (10m) expired")
		})

		It("should not delete non-terminal VRR", func() {
			// Given: Non-terminal VRR
			vrr := &storagev1alpha1.VolumeRestoreRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vrr-non-terminal",
					Namespace: "default",
					UID:       types.UID("vrr-uid-non-terminal"),
				},
				Status: storagev1alpha1.VolumeRestoreRequestStatus{
					// No Ready condition - non-terminal
				},
			}
			Expect(client.Create(ctx, vrr)).To(Succeed())

			// When: TTL scanner runs
			ctrl.scanAndDeleteExpiredVRRs(ctx, client)

			// Then: VRR still exists
			existingVRR := &storagev1alpha1.VolumeRestoreRequest{}
			err := client.Get(ctx, types.NamespacedName{Name: "test-vrr-non-terminal", Namespace: "default"}, existingVRR)
			Expect(err).ToNot(HaveOccurred(), "Non-terminal VRR should NOT be deleted")
		})

		It("should use config TTL, not annotation TTL", func() {
			// Given: Terminal VRR with annotation TTL=1h, but config TTL=10m
			// completionTime = 15m ago, so config TTL expired but annotation TTL not expired
			now := time.Now()
			completionTime := metav1.NewTime(now.Add(-15 * time.Minute))

			vrr := &storagev1alpha1.VolumeRestoreRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vrr-config-ttl",
					Namespace: "default",
					UID:       types.UID("vrr-uid-config-ttl"),
					Annotations: map[string]string{
						AnnotationKeyTTL: "1h", // Annotation TTL - should be ignored
					},
				},
				Status: storagev1alpha1.VolumeRestoreRequestStatus{
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
			Expect(client.Create(ctx, vrr)).To(Succeed())

			// When: TTL scanner runs
			ctrl.scanAndDeleteExpiredVRRs(ctx, client)

			// Then: VRR deleted (used config TTL, not annotation TTL)
			deletedVRR := &storagev1alpha1.VolumeRestoreRequest{}
			err := client.Get(ctx, types.NamespacedName{Name: "test-vrr-config-ttl", Namespace: "default"}, deletedVRR)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "VRR should be deleted because config TTL (10m) expired, not annotation TTL (1h)")
		})

		It("should delete Ready=False VRR when TTL expired", func() {
			// Given: Failed VRR with TTL expired
			now := time.Now()
			completionTime := metav1.NewTime(now.Add(-15 * time.Minute))

			vrr := &storagev1alpha1.VolumeRestoreRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vrr-failed-ttl",
					Namespace: "default",
					UID:       types.UID("vrr-uid-failed-ttl"),
				},
				Status: storagev1alpha1.VolumeRestoreRequestStatus{
					CompletionTimestamp: &completionTime,
					Conditions: []metav1.Condition{
						{
							Type:               storagev1alpha1.ConditionTypeReady,
							Status:             metav1.ConditionFalse,
							Reason:             storagev1alpha1.ConditionReasonNotFound,
							LastTransitionTime: completionTime,
						},
					},
				},
			}
			Expect(client.Create(ctx, vrr)).To(Succeed())

			// When: TTL scanner runs
			ctrl.scanAndDeleteExpiredVRRs(ctx, client)

			// Then: VRR deleted
			deletedVRR := &storagev1alpha1.VolumeRestoreRequest{}
			err := client.Get(ctx, types.NamespacedName{Name: "test-vrr-failed-ttl", Namespace: "default"}, deletedVRR)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Failed VRR should be deleted when TTL expired")
		})
	})

	Describe("setTTLAnnotation", func() {
		It("should set TTL annotation when not exists", func() {
			vrr := &storagev1alpha1.VolumeRestoreRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vrr",
					Namespace: "default",
				},
			}

			ctrl.setTTLAnnotation(vrr)

			Expect(vrr.Annotations).ToNot(BeNil())
			Expect(vrr.Annotations[AnnotationKeyTTL]).To(Equal("10m"))
		})

		It("should not overwrite existing TTL annotation", func() {
			vrr := &storagev1alpha1.VolumeRestoreRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vrr",
					Namespace: "default",
					Annotations: map[string]string{
						AnnotationKeyTTL: "30m",
					},
				},
			}

			ctrl.setTTLAnnotation(vrr)

			Expect(vrr.Annotations[AnnotationKeyTTL]).To(Equal("30m"))
		})

		It("should use config TTL when available", func() {
			cfg.RequestTTLStr = "20m"
			vrr := &storagev1alpha1.VolumeRestoreRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vrr",
					Namespace: "default",
				},
			}

			ctrl.setTTLAnnotation(vrr)

			Expect(vrr.Annotations[AnnotationKeyTTL]).To(Equal("20m"))
		})
	})
})
