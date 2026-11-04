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

package dataexport

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func podWithClaim(name, claim string, phase corev1.PodPhase) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: dataExportNamespace},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "example.com/app"}},
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func TestPodsUsingClaim(t *testing.T) {
	deleting := podWithClaim("deleting", testUserPVCName, corev1.PodRunning)
	deleting.DeletionTimestamp = &metav1.Time{}
	deleting.Finalizers = []string{"example.com/hold"}

	twice := podWithClaim("twice", testUserPVCName, corev1.PodRunning)
	twice.Spec.Volumes = append(twice.Spec.Volumes, twice.Spec.Volumes[0])
	twice.Spec.Volumes[1].Name = "data-again"

	tests := []struct {
		name string
		pod  corev1.Pod
		want bool
	}{
		{name: "running", pod: podWithClaim("running", testUserPVCName, corev1.PodRunning), want: true},
		{name: "pending", pod: podWithClaim("pending", testUserPVCName, corev1.PodPending), want: true},
		{name: "unknown", pod: podWithClaim("unknown", testUserPVCName, corev1.PodUnknown), want: true},
		{name: "running and being deleted", pod: deleting, want: true},
		{name: "succeeded", pod: podWithClaim("succeeded", testUserPVCName, corev1.PodSucceeded), want: false},
		{name: "failed", pod: podWithClaim("failed", testUserPVCName, corev1.PodFailed), want: false},
		{name: "running with another claim", pod: podWithClaim("other", "other-pvc", corev1.PodRunning), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := podsUsingClaim([]corev1.Pod{tt.pod}, testUserPVCName)
			if !tt.want {
				assert.Empty(t, got)
				return
			}
			require.Len(t, got, 1)
			assert.Equal(t, tt.pod.Name, got[0].Name)
		})
	}

	t.Run("a pod that mounts the claim twice is listed once", func(t *testing.T) {
		got := podsUsingClaim([]corev1.Pod{twice}, testUserPVCName)
		require.Len(t, got, 1)
		assert.Equal(t, "twice", got[0].Name)
	})
}

// newPVCValidationReconciler builds a reconciler over a bound user PVC and its PV plus the given pods and
// VolumeAttachments, the objects validateUserPVCAndGetPV reads.
func newPVCValidationReconciler(t *testing.T, objs ...client.Object) (*DataexportReconciler, *corev1.PersistentVolumeClaim) {
	t.Helper()
	scheme := setupTestScheme()
	require.NoError(t, storagev1.AddToScheme(scheme))

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: testUserPVCName, Namespace: dataExportNamespace},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: testUserPVName},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: testUserPVName},
		Spec: corev1.PersistentVolumeSpec{
			ClaimRef: &corev1.ObjectReference{Name: testUserPVCName, Namespace: dataExportNamespace},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append([]client.Object{pvc, pv}, objs...)...).Build()
	return createTestReconciler(c, c, createTestConfig()), pvc
}

func attachedVA(pvName string) *storagev1.VolumeAttachment {
	return &storagev1.VolumeAttachment{
		ObjectMeta: metav1.ObjectMeta{Name: "csi-attachment"},
		Spec: storagev1.VolumeAttachmentSpec{
			Attacher: "example.csi.driver",
			NodeName: "node-1",
			Source:   storagev1.VolumeAttachmentSource{PersistentVolumeName: &pvName},
		},
		Status: storagev1.VolumeAttachmentStatus{Attached: true},
	}
}

// TestValidateUserPVCAndGetPV_AttachedVolumeHoldsTheClaim guards the namespace of the VolumeAttachment
// list: the object is cluster-scoped, so restricting the list to the PVC's namespace hid every attachment.
func TestValidateUserPVCAndGetPV_AttachedVolumeHoldsTheClaim(t *testing.T) {
	r, pvc := newPVCValidationReconciler(t, attachedVA(testUserPVName))

	pv, err := r.validateUserPVCAndGetPV(context.Background(), pvc)
	require.ErrorIs(t, err, ErrPVCValidationFailed)
	assert.Nil(t, pv)
	assert.Contains(t, err.Error(), "volumeAttachments with status True: csi-attachment")
}

func TestValidateUserPVCAndGetPV_FinishedPodDoesNotHoldTheClaim(t *testing.T) {
	done := podWithClaim("writer", testUserPVCName, corev1.PodSucceeded)
	r, pvc := newPVCValidationReconciler(t, &done)

	pv, err := r.validateUserPVCAndGetPV(context.Background(), pvc)
	require.NoError(t, err)
	require.NotNil(t, pv)
	assert.Equal(t, testUserPVName, pv.Name)
}

func TestValidateUserPVCAndGetPV_RunningPodHoldsTheClaim(t *testing.T) {
	running := podWithClaim("writer", testUserPVCName, corev1.PodRunning)
	r, pvc := newPVCValidationReconciler(t, &running)

	pv, err := r.validateUserPVCAndGetPV(context.Background(), pvc)
	require.ErrorIs(t, err, ErrPVCValidationFailed)
	assert.Nil(t, pv)
	assert.Contains(t, err.Error(), "occupied by pods writer")
}

// TestValidateUserPVCAndGetPV_FinishedPodStillAttached covers the window between a pod finishing and its
// volume being detached: the pod no longer holds the claim, but the attachment still does.
func TestValidateUserPVCAndGetPV_FinishedPodStillAttached(t *testing.T) {
	done := podWithClaim("writer", testUserPVCName, corev1.PodSucceeded)
	r, pvc := newPVCValidationReconciler(t, &done, attachedVA(testUserPVName))

	pv, err := r.validateUserPVCAndGetPV(context.Background(), pvc)
	require.ErrorIs(t, err, ErrPVCValidationFailed)
	assert.Nil(t, pv)
	assert.Contains(t, err.Error(), "volumeAttachments with status True: csi-attachment")
	assert.NotContains(t, err.Error(), "occupied by pods")
}
