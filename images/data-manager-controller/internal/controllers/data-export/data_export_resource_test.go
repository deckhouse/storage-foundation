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

package dataexport

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/common/config"
)

const (
	dataExportName      = "test-de"
	dataExportNamespace = "test-ns"
	testUserPVCName     = "test-pvc"
)

var testNames = common.NewNames(dev1alpha1.KindPVC, testUserPVCName, dataExportNamespace, dataExportName)

func setupTestScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = dev1alpha1.AddToScheme(scheme)
	_ = corev1.SchemeBuilder.AddToScheme(scheme)
	_ = networkingv1.SchemeBuilder.AddToScheme(scheme)
	_ = appsv1.SchemeBuilder.AddToScheme(scheme)
	_ = apiextensionsv1.AddToScheme(scheme)
	return scheme
}

func createTestReconciler(client client.Client, reader client.Reader, cfg *config.Options) *DataexportReconciler {
	return &DataexportReconciler{
		Client: client,
		Reader: reader,
		Config: cfg,
	}
}

func createTestConfig() *config.Options {
	return &config.Options{
		ControllerNamespace: "test-namespace",
	}
}

func newFakeClientWithStatus(t *testing.T, scheme *runtime.Scheme, objs ...client.Object) client.Client {
	t.Helper()
	builder := fake.NewClientBuilder().WithScheme(scheme)
	if len(objs) > 0 {
		builder = builder.WithObjects(objs...)
	}
	// Reconcile uses Status().Update for DataExport in multiple branches, so we enable status subresource.
	return builder.WithStatusSubresource(&dev1alpha1.DataExport{}).Build()
}

func createDataExport(spec dev1alpha1.DataExportSpec) *dev1alpha1.DataExport {
	return &dev1alpha1.DataExport{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dataExportName,
			Namespace: dataExportNamespace,
		},
		Spec: spec,
	}
}

// TestReconcile_ResourceNotFound tests Case 1: Resource not found
func TestReconcile_ResourceNotFound(t *testing.T) {
	scheme := setupTestScheme()
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	cfg := createTestConfig()
	reconciler := createTestReconciler(fakeClient, fakeClient, cfg)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "non-existent",
			Namespace: "test-namespace",
		},
	}

	result, err := reconciler.Reconcile(context.Background(), req)

	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)
}

// TestReconcile_ValidationFailed tests validation failure scenario
func TestReconcile_ValidationFailed(t *testing.T) {
	scheme := setupTestScheme()

	// Create DataExport with VirtualDisk kind but without CRD
	dataExport := createDataExport(dev1alpha1.DataExportSpec{
		Ttl:     "1h",
		Publish: false,
		TargetRef: dev1alpha1.DataExportTargetRefSpec{
			Group: virtualDisksGroup,
			Kind:  virtualDiskKind,
			Name:  "test-vd",
		},
	})

	fakeClient := newFakeClientWithStatus(t, scheme, dataExport)
	cfg := createTestConfig()
	reconciler := createTestReconciler(fakeClient, fakeClient, cfg)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-de",
			Namespace: "test-ns",
		},
	}

	result, err := reconciler.Reconcile(context.Background(), req)

	// Validation should fail, but error should be handled gracefully
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Verify condition was updated
	updatedDE := &dev1alpha1.DataExport{}
	err = fakeClient.Get(context.Background(), req.NamespacedName, updatedDE)
	require.NoError(t, err)

	condition := common.GetCondition(updatedDE.Status.Conditions, common.ConditionReady)
	assert.Equal(t, condition.Type, string(common.ConditionReady))
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, string(common.ReasonValidationFailed), condition.Reason)
}

// TestReconcile_ResourceMarkedForDelete tests Case 2: Resource marked for delete
func TestReconcile_ResourceMarkedForDelete(t *testing.T) {
	scheme := setupTestScheme()

	now := metav1.Now()
	dataExport := createDataExport(dev1alpha1.DataExportSpec{
		Ttl:     "1h",
		Publish: false,
		TargetRef: dev1alpha1.DataExportTargetRefSpec{
			Kind: dev1alpha1.KindPVC,
			Name: "test-pvc",
		},
	})
	dataExport.DeletionTimestamp = &now
	dataExport.Finalizers = []string{dev1alpha1.StorageManagerFinalizerName}

	fakeClient := newFakeClientWithStatus(t, scheme, dataExport)
	cfg := createTestConfig()
	reconciler := createTestReconciler(fakeClient, fakeClient, cfg)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-de",
			Namespace: "test-ns",
		},
	}

	result, err := reconciler.Reconcile(context.Background(), req)

	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// clearDataExportProviding should remove finalizers (if any).
	// In real Kubernetes, once finalizers are removed and DeletionTimestamp is set,
	// the object may be deleted asynchronously. The fake client simulates this by
	// potentially removing the object from the store.
	updatedDE := &dev1alpha1.DataExport{}
	getErr := fakeClient.Get(context.Background(), req.NamespacedName, updatedDE)
	if getErr != nil {
		assert.True(t, client.IgnoreNotFound(getErr) == nil)
		return
	}
	assert.Empty(t, updatedDE.Finalizers)
}

