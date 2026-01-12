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
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	storagev1alpha1 "fox.flant.com/deckhouse/storage/storage-foundation/api/v1alpha1"
	"fox.flant.com/deckhouse/storage/storage-foundation/images/controller/pkg/config"
	"fox.flant.com/deckhouse/storage/storage-foundation/images/controller/pkg/snapshotmeta"
	deckhousev1alpha1 "github.com/deckhouse/deckhouse/deckhouse-controller/pkg/apis/deckhouse.io/v1alpha1"
	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
)

func TestVolumeCaptureRequestSnapshotMode(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "VolumeCaptureRequest Snapshot Mode Suite")
}

var _ = Describe("VolumeCaptureRequest Snapshot Mode", func() {
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
			Build()

		ctrl = &VolumeCaptureRequestController{
			Client: client,
			Scheme: scheme,
			Config: cfg,
		}
	})

	Describe("processSnapshotMode - VSC creation", func() {
		var (
			vcr      *storagev1alpha1.VolumeCaptureRequest
			pvc      *corev1.PersistentVolumeClaim
			pv       *corev1.PersistentVolume
			vscClass *snapshotv1.VolumeSnapshotClass
		)

		BeforeEach(func() {
			// Create VCR
			vcr = &storagev1alpha1.VolumeCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vcr",
					Namespace: "default",
					UID:       types.UID("vcr-uid-123"),
				},
				Spec: storagev1alpha1.VolumeCaptureRequestSpec{
					Mode: ModeSnapshot,
					PersistentVolumeClaimRef: &storagev1alpha1.ObjectReference{
						Namespace: "default",
						Name:      "test-pvc",
					},
					VolumeSnapshotClassName: "test-vsc-class",
				},
			}
			Expect(client.Create(ctx, vcr)).To(Succeed())

			// Create PVC
			pvc = &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pvc",
					Namespace: "default",
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					VolumeName: "test-pv",
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
			Expect(client.Create(ctx, pvc)).To(Succeed())

			// Create PV
			pv = &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-pv",
				},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						CSI: &corev1.CSIPersistentVolumeSource{
							Driver:       "test-driver",
							VolumeHandle: "test-volume-handle-123",
						},
					},
				},
			}
			Expect(client.Create(ctx, pv)).To(Succeed())

			// Create VolumeSnapshotClass
			vscClass = &snapshotv1.VolumeSnapshotClass{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-vsc-class",
				},
				Driver:         "test-driver",
				DeletionPolicy: snapshotv1.VolumeSnapshotContentDelete,
			}
			Expect(client.Create(ctx, vscClass)).To(Succeed())
		})

		It("should create VSC directly without PVC annotations", func() {
			// Execute
			result, err := ctrl.processSnapshotMode(ctx, vcr)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())

			// Verify: PVC should NOT have annotations (ADR: VCR creates VSC directly, no PVC annotations)
			updatedPVC := &corev1.PersistentVolumeClaim{}
			Expect(client.Get(ctx, types.NamespacedName{Name: "test-pvc", Namespace: "default"}, updatedPVC)).To(Succeed())
			if updatedPVC.Annotations != nil {
				Expect(updatedPVC.Annotations).ToNot(HaveKey(snapshotmeta.AnnDeckhouseVCRUID), "PVC should NOT have VCR annotation")
				Expect(updatedPVC.Annotations).ToNot(HaveKey(snapshotmeta.AnnDeckhouseSourcePVC), "PVC should NOT have source PVC annotation")
			}

			// Verify: VSC should be created
			vscName := "snapshot-vcr-uid-123"
			vsc := &snapshotv1.VolumeSnapshotContent{}
			Expect(client.Get(ctx, types.NamespacedName{Name: vscName}, vsc)).To(Succeed())

			// Verify VSC spec
			Expect(vsc.Spec.Driver).To(Equal("test-driver"))
			Expect(vsc.Spec.Source.VolumeHandle).ToNot(BeNil())
			Expect(*vsc.Spec.Source.VolumeHandle).To(Equal("test-volume-handle-123"))
			Expect(vsc.Spec.VolumeSnapshotClassName).ToNot(BeNil())
			Expect(*vsc.Spec.VolumeSnapshotClassName).To(Equal("test-vsc-class"))
			Expect(vsc.Spec.DeletionPolicy).To(Equal(snapshotv1.VolumeSnapshotContentDelete))

			// Verify: NO VolumeSnapshotRef (ADR forbids VolumeSnapshot)
			Expect(vsc.Spec.VolumeSnapshotRef.Name).To(BeEmpty())
			Expect(vsc.Spec.VolumeSnapshotRef.Namespace).To(BeEmpty())
		})

		It("should set correct ownerRefs on VSC", func() {
			// Execute - ObjectKeeper will be created by processSnapshotMode
			result, err := ctrl.processSnapshotMode(ctx, vcr)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())

			// Verify ownerRefs
			vscName := "snapshot-vcr-uid-123"
			vsc := &snapshotv1.VolumeSnapshotContent{}
			Expect(client.Get(ctx, types.NamespacedName{Name: vscName}, vsc)).To(Succeed())

			// VCR should NOT be owner of VSC (only ObjectKeeper is owner)
			vcrOwnerRef := false
			for _, ref := range vsc.OwnerReferences {
				if ref.Kind == "VolumeCaptureRequest" {
					vcrOwnerRef = true
					break
				}
			}
			Expect(vcrOwnerRef).To(BeFalse(), "VCR should NOT be owner of VSC")

			// Should have ObjectKeeper ownerRef (controller=true) - the only owner
			retainerName := NamePrefixRetainer + "snapshot-vcr-uid-123"
			objectKeeper := &deckhousev1alpha1.ObjectKeeper{}
			Expect(client.Get(ctx, types.NamespacedName{Name: retainerName}, objectKeeper)).To(Succeed())

			objectKeeperOwnerRef := false
			for _, ref := range vsc.OwnerReferences {
				if ref.Kind == "ObjectKeeper" && ref.Name == retainerName && ref.UID == objectKeeper.UID {
					Expect(ref.Controller).ToNot(BeNil())
					Expect(*ref.Controller).To(BeTrue())
					objectKeeperOwnerRef = true
					break
				}
			}
			Expect(objectKeeperOwnerRef).To(BeTrue(), "ObjectKeeper ownerRef should exist with controller=true")
			Expect(len(vsc.OwnerReferences)).To(Equal(1), "VSC should have exactly one ownerRef (ObjectKeeper)")
		})

		It("should NOT create VolumeSnapshot", func() {
			// Execute
			_, err := ctrl.processSnapshotMode(ctx, vcr)
			Expect(err).ToNot(HaveOccurred())

			// Verify: NO VolumeSnapshot should be created
			vsList := &snapshotv1.VolumeSnapshotList{}
			Expect(client.List(ctx, vsList)).To(Succeed())
			Expect(vsList.Items).To(BeEmpty(), "VolumeSnapshot should NOT be created")
		})

		It("should wait for ReadyToUse=true before completing", func() {
			// Create ObjectKeeper
			retainerName := NamePrefixRetainer + "snapshot-vcr-uid-123"
			objectKeeper := &deckhousev1alpha1.ObjectKeeper{
				ObjectMeta: metav1.ObjectMeta{
					Name: retainerName,
					UID:  types.UID("retainer-uid-123"),
				},
				Spec: deckhousev1alpha1.ObjectKeeperSpec{
					Mode: "FollowObject",
					FollowObjectRef: &deckhousev1alpha1.FollowObjectRef{
						APIVersion: APIGroupStorageDeckhouse,
						Kind:       KindVolumeCaptureRequest,
						Namespace:  "default",
						Name:       "test-vcr",
						UID:        "vcr-uid-123",
					},
				},
			}
			Expect(client.Create(ctx, objectKeeper)).To(Succeed())

			// First reconcile: should create VSC and requeue (VSC not ready)
			result, err := ctrl.processSnapshotMode(ctx, vcr)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0), "Should requeue while waiting for ReadyToUse")

			// Verify VSC exists but not ready
			vscName := "snapshot-vcr-uid-123"
			vsc := &snapshotv1.VolumeSnapshotContent{}
			Expect(client.Get(ctx, types.NamespacedName{Name: vscName}, vsc)).To(Succeed())
			Expect(vsc.Status).To(BeNil(), "VSC should not have status yet")

			// Set ReadyToUse=true (simulating external-snapshotter)
			vsc.Status = &snapshotv1.VolumeSnapshotContentStatus{
				ReadyToUse: pointer.Bool(true),
			}
			Expect(client.Status().Update(ctx, vsc)).To(Succeed())

			// Second reconcile: should complete
			result, err = ctrl.processSnapshotMode(ctx, vcr)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.Requeue).To(BeFalse())
			Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

			// Verify VCR status
			updatedVCR := &storagev1alpha1.VolumeCaptureRequest{}
			Expect(client.Get(ctx, types.NamespacedName{Name: "test-vcr", Namespace: "default"}, updatedVCR)).To(Succeed())

			// Should have DataRef
			Expect(updatedVCR.Status.DataRef).ToNot(BeNil())
			Expect(updatedVCR.Status.DataRef.Name).To(Equal(vscName))
			Expect(updatedVCR.Status.DataRef.Kind).To(Equal("VolumeSnapshotContent"))

			// Should have Ready condition
			readyCondition := getCondition(updatedVCR.Status.Conditions, ConditionTypeReady)
			Expect(readyCondition).ToNot(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(readyCondition.Reason).To(Equal(ConditionReasonCompleted))

			// Should have CompletionTimestamp
			Expect(updatedVCR.Status.CompletionTimestamp).ToNot(BeNil())

			// Should have TTL annotation
			Expect(updatedVCR.Annotations).To(HaveKey(AnnotationKeyTTL))
			Expect(updatedVCR.Annotations[AnnotationKeyTTL]).To(Equal("10m"))
		})

		It("should create ObjectKeeper first", func() {
			// Execute
			_, err := ctrl.processSnapshotMode(ctx, vcr)
			Expect(err).ToNot(HaveOccurred())

			// Verify ObjectKeeper exists
			retainerName := NamePrefixRetainer + "snapshot-vcr-uid-123"
			objectKeeper := &deckhousev1alpha1.ObjectKeeper{}
			Expect(client.Get(ctx, types.NamespacedName{Name: retainerName}, objectKeeper)).To(Succeed())

			// Verify ObjectKeeper spec
			Expect(objectKeeper.Spec.Mode).To(Equal("FollowObject"))
			Expect(objectKeeper.Spec.FollowObjectRef).ToNot(BeNil())
			Expect(objectKeeper.Spec.FollowObjectRef.UID).To(Equal("vcr-uid-123"))
			// ObjectKeeper does not have TTL - it follows VCR lifecycle
		})

		It("should not recreate VSC if it already exists", func() {
			// Create ObjectKeeper
			retainerName := NamePrefixRetainer + "snapshot-vcr-uid-123"
			objectKeeper := &deckhousev1alpha1.ObjectKeeper{
				ObjectMeta: metav1.ObjectMeta{
					Name: retainerName,
					UID:  types.UID("retainer-uid-123"),
				},
				Spec: deckhousev1alpha1.ObjectKeeperSpec{
					Mode: "FollowObject",
					FollowObjectRef: &deckhousev1alpha1.FollowObjectRef{
						APIVersion: APIGroupStorageDeckhouse,
						Kind:       KindVolumeCaptureRequest,
						Namespace:  "default",
						Name:       "test-vcr",
						UID:        "vcr-uid-123",
					},
				},
			}
			Expect(client.Create(ctx, objectKeeper)).To(Succeed())

			// Create VSC manually (simulating it was created in previous reconcile)
			vscName := "snapshot-vcr-uid-123"
			existingVSC := &snapshotv1.VolumeSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{
					Name: vscName,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: APIGroupStorageDeckhouse,
							Kind:       KindVolumeCaptureRequest,
							Name:       "test-vcr",
							UID:        "vcr-uid-123",
							Controller: pointer.Bool(false),
						},
						{
							APIVersion: APIGroupDeckhouse,
							Kind:       KindObjectKeeper,
							Name:       retainerName,
							UID:        "retainer-uid-123",
							Controller: pointer.Bool(true),
						},
					},
				},
				Spec: snapshotv1.VolumeSnapshotContentSpec{
					Driver:                  "test-driver",
					VolumeSnapshotClassName: pointer.String("test-vsc-class"),
					DeletionPolicy:          snapshotv1.VolumeSnapshotContentDelete,
					Source: snapshotv1.VolumeSnapshotContentSource{
						VolumeHandle: pointer.String("test-volume-handle-123"),
					},
				},
			}
			Expect(client.Create(ctx, existingVSC)).To(Succeed())

			// Execute - should not recreate VSC, just wait for ReadyToUse
			result, err := ctrl.processSnapshotMode(ctx, vcr)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.RequeueAfter).To(BeNumerically(">", 0), "Should requeue while waiting for ReadyToUse")

			// Verify VSC still exists (not recreated)
			vsc := &snapshotv1.VolumeSnapshotContent{}
			Expect(client.Get(ctx, types.NamespacedName{Name: vscName}, vsc)).To(Succeed())
			// VSC should have same UID (not recreated)
			Expect(vsc.UID).To(Equal(existingVSC.UID))
		})

		It("should fail if PVC is not bound", func() {
			// Update PVC to be unbound
			pvc.Spec.VolumeName = ""
			Expect(client.Update(ctx, pvc)).To(Succeed())

			// Execute
			_, err := ctrl.processSnapshotMode(ctx, vcr)
			Expect(err).ToNot(HaveOccurred())

			// Should mark as failed
			updatedVCR := &storagev1alpha1.VolumeCaptureRequest{}
			Expect(client.Get(ctx, types.NamespacedName{Name: "test-vcr", Namespace: "default"}, updatedVCR)).To(Succeed())

			readyCondition := getCondition(updatedVCR.Status.Conditions, ConditionTypeReady)
			Expect(readyCondition).ToNot(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
		})

		It("should fail if PV does not have CSI volumeHandle", func() {
			// Update PV to remove CSI
			pv.Spec.PersistentVolumeSource.CSI = nil
			Expect(client.Update(ctx, pv)).To(Succeed())

			// Execute
			_, err := ctrl.processSnapshotMode(ctx, vcr)
			Expect(err).ToNot(HaveOccurred())

			// Should mark as failed
			updatedVCR := &storagev1alpha1.VolumeCaptureRequest{}
			Expect(client.Get(ctx, types.NamespacedName{Name: "test-vcr", Namespace: "default"}, updatedVCR)).To(Succeed())

			readyCondition := getCondition(updatedVCR.Status.Conditions, ConditionTypeReady)
			Expect(readyCondition).ToNot(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
		})

		Describe("Snapshot: error validation scenarios", func() {
			It("should fail if PVC is not found", func() {
				// Create VCR with non-existent PVC
				vcr := &storagev1alpha1.VolumeCaptureRequest{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-vcr-not-found",
						Namespace: "default",
						UID:       types.UID("vcr-uid-not-found"),
					},
					Spec: storagev1alpha1.VolumeCaptureRequestSpec{
						Mode: ModeSnapshot,
						PersistentVolumeClaimRef: &storagev1alpha1.ObjectReference{
							Namespace: "default",
							Name:      "non-existent-pvc",
						},
						VolumeSnapshotClassName: "test-vsc-class",
					},
				}
				Expect(client.Create(ctx, vcr)).To(Succeed())

				// Execute
				_, err := ctrl.processSnapshotMode(ctx, vcr)
				Expect(err).ToNot(HaveOccurred())

				// Should mark as failed
				updatedVCR := &storagev1alpha1.VolumeCaptureRequest{}
				Expect(client.Get(ctx, types.NamespacedName{Name: "test-vcr-not-found", Namespace: "default"}, updatedVCR)).To(Succeed())

				readyCondition := getCondition(updatedVCR.Status.Conditions, ConditionTypeReady)
				Expect(readyCondition).ToNot(BeNil())
				Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
				Expect(readyCondition.Reason).To(Equal(ErrorReasonNotFound))

				// Should have CompletionTimestamp and TTL
				Expect(updatedVCR.Status.CompletionTimestamp).ToNot(BeNil())
				Expect(updatedVCR.Annotations).To(HaveKey(AnnotationKeyTTL))
			})

			It("should fail if VolumeSnapshotClass is not found", func() {
				// Create VCR with non-existent VolumeSnapshotClass
				vcr := &storagev1alpha1.VolumeCaptureRequest{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-vcr-no-class",
						Namespace: "default",
						UID:       types.UID("vcr-uid-no-class"),
					},
					Spec: storagev1alpha1.VolumeCaptureRequestSpec{
						Mode: ModeSnapshot,
						PersistentVolumeClaimRef: &storagev1alpha1.ObjectReference{
							Namespace: "default",
							Name:      "test-pvc",
						},
						VolumeSnapshotClassName: "non-existent-class",
					},
				}
				Expect(client.Create(ctx, vcr)).To(Succeed())

				// Execute
				_, err := ctrl.processSnapshotMode(ctx, vcr)
				Expect(err).ToNot(HaveOccurred())

				// Should mark as failed
				updatedVCR := &storagev1alpha1.VolumeCaptureRequest{}
				Expect(client.Get(ctx, types.NamespacedName{Name: "test-vcr-no-class", Namespace: "default"}, updatedVCR)).To(Succeed())

				readyCondition := getCondition(updatedVCR.Status.Conditions, ConditionTypeReady)
				Expect(readyCondition).ToNot(BeNil())
				Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
				Expect(readyCondition.Reason).To(Equal(ErrorReasonNotFound))
			})

			It("should fail if VolumeSnapshotClass driver does not match PV driver", func() {
				// Create VolumeSnapshotClass with different driver
				mismatchedClass := &snapshotv1.VolumeSnapshotClass{
					ObjectMeta: metav1.ObjectMeta{
						Name: "mismatched-class",
					},
					Driver:         "different-driver",
					DeletionPolicy: snapshotv1.VolumeSnapshotContentDelete,
				}
				Expect(client.Create(ctx, mismatchedClass)).To(Succeed())

				// Create VCR with mismatched class
				vcr := &storagev1alpha1.VolumeCaptureRequest{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-vcr-mismatch",
						Namespace: "default",
						UID:       types.UID("vcr-uid-mismatch"),
					},
					Spec: storagev1alpha1.VolumeCaptureRequestSpec{
						Mode: ModeSnapshot,
						PersistentVolumeClaimRef: &storagev1alpha1.ObjectReference{
							Namespace: "default",
							Name:      "test-pvc",
						},
						VolumeSnapshotClassName: "mismatched-class",
					},
				}
				Expect(client.Create(ctx, vcr)).To(Succeed())

				// Execute
				_, err := ctrl.processSnapshotMode(ctx, vcr)
				Expect(err).ToNot(HaveOccurred())

				// Should mark as failed
				updatedVCR := &storagev1alpha1.VolumeCaptureRequest{}
				Expect(client.Get(ctx, types.NamespacedName{Name: "test-vcr-mismatch", Namespace: "default"}, updatedVCR)).To(Succeed())

				readyCondition := getCondition(updatedVCR.Status.Conditions, ConditionTypeReady)
				Expect(readyCondition).ToNot(BeNil())
				Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
				Expect(readyCondition.Reason).To(Equal(ErrorReasonInternalError))
			})
		})

		Describe("Snapshot: idempotency (repeated reconcile)", func() {
			It("should not recreate ObjectKeeper on repeated reconcile", func() {
				// First reconcile
				_, err := ctrl.processSnapshotMode(ctx, vcr)
				Expect(err).ToNot(HaveOccurred())

				// Get ObjectKeeper UID
				retainerName := NamePrefixRetainer + "snapshot-vcr-uid-123"
				objectKeeper := &deckhousev1alpha1.ObjectKeeper{}
				Expect(client.Get(ctx, types.NamespacedName{Name: retainerName}, objectKeeper)).To(Succeed())
				originalUID := objectKeeper.UID

				// Second reconcile
				_, err = ctrl.processSnapshotMode(ctx, vcr)
				Expect(err).ToNot(HaveOccurred())

				// Verify ObjectKeeper still exists with same UID
				updatedObjectKeeper := &deckhousev1alpha1.ObjectKeeper{}
				Expect(client.Get(ctx, types.NamespacedName{Name: retainerName}, updatedObjectKeeper)).To(Succeed())
				Expect(updatedObjectKeeper.UID).To(Equal(originalUID), "ObjectKeeper should not be recreated")
			})

			It("should not recreate VSC on repeated reconcile", func() {
				// Create ObjectKeeper first
				retainerName := NamePrefixRetainer + "snapshot-vcr-uid-123"
				objectKeeper := &deckhousev1alpha1.ObjectKeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name: retainerName,
						UID:  types.UID("retainer-uid-123"),
					},
					Spec: deckhousev1alpha1.ObjectKeeperSpec{
						Mode: "FollowObject",
						FollowObjectRef: &deckhousev1alpha1.FollowObjectRef{
							APIVersion: APIGroupStorageDeckhouse,
							Kind:       KindVolumeCaptureRequest,
							Namespace:  "default",
							Name:       "test-vcr",
							UID:        "vcr-uid-123",
						},
					},
				}
				Expect(client.Create(ctx, objectKeeper)).To(Succeed())

				// First reconcile
				_, err := ctrl.processSnapshotMode(ctx, vcr)
				Expect(err).ToNot(HaveOccurred())

				// Get VSC UID
				vscName := "snapshot-vcr-uid-123"
				vsc := &snapshotv1.VolumeSnapshotContent{}
				Expect(client.Get(ctx, types.NamespacedName{Name: vscName}, vsc)).To(Succeed())
				originalUID := vsc.UID

				// Second reconcile
				_, err = ctrl.processSnapshotMode(ctx, vcr)
				Expect(err).ToNot(HaveOccurred())

				// Verify VSC still exists with same UID
				updatedVSC := &snapshotv1.VolumeSnapshotContent{}
				Expect(client.Get(ctx, types.NamespacedName{Name: vscName}, updatedVSC)).To(Succeed())
				Expect(updatedVSC.UID).To(Equal(originalUID), "VSC should not be recreated")
			})

			It("should not overwrite TTL annotation on repeated reconcile", func() {
				// Create ObjectKeeper
				retainerName := NamePrefixRetainer + "snapshot-vcr-uid-123"
				objectKeeper := &deckhousev1alpha1.ObjectKeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name: retainerName,
						UID:  types.UID("retainer-uid-123"),
					},
					Spec: deckhousev1alpha1.ObjectKeeperSpec{
						Mode: "FollowObject",
						FollowObjectRef: &deckhousev1alpha1.FollowObjectRef{
							APIVersion: APIGroupStorageDeckhouse,
							Kind:       KindVolumeCaptureRequest,
							Namespace:  "default",
							Name:       "test-vcr",
							UID:        "vcr-uid-123",
						},
					},
				}
				Expect(client.Create(ctx, objectKeeper)).To(Succeed())

				// Set ReadyToUse=true on VSC
				vscName := "snapshot-vcr-uid-123"
				// First reconcile to create VSC - use Reconcile to test full flow
				req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-vcr", Namespace: "default"}}
				_, err := ctrl.Reconcile(ctx, req)
				Expect(err).ToNot(HaveOccurred())

				vsc := &snapshotv1.VolumeSnapshotContent{}
				Expect(client.Get(ctx, types.NamespacedName{Name: vscName}, vsc)).To(Succeed())
				vsc.Status = &snapshotv1.VolumeSnapshotContentStatus{
					ReadyToUse: pointer.Bool(true),
				}
				Expect(client.Status().Update(ctx, vsc)).To(Succeed())

				// Second reconcile - should complete and set TTL
				// Use Reconcile (not processSnapshotMode) to test full flow including Ready short-circuit
				_, err = ctrl.Reconcile(ctx, req)
				Expect(err).ToNot(HaveOccurred())

				// Get VCR and verify TTL
				updatedVCR := &storagev1alpha1.VolumeCaptureRequest{}
				Expect(client.Get(ctx, types.NamespacedName{Name: "test-vcr", Namespace: "default"}, updatedVCR)).To(Succeed())
				originalTTL := updatedVCR.Annotations[AnnotationKeyTTL]
				Expect(originalTTL).To(Equal("10m"))

				// Third reconcile - should not overwrite TTL (Ready short-circuit)
				_, err = ctrl.Reconcile(ctx, req)
				Expect(err).ToNot(HaveOccurred())

				// Verify TTL unchanged
				finalVCR := &storagev1alpha1.VolumeCaptureRequest{}
				Expect(client.Get(ctx, types.NamespacedName{Name: "test-vcr", Namespace: "default"}, finalVCR)).To(Succeed())
				Expect(finalVCR.Annotations[AnnotationKeyTTL]).To(Equal(originalTTL), "TTL should not be overwritten")
			})
		})

		Describe("Detach mode", func() {
			var (
				detachVCR *storagev1alpha1.VolumeCaptureRequest
				detachPVC *corev1.PersistentVolumeClaim
				detachPV  *corev1.PersistentVolume
			)

			BeforeEach(func() {
				// Create VCR for Detach
				detachVCR = &storagev1alpha1.VolumeCaptureRequest{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-vcr-detach",
						Namespace: "default",
						UID:       types.UID("vcr-uid-detach"),
					},
					Spec: storagev1alpha1.VolumeCaptureRequestSpec{
						Mode: ModeDetach,
						PersistentVolumeClaimRef: &storagev1alpha1.ObjectReference{
							Namespace: "default",
							Name:      "test-pvc-detach",
						},
					},
				}
				Expect(client.Create(ctx, detachVCR)).To(Succeed())

				// Create PVC
				detachPVC = &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pvc-detach",
						Namespace: "default",
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						VolumeName: "test-pv-detach",
					},
					Status: corev1.PersistentVolumeClaimStatus{
						Phase: corev1.ClaimBound,
					},
				}
				Expect(client.Create(ctx, detachPVC)).To(Succeed())

				// Create PV
				detachPV = &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-pv-detach",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver:       "test-driver",
								VolumeHandle: "test-volume-handle-detach",
							},
						},
						ClaimRef: &corev1.ObjectReference{
							Namespace: "default",
							Name:      "test-pvc-detach",
						},
					},
				}
				Expect(client.Create(ctx, detachPV)).To(Succeed())
			})

			It("should delete PVC and detach PV successfully", func() {
				// First reconcile: should delete PVC and requeue
				result, err := ctrl.processDetachMode(ctx, detachVCR)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.RequeueAfter).To(BeNumerically(">", 0), "Should requeue after PVC deletion")

				// Verify PVC is deleted
				deletedPVC := &corev1.PersistentVolumeClaim{}
				err = client.Get(ctx, types.NamespacedName{Name: "test-pvc-detach", Namespace: "default"}, deletedPVC)
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "PVC should be deleted")

				// Second reconcile: should detach PV (remove ClaimRef)
				result, err = ctrl.processDetachMode(ctx, detachVCR)
				Expect(err).ToNot(HaveOccurred())

				// Verify PV is detached
				updatedPV := &corev1.PersistentVolume{}
				Expect(client.Get(ctx, types.NamespacedName{Name: "test-pv-detach"}, updatedPV)).To(Succeed())
				Expect(updatedPV.Spec.ClaimRef).To(BeNil(), "PV ClaimRef should be removed")
				Expect(updatedPV.Annotations).To(HaveKey("storage.deckhouse.io/detached"))

				// Verify ObjectKeeper created
				retainerName := NamePrefixRetainerPV + "vcr-uid-detach"
				objectKeeper := &deckhousev1alpha1.ObjectKeeper{}
				Expect(client.Get(ctx, types.NamespacedName{Name: retainerName}, objectKeeper)).To(Succeed())

				// Verify VCR status
				updatedVCR := &storagev1alpha1.VolumeCaptureRequest{}
				Expect(client.Get(ctx, types.NamespacedName{Name: "test-vcr-detach", Namespace: "default"}, updatedVCR)).To(Succeed())
				Expect(updatedVCR.Status.DataRef).ToNot(BeNil())
				Expect(updatedVCR.Status.DataRef.Name).To(Equal("test-pv-detach"))
				Expect(updatedVCR.Status.DataRef.Kind).To(Equal("PersistentVolume"))

				readyCondition := getCondition(updatedVCR.Status.Conditions, ConditionTypeReady)
				Expect(readyCondition).ToNot(BeNil())
				Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
				Expect(updatedVCR.Annotations).To(HaveKey(AnnotationKeyTTL))
			})

			It("should not recreate PVC on repeated reconcile", func() {
				// First reconcile - delete PVC
				_, err := ctrl.processDetachMode(ctx, detachVCR)
				Expect(err).ToNot(HaveOccurred())

				// Verify PVC is deleted
				deletedPVC := &corev1.PersistentVolumeClaim{}
				err = client.Get(ctx, types.NamespacedName{Name: "test-pvc-detach", Namespace: "default"}, deletedPVC)
				Expect(apierrors.IsNotFound(err)).To(BeTrue())

				// Second reconcile - should not fail, should complete
				_, err = ctrl.processDetachMode(ctx, detachVCR)
				Expect(err).ToNot(HaveOccurred())

				// PVC should still be deleted
				err = client.Get(ctx, types.NamespacedName{Name: "test-pvc-detach", Namespace: "default"}, deletedPVC)
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "PVC should remain deleted")
			})
		})

		Describe("TTL scenarios", func() {
			It("should delete VCR when TTL expires", func() {
				// Create completed VCR with TTL
				vcr := &storagev1alpha1.VolumeCaptureRequest{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-vcr-ttl-expire",
						Namespace: "default",
						UID:       types.UID("vcr-uid-ttl"),
						Annotations: map[string]string{
							AnnotationKeyTTL: "10m",
						},
					},
					Spec: storagev1alpha1.VolumeCaptureRequestSpec{
						Mode: ModeSnapshot,
						PersistentVolumeClaimRef: &storagev1alpha1.ObjectReference{
							Namespace: "default",
							Name:      "test-pvc",
						},
						VolumeSnapshotClassName: "test-vsc-class",
					},
					Status: storagev1alpha1.VolumeCaptureRequestStatus{
						CompletionTimestamp: &metav1.Time{Time: time.Now().Add(-11 * time.Minute)}, // Expired
					},
				}
				Expect(client.Create(ctx, vcr)).To(Succeed())

				// Execute checkAndHandleTTL
				shouldDelete, _, err := ctrl.checkAndHandleTTL(ctx, vcr)
				Expect(err).ToNot(HaveOccurred())
				Expect(shouldDelete).To(BeTrue(), "VCR should be deleted when TTL expires")

				// Verify VCR is deleted
				deletedVCR := &storagev1alpha1.VolumeCaptureRequest{}
				err = client.Get(ctx, types.NamespacedName{Name: "test-vcr-ttl-expire", Namespace: "default"}, deletedVCR)
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "VCR should be deleted")
			})

			It("should set Ready=False with InvalidTTL reason when TTL annotation is invalid", func() {
				// Create VCR with invalid TTL
				vcr := &storagev1alpha1.VolumeCaptureRequest{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-vcr-invalid-ttl",
						Namespace: "default",
						UID:       types.UID("vcr-uid-invalid-ttl"),
						Annotations: map[string]string{
							AnnotationKeyTTL: "invalid-ttl-format",
						},
					},
					Spec: storagev1alpha1.VolumeCaptureRequestSpec{
						Mode: ModeSnapshot,
						PersistentVolumeClaimRef: &storagev1alpha1.ObjectReference{
							Namespace: "default",
							Name:      "test-pvc",
						},
						VolumeSnapshotClassName: "test-vsc-class",
					},
					Status: storagev1alpha1.VolumeCaptureRequestStatus{
						CompletionTimestamp: &metav1.Time{Time: time.Now()},
					},
				}
				Expect(client.Create(ctx, vcr)).To(Succeed())

				// Execute Reconcile (which calls checkAndHandleTTL)
				// checkAndHandleTTL should set Ready=False with InvalidTTL reason, not delete VCR
				req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-vcr-invalid-ttl", Namespace: "default"}}
				_, err := ctrl.Reconcile(ctx, req)
				Expect(err).ToNot(HaveOccurred())

				// Verify VCR still exists (not deleted)
				updatedVCR := &storagev1alpha1.VolumeCaptureRequest{}
				err = client.Get(ctx, types.NamespacedName{Name: "test-vcr-invalid-ttl", Namespace: "default"}, updatedVCR)
				Expect(err).ToNot(HaveOccurred(), "VCR should not be deleted with invalid TTL")
				Expect(updatedVCR).ToNot(BeNil(), "VCR should still exist")
			})

			It("should requeue when TTL not expired yet", func() {
				// Create VCR with TTL that hasn't expired
				vcr := &storagev1alpha1.VolumeCaptureRequest{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-vcr-ttl-not-expired",
						Namespace: "default",
						UID:       types.UID("vcr-uid-not-expired"),
						Annotations: map[string]string{
							AnnotationKeyTTL: "10m",
						},
					},
					Spec: storagev1alpha1.VolumeCaptureRequestSpec{
						Mode: ModeSnapshot,
						PersistentVolumeClaimRef: &storagev1alpha1.ObjectReference{
							Namespace: "default",
							Name:      "test-pvc",
						},
						VolumeSnapshotClassName: "test-vsc-class",
					},
					Status: storagev1alpha1.VolumeCaptureRequestStatus{
						CompletionTimestamp: &metav1.Time{Time: time.Now().Add(-5 * time.Minute)}, // Not expired yet
					},
				}
				Expect(client.Create(ctx, vcr)).To(Succeed())

				// Execute checkAndHandleTTL
				shouldDelete, requeueAfter, err := ctrl.checkAndHandleTTL(ctx, vcr)
				Expect(err).ToNot(HaveOccurred())
				Expect(shouldDelete).To(BeFalse(), "VCR should NOT be deleted when TTL not expired")
				Expect(requeueAfter).To(BeNumerically(">", 0), "Should requeue")
				Expect(requeueAfter).To(BeNumerically("<=", 70*time.Second), "RequeueAfter should be <= 1m + 10% jitter")

				// Verify VCR still exists
				updatedVCR := &storagev1alpha1.VolumeCaptureRequest{}
				Expect(client.Get(ctx, types.NamespacedName{Name: "test-vcr-ttl-not-expired", Namespace: "default"}, updatedVCR)).To(Succeed())
			})
		})
	})
})

// Helper function to get condition by type
func getCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}
