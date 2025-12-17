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
	storagev1 "k8s.io/api/storage/v1"
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
			Build()

		ctrl = &VolumeCaptureRequestController{
			Client:    client,
			APIReader: client, // Use same client for tests
			Scheme:    scheme,
			Config:    cfg,
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
				},
			}
			Expect(client.Create(ctx, vcr)).To(Succeed())

			// Create StorageClass with VolumeSnapshotClass annotation
			storageClass := &storagev1.StorageClass{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-storage-class",
					Annotations: map[string]string{
						"storage.deckhouse.io/volumesnapshotclass": "test-vsc-class",
					},
				},
				Provisioner: "test-driver",
			}
			Expect(client.Create(ctx, storageClass)).To(Succeed())

			// Create PVC with StorageClassName
			pvc = &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pvc",
					Namespace: "default",
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					StorageClassName: pointer.String("test-storage-class"),
					VolumeName:       "test-pv",
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

			// Create VolumeSnapshotClass (referenced by StorageClass annotation)
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
			Expect(*vsc.Spec.VolumeSnapshotClassName).To(Equal(vscClass.Name)) // Should use the default class found
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
			readyCondition := getCondition(updatedVCR.Status.Conditions, storagev1alpha1.ConditionTypeReady)
			Expect(readyCondition).ToNot(BeNil())
			Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
			Expect(readyCondition.Reason).To(Equal(storagev1alpha1.ConditionReasonCompleted))

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
					VolumeSnapshotClassName: pointer.String(vscClass.Name), // Use the default class name
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

			readyCondition := getCondition(updatedVCR.Status.Conditions, storagev1alpha1.ConditionTypeReady)
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

			readyCondition := getCondition(updatedVCR.Status.Conditions, storagev1alpha1.ConditionTypeReady)
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
					},
				}
				Expect(client.Create(ctx, vcr)).To(Succeed())

				// Execute
				_, err := ctrl.processSnapshotMode(ctx, vcr)
				Expect(err).ToNot(HaveOccurred())

				// Should mark as failed
				updatedVCR := &storagev1alpha1.VolumeCaptureRequest{}
				Expect(client.Get(ctx, types.NamespacedName{Name: "test-vcr-not-found", Namespace: "default"}, updatedVCR)).To(Succeed())

				readyCondition := getCondition(updatedVCR.Status.Conditions, storagev1alpha1.ConditionTypeReady)
				Expect(readyCondition).ToNot(BeNil())
				Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
				Expect(readyCondition.Reason).To(Equal(storagev1alpha1.ConditionReasonNotFound))

				// Should have CompletionTimestamp and TTL
				Expect(updatedVCR.Status.CompletionTimestamp).ToNot(BeNil())
				Expect(updatedVCR.Annotations).To(HaveKey(AnnotationKeyTTL))
			})

			It("should fail if StorageClass does not have volumesnapshotclass annotation", func() {
				// Create StorageClass without volumesnapshotclass annotation
				storageClassNoAnnotation := &storagev1.StorageClass{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-storage-class-no-annotation",
					},
					Provisioner: "test-driver",
				}
				Expect(client.Create(ctx, storageClassNoAnnotation)).To(Succeed())

				// Create PV for this test case (unique PV per test)
				pvNoAnnotation := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-pv-no-annotation",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver:       "test-driver",
								VolumeHandle: "test-volume-handle-no-annotation",
							},
						},
					},
				}
				Expect(client.Create(ctx, pvNoAnnotation)).To(Succeed())

				// Create PVC with StorageClass without annotation
				pvcNoAnnotation := &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pvc-no-annotation",
						Namespace: "default",
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						StorageClassName: pointer.String("test-storage-class-no-annotation"),
						VolumeName:       "test-pv-no-annotation",
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
				Expect(client.Create(ctx, pvcNoAnnotation)).To(Succeed())

				// Create VCR
				vcr := &storagev1alpha1.VolumeCaptureRequest{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-vcr-no-annotation",
						Namespace: "default",
						UID:       types.UID("vcr-uid-no-annotation"),
					},
					Spec: storagev1alpha1.VolumeCaptureRequestSpec{
						Mode: ModeSnapshot,
						PersistentVolumeClaimRef: &storagev1alpha1.ObjectReference{
							Namespace: "default",
							Name:      "test-pvc-no-annotation",
						},
					},
				}
				Expect(client.Create(ctx, vcr)).To(Succeed())

				// Execute
				_, err := ctrl.processSnapshotMode(ctx, vcr)
				Expect(err).ToNot(HaveOccurred())

				// Should mark as failed
				updatedVCR := &storagev1alpha1.VolumeCaptureRequest{}
				Expect(client.Get(ctx, types.NamespacedName{Name: "test-vcr-no-annotation", Namespace: "default"}, updatedVCR)).To(Succeed())

				readyCondition := getCondition(updatedVCR.Status.Conditions, storagev1alpha1.ConditionTypeReady)
				Expect(readyCondition).ToNot(BeNil())
				Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
				Expect(readyCondition.Reason).To(Equal(storagev1alpha1.ConditionReasonNotFound))
				Expect(readyCondition.Message).To(ContainSubstring("does not have storage.deckhouse.io/volumesnapshotclass annotation"))
			})

			It("should fail if VolumeSnapshotClass from StorageClass annotation not found", func() {
				// Create StorageClass with non-existent VolumeSnapshotClass annotation
				storageClassBadAnnotation := &storagev1.StorageClass{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-storage-class-bad-annotation",
						Annotations: map[string]string{
							"storage.deckhouse.io/volumesnapshotclass": "non-existent-vsc-class",
						},
					},
					Provisioner: "test-driver",
				}
				Expect(client.Create(ctx, storageClassBadAnnotation)).To(Succeed())

				// Create PV for this test case (unique PV per test)
				pvBadAnnotation := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-pv-bad-annotation",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver:       "test-driver",
								VolumeHandle: "test-volume-handle-bad-annotation",
							},
						},
					},
				}
				Expect(client.Create(ctx, pvBadAnnotation)).To(Succeed())

				// Create PVC with StorageClass with bad annotation
				pvcBadAnnotation := &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pvc-bad-annotation",
						Namespace: "default",
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						StorageClassName: pointer.String("test-storage-class-bad-annotation"),
						VolumeName:       "test-pv-bad-annotation",
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
				Expect(client.Create(ctx, pvcBadAnnotation)).To(Succeed())

				// Create VCR
				vcr := &storagev1alpha1.VolumeCaptureRequest{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-vcr-bad-annotation",
						Namespace: "default",
						UID:       types.UID("vcr-uid-bad-annotation"),
					},
					Spec: storagev1alpha1.VolumeCaptureRequestSpec{
						Mode: ModeSnapshot,
						PersistentVolumeClaimRef: &storagev1alpha1.ObjectReference{
							Namespace: "default",
							Name:      "test-pvc-bad-annotation",
						},
					},
				}
				Expect(client.Create(ctx, vcr)).To(Succeed())

				// Execute
				_, err := ctrl.processSnapshotMode(ctx, vcr)
				Expect(err).ToNot(HaveOccurred())

				// Should mark as failed
				updatedVCR := &storagev1alpha1.VolumeCaptureRequest{}
				Expect(client.Get(ctx, types.NamespacedName{Name: "test-vcr-bad-annotation", Namespace: "default"}, updatedVCR)).To(Succeed())

				readyCondition := getCondition(updatedVCR.Status.Conditions, storagev1alpha1.ConditionTypeReady)
				Expect(readyCondition).ToNot(BeNil())
				Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
				Expect(readyCondition.Reason).To(Equal(storagev1alpha1.ConditionReasonNotFound))
				Expect(readyCondition.Message).To(ContainSubstring("not found"))
			})

			It("should fail VCR when VSC has terminal error", func() {
				// Create ObjectKeeper
				retainerName := NamePrefixRetainer + "snapshot-vcr-uid-terminal-error"
				objectKeeper := &deckhousev1alpha1.ObjectKeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name: retainerName,
						UID:  types.UID("retainer-uid-terminal-error"),
					},
					Spec: deckhousev1alpha1.ObjectKeeperSpec{
						Mode: "FollowObject",
						FollowObjectRef: &deckhousev1alpha1.FollowObjectRef{
							APIVersion: APIGroupStorageDeckhouse,
							Kind:       KindVolumeCaptureRequest,
							Namespace:  "default",
							Name:       "test-vcr-terminal-error",
							UID:        "vcr-uid-terminal-error",
						},
					},
				}
				Expect(client.Create(ctx, objectKeeper)).To(Succeed())

				// Create VCR
				vcr := &storagev1alpha1.VolumeCaptureRequest{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-vcr-terminal-error",
						Namespace: "default",
						UID:       types.UID("vcr-uid-terminal-error"),
					},
					Spec: storagev1alpha1.VolumeCaptureRequestSpec{
						Mode: ModeSnapshot,
						PersistentVolumeClaimRef: &storagev1alpha1.ObjectReference{
							Namespace: "default",
							Name:      "test-pvc",
						},
					},
				}
				Expect(client.Create(ctx, vcr)).To(Succeed())

				// First reconcile: should create VSC and requeue
				result, err := ctrl.processSnapshotMode(ctx, vcr)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.RequeueAfter).To(BeNumerically(">", 0), "Should requeue while waiting for ReadyToUse")

				// Verify VSC exists
				vscName := "snapshot-vcr-uid-terminal-error"
				vsc := &snapshotv1.VolumeSnapshotContent{}
				Expect(client.Get(ctx, types.NamespacedName{Name: vscName}, vsc)).To(Succeed())

				// Set terminal error on VSC (simulating CSI driver failure)
				errorMsg := "provided secret is empty"
				vsc.Status = &snapshotv1.VolumeSnapshotContentStatus{
					ReadyToUse: pointer.Bool(false),
					Error: &snapshotv1.VolumeSnapshotError{
						Message: &errorMsg,
						Time:    &metav1.Time{Time: time.Now()},
					},
				}
				Expect(client.Status().Update(ctx, vsc)).To(Succeed())

				// Second reconcile: should detect terminal error and mark VCR as failed
				result, err = ctrl.processSnapshotMode(ctx, vcr)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.Requeue).To(BeFalse(), "Should not requeue after terminal error")
				Expect(result.RequeueAfter).To(Equal(time.Duration(0)), "Should not requeue after terminal error")

				// Verify VCR status
				updatedVCR := &storagev1alpha1.VolumeCaptureRequest{}
				Expect(client.Get(ctx, types.NamespacedName{Name: "test-vcr-terminal-error", Namespace: "default"}, updatedVCR)).To(Succeed())

				// Should have Ready=False condition
				readyCondition := getCondition(updatedVCR.Status.Conditions, storagev1alpha1.ConditionTypeReady)
				Expect(readyCondition).ToNot(BeNil())
				Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
				Expect(readyCondition.Reason).To(Equal(storagev1alpha1.ConditionReasonSnapshotCreationFailed))
				Expect(readyCondition.Message).To(ContainSubstring("CSI snapshot creation failed"))
				Expect(readyCondition.Message).To(ContainSubstring(errorMsg))
				Expect(readyCondition.Message).To(ContainSubstring(vscName))
				Expect(readyCondition.Message).To(ContainSubstring("test-pvc"))

				// Should have CompletionTimestamp
				Expect(updatedVCR.Status.CompletionTimestamp).ToNot(BeNil())

				// Should have TTL annotation
				Expect(updatedVCR.Annotations).To(HaveKey(AnnotationKeyTTL))
				Expect(updatedVCR.Annotations[AnnotationKeyTTL]).To(Equal("10m"))

				// Should have DataRef pointing to problematic VSC
				Expect(updatedVCR.Status.DataRef).ToNot(BeNil())
				Expect(updatedVCR.Status.DataRef.Name).To(Equal(vscName))
				Expect(updatedVCR.Status.DataRef.Kind).To(Equal("VolumeSnapshotContent"))

				// Should have ObservedGeneration set
				Expect(updatedVCR.Status.ObservedGeneration).To(Equal(updatedVCR.Generation))
			})

			It("should not requeue when VSC.status is nil (not terminal error)", func() {
				// Create ObjectKeeper
				retainerName := NamePrefixRetainer + "snapshot-vcr-uid-no-status"
				objectKeeper := &deckhousev1alpha1.ObjectKeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name: retainerName,
						UID:  types.UID("retainer-uid-no-status"),
					},
					Spec: deckhousev1alpha1.ObjectKeeperSpec{
						Mode: "FollowObject",
						FollowObjectRef: &deckhousev1alpha1.FollowObjectRef{
							APIVersion: APIGroupStorageDeckhouse,
							Kind:       KindVolumeCaptureRequest,
							Namespace:  "default",
							Name:       "test-vcr-no-status",
							UID:        "vcr-uid-no-status",
						},
					},
				}
				Expect(client.Create(ctx, objectKeeper)).To(Succeed())

				// Create VCR
				vcr := &storagev1alpha1.VolumeCaptureRequest{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-vcr-no-status",
						Namespace: "default",
						UID:       types.UID("vcr-uid-no-status"),
					},
					Spec: storagev1alpha1.VolumeCaptureRequestSpec{
						Mode: ModeSnapshot,
						PersistentVolumeClaimRef: &storagev1alpha1.ObjectReference{
							Namespace: "default",
							Name:      "test-pvc",
						},
					},
				}
				Expect(client.Create(ctx, vcr)).To(Succeed())

				// Execute - VSC exists but Status is nil
				result, err := ctrl.processSnapshotMode(ctx, vcr)
				Expect(err).ToNot(HaveOccurred())
				// Should requeue (waiting for snapshotter to set status)
				Expect(result.RequeueAfter).To(BeNumerically(">", 0), "Should requeue when VSC.Status is nil")

				// VCR should not be marked as failed
				updatedVCR := &storagev1alpha1.VolumeCaptureRequest{}
				Expect(client.Get(ctx, types.NamespacedName{Name: "test-vcr-no-status", Namespace: "default"}, updatedVCR)).To(Succeed())
				readyCondition := getCondition(updatedVCR.Status.Conditions, storagev1alpha1.ConditionTypeReady)
				Expect(readyCondition).To(BeNil(), "VCR should not have Ready condition when VSC.Status is nil")
			})

			It("should not change Failed condition on repeated reconcile", func() {
				// Create ObjectKeeper
				retainerName := NamePrefixRetainer + "snapshot-vcr-uid-repeated-failed"
				objectKeeper := &deckhousev1alpha1.ObjectKeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name: retainerName,
						UID:  types.UID("retainer-uid-repeated-failed"),
					},
					Spec: deckhousev1alpha1.ObjectKeeperSpec{
						Mode: "FollowObject",
						FollowObjectRef: &deckhousev1alpha1.FollowObjectRef{
							APIVersion: APIGroupStorageDeckhouse,
							Kind:       KindVolumeCaptureRequest,
							Namespace:  "default",
							Name:       "test-vcr-repeated-failed",
							UID:        "vcr-uid-repeated-failed",
						},
					},
				}
				Expect(client.Create(ctx, objectKeeper)).To(Succeed())

				// Create VCR
				vcr := &storagev1alpha1.VolumeCaptureRequest{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-vcr-repeated-failed",
						Namespace: "default",
						UID:       types.UID("vcr-uid-repeated-failed"),
					},
					Spec: storagev1alpha1.VolumeCaptureRequestSpec{
						Mode: ModeSnapshot,
						PersistentVolumeClaimRef: &storagev1alpha1.ObjectReference{
							Namespace: "default",
							Name:      "test-pvc",
						},
					},
				}
				Expect(client.Create(ctx, vcr)).To(Succeed())

				// Create VSC with terminal error
				vscName := "snapshot-vcr-uid-repeated-failed"
				errorMsg := "provided secret is empty"
				vsc := &snapshotv1.VolumeSnapshotContent{
					ObjectMeta: metav1.ObjectMeta{
						Name: vscName,
						OwnerReferences: []metav1.OwnerReference{
							{
								APIVersion: APIGroupDeckhouse,
								Kind:       KindObjectKeeper,
								Name:       retainerName,
								UID:        "retainer-uid-repeated-failed",
								Controller: pointer.Bool(true),
							},
						},
					},
					Spec: snapshotv1.VolumeSnapshotContentSpec{
						Driver:                  "test-driver",
						VolumeSnapshotClassName: pointer.String(vscClass.Name), // Use the default class name
						DeletionPolicy:          snapshotv1.VolumeSnapshotContentDelete,
						Source: snapshotv1.VolumeSnapshotContentSource{
							VolumeHandle: pointer.String("test-volume-handle-123"),
						},
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

				// First reconcile: should mark as failed
				_, err := ctrl.processSnapshotMode(ctx, vcr)
				Expect(err).ToNot(HaveOccurred())

				// Get VCR after first reconcile
				updatedVCR := &storagev1alpha1.VolumeCaptureRequest{}
				Expect(client.Get(ctx, types.NamespacedName{Name: "test-vcr-repeated-failed", Namespace: "default"}, updatedVCR)).To(Succeed())
				originalTTL := updatedVCR.Annotations[AnnotationKeyTTL]
				originalMessage := getCondition(updatedVCR.Status.Conditions, storagev1alpha1.ConditionTypeReady).Message
				originalCompletionTime := updatedVCR.Status.CompletionTimestamp

				// Second reconcile: should not change Failed condition or TTL
				_, err = ctrl.processSnapshotMode(ctx, updatedVCR)
				Expect(err).ToNot(HaveOccurred())

				// Verify VCR unchanged
				finalVCR := &storagev1alpha1.VolumeCaptureRequest{}
				Expect(client.Get(ctx, types.NamespacedName{Name: "test-vcr-repeated-failed", Namespace: "default"}, finalVCR)).To(Succeed())

				// TTL should not be overwritten
				Expect(finalVCR.Annotations[AnnotationKeyTTL]).To(Equal(originalTTL), "TTL should not be overwritten")

				// Condition should not change
				finalCondition := getCondition(finalVCR.Status.Conditions, storagev1alpha1.ConditionTypeReady)
				Expect(finalCondition).ToNot(BeNil())
				Expect(finalCondition.Status).To(Equal(metav1.ConditionFalse))
				Expect(finalCondition.Reason).To(Equal(storagev1alpha1.ConditionReasonSnapshotCreationFailed))
				Expect(finalCondition.Message).To(Equal(originalMessage), "Message should not change")

				// CompletionTimestamp should not change
				Expect(finalVCR.Status.CompletionTimestamp).To(Equal(originalCompletionTime), "CompletionTimestamp should not change")
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

				readyCondition := getCondition(updatedVCR.Status.Conditions, storagev1alpha1.ConditionTypeReady)
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

		Describe("Error scenarios and resilience", func() {
			var (
				vcr      *storagev1alpha1.VolumeCaptureRequest
				pvc      *corev1.PersistentVolumeClaim
				pv       *corev1.PersistentVolume
				vscClass *snapshotv1.VolumeSnapshotClass
			)

			BeforeEach(func() {
				// Create StorageClass with VolumeSnapshotClass annotation
				// Use unique name per test to avoid conflicts (StorageClass is cluster-scoped)
				storageClassName := "test-storage-class-error-scenarios"
				storageClass := &storagev1.StorageClass{
					ObjectMeta: metav1.ObjectMeta{
						Name: storageClassName,
						Annotations: map[string]string{
							"storage.deckhouse.io/volumesnapshotclass": "test-vsc-class-error-scenarios",
						},
					},
					Provisioner: "test-driver",
				}
				Expect(client.Create(ctx, storageClass)).To(Succeed())

				// Setup common resources
				pvc = &corev1.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-pvc-error-scenarios",
						Namespace: "default",
					},
					Spec: corev1.PersistentVolumeClaimSpec{
						StorageClassName: pointer.String("test-storage-class-error-scenarios"),
						VolumeName:       "test-pv-error-scenarios",
					},
				}
				Expect(client.Create(ctx, pvc)).To(Succeed())

				pv = &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-pv-error-scenarios",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver:       "test-driver",
								VolumeHandle: "test-volume-handle-error-scenarios",
							},
						},
					},
				}
				Expect(client.Create(ctx, pv)).To(Succeed())

				// Create StorageClass with cephfs driver (with unique name to avoid conflicts)
				// Note: Provisioner is immutable, so we create a new StorageClass instead of updating
				storageClassCephfs := &storagev1.StorageClass{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-storage-class-cephfs",
						Annotations: map[string]string{
							"storage.deckhouse.io/volumesnapshotclass": "test-vsc-class-cephfs",
						},
					},
					Provisioner: "cephfs.csi.ceph.com",
				}
				Expect(client.Create(ctx, storageClassCephfs)).To(Succeed())

				// Update PVC to use the new StorageClass
				pvc.Spec.StorageClassName = pointer.String("test-storage-class-cephfs")
				Expect(client.Update(ctx, pvc)).To(Succeed())

				vscClass = &snapshotv1.VolumeSnapshotClass{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-vsc-class-cephfs",
					},
					Driver: "cephfs.csi.ceph.com",
					Parameters: map[string]string{
						"csi.storage.k8s.io/snapshotter-secret-name":      "csi-ceph-secret",
						"csi.storage.k8s.io/snapshotter-secret-namespace": "d8-csi-ceph",
					},
				}
				Expect(client.Create(ctx, vscClass)).To(Succeed())

				vcr = &storagev1alpha1.VolumeCaptureRequest{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-vcr-error-scenarios",
						Namespace: "default",
						// Don't set UID - let apiserver assign it
					},
					Spec: storagev1alpha1.VolumeCaptureRequestSpec{
						Mode: ModeSnapshot,
						PersistentVolumeClaimRef: &storagev1alpha1.ObjectReference{
							Namespace: "default",
							Name:      "test-pvc-error-scenarios",
						},
					},
				}
				Expect(client.Create(ctx, vcr)).To(Succeed())
				// Re-read to get actual UID assigned by apiserver
				Expect(client.Get(ctx, types.NamespacedName{Name: "test-vcr-error-scenarios", Namespace: "default"}, vcr)).To(Succeed())
				Expect(vcr.UID).ToNot(BeEmpty(), "VCR should have UID assigned by apiserver")
			})

			It("should fail VCR when secret is missing (secret not found error)", func() {
				// Re-read VCR to get actual UID assigned by apiserver
				Expect(client.Get(ctx, types.NamespacedName{Name: "test-vcr-error-scenarios", Namespace: "default"}, vcr)).To(Succeed())
				Expect(vcr.UID).ToNot(BeEmpty(), "VCR should have UID assigned by apiserver")

				// Create ObjectKeeper (using actual VCR UID)
				retainerName := NamePrefixRetainer + "snapshot-" + string(vcr.UID)
				objectKeeper := &deckhousev1alpha1.ObjectKeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name: retainerName,
					},
					Spec: deckhousev1alpha1.ObjectKeeperSpec{
						Mode: "FollowObject",
						FollowObjectRef: &deckhousev1alpha1.FollowObjectRef{
							APIVersion: APIGroupStorageDeckhouse,
							Kind:       KindVolumeCaptureRequest,
							Namespace:  "default",
							Name:       "test-vcr-error-scenarios",
							UID:        string(vcr.UID),
						},
					},
				}
				Expect(client.Create(ctx, objectKeeper)).To(Succeed())

				// First reconcile: should create VSC
				result, err := ctrl.processSnapshotMode(ctx, vcr)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.RequeueAfter).To(BeNumerically(">", 0), "Should requeue while waiting for snapshot-controller")

				// Verify VSC exists (using actual VCR UID)
				vscName := "snapshot-" + string(vcr.UID)
				vsc := &snapshotv1.VolumeSnapshotContent{}
				Expect(client.Get(ctx, types.NamespacedName{Name: vscName}, vsc)).To(Succeed())

				// Simulate snapshot-controller error: secret not found
				errorMsg := "failed to get credentials from class parameters: secrets \"csi-ceph-secret\" not found"
				vsc.Status = &snapshotv1.VolumeSnapshotContentStatus{
					ReadyToUse: pointer.Bool(false),
					Error: &snapshotv1.VolumeSnapshotError{
						Message: &errorMsg,
						Time:    &metav1.Time{Time: time.Now()},
					},
				}
				Expect(client.Status().Update(ctx, vsc)).To(Succeed())

				// Second reconcile: should detect error and mark VCR as failed
				result, err = ctrl.processSnapshotMode(ctx, vcr)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.Requeue).To(BeFalse(), "Should not requeue after terminal error")
				Expect(result.RequeueAfter).To(Equal(time.Duration(0)), "Should not requeue after terminal error")

				// Verify VCR is marked as failed
				updatedVCR := &storagev1alpha1.VolumeCaptureRequest{}
				Expect(client.Get(ctx, types.NamespacedName{Name: "test-vcr", Namespace: "default"}, updatedVCR)).To(Succeed())

				readyCondition := getCondition(updatedVCR.Status.Conditions, storagev1alpha1.ConditionTypeReady)
				Expect(readyCondition).ToNot(BeNil())
				Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
				Expect(readyCondition.Reason).To(Equal(storagev1alpha1.ConditionReasonSnapshotCreationFailed))
				Expect(readyCondition.Message).To(ContainSubstring(errorMsg))
				Expect(updatedVCR.Status.ObservedGeneration).To(Equal(updatedVCR.Generation))
				Expect(updatedVCR.Status.CompletionTimestamp).ToNot(BeNil())
				Expect(updatedVCR.Status.DataRef).ToNot(BeNil())
				Expect(updatedVCR.Status.DataRef.Name).To(Equal(vscName))
			})

			It("should handle different snapshot error types", func() {
				// Create ObjectKeeper
				retainerName := NamePrefixRetainer + "snapshot-vcr-uid-test"
				objectKeeper := &deckhousev1alpha1.ObjectKeeper{
					ObjectMeta: metav1.ObjectMeta{
						Name: retainerName,
						UID:  types.UID("retainer-uid-test"),
					},
					Spec: deckhousev1alpha1.ObjectKeeperSpec{
						Mode: "FollowObject",
						FollowObjectRef: &deckhousev1alpha1.FollowObjectRef{
							APIVersion: APIGroupStorageDeckhouse,
							Kind:       KindVolumeCaptureRequest,
							Namespace:  "default",
							Name:       "test-vcr-error-scenarios",
							UID:        "vcr-uid-test",
						},
					},
				}
				Expect(client.Create(ctx, objectKeeper)).To(Succeed())

				// Test different error types from VSC.status.error
				// Note: "driver mismatch" is NOT a VSC error - it's validated by VCR controller before VSC creation
				errorScenarios := []struct {
					nameSlug string // Valid Kubernetes name (no spaces, lowercase)
					name     string // Human-readable name for By() message
					errorMsg string
				}{
					{
						nameSlug: "provided-secret-is-empty",
						name:     "provided secret is empty",
						errorMsg: "rpc error: code = Unknown desc = provided secret is empty",
					},
					{
						nameSlug: "invalid-argument",
						name:     "invalid argument",
						errorMsg: "rpc error: code = InvalidArgument desc = invalid snapshot parameters",
					},
					{
						nameSlug: "permission-denied",
						name:     "permission denied",
						errorMsg: "rpc error: code = PermissionDenied desc = access denied",
					},
				}

				for _, scenario := range errorScenarios {
					By("Testing error scenario: " + scenario.name)

					// Create fresh VCR for each scenario (without UID - let apiserver assign it)
					vcrName := "test-vcr-" + scenario.nameSlug
					testVCR := vcr.DeepCopy()
					testVCR.Name = vcrName
					testVCR.UID = "" // Let apiserver assign UID
					Expect(client.Create(ctx, testVCR)).To(Succeed())

					// Re-read VCR to get actual UID assigned by apiserver
					Expect(client.Get(ctx, types.NamespacedName{Name: vcrName, Namespace: "default"}, testVCR)).To(Succeed())
					Expect(testVCR.UID).ToNot(BeEmpty(), "VCR should have UID assigned by apiserver")

					// Create ObjectKeeper for this VCR (using actual UID)
					testRetainerName := NamePrefixRetainer + "snapshot-" + string(testVCR.UID)
					testObjectKeeper := objectKeeper.DeepCopy()
					testObjectKeeper.Name = testRetainerName
					testObjectKeeper.UID = "" // Let apiserver assign UID
					testObjectKeeper.Spec.FollowObjectRef.UID = string(testVCR.UID)
					testObjectKeeper.Spec.FollowObjectRef.Name = vcrName
					Expect(client.Create(ctx, testObjectKeeper)).To(Succeed())

					// Re-read ObjectKeeper to get actual UID
					Expect(client.Get(ctx, types.NamespacedName{Name: testRetainerName}, testObjectKeeper)).To(Succeed())
					Expect(testObjectKeeper.UID).ToNot(BeEmpty(), "ObjectKeeper should have UID assigned by apiserver")

					// First reconcile: create VSC
					_, err := ctrl.processSnapshotMode(ctx, testVCR)
					Expect(err).ToNot(HaveOccurred())

					// Set error on VSC (using actual VCR UID for VSC name)
					vscName := "snapshot-" + string(testVCR.UID)
					vsc := &snapshotv1.VolumeSnapshotContent{}
					Expect(client.Get(ctx, types.NamespacedName{Name: vscName}, vsc)).To(Succeed())
					vsc.Status = &snapshotv1.VolumeSnapshotContentStatus{
						ReadyToUse: pointer.Bool(false),
						Error: &snapshotv1.VolumeSnapshotError{
							Message: &scenario.errorMsg,
							Time:    &metav1.Time{Time: time.Now()},
						},
					}
					Expect(client.Status().Update(ctx, vsc)).To(Succeed())

					// Second reconcile: should detect error
					result, err := ctrl.processSnapshotMode(ctx, testVCR)
					Expect(err).ToNot(HaveOccurred())
					Expect(result.Requeue).To(BeFalse(), "Should not requeue after terminal error")
					Expect(result.RequeueAfter).To(Equal(time.Duration(0)), "Should not requeue after terminal error")

					// Verify VCR is failed
					updatedVCR := &storagev1alpha1.VolumeCaptureRequest{}
					Expect(client.Get(ctx, types.NamespacedName{Name: vcrName, Namespace: "default"}, updatedVCR)).To(Succeed())
					readyCondition := getCondition(updatedVCR.Status.Conditions, storagev1alpha1.ConditionTypeReady)
					Expect(readyCondition).ToNot(BeNil())
					Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse))
					Expect(readyCondition.Reason).To(Equal(storagev1alpha1.ConditionReasonSnapshotCreationFailed))
					Expect(readyCondition.Message).To(ContainSubstring(scenario.errorMsg))
				}
			})

			It("should create ObjectKeeper if it doesn't exist", func() {
				// Re-read VCR to get actual UID
				Expect(client.Get(ctx, types.NamespacedName{Name: "test-vcr-error-scenarios", Namespace: "default"}, vcr)).To(Succeed())
				Expect(vcr.UID).ToNot(BeEmpty())

				// ObjectKeeper should NOT exist yet
				retainerName := NamePrefixRetainer + "snapshot-" + string(vcr.UID)
				objectKeeper := &deckhousev1alpha1.ObjectKeeper{}
				err := client.Get(ctx, types.NamespacedName{Name: retainerName}, objectKeeper)
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "ObjectKeeper should not exist before reconcile")

				// First reconcile: should create ObjectKeeper
				result, err := ctrl.processSnapshotMode(ctx, vcr)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.RequeueAfter).To(BeNumerically(">", 0))

				// Verify ObjectKeeper was created by controller
				Expect(client.Get(ctx, types.NamespacedName{Name: retainerName}, objectKeeper)).To(Succeed())
				Expect(objectKeeper.Spec.Mode).To(Equal("FollowObject"))
				Expect(objectKeeper.Spec.FollowObjectRef).ToNot(BeNil())
				Expect(objectKeeper.Spec.FollowObjectRef.UID).To(Equal(string(vcr.UID)))
				Expect(objectKeeper.Spec.FollowObjectRef.Name).To(Equal("test-vcr-error-scenarios"))
				Expect(objectKeeper.Spec.FollowObjectRef.Namespace).To(Equal("default"))
			})

			It("should prioritize error over ReadyToUse (error wins even if ReadyToUse=true)", func() {
				// Re-read VCR to get actual UID
				Expect(client.Get(ctx, types.NamespacedName{Name: "test-vcr", Namespace: "default"}, vcr)).To(Succeed())
				Expect(vcr.UID).ToNot(BeEmpty())

				// First reconcile: create ObjectKeeper and VSC
				result, err := ctrl.processSnapshotMode(ctx, vcr)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.RequeueAfter).To(BeNumerically(">", 0))

				// Simulate edge case: VSC has both ReadyToUse=true AND error
				// This shouldn't happen in practice, but we handle it defensively
				vscName := "snapshot-" + string(vcr.UID)
				vsc := &snapshotv1.VolumeSnapshotContent{}
				Expect(client.Get(ctx, types.NamespacedName{Name: vscName}, vsc)).To(Succeed())
				errorMsg := "rpc error: code = Unknown desc = provided secret is empty"
				vsc.Status = &snapshotv1.VolumeSnapshotContentStatus{
					ReadyToUse: pointer.Bool(true), // ReadyToUse=true
					Error: &snapshotv1.VolumeSnapshotError{
						Message: &errorMsg, // But error exists
						Time:    &metav1.Time{Time: time.Now()},
					},
				}
				Expect(client.Status().Update(ctx, vsc)).To(Succeed())

				// Reconcile: should detect error and fail VCR (error has priority)
				result, err = ctrl.processSnapshotMode(ctx, vcr)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.Requeue).To(BeFalse(), "Should not requeue after terminal error")
				Expect(result.RequeueAfter).To(Equal(time.Duration(0)), "Should not requeue after terminal error")

				// Verify VCR is failed (error wins over ReadyToUse)
				updatedVCR := &storagev1alpha1.VolumeCaptureRequest{}
				Expect(client.Get(ctx, types.NamespacedName{Name: "test-vcr", Namespace: "default"}, updatedVCR)).To(Succeed())
				readyCondition := getCondition(updatedVCR.Status.Conditions, storagev1alpha1.ConditionTypeReady)
				Expect(readyCondition).ToNot(BeNil())
				Expect(readyCondition.Status).To(Equal(metav1.ConditionFalse), "Error should have priority over ReadyToUse")
				Expect(readyCondition.Reason).To(Equal(storagev1alpha1.ConditionReasonSnapshotCreationFailed))
				Expect(readyCondition.Message).To(ContainSubstring(errorMsg))
			})

			It("should handle controller restart gracefully (idempotency)", func() {
				// Re-read VCR to get actual UID
				Expect(client.Get(ctx, types.NamespacedName{Name: "test-vcr-error-scenarios", Namespace: "default"}, vcr)).To(Succeed())
				Expect(vcr.UID).ToNot(BeEmpty())

				// First reconcile: create ObjectKeeper and VSC
				result, err := ctrl.processSnapshotMode(ctx, vcr)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.RequeueAfter).To(BeNumerically(">", 0))

				// Verify ObjectKeeper exists
				retainerName := NamePrefixRetainer + "snapshot-" + string(vcr.UID)
				objectKeeper := &deckhousev1alpha1.ObjectKeeper{}
				Expect(client.Get(ctx, types.NamespacedName{Name: retainerName}, objectKeeper)).To(Succeed())
				originalOKUID := objectKeeper.UID
				Expect(originalOKUID).ToNot(BeEmpty())

				// Verify VSC exists
				vscName := "snapshot-" + string(vcr.UID)
				vsc := &snapshotv1.VolumeSnapshotContent{}
				Expect(client.Get(ctx, types.NamespacedName{Name: vscName}, vsc)).To(Succeed())
				originalVSCUID := vsc.UID
				Expect(originalVSCUID).ToNot(BeEmpty())

				// Simulate controller restart: create new controller instance with same client
				// This simulates the scenario where controller restarts but state persists in API server
				newCtrl := &VolumeCaptureRequestController{
					Client: client,
					Scheme: scheme,
					Config: cfg,
				}

				// Re-read VCR from API (simulating controller restart)
				restartedVCR := &storagev1alpha1.VolumeCaptureRequest{}
				Expect(client.Get(ctx, types.NamespacedName{Name: "test-vcr-error-scenarios", Namespace: "default"}, restartedVCR)).To(Succeed())

				// Second reconcile with "restarted" controller: should be idempotent
				// Should not recreate VSC, should continue waiting
				result, err = newCtrl.processSnapshotMode(ctx, restartedVCR)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.RequeueAfter).To(BeNumerically(">", 0), "Should still requeue while waiting")

				// Verify VSC still exists and wasn't recreated
				existingVSC := &snapshotv1.VolumeSnapshotContent{}
				Expect(client.Get(ctx, types.NamespacedName{Name: vscName}, existingVSC)).To(Succeed())
				Expect(existingVSC.UID).To(Equal(originalVSCUID), "VSC should not be recreated")

				// Verify ObjectKeeper still exists
				existingOK := &deckhousev1alpha1.ObjectKeeper{}
				Expect(client.Get(ctx, types.NamespacedName{Name: retainerName}, existingOK)).To(Succeed())
				Expect(existingOK.UID).To(Equal(originalOKUID), "ObjectKeeper should not be recreated")

				// Now simulate ReadyToUse=true
				existingVSC.Status = &snapshotv1.VolumeSnapshotContentStatus{
					ReadyToUse: pointer.Bool(true),
				}
				Expect(client.Status().Update(ctx, existingVSC)).To(Succeed())

				// Third reconcile: should complete successfully
				result, err = newCtrl.processSnapshotMode(ctx, restartedVCR)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.Requeue).To(BeFalse())
				Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

				// Verify VCR is Ready
				finalVCR := &storagev1alpha1.VolumeCaptureRequest{}
				Expect(client.Get(ctx, types.NamespacedName{Name: "test-vcr", Namespace: "default"}, finalVCR)).To(Succeed())
				readyCondition := getCondition(finalVCR.Status.Conditions, storagev1alpha1.ConditionTypeReady)
				Expect(readyCondition).ToNot(BeNil())
				Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
			})

			It("should handle leader switch gracefully (multiple reconciles)", func() {
				// Re-read VCR to get actual UID
				Expect(client.Get(ctx, types.NamespacedName{Name: "test-vcr-error-scenarios", Namespace: "default"}, vcr)).To(Succeed())
				Expect(vcr.UID).ToNot(BeEmpty())

				// Simulate leader 1: first reconcile creates ObjectKeeper and VSC
				result, err := ctrl.processSnapshotMode(ctx, vcr)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.RequeueAfter).To(BeNumerically(">", 0))

				// Verify ObjectKeeper exists
				retainerName := NamePrefixRetainer + "snapshot-" + string(vcr.UID)
				objectKeeper := &deckhousev1alpha1.ObjectKeeper{}
				Expect(client.Get(ctx, types.NamespacedName{Name: retainerName}, objectKeeper)).To(Succeed())

				// Verify VSC exists
				vscName := "snapshot-" + string(vcr.UID)
				vsc := &snapshotv1.VolumeSnapshotContent{}
				Expect(client.Get(ctx, types.NamespacedName{Name: vscName}, vsc)).To(Succeed())

				// Simulate leader switch: new controller instance (leader 2)
				// In real scenario, leader election ensures only one controller processes requests
				// But we test that multiple reconciles are idempotent
				leader2Ctrl := &VolumeCaptureRequestController{
					Client:    client,
					APIReader: client, // Use same client for tests
					Scheme:    scheme,
					Config:    cfg,
				}

				// Leader 2 reconciles: should be idempotent, should not recreate resources
				reReadVCR := &storagev1alpha1.VolumeCaptureRequest{}
				Expect(client.Get(ctx, types.NamespacedName{Name: "test-vcr-error-scenarios", Namespace: "default"}, reReadVCR)).To(Succeed())

				result, err = leader2Ctrl.processSnapshotMode(ctx, reReadVCR)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.RequeueAfter).To(BeNumerically(">", 0), "Should still requeue while waiting")

				// Verify resources weren't duplicated
				vscList := &snapshotv1.VolumeSnapshotContentList{}
				Expect(client.List(ctx, vscList)).To(Succeed())
				Expect(vscList.Items).To(HaveLen(1), "Should have exactly one VSC")

				okList := &deckhousev1alpha1.ObjectKeeperList{}
				Expect(client.List(ctx, okList)).To(Succeed())
				Expect(okList.Items).To(HaveLen(1), "Should have exactly one ObjectKeeper")

				// Simulate ReadyToUse=true
				vsc.Status = &snapshotv1.VolumeSnapshotContentStatus{
					ReadyToUse: pointer.Bool(true),
				}
				Expect(client.Status().Update(ctx, vsc)).To(Succeed())

				// Leader 1 reconciles again: should complete
				result, err = ctrl.processSnapshotMode(ctx, vcr)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.Requeue).To(BeFalse())
				Expect(result.RequeueAfter).To(Equal(time.Duration(0)))

				// Verify VCR is Ready
				finalVCR := &storagev1alpha1.VolumeCaptureRequest{}
				Expect(client.Get(ctx, types.NamespacedName{Name: "test-vcr", Namespace: "default"}, finalVCR)).To(Succeed())
				readyCondition := getCondition(finalVCR.Status.Conditions, storagev1alpha1.ConditionTypeReady)
				Expect(readyCondition).ToNot(BeNil())
				Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))

				// Leader 2 reconciles again: should be no-op (already Ready)
				result, err = leader2Ctrl.processSnapshotMode(ctx, finalVCR)
				Expect(err).ToNot(HaveOccurred())
				Expect(result.Requeue).To(BeFalse())
				Expect(result.RequeueAfter).To(Equal(time.Duration(0)))
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