// TestReconcile_TTLExpired tests Case 2: TTL expired
func TestReconcile_TTLExpired(t *testing.T) {
	scheme := setupTestScheme()

	dataExport := createDataExport(dev1alpha1.DataExportSpec{
		Ttl:     "1h",
		Publish: false,
		TargetRef: dev1alpha1.DataExportTargetRefSpec{
			Kind: dev1alpha1.KindPVC,
			Name: "test-pvc",
		},
	})
	dataExport.Status.Conditions = []metav1.Condition{
		{
			Type:               string(common.ConditionExpired),
			Status:             metav1.ConditionTrue,
			Reason:             string(common.ReasonExpired),
			Message:            "TTL expired",
			ObservedGeneration: dataExport.Generation,
			LastTransitionTime: metav1.NewTime(time.Now()),
		},
		{
			Type:               string(common.ConditionReady),
			Status:             metav1.ConditionTrue,
			Reason:             string(common.ReasonPodReady),
			Message:            "Pod is ready",
			ObservedGeneration: dataExport.Generation,
			LastTransitionTime: metav1.NewTime(time.Now()),
		},
	}
	dataExport.Finalizers = []string{dev1alpha1.StorageManagerFinalizerName}

	fakeClient := newFakeClientWithStatus(t, scheme, dataExport)
	cfg := createTestConfig()
	reconciler := createTestReconciler(fakeClient, fakeClient, cfg)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-de",
			Namespace: "test-ns",
		},
	}

	result, err := reconciler.Reconcile(context.Background(), req)

	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	updatedDE := &dev1alpha1.DataExport{}
	require.NoError(t, fakeClient.Get(context.Background(), req.NamespacedName, updatedDE))

	// Ready should be flipped to Expired.
	condition := common.GetCondition(updatedDE.Status.Conditions, common.ConditionReady)
	assert.Equal(t, condition.Type, string(common.ConditionReady))
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, string(common.ReasonExpired), condition.Reason)

	// Finalizer should be removed by clearDataExportProviding.
	assert.Empty(t, updatedDE.Finalizers)
}

// TestReconcile_NewlyCreatedResource tests Case 3: Newly created resource
func TestReconcile_NewlyCreatedResource(t *testing.T) {
	scheme := setupTestScheme()

	dataExport := createDataExport(dev1alpha1.DataExportSpec{
		Ttl:     "1h",
		Publish: false,
		TargetRef: dev1alpha1.DataExportTargetRefSpec{
			Kind: dev1alpha1.KindPVC,
			Name: "test-pvc",
		},
	})
	// No conditions set - newly created resource

	fakeClient := newFakeClientWithStatus(t, scheme, dataExport)
	cfg := createTestConfig()
	reconciler := createTestReconciler(fakeClient, fakeClient, cfg)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-de",
			Namespace: "test-ns",
		},
	}

	result, err := reconciler.Reconcile(context.Background(), req)

	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Verify conditions were initialized
	updatedDE := &dev1alpha1.DataExport{}
	err = fakeClient.Get(context.Background(), req.NamespacedName, updatedDE)
	require.NoError(t, err)

	condition := common.GetCondition(updatedDE.Status.Conditions, common.ConditionReady)
	assert.Equal(t, condition.Type, string(common.ConditionReady))

	condition = common.GetCondition(updatedDE.Status.Conditions, common.ConditionExpired)
	assert.Equal(t, condition.Type, string(common.ConditionExpired))

	// Verify finalizer was added
	assert.Contains(t, updatedDE.Finalizers, dev1alpha1.StorageManagerFinalizerName)
}

// TestReconcile_ResourceNeedsImplementation tests Case 4: Resource needs implementation
func TestReconcile_ResourceNeedsImplementation(t *testing.T) {
	scheme := setupTestScheme()

	dataExport := createDataExport(dev1alpha1.DataExportSpec{
		Ttl:     "1h",
		Publish: false,
		TargetRef: dev1alpha1.DataExportTargetRefSpec{
			Kind: dev1alpha1.KindPVC,
			Name: "test-pvc",
		},
	})
	dataExport.Status.Conditions = []metav1.Condition{
		{
			Type:               string(common.ConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             string(common.ReasonPending),
			Message:            "Started",
			ObservedGeneration: dataExport.Generation,
			LastTransitionTime: metav1.NewTime(time.Now()),
		},
	}

	// Create PVC for the target
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pvc",
			Namespace: "test-ns",
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound,
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(dataExport, pvc).
		WithStatusSubresource(&dev1alpha1.DataExport{}).
		Build()
	cfg := createTestConfig()
	reconciler := createTestReconciler(fakeClient, fakeClient, cfg)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-de",
			Namespace: "test-ns",
		},
	}

	result, err := reconciler.Reconcile(context.Background(), req)

	// PVC doesn't have VolumeName -> ErrPVCValidationFailed -> controller should:
	// - set Ready=False, Reason=ValidationFailed
	// - requeue after 10s
	assert.NoError(t, err)
	assert.Equal(t, 10*time.Second, result.RequeueAfter)

	updatedDE := &dev1alpha1.DataExport{}
	require.NoError(t, fakeClient.Get(context.Background(), req.NamespacedName, updatedDE))
	condition := common.GetCondition(updatedDE.Status.Conditions, common.ConditionReady)
	assert.Equal(t, condition.Type, string(common.ConditionReady))
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, string(common.ReasonValidationFailed), condition.Reason)
	assert.Contains(t, condition.Message, "VolumeName")
}

// TestReconcile_ResourceAlreadyImplemented tests Case 5: Resource already implemented
func TestReconcile_ResourceAlreadyImplemented(t *testing.T) {
	scheme := setupTestScheme()

	dataExport := createDataExport(dev1alpha1.DataExportSpec{
		Ttl:     "1h",
		Publish: false,
		TargetRef: dev1alpha1.DataExportTargetRefSpec{
			Kind: dev1alpha1.KindPVC,
			Name: "test-pvc",
		},
	})
	dataExport.Status.Conditions = []metav1.Condition{
		{
			Type:               string(common.ConditionReady),
			Status:             metav1.ConditionTrue,
			Reason:             string(common.ReasonPodReady),
			Message:            "Pod is ready and export started",
			ObservedGeneration: dataExport.Generation,
			LastTransitionTime: metav1.NewTime(time.Now()),
		},
	}

	fakeClient := newFakeClientWithStatus(t, scheme, dataExport)
	cfg := createTestConfig()
	reconciler := createTestReconciler(fakeClient, fakeClient, cfg)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-de",
			Namespace: "test-ns",
		},
	}

	result, err := reconciler.Reconcile(context.Background(), req)

	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)
}

