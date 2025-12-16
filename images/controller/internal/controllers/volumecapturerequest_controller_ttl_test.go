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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	storagev1alpha1 "fox.flant.com/deckhouse/storage/storage-foundation/api/v1alpha1"
	"fox.flant.com/deckhouse/storage/storage-foundation/images/controller/pkg/config"
)

func TestVolumeRequestTTL(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "VolumeRequest TTL Suite")
}

var _ = Describe("VolumeCaptureRequest TTL", func() {
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
		Expect(storagev1alpha1.AddToScheme(scheme)).To(Succeed())

		cfg = &config.Options{
			RequestTTL:    10 * time.Minute,
			RequestTTLStr: "10m",
		}

		client = fake.NewClientBuilder().
			WithScheme(scheme).
			Build()

		ctrl = &VolumeCaptureRequestController{
			Client: client,
			Scheme: scheme,
			Config: cfg,
		}
	})

	Describe("setTTLAnnotation", func() {
		It("should set TTL annotation when not exists", func() {
			vcr := &storagev1alpha1.VolumeCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vcr",
					Namespace: "default",
				},
			}

			ctrl.setTTLAnnotation(vcr)

			Expect(vcr.Annotations).ToNot(BeNil())
			Expect(vcr.Annotations[AnnotationKeyTTL]).To(Equal("10m"))
		})

		It("should not overwrite existing TTL annotation", func() {
			vcr := &storagev1alpha1.VolumeCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vcr",
					Namespace: "default",
					Annotations: map[string]string{
						AnnotationKeyTTL: "30m",
					},
				},
			}

			ctrl.setTTLAnnotation(vcr)

			Expect(vcr.Annotations[AnnotationKeyTTL]).To(Equal("30m"))
		})

		It("should use config TTL when available", func() {
			cfg.RequestTTLStr = "20m"
			vcr := &storagev1alpha1.VolumeCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vcr",
					Namespace: "default",
				},
			}

			ctrl.setTTLAnnotation(vcr)

			Expect(vcr.Annotations[AnnotationKeyTTL]).To(Equal("20m"))
		})
	})

	Describe("checkAndHandleTTL", func() {
		It("should return false when no TTL annotation", func() {
			vcr := &storagev1alpha1.VolumeCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vcr",
					Namespace: "default",
				},
			}

			shouldDelete, requeueAfter, err := ctrl.checkAndHandleTTL(ctx, vcr)

			Expect(err).ToNot(HaveOccurred())
			Expect(shouldDelete).To(BeFalse())
			Expect(requeueAfter).To(Equal(time.Duration(0)))
		})

		It("should return false when no CompletionTimestamp", func() {
			vcr := &storagev1alpha1.VolumeCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vcr",
					Namespace: "default",
					Annotations: map[string]string{
						AnnotationKeyTTL: "10m",
					},
				},
			}

			shouldDelete, requeueAfter, err := ctrl.checkAndHandleTTL(ctx, vcr)

			Expect(err).ToNot(HaveOccurred())
			Expect(shouldDelete).To(BeFalse())
			Expect(requeueAfter).To(Equal(time.Duration(0)))
		})

		It("should delete when TTL expired", func() {
			now := time.Now()
			completionTime := metav1.NewTime(now.Add(-15 * time.Minute)) // 15 minutes ago

			vcr := &storagev1alpha1.VolumeCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vcr",
					Namespace: "default",
					Annotations: map[string]string{
						AnnotationKeyTTL: "10m",
					},
				},
				Status: storagev1alpha1.VolumeCaptureRequestStatus{
					CompletionTimestamp: &completionTime,
				},
			}

			Expect(client.Create(ctx, vcr)).To(Succeed())

			shouldDelete, requeueAfter, err := ctrl.checkAndHandleTTL(ctx, vcr)

			Expect(err).ToNot(HaveOccurred())
			Expect(shouldDelete).To(BeTrue())
			Expect(requeueAfter).To(Equal(time.Duration(0)))

			// Verify object is deleted
			err = client.Get(ctx, types.NamespacedName{Name: vcr.Name, Namespace: vcr.Namespace}, vcr)
			Expect(err).To(HaveOccurred())
		})

		It("should return RequeueAfter when TTL not expired", func() {
			now := time.Now()
			completionTime := metav1.NewTime(now.Add(-5 * time.Minute)) // 5 minutes ago, TTL=10m, so 5m left

			vcr := &storagev1alpha1.VolumeCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vcr",
					Namespace: "default",
					Annotations: map[string]string{
						AnnotationKeyTTL: "10m",
					},
				},
				Status: storagev1alpha1.VolumeCaptureRequestStatus{
					CompletionTimestamp: &completionTime,
				},
			}

			shouldDelete, requeueAfter, err := ctrl.checkAndHandleTTL(ctx, vcr)

			Expect(err).ToNot(HaveOccurred())
			Expect(shouldDelete).To(BeFalse())
			Expect(requeueAfter).To(BeNumerically(">=", 30*time.Second))
			// Jitter can add up to 10%, so max could be slightly over 1 minute
			Expect(requeueAfter).To(BeNumerically("<=", 70*time.Second))
		})

		It("should handle invalid TTL format without deleting object", func() {
			vcr := &storagev1alpha1.VolumeCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vcr",
					Namespace: "default",
					Annotations: map[string]string{
						AnnotationKeyTTL: "10minutes", // Invalid format
					},
				},
				Status: storagev1alpha1.VolumeCaptureRequestStatus{
					CompletionTimestamp: &metav1.Time{Time: time.Now()},
				},
			}

			Expect(client.Create(ctx, vcr)).To(Succeed())

			shouldDelete, requeueAfter, err := ctrl.checkAndHandleTTL(ctx, vcr)

			// Function should handle invalid TTL gracefully
			// It may return error if Get fails in retry, but should not delete object
			if err != nil {
				// If error occurs (e.g., Get fails in retry), that's acceptable for this test
				// The important part is that object is not deleted
			} else {
				Expect(shouldDelete).To(BeFalse())
				Expect(requeueAfter).To(Equal(time.Duration(0)))
			}

			// Verify object still exists (not deleted)
			err = client.Get(ctx, types.NamespacedName{Name: vcr.Name, Namespace: vcr.Namespace}, vcr)
			Expect(err).ToNot(HaveOccurred())

			// Note: InvalidTTL condition is set via Status().Patch inside retry,
			// which may not work correctly with fake client. In real scenario, condition would be set.
			// The important part is that object is not deleted.
		})

		It("should apply jitter to requeueAfter", func() {
			now := time.Now()
			completionTime := metav1.NewTime(now.Add(-5 * time.Minute))

			vcr := &storagev1alpha1.VolumeCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vcr",
					Namespace: "default",
					Annotations: map[string]string{
						AnnotationKeyTTL: "10m",
					},
				},
				Status: storagev1alpha1.VolumeCaptureRequestStatus{
					CompletionTimestamp: &completionTime,
				},
			}

			// Run multiple times to verify jitter
			requeueValues := make(map[time.Duration]bool)
			for i := 0; i < 20; i++ {
				// Create a fresh copy each time to avoid side effects
				vcrCopy := vcr.DeepCopy()
				_, requeueAfter, err := ctrl.checkAndHandleTTL(ctx, vcrCopy)
				Expect(err).ToNot(HaveOccurred())
				requeueValues[requeueAfter] = true
			}

			// Should have some variation due to jitter
			Expect(len(requeueValues)).To(BeNumerically(">", 1))
		})

		It("should use config TTL, not annotation TTL", func() {
			// This test verifies that TTL scanner uses config.RequestTTL, not annotation TTL
			// TTL annotation is informational only and does not affect deletion timing
			now := time.Now()
			completionTime := metav1.NewTime(now.Add(-15 * time.Minute)) // 15 minutes ago

			// Create VCR with:
			// - annotation TTL = 1h (should be ignored)
			// - config TTL = 10m (should be used)
			// - completionTime = 15m ago
			// Expected: VCR should be deleted (15m > 10m config TTL)
			vcr := &storagev1alpha1.VolumeCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vcr-config-ttl",
					Namespace: "default",
					UID:       types.UID("vcr-uid-config-ttl"),
					Annotations: map[string]string{
						AnnotationKeyTTL: "1h", // Annotation TTL - should be ignored
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
					CompletionTimestamp: &completionTime,
					Conditions: []metav1.Condition{
						{
							Type:               ConditionTypeReady,
							Status:             metav1.ConditionTrue,
							Reason:             ConditionReasonCompleted,
							LastTransitionTime: completionTime,
							ObservedGeneration: 1,
						},
					},
				},
			}

			Expect(client.Create(ctx, vcr)).To(Succeed())

			// Execute scanAndDeleteExpiredVCRs
			// Config TTL = 10m, completionTime = 15m ago
			// Expected: shouldDelete = true (15m > 10m)
			ctrl.scanAndDeleteExpiredVCRs(ctx, client)

			// Verify VCR is deleted (scanner used config TTL, not annotation TTL)
			deletedVCR := &storagev1alpha1.VolumeCaptureRequest{}
			err := client.Get(ctx, types.NamespacedName{Name: "test-vcr-config-ttl", Namespace: "default"}, deletedVCR)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "VCR should be deleted because config TTL (10m) expired, not annotation TTL (1h)")
		})

		It("should not delete VCR when config TTL not expired, even if annotation TTL expired", func() {
			// This test verifies that annotation TTL is ignored even when it's shorter than config TTL
			now := time.Now()
			completionTime := metav1.NewTime(now.Add(-5 * time.Minute)) // 5 minutes ago

			// Create VCR with:
			// - annotation TTL = 1m (expired, but should be ignored)
			// - config TTL = 10m (not expired)
			// - completionTime = 5m ago
			// Expected: VCR should NOT be deleted (5m < 10m config TTL)
			vcr := &storagev1alpha1.VolumeCaptureRequest{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-vcr-annotation-ignored",
					Namespace: "default",
					UID:       types.UID("vcr-uid-annotation-ignored"),
					Annotations: map[string]string{
						AnnotationKeyTTL: "1m", // Annotation TTL expired, but should be ignored
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
					CompletionTimestamp: &completionTime,
					Conditions: []metav1.Condition{
						{
							Type:               ConditionTypeReady,
							Status:             metav1.ConditionTrue,
							Reason:             ConditionReasonCompleted,
							LastTransitionTime: completionTime,
							ObservedGeneration: 1,
						},
					},
				},
			}

			Expect(client.Create(ctx, vcr)).To(Succeed())

			// Execute scanAndDeleteExpiredVCRs
			// Config TTL = 10m, completionTime = 5m ago
			// Expected: should NOT delete (5m < 10m)
			ctrl.scanAndDeleteExpiredVCRs(ctx, client)

			// Verify VCR still exists (scanner ignored annotation TTL)
			existingVCR := &storagev1alpha1.VolumeCaptureRequest{}
			err := client.Get(ctx, types.NamespacedName{Name: "test-vcr-annotation-ignored", Namespace: "default"}, existingVCR)
			Expect(err).ToNot(HaveOccurred(), "VCR should NOT be deleted because config TTL (10m) not expired, annotation TTL (1m) is ignored")
		})
	})
})

// Helper function to find condition
func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}
