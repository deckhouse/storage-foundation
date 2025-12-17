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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/images/controller/pkg/config"
	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
)

func TestVolumeRestoreRequest(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "VolumeRestoreRequest Suite")
}

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

		cfg = &config.Options{
			RequestTTL:    10 * time.Minute,
			RequestTTLStr: "10m",
		}

		client = fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&snapshotv1.VolumeSnapshot{}).
			WithStatusSubresource(&snapshotv1.VolumeSnapshotContent{}).
			WithStatusSubresource(&storagev1alpha1.VolumeRestoreRequest{}).
			Build()

		ctrl = &VolumeRestoreRequestController{
			Client:    client,
			APIReader: client, // Use same client for tests
			Scheme:    scheme,
			Config:    cfg,
		}
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

	Describe("TTL Scanner", func() {
		It("should delete terminal VRR when TTL expired", func() {
			// Create completed VRR with TTL expired
			now := time.Now()
			completionTime := metav1.NewTime(now.Add(-15 * time.Minute)) // 15 minutes ago, TTL=10m, so expired

			vrr := &storagev1alpha1.VolumeRestoreRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vrr-ttl-expire",
					Namespace: "default",
					UID:       types.UID("vrr-uid-ttl"),
					Annotations: map[string]string{
						AnnotationKeyTTL: "10m", // Informational only
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

			// Execute TTL scanner
			ctrl.scanAndDeleteExpiredVRRs(ctx, client)

			// Verify VRR is deleted (scanner used config TTL, not annotation TTL)
			deletedVRR := &storagev1alpha1.VolumeRestoreRequest{}
			err := client.Get(ctx, types.NamespacedName{Name: "test-vrr-ttl-expire", Namespace: "default"}, deletedVRR)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "VRR should be deleted because config TTL (10m) expired")
		})

		It("should NOT delete terminal VRR when TTL not expired", func() {
			// Create completed VRR with TTL not expired
			now := time.Now()
			completionTime := metav1.NewTime(now.Add(-5 * time.Minute)) // 5 minutes ago, TTL=10m, so not expired

			vrr := &storagev1alpha1.VolumeRestoreRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vrr-ttl-not-expired",
					Namespace: "default",
					UID:       types.UID("vrr-uid-not-expired"),
					Annotations: map[string]string{
						AnnotationKeyTTL: "10m",
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

			// Execute TTL scanner
			ctrl.scanAndDeleteExpiredVRRs(ctx, client)

			// Verify VRR still exists
			existingVRR := &storagev1alpha1.VolumeRestoreRequest{}
			err := client.Get(ctx, types.NamespacedName{Name: "test-vrr-ttl-not-expired", Namespace: "default"}, existingVRR)
			Expect(err).ToNot(HaveOccurred(), "VRR should NOT be deleted because config TTL (10m) not expired")
		})

		It("should NOT delete non-terminal VRR even if TTL expired", func() {
			// Create non-terminal VRR with TTL expired
			now := time.Now()
			completionTime := metav1.NewTime(now.Add(-15 * time.Minute)) // 15 minutes ago

			vrr := &storagev1alpha1.VolumeRestoreRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vrr-non-terminal",
					Namespace: "default",
					UID:       types.UID("vrr-uid-non-terminal"),
					Annotations: map[string]string{
						AnnotationKeyTTL: "10m",
					},
				},
				Status: storagev1alpha1.VolumeRestoreRequestStatus{
					CompletionTimestamp: &completionTime,
					// No Ready condition - non-terminal
				},
			}
			Expect(client.Create(ctx, vrr)).To(Succeed())

			// Execute TTL scanner
			ctrl.scanAndDeleteExpiredVRRs(ctx, client)

			// Verify VRR still exists (non-terminal VRRs are not deleted)
			existingVRR := &storagev1alpha1.VolumeRestoreRequest{}
			err := client.Get(ctx, types.NamespacedName{Name: "test-vrr-non-terminal", Namespace: "default"}, existingVRR)
			Expect(err).ToNot(HaveOccurred(), "Non-terminal VRR should NOT be deleted even if TTL expired")
		})

		It("should delete Ready=False VRR when TTL expired", func() {
			// Create failed VRR with TTL expired
			now := time.Now()
			completionTime := metav1.NewTime(now.Add(-15 * time.Minute)) // 15 minutes ago

			vrr := &storagev1alpha1.VolumeRestoreRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vrr-failed-ttl-expire",
					Namespace: "default",
					UID:       types.UID("vrr-uid-failed-ttl"),
					Annotations: map[string]string{
						AnnotationKeyTTL: "10m",
					},
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

			// Execute TTL scanner
			ctrl.scanAndDeleteExpiredVRRs(ctx, client)

			// Verify VRR is deleted
			deletedVRR := &storagev1alpha1.VolumeRestoreRequest{}
			err := client.Get(ctx, types.NamespacedName{Name: "test-vrr-failed-ttl-expire", Namespace: "default"}, deletedVRR)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Failed VRR should be deleted when TTL expired")
		})

		It("should use config TTL, not annotation TTL", func() {
			// This test verifies that TTL scanner uses config.RequestTTL, not annotation TTL
			now := time.Now()
			completionTime := metav1.NewTime(now.Add(-15 * time.Minute)) // 15 minutes ago

			// Create VRR with:
			// - annotation TTL = 1h (should be ignored)
			// - config TTL = 10m (should be used)
			// - completionTime = 15m ago
			// Expected: VRR should be deleted (15m > 10m config TTL)
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

			// Execute scanAndDeleteExpiredVRRs
			ctrl.scanAndDeleteExpiredVRRs(ctx, client)

			// Verify VRR is deleted (scanner used config TTL, not annotation TTL)
			deletedVRR := &storagev1alpha1.VolumeRestoreRequest{}
			err := client.Get(ctx, types.NamespacedName{Name: "test-vrr-config-ttl", Namespace: "default"}, deletedVRR)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "VRR should be deleted because config TTL (10m) expired, not annotation TTL (1h)")
		})

		It("should not delete VRR when config TTL not expired, even if annotation TTL expired", func() {
			// This test verifies that annotation TTL is ignored even when it's shorter than config TTL
			now := time.Now()
			completionTime := metav1.NewTime(now.Add(-5 * time.Minute)) // 5 minutes ago

			// Create VRR with:
			// - annotation TTL = 1m (expired, but should be ignored)
			// - config TTL = 10m (not expired)
			// - completionTime = 5m ago
			// Expected: VRR should NOT be deleted (5m < 10m config TTL)
			vrr := &storagev1alpha1.VolumeRestoreRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vrr-annotation-ignored",
					Namespace: "default",
					UID:       types.UID("vrr-uid-annotation-ignored"),
					Annotations: map[string]string{
						AnnotationKeyTTL: "1m", // Annotation TTL expired, but should be ignored
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

			// Execute scanAndDeleteExpiredVRRs
			ctrl.scanAndDeleteExpiredVRRs(ctx, client)

			// Verify VRR still exists (scanner ignored annotation TTL)
			existingVRR := &storagev1alpha1.VolumeRestoreRequest{}
			err := client.Get(ctx, types.NamespacedName{Name: "test-vrr-annotation-ignored", Namespace: "default"}, existingVRR)
			Expect(err).ToNot(HaveOccurred(), "VRR should NOT be deleted because config TTL (10m) not expired, annotation TTL (1m) is ignored")
		})
	})
})