// TestReconcile_ClientGetError tests error handling when getting resource fails
func TestReconcile_ClientGetError(t *testing.T) {
	// Build a scheme without registering DataExport types to force a non-NotFound error from client.Get.
	scheme := runtime.NewScheme()
	_ = corev1.SchemeBuilder.AddToScheme(scheme)
	_ = appsv1.SchemeBuilder.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	cfg := createTestConfig()
	reconciler := createTestReconciler(fakeClient, fakeClient, cfg)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-de",
			Namespace: "test-ns",
		},
	}

	result, err := reconciler.Reconcile(context.Background(), req)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get DataExport resource from cache")
	assert.Equal(t, ctrl.Result{}, result)
}

// TestReconcile_UpdateConditionError tests error handling when updating condition fails
func TestReconcile_UpdateConditionError(t *testing.T) {
	scheme := setupTestScheme()

	// Create DataExport with VirtualDisk kind but without CRD
	dataExport := createDataExport(dev1alpha1.DataExportSpec{
		Ttl:     "1h",
		Publish: false,
		TargetRef: dev1alpha1.DataExportTargetRefSpec{
			Group: virtualDisksGroup,
			Kind:  virtualDiskKind,
			Name:  "test-vd",
		},
	})

	// Intentionally do NOT enable status subresource: deferred status flush should fail and Reconcile should surface it.
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dataExport).Build()
	cfg := createTestConfig()
	reconciler := createTestReconciler(fakeClient, fakeClient, cfg)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-de",
			Namespace: "test-ns",
		},
	}

	result, err := reconciler.Reconcile(context.Background(), req)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "update DataExport status failed")
	assert.Equal(t, ctrl.Result{}, result)
}

// TestReconcile_WithFinalizer tests that finalizer is properly added
func TestReconcile_WithFinalizer(t *testing.T) {
	scheme := setupTestScheme()

	dataExport := createDataExport(dev1alpha1.DataExportSpec{
		Ttl:     "1h",
		Publish: false,
		TargetRef: dev1alpha1.DataExportTargetRefSpec{
			Kind: dev1alpha1.KindPVC,
			Name: "test-pvc",
		},
	})

	fakeClient := newFakeClientWithStatus(t, scheme, dataExport)
	cfg := createTestConfig()
	reconciler := createTestReconciler(fakeClient, fakeClient, cfg)

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-de",
			Namespace: "test-ns",
		},
	}

	result, err := reconciler.Reconcile(context.Background(), req)

	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Verify finalizer was added
	updatedDE := &dev1alpha1.DataExport{}
	err = fakeClient.Get(context.Background(), req.NamespacedName, updatedDE)
	require.NoError(t, err)
	assert.Contains(t, updatedDE.Finalizers, dev1alpha1.StorageManagerFinalizerName)
}

// Test helpers for pure function tests

func createTestPV(name string, annotations, labels map[string]string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: annotations,
			Labels:      labels,
		},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
		},
	}
}

func makeFullAnnotations() map[string]string {
	return map[string]string{
		dev1alpha1.AnnotationUserPVCNamespaceKey:        dataExportNamespace,
		dev1alpha1.AnnotationUserPVCNameKey:             testUserPVCName,
		dev1alpha1.AnnotationStorageManagerNamespaceKey: dataExportNamespace,
		dev1alpha1.AnnotationStorageManagerNameKey:      dataExportName,
		dev1alpha1.AnnotationPVTargetKindShortKey:       testNames.TargetKindShort,
		dev1alpha1.AnnotationPVHashSuffixKey:            testNames.HashSuffix,
		dev1alpha1.AnnotationOriginalReclaimPolicyKey:   "Delete",
	}
}

func withAnnotations(mods map[string]*string) map[string]string {
	result := makeFullAnnotations()
	for k, v := range mods {
		if v == nil {
			delete(result, k)
			continue
		}

		result[k] = *v
	}
	return result
}

// assertPVExportMetadataRemoved verifies that all export annotations and label are removed from PV.
func assertPVExportMetadataRemoved(t *testing.T, pv *corev1.PersistentVolume) {
	t.Helper()
	for key := range makeFullAnnotations() {
		_, exists := pv.Annotations[key]
		assert.False(t, exists, "annotation %s should be removed", key)
	}
	_, exists := pv.Labels[dev1alpha1.LabelPVDataExporter]
	assert.False(t, exists, "label %s should be removed", dev1alpha1.LabelPVDataExporter)
}

// TestValidatePVNotOwnedByAnotherDataExport tests the validatePVNotOwnedByAnotherDataExport function
func TestValidatePVNotOwnedByAnotherDataExport(t *testing.T) {
	tests := []struct {
		name              string
		pvAnnotations     map[string]string
		expectedNamespace string
		expectedName      string
		wantErr           bool
		errContains       string
	}{
		{
			name:              "PV without annotations - OK",
			pvAnnotations:     nil,
			expectedNamespace: "test-ns",
			expectedName:      "test-de",
			wantErr:           false,
		},
		{
			name:              "PV with empty annotations - OK",
			pvAnnotations:     map[string]string{},
			expectedNamespace: "test-ns",
			expectedName:      "test-de",
			wantErr:           false,
		},
		{
			name: "PV with correct annotations (same DataExport) - OK",
			pvAnnotations: map[string]string{
				dev1alpha1.AnnotationStorageManagerNamespaceKey: "test-ns",
				dev1alpha1.AnnotationStorageManagerNameKey:      "test-de",
			},
			expectedNamespace: "test-ns",
			expectedName:      "test-de",
			wantErr:           false,
		},
		{
			name: "PV with annotations of another DataExport - error",
			pvAnnotations: map[string]string{
				dev1alpha1.AnnotationStorageManagerNamespaceKey: "other-ns",
				dev1alpha1.AnnotationStorageManagerNameKey:      "other-de",
			},
			expectedNamespace: "test-ns",
			expectedName:      "test-de",
			wantErr:           true,
			errContains:       "already owned by DataExport other-ns/other-de",
		},
		{
			name: "PV with only namespace annotation (inconsistent) - error",
			pvAnnotations: map[string]string{
				dev1alpha1.AnnotationStorageManagerNamespaceKey: "test-ns",
			},
			expectedNamespace: "test-ns",
			expectedName:      "test-de",
			wantErr:           true,
			errContains:       "inconsistent storage manager annotations",
		},
		{
			name: "PV with only name annotation (inconsistent) - error",
			pvAnnotations: map[string]string{
				dev1alpha1.AnnotationStorageManagerNameKey: "test-de",
			},
			expectedNamespace: "test-ns",
			expectedName:      "test-de",
			wantErr:           true,
			errContains:       "inconsistent storage manager annotations",
		},
		{
			name: "PV with same namespace but different name - error",
			pvAnnotations: map[string]string{
				dev1alpha1.AnnotationStorageManagerNamespaceKey: "test-ns",
				dev1alpha1.AnnotationStorageManagerNameKey:      "other-de",
			},
			expectedNamespace: "test-ns",
			expectedName:      "test-de",
			wantErr:           true,
			errContains:       "already owned by DataExport test-ns/other-de",
		},
		{
			name: "PV with different namespace but same name - error",
			pvAnnotations: map[string]string{
				dev1alpha1.AnnotationStorageManagerNamespaceKey: "other-ns",
				dev1alpha1.AnnotationStorageManagerNameKey:      "test-de",
			},
			expectedNamespace: "test-ns",
			expectedName:      "test-de",
			wantErr:           true,
			errContains:       "already owned by DataExport other-ns/test-de",
		},
		{
			name: "PV with unrelated annotations - OK",
			pvAnnotations: map[string]string{
				"some-other-annotation": "value",
			},
			expectedNamespace: "test-ns",
			expectedName:      "test-de",
			wantErr:           false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pv := createTestPV("test-pv", tt.pvAnnotations, nil)
			err := validatePVNotOwnedByAnotherDataExport(pv, tt.expectedNamespace, tt.expectedName)

			if !tt.wantErr {
				assert.NoError(t, err)
				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errContains)
		})
	}
}

// TestRemovePVExportMetadata tests the removePVExportMetadata function
func TestRemovePVExportMetadata(t *testing.T) {
	tests := []struct {
		name                     string
		pvAnnotations            map[string]string
		pvLabels                 map[string]string
		wantChanged              bool
		wantRemainingLabels      map[string]string
		wantRemainingAnnotations map[string]string
	}{
		{
			name:                     "PV without annotations or labels - no change",
			pvAnnotations:            nil,
			pvLabels:                 nil,
			wantChanged:              false,
			wantRemainingLabels:      nil,
			wantRemainingAnnotations: nil,
		},
		{
			name:                     "PV with empty maps - no change",
			pvAnnotations:            map[string]string{},
			pvLabels:                 map[string]string{},
			wantChanged:              false,
			wantRemainingLabels:      map[string]string{},
			wantRemainingAnnotations: map[string]string{},
		},
		{
			name:          "PV with all storage manager annotations and label - removes all",
			pvAnnotations: makeFullAnnotations(),
			pvLabels: map[string]string{
				dev1alpha1.LabelPVDataExporter: "true",
			},
			wantChanged:              true,
			wantRemainingLabels:      map[string]string{},
			wantRemainingAnnotations: map[string]string{},
		},
		{
			name: "PV with only some annotations - removes those present",
			pvAnnotations: map[string]string{
				dev1alpha1.AnnotationUserPVCNamespaceKey:      "test-ns",
				dev1alpha1.AnnotationOriginalReclaimPolicyKey: "Delete",
			},
			pvLabels:                 map[string]string{},
			wantChanged:              true,
			wantRemainingLabels:      map[string]string{},
			wantRemainingAnnotations: map[string]string{},
		},
		{
			name:          "PV with only label - removes label",
			pvAnnotations: map[string]string{},
			pvLabels: map[string]string{
				dev1alpha1.LabelPVDataExporter: "true",
			},
			wantChanged:              true,
			wantRemainingLabels:      map[string]string{},
			wantRemainingAnnotations: map[string]string{},
		},
		{
			name: "PV with unrelated annotations and labels - no change, keeps unrelated",
			pvAnnotations: map[string]string{
				"some-other-annotation": "value",
			},
			pvLabels: map[string]string{
				"some-other-label": "value",
			},
			wantChanged: false,
			wantRemainingLabels: map[string]string{
				"some-other-label": "value",
			},
			wantRemainingAnnotations: map[string]string{
				"some-other-annotation": "value",
			},
		},
		{
			name: "PV with mixed annotations - removes storage manager ones, keeps others",
			pvAnnotations: map[string]string{
				dev1alpha1.AnnotationUserPVCNamespaceKey:        "test-ns",
				dev1alpha1.AnnotationStorageManagerNamespaceKey: "test-ns",
				"custom-annotation":                             "custom-value",
			},
			pvLabels: map[string]string{
				dev1alpha1.LabelPVDataExporter: "true",
				"custom-label":                 "custom-value",
			},
			wantChanged: true,
			wantRemainingLabels: map[string]string{
				"custom-label": "custom-value",
			},
			wantRemainingAnnotations: map[string]string{
				"custom-annotation": "custom-value",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pv := createTestPV("test-pv", tt.pvAnnotations, tt.pvLabels)
			changed := removePVExportMetadata(pv)

			assert.Equal(t, tt.wantChanged, changed)
			assert.Equal(t, tt.wantRemainingAnnotations, pv.Annotations)
			assert.Equal(t, tt.wantRemainingLabels, pv.Labels)
		})
	}
}

// TestRestorePVReclaimPolicy tests the restorePVReclaimPolicy function
func TestRestorePVReclaimPolicy(t *testing.T) {
	tests := []struct {
		name          string
		annotations   map[string]string
		currentPolicy corev1.PersistentVolumeReclaimPolicy
		wantChanged   bool
		wantErr       bool
		wantPolicy    corev1.PersistentVolumeReclaimPolicy
	}{
		{
			name:          "No annotation - error",
			annotations:   nil,
			currentPolicy: corev1.PersistentVolumeReclaimRetain,
			wantChanged:   false,
			wantErr:       true,
			wantPolicy:    corev1.PersistentVolumeReclaimRetain,
		},
		{
			name:          "Empty annotation map - error",
			annotations:   map[string]string{},
			currentPolicy: corev1.PersistentVolumeReclaimRetain,
			wantChanged:   false,
			wantErr:       true,
			wantPolicy:    corev1.PersistentVolumeReclaimRetain,
		},
		{
			name: "Empty annotation value - error",
			annotations: map[string]string{
				dev1alpha1.AnnotationOriginalReclaimPolicyKey: "",
			},
			currentPolicy: corev1.PersistentVolumeReclaimRetain,
			wantChanged:   false,
			wantErr:       true,
			wantPolicy:    corev1.PersistentVolumeReclaimRetain,
		},
		{
			name: "Annotation with Delete - restores Delete",
			annotations: map[string]string{
				dev1alpha1.AnnotationOriginalReclaimPolicyKey: "Delete",
			},
			currentPolicy: corev1.PersistentVolumeReclaimRetain,
			wantChanged:   true,
			wantPolicy:    corev1.PersistentVolumeReclaimDelete,
		},
		{
			name: "Annotation with Retain - restores Retain",
			annotations: map[string]string{
				dev1alpha1.AnnotationOriginalReclaimPolicyKey: "Retain",
			},
			currentPolicy: corev1.PersistentVolumeReclaimDelete,
			wantChanged:   true,
			wantPolicy:    corev1.PersistentVolumeReclaimRetain,
		},
		{
			name: "Annotation with Recycle - restores Recycle",
			annotations: map[string]string{
				dev1alpha1.AnnotationOriginalReclaimPolicyKey: "Recycle",
			},
			currentPolicy: corev1.PersistentVolumeReclaimRetain,
			wantChanged:   true,
			wantPolicy:    corev1.PersistentVolumeReclaimRecycle,
		},
		{
			name: "Invalid policy value - error",
			annotations: map[string]string{
				dev1alpha1.AnnotationOriginalReclaimPolicyKey: "InvalidPolicy",
			},
			currentPolicy: corev1.PersistentVolumeReclaimRetain,
			wantChanged:   false,
			wantErr:       true,
			wantPolicy:    corev1.PersistentVolumeReclaimRetain,
		},
		{
			name: "Policy already matches - no change",
			annotations: map[string]string{
				dev1alpha1.AnnotationOriginalReclaimPolicyKey: "Delete",
			},
			currentPolicy: corev1.PersistentVolumeReclaimDelete,
			wantChanged:   false,
			wantPolicy:    corev1.PersistentVolumeReclaimDelete,
		},
		{
			name: "Retain policy already matches - no change",
			annotations: map[string]string{
				dev1alpha1.AnnotationOriginalReclaimPolicyKey: "Retain",
			},
			currentPolicy: corev1.PersistentVolumeReclaimRetain,
			wantChanged:   false,
			wantPolicy:    corev1.PersistentVolumeReclaimRetain,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pv := &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-pv",
					Annotations: tt.annotations,
				},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeReclaimPolicy: tt.currentPolicy,
				},
			}
			changed, err := restorePVReclaimPolicy(pv)

			if tt.wantErr {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.wantChanged, changed)
			assert.Equal(t, tt.wantPolicy, pv.Spec.PersistentVolumeReclaimPolicy)
		})
	}
}

func TestParsePVRecoveryInfo(t *testing.T) {
	correctLabel := map[string]string{dev1alpha1.LabelPVDataExporter: "true"}
	ptr := func(s string) *string { return &s }

	tests := []struct {
		name          string
		pvAnnotations map[string]string
		pvLabels      map[string]string
		deNS          string
		deName        string
		wantErr       bool
		errContains   string
	}{
		{
			name:          "All correct annotations and labels - success",
			pvAnnotations: makeFullAnnotations(),
			pvLabels:      correctLabel,
			deNS:          dataExportNamespace,
			deName:        dataExportName,
		},
		{
			name:          "Label present but wrong value - error",
			pvAnnotations: makeFullAnnotations(),
			pvLabels:      map[string]string{dev1alpha1.LabelPVDataExporter: "false"},
			deNS:          dataExportNamespace,
			deName:        dataExportName,
			wantErr:       true,
			errContains:   "has invalid label",
		},
		{
			name:          "Missing AnnotationUserPVCNamespaceKey - error",
			pvAnnotations: withAnnotations(map[string]*string{dev1alpha1.AnnotationUserPVCNamespaceKey: nil}),
			pvLabels:      correctLabel,
			deNS:          dataExportNamespace,
			deName:        dataExportName,
			wantErr:       true,
			errContains:   dev1alpha1.AnnotationUserPVCNamespaceKey,
		},
		{
			name:          "Missing AnnotationOriginalReclaimPolicyKey - error",
			pvAnnotations: withAnnotations(map[string]*string{dev1alpha1.AnnotationOriginalReclaimPolicyKey: nil}),
			pvLabels:      correctLabel,
			deNS:          dataExportNamespace,
			deName:        dataExportName,
			wantErr:       true,
			errContains:   "invalid PV reclaim policy",
		},
		{
			name:          "Wrong UserPVCNamespace value - error",
			pvAnnotations: withAnnotations(map[string]*string{dev1alpha1.AnnotationUserPVCNamespaceKey: ptr("wrong-ns")}),
			pvLabels:      correctLabel,
			deNS:          dataExportNamespace,
			deName:        dataExportName,
			wantErr:       true,
			errContains:   dev1alpha1.AnnotationUserPVCNamespaceKey,
		},
		{
			name:          "Wrong DataExportName value - error",
			pvAnnotations: withAnnotations(map[string]*string{dev1alpha1.AnnotationStorageManagerNameKey: ptr("wrong-de")}),
			pvLabels:      correctLabel,
			deNS:          dataExportNamespace,
			deName:        dataExportName,
			wantErr:       true,
			errContains:   dev1alpha1.AnnotationStorageManagerNameKey,
		},
		{
			name:          "Wrong TargetKindShort value - error",
			pvAnnotations: withAnnotations(map[string]*string{dev1alpha1.AnnotationPVTargetKindShortKey: ptr("unknown")}),
			pvLabels:      correctLabel,
			deNS:          dataExportNamespace,
			deName:        dataExportName,
			wantErr:       true,
			errContains:   "invalid targetKindShort",
		},
		{
			name:          "Wrong HashSuffix value - error",
			pvAnnotations: withAnnotations(map[string]*string{dev1alpha1.AnnotationPVHashSuffixKey: ptr("wrong-hash")}),
			pvLabels:      correctLabel,
			deNS:          dataExportNamespace,
			deName:        dataExportName,
			wantErr:       true,
			errContains:   "invalid hashSuffix",
		},
		{
			name:          "OriginalReclaimPolicy with empty value - error",
			pvAnnotations: withAnnotations(map[string]*string{dev1alpha1.AnnotationOriginalReclaimPolicyKey: ptr("")}),
			pvLabels:      correctLabel,
			deNS:          dataExportNamespace,
			deName:        dataExportName,
			wantErr:       true,
			errContains:   "invalid PV reclaim policy",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pv := createTestPV("test-pv", tt.pvAnnotations, tt.pvLabels)
			info, err := parsePVRecoveryInfo(pv, tt.deNS, tt.deName)

			if tt.wantErr {
				require.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
				assert.Nil(t, info)
				return
			}

			require.NoError(t, err)
			require.NotNil(t, info)
			assert.Equal(t, dataExportNamespace, info.UserPVCNamespace)
			assert.Equal(t, testUserPVCName, info.UserPVCName)
			assert.Equal(t, dataExportNamespace, info.DataExportNamespace)
			assert.Equal(t, dataExportName, info.DataExportName)
			assert.Equal(t, testNames.TargetKindShort, info.TargetKindShort)
			assert.Equal(t, testNames.HashSuffix, info.HashSuffix)
			assert.Equal(t, corev1.PersistentVolumeReclaimDelete, info.OriginalReclaimPolicy)
		})
	}
}

func TestPatchPVLabelAnnotationsClaimRef(t *testing.T) {
	scheme := setupTestScheme()
	_ = corev1.SchemeBuilder.AddToScheme(scheme)

	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test-pv",
			ResourceVersion: "1",
		},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
		},
	}

	exportPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "export-pvc",
			Namespace:       "test-ns",
			UID:             "pvc-uid-123",
			ResourceVersion: "2",
		},
	}

	dataExport := &dev1alpha1.DataExport{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dataExportName,
			Namespace: dataExportNamespace,
		},
		Spec: dev1alpha1.DataExportSpec{
			TargetRef: dev1alpha1.DataExportTargetRefSpec{
				Name: testUserPVCName,
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pv).Build()
	reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

	err := reconciler.patchPVLabelAnnotationsClaimRef(context.Background(), pv, exportPVC, dataExport, testNames, testUserPVCName)
	require.NoError(t, err)

	// Verify PV was updated
	updatedPV := &corev1.PersistentVolume{}
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-pv"}, updatedPV)
	require.NoError(t, err)

	// Check ClaimRef
	require.NotNil(t, updatedPV.Spec.ClaimRef)
	assert.Equal(t, exportPVC.Name, updatedPV.Spec.ClaimRef.Name)
	assert.Equal(t, exportPVC.Namespace, updatedPV.Spec.ClaimRef.Namespace)
	assert.Equal(t, exportPVC.UID, updatedPV.Spec.ClaimRef.UID)

	// Check annotations
	assert.Equal(t, dataExportNamespace, updatedPV.Annotations[dev1alpha1.AnnotationUserPVCNamespaceKey])
	assert.Equal(t, testUserPVCName, updatedPV.Annotations[dev1alpha1.AnnotationUserPVCNameKey])
	assert.Equal(t, dataExportNamespace, updatedPV.Annotations[dev1alpha1.AnnotationStorageManagerNamespaceKey])
	assert.Equal(t, dataExportName, updatedPV.Annotations[dev1alpha1.AnnotationStorageManagerNameKey])
	assert.Equal(t, testNames.TargetKindShort, updatedPV.Annotations[dev1alpha1.AnnotationPVTargetKindShortKey])
	assert.Equal(t, testNames.HashSuffix, updatedPV.Annotations[dev1alpha1.AnnotationPVHashSuffixKey])
	assert.Equal(t, "Delete", updatedPV.Annotations[dev1alpha1.AnnotationOriginalReclaimPolicyKey])

	// Check label
	assert.Equal(t, "true", updatedPV.Labels[dev1alpha1.LabelPVDataExporter])

	// Check ReclaimPolicy changed to Retain
	assert.Equal(t, corev1.PersistentVolumeReclaimRetain, updatedPV.Spec.PersistentVolumeReclaimPolicy)
}

func TestRestoreOriginalPVState(t *testing.T) {
	scheme := setupTestScheme()
	_ = corev1.SchemeBuilder.AddToScheme(scheme)

	tests := []struct {
		name                 string
		pv                   *corev1.PersistentVolume
		wantErr              bool
		wantReclaimPolicy    corev1.PersistentVolumeReclaimPolicy
		wantAnnotationsEmpty bool
		wantLabelsEmpty      bool
	}{
		{
			name: "PV with export metadata - restores original state",
			pv: &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "test-pv",
					ResourceVersion: "1",
					Annotations:     makeFullAnnotations(),
					Labels:          map[string]string{dev1alpha1.LabelPVDataExporter: "true"},
				},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
				},
			},
			wantReclaimPolicy:    corev1.PersistentVolumeReclaimDelete,
			wantAnnotationsEmpty: true,
			wantLabelsEmpty:      true,
		},
		{
			name: "PV without reclaim policy annotation - error blocks metadata removal",
			pv: &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name:            "clean-pv",
					ResourceVersion: "1",
				},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.pv).Build()
			reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

			err := reconciler.restoreOriginalPVState(context.Background(), tt.pv)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)

			updatedPV := &corev1.PersistentVolume{}
			err = fakeClient.Get(context.Background(), types.NamespacedName{Name: tt.pv.Name}, updatedPV)
			require.NoError(t, err)

			assert.Equal(t, tt.wantReclaimPolicy, updatedPV.Spec.PersistentVolumeReclaimPolicy)

			if tt.wantAnnotationsEmpty && tt.wantLabelsEmpty {
				assertPVExportMetadataRemoved(t, updatedPV)
			}
		})
	}
}

func TestRecoverOrphanedUserPVC_PVCTarget(t *testing.T) {
	scheme := setupTestScheme()
	_ = corev1.SchemeBuilder.AddToScheme(scheme)

	userPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "user-pvc",
			Namespace:       "test-ns",
			UID:             "user-pvc-uid",
			ResourceVersion: "1",
			Annotations: map[string]string{
				DataExportInProgressKey: "true",
			},
			Finalizers: []string{dev1alpha1.StorageManagerFinalizerName},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName: "test-pv",
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound,
		},
	}

	// PV is currently bound to export PVC
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test-pv",
			ResourceVersion: "1",
			Annotations:     makeFullAnnotations(),
			Labels:          map[string]string{dev1alpha1.LabelPVDataExporter: "true"},
		},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			ClaimRef: &corev1.ObjectReference{
				Name:      "export-pvc",
				Namespace: "controller-ns",
				UID:       "export-pvc-uid",
			},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pv, userPVC).Build()
	reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

	err := reconciler.recoverOrphanedUserPVC(context.Background(), pv, "test-ns", "user-pvc", dataExportName)
	require.NoError(t, err)

	// Verify PV ClaimRef now points to user PVC
	updatedPV := &corev1.PersistentVolume{}
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-pv"}, updatedPV)
	require.NoError(t, err)
	require.NotNil(t, updatedPV.Spec.ClaimRef)
	assert.Equal(t, "user-pvc", updatedPV.Spec.ClaimRef.Name)
	assert.Equal(t, "test-ns", updatedPV.Spec.ClaimRef.Namespace)

	// Verify PV annotations/labels removed and ReclaimPolicy restored
	assertPVExportMetadataRemoved(t, updatedPV)
	assert.Equal(t, corev1.PersistentVolumeReclaimDelete, updatedPV.Spec.PersistentVolumeReclaimPolicy)

	// Verify user PVC annotations/finalizers removed
	updatedUserPVC := &corev1.PersistentVolumeClaim{}
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "user-pvc", Namespace: "test-ns"}, updatedUserPVC)
	require.NoError(t, err)
	_, hasAnnotation := updatedUserPVC.Annotations[DataExportInProgressKey]
	assert.False(t, hasAnnotation, "export annotation should be removed from user PVC")
	assert.NotContains(t, updatedUserPVC.Finalizers, dev1alpha1.StorageManagerFinalizerName)
}

func TestRecoverOrphanedUserPVC_SnapshotBasedTarget(t *testing.T) {
	scheme := setupTestScheme()
	_ = corev1.SchemeBuilder.AddToScheme(scheme)

	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test-pv",
			ResourceVersion: "1",
			Annotations:     makeFullAnnotations(),
			Labels:          map[string]string{dev1alpha1.LabelPVDataExporter: "true"},
		},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pv).Build()
	reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

	// Empty namespace and name = snapshot-based export
	err := reconciler.recoverOrphanedUserPVC(context.Background(), pv, "", "", dataExportName)
	require.NoError(t, err)

	updatedPV := &corev1.PersistentVolume{}
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-pv"}, updatedPV)
	require.NoError(t, err)

	// Annotations and labels should be removed, ReclaimPolicy restored
	assertPVExportMetadataRemoved(t, updatedPV)
	assert.Equal(t, corev1.PersistentVolumeReclaimDelete, updatedPV.Spec.PersistentVolumeReclaimPolicy)
}

func TestDeleteOrphanedDeployment(t *testing.T) {
	scheme := setupTestScheme()

	// Generate valid hash from known namespace/name pair
	names := common.NewNames(dev1alpha1.KindPVC, "test-pvc", "test-ns", "test-de")
	validHash := names.HashSuffix
	deployName := common.DeployNameForHash(dev1alpha1.KindPVCShort, validHash)

	tests := []struct {
		name            string
		targetKindShort string
		hashSuffix      string
		existingDeploy  *appsv1.Deployment
		wantErr         bool
		errContains     string
		wantDeleted     bool
	}{
		{
			name:            "Deployment exists with correct label - deleted",
			targetKindShort: dev1alpha1.KindPVCShort,
			hashSuffix:      validHash,
			existingDeploy: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deployName,
					Namespace: "test-namespace",
					Labels: map[string]string{
						dev1alpha1.LabelApplicationKey: dev1alpha1.LabelDataExportValue,
					},
				},
			},
			wantDeleted: true,
		},
		{
			name:            "Deployment does not exist - no error",
			targetKindShort: dev1alpha1.KindPVCShort,
			hashSuffix:      validHash,
			existingDeploy:  nil,
		},
		{
			name:            "Deployment without app label - error",
			targetKindShort: dev1alpha1.KindPVCShort,
			hashSuffix:      validHash,
			existingDeploy: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deployName,
					Namespace: "test-namespace",
				},
			},
			wantErr:     true,
			errContains: "not managed by data-exporter",
		},
		{
			name:            "Deployment with wrong app label - error",
			targetKindShort: dev1alpha1.KindPVCShort,
			hashSuffix:      validHash,
			existingDeploy: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      deployName,
					Namespace: "test-namespace",
					Labels: map[string]string{
						dev1alpha1.LabelApplicationKey: "something-else",
					},
				},
			},
			wantErr:     true,
			errContains: "not managed by data-exporter",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if tt.existingDeploy != nil {
				builder = builder.WithObjects(tt.existingDeploy)
			}
			fakeClient := builder.Build()
			reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

			err := reconciler.deleteOrphanedDeployment(context.Background(), tt.targetKindShort, tt.hashSuffix)

			if tt.wantErr {
				require.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
				return
			}

			require.NoError(t, err)

			if tt.wantDeleted {
				err = fakeClient.Get(context.Background(), types.NamespacedName{Name: deployName, Namespace: "test-namespace"}, &appsv1.Deployment{})
				assert.True(t, client.IgnoreNotFound(err) == nil, "deployment should be deleted")
			}
		})
	}
}

func TestDeleteOrphanedExportPVCs(t *testing.T) {
	scheme := setupTestScheme()

	// Generate valid hash from known namespace/name pair
	names := common.NewNames(dev1alpha1.KindPVC, "test-pvc", "test-ns", "test-de")
	validHash := names.HashSuffix
	pvcName := common.ExportPVCNameForHash(dev1alpha1.KindPVCShort, validHash)

	tests := []struct {
		name            string
		targetKindShort string
		hashSuffix      string
		existingPVC     *corev1.PersistentVolumeClaim
		wantErr         bool
		errContains     string
		wantDeleted     bool
	}{
		{
			name:            "PVC exists with correct label - deleted",
			targetKindShort: dev1alpha1.KindPVCShort,
			hashSuffix:      validHash,
			existingPVC: &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      pvcName,
					Namespace: "test-namespace",
					Labels: map[string]string{
						dev1alpha1.LabelApplicationKey: dev1alpha1.LabelDataExportValue,
					},
				},
			},
			wantDeleted: true,
		},
		{
			name:            "PVC does not exist - no error",
			targetKindShort: dev1alpha1.KindPVCShort,
			hashSuffix:      validHash,
			existingPVC:     nil,
		},
		{
			name:            "PVC without app label - error",
			targetKindShort: dev1alpha1.KindPVCShort,
			hashSuffix:      validHash,
			existingPVC: &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      pvcName,
					Namespace: "test-namespace",
				},
			},
			wantErr:     true,
			errContains: "not managed by data-exporter",
		},
		{
			name:            "PVC with wrong app label - error",
			targetKindShort: dev1alpha1.KindPVCShort,
			hashSuffix:      validHash,
			existingPVC: &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{
					Name:      pvcName,
					Namespace: "test-namespace",
					Labels: map[string]string{
						dev1alpha1.LabelApplicationKey: "something-else",
					},
				},
			},
			wantErr:     true,
			errContains: "not managed by data-exporter",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if tt.existingPVC != nil {
				builder = builder.WithObjects(tt.existingPVC)
			}
			fakeClient := builder.Build()
			reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

			err := reconciler.deleteOrphanedExportPVCs(context.Background(), tt.targetKindShort, tt.hashSuffix)

			if tt.wantErr {
				require.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
				return
			}

			require.NoError(t, err)

			if tt.wantDeleted {
				err = fakeClient.Get(context.Background(), types.NamespacedName{Name: pvcName, Namespace: "test-namespace"}, &corev1.PersistentVolumeClaim{})
				assert.True(t, client.IgnoreNotFound(err) == nil, "export PVC should be deleted")
			}
		})
	}
}
