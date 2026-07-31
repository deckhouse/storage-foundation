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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/common/config"
)

const (
	dataExportName      = "test-de"
	dataExportNamespace = "test-ns"
	testUserPVCName     = "test-pvc"

	testDataExportUID = types.UID("de-uid-1")
	testUserPVCUID    = types.UID("user-pvc-uid-1")
	testExportPVCUID  = types.UID("export-pvc-uid-1")
	testPVUID         = types.UID("pv-uid-1")
)

var testNames = common.NewNames(dev1alpha1.KindPVC, testUserPVCName, dataExportNamespace, dataExportName)

func setupTestScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = dev1alpha1.AddToScheme(scheme)
	_ = corev1.SchemeBuilder.AddToScheme(scheme)
	_ = networkingv1.SchemeBuilder.AddToScheme(scheme)
	_ = appsv1.SchemeBuilder.AddToScheme(scheme)
	_ = storagev1.SchemeBuilder.AddToScheme(scheme)
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
	// New expiry model: the exporter pod signals idle-TTL expiry via serverState=IdleExpired (there is
	// no standalone Expired condition anymore). Ready starts up (ServerReady); the controller must flip
	// it to False/Expired and remove the finalizer.
	dataExport.Status.ServerState = string(common.ServerStateIdleExpired)
	dataExport.Status.Conditions = []metav1.Condition{
		{
			Type:               string(common.ConditionReady),
			Status:             metav1.ConditionTrue,
			Reason:             string(common.ReasonServerReady),
			Message:            "Server is ready and export started",
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

	// DataExport catalog has a single condition (Ready). There is no standalone Expired condition
	// anymore — expiry is Ready=False/Expired + phase=Expired. (ConditionExpired was removed from the
	// catalog, so probe by string literal.)
	assert.Nil(t, common.GetCondition(updatedDE.Status.Conditions, common.ConditionType("Expired")),
		"DataExport must not carry a standalone Expired condition")

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
			Reason:             string(common.ReasonServerReady),
			Message:            "Server is ready and export started",
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

// deInRecoveryFixture builds a serving DataExport that has already recorded a lost export claim, i.e. an
// object whose only legal next step is recovery.
func deInRecoveryFixture(t *testing.T) (*dev1alpha1.DataExport, client.Client, *DataexportReconciler) {
	t.Helper()

	dataExport := createDataExport(dev1alpha1.DataExportSpec{
		Ttl:       "1h",
		TargetRef: dev1alpha1.DataExportTargetRefSpec{Kind: dev1alpha1.KindPVC, Name: "test-pvc"},
	})
	dataExport.Finalizers = []string{dev1alpha1.StorageManagerFinalizerName}
	dataExport.Status.CleanupReason = string(common.CleanupReasonExportPVCPostRebindLost)
	dataExport.Status.Recovery = &dev1alpha1.RecoveryStatus{
		SourcePVCUID: string(testUserPVCUID),
		ExportPVCUID: string(testExportPVCUID),
		PVName:       "test-pv",
		PVUID:        string(testPVUID),
	}
	dataExport.Status.Conditions = []metav1.Condition{{
		Type:               string(common.ConditionReady),
		Status:             metav1.ConditionFalse,
		Reason:             string(common.ReasonManagedResourceLost),
		Message:            "export PVC gone after rebind",
		ObservedGeneration: dataExport.Generation,
		LastTransitionTime: metav1.NewTime(time.Now()),
	}}

	// A pod still holds the export claim, so the recovery cannot get past its first barrier: the object
	// stays in the state this test is about — owing a recovery it has not finished.
	blocker := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: recoveryBlockerPodName, Namespace: testExportPVCNamespace},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
			Name: "data",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: testNames.ExportPVCName},
			},
		}}},
	}

	fakeClient := newFakeClientWithStatus(t, setupTestScheme(), dataExport, blocker)
	return dataExport, fakeClient, createTestReconciler(fakeClient, fakeClient, createTestConfig())
}

const recoveryBlockerPodName = "still-mounted"

var deRequest = ctrl.Request{NamespacedName: types.NamespacedName{Name: dataExportName, Namespace: dataExportNamespace}}

// TestReconcile_RecoveryRoutingPrecedesExpiryAndTerminal locks the branch order: an object that owes a
// recovery must not fall through to expiry cleanup, to the terminal no-op, or back into provisioning.
// Expiry is the dangerous one — it would run the ordinary teardown, which assumes the export claim still
// exists, and drop the finalizer that is currently the only thing keeping the recovery reachable.
func TestReconcile_RecoveryRoutingPrecedesExpiryAndTerminal(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*dev1alpha1.DataExport)
	}{
		{name: "plain recovery"},
		{
			name:   "idle-expired while owing recovery",
			mutate: func(de *dev1alpha1.DataExport) { de.Status.ServerState = string(common.ServerStateIdleExpired) },
		},
		{
			name:   "settled terminal phase while owing recovery",
			mutate: func(de *dev1alpha1.DataExport) { de.Status.Phase = string(common.PhaseFailed) },
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dataExport, fakeClient, reconciler := deInRecoveryFixture(t)
			if tt.mutate != nil {
				tt.mutate(dataExport)
				require.NoError(t, fakeClient.Status().Update(context.Background(), dataExport))
			}

			_, err := reconciler.Reconcile(context.Background(), deRequest)
			require.NoError(t, err)

			got := &dev1alpha1.DataExport{}
			require.NoError(t, fakeClient.Get(context.Background(), deRequest.NamespacedName, got))

			assert.Equal(t, string(common.CleanupReasonExportPVCPostRebindLost), got.Status.CleanupReason,
				"the discriminator must survive until the recovery actually completes")
			assert.False(t, common.Phase(got.Status.Phase).IsTerminal(),
				"an object owing recovery must stay non-terminal, phase=%s", got.Status.Phase)
			assert.Nil(t, got.Status.CompletionTimestamp)
			assert.Contains(t, got.Finalizers, dev1alpha1.StorageManagerFinalizerName,
				"expiry teardown must not have run and dropped the finalizer")

			ready := common.GetCondition(got.Status.Conditions, common.ConditionReady)
			require.NotNil(t, ready)
			assert.Equal(t, string(common.ReasonCleanupBlocked), ready.Reason,
				"the recovery it owes is what the object reports on, not expiry")
			assert.Contains(t, ready.Message, "B1", "and it says what is holding the recovery up")
		})
	}
}

// TestReconcile_DeletionWinsOverRecovery keeps deletion at the top of the branch order and holds it to the
// same contract as every other path: the object is released only once the teardown is actually done. A
// deletion that dropped the finalizer while a pod still held the volume would leave nothing behind to
// bring that volume home. Completion of each path is covered by
// TestTeardown_EveryEntryObeysTheSameContract.
func TestReconcile_DeletionWinsOverRecovery(t *testing.T) {
	dataExport, fakeClient, reconciler := deInRecoveryFixture(t)
	now := metav1.Now()
	dataExport.DeletionTimestamp = &now
	require.NoError(t, fakeClient.Delete(context.Background(), dataExport))

	_, err := reconciler.Reconcile(context.Background(), deRequest)
	require.NoError(t, err)

	got := &dev1alpha1.DataExport{}
	require.NoError(t, fakeClient.Get(context.Background(), deRequest.NamespacedName, got))
	assert.Contains(t, got.Finalizers, dev1alpha1.StorageManagerFinalizerName,
		"the object may not be released while a consumer still holds the volume")
	ready := common.GetCondition(got.Status.Conditions, common.ConditionReady)
	require.NotNil(t, ready)
	assert.Equal(t, string(common.ReasonCleanupBlocked), ready.Reason)
}

// TestReconcile_TerminalWithoutRecoveryStaysInert is the regression guard for objects that owe nothing:
// the new branch must not wake up a settled terminal DataExport.
func TestReconcile_TerminalWithoutRecoveryStaysInert(t *testing.T) {
	dataExport, fakeClient, reconciler := deInRecoveryFixture(t)
	dataExport.Status.CleanupReason = ""
	dataExport.Status.Phase = string(common.PhaseFailed)
	stamped := metav1.NewTime(time.Now().Add(-time.Hour))
	dataExport.Status.CompletionTimestamp = &stamped
	require.NoError(t, fakeClient.Status().Update(context.Background(), dataExport))

	_, err := reconciler.Reconcile(context.Background(), deRequest)
	require.NoError(t, err)

	got := &dev1alpha1.DataExport{}
	require.NoError(t, fakeClient.Get(context.Background(), deRequest.NamespacedName, got))
	assert.Equal(t, string(common.PhaseFailed), got.Status.Phase)
	require.NotNil(t, got.Status.CompletionTimestamp)
	assert.Equal(t, stamped.Time.Unix(), got.Status.CompletionTimestamp.Time.Unix(), "retention clock must not restart")
	assert.Contains(t, got.Finalizers, dev1alpha1.StorageManagerFinalizerName)
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

	dataExport := createDataExport(dev1alpha1.DataExportSpec{
		Ttl:     "1h",
		Publish: false,
		TargetRef: dev1alpha1.DataExportTargetRefSpec{
			Kind: dev1alpha1.KindPVC,
			Name: "test-pvc",
		},
	})

	// Force a NON-conflict error on the deferred status flush so Reconcile surfaces it. A plain fake
	// client without a status subresource no longer errors on Status().Update (it just persists the
	// whole object), so an interceptor is the reliable trigger. RetryOnConflict does not retry a
	// non-conflict error, so it propagates immediately, wrapped as "update DataExport status failed".
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(dataExport).
		WithObjects(dataExport).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(_ context.Context, _ client.Client, _ string, _ client.Object, _ ...client.SubResourceUpdateOption) error {
				return apierrors.NewInternalError(fmt.Errorf("injected status update failure"))
			},
		}).
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

func withUIDAnnotations(dataExportUID, userPVCUID types.UID) map[string]string {
	return withAnnotations(map[string]*string{
		dev1alpha1.AnnotationDataExportUIDKey: ptrTo(string(dataExportUID)),
		dev1alpha1.AnnotationUserPVCUIDKey:    ptrTo(string(userPVCUID)),
	})
}

func ptrTo[T any](v T) *T { return &v }

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
	for _, key := range []string{dev1alpha1.AnnotationDataExportUIDKey, dev1alpha1.AnnotationUserPVCUIDKey} {
		_, exists := pv.Annotations[key]
		assert.False(t, exists, "takeover identity %s must not outlive the takeover", key)
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
		expectDEUID   types.UID
		expectPVCUID  types.UID
		wantErr       bool
		wantConflict  bool
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
			// Legacy takeover: the export was provisioned before the UID model existed. Absence of both
			// annotations is not corruption, so the parse stays silent and the old flow keeps working.
			name:          "Legacy PV without UID annotations - success",
			pvAnnotations: makeFullAnnotations(),
			pvLabels:      correctLabel,
			deNS:          dataExportNamespace,
			deName:        dataExportName,
			expectDEUID:   testDataExportUID,
			expectPVCUID:  testUserPVCUID,
		},
		{
			name:          "Matching UIDs - success",
			pvAnnotations: withUIDAnnotations(testDataExportUID, testUserPVCUID),
			pvLabels:      correctLabel,
			deNS:          dataExportNamespace,
			deName:        dataExportName,
			expectDEUID:   testDataExportUID,
			expectPVCUID:  testUserPVCUID,
		},
		{
			// The name check cannot see this: a DataExport deleted and recreated under the same name is a
			// different object, and adopting the PV its predecessor took over would hide a live takeover.
			name:          "PV taken over by a same-named predecessor DataExport - error",
			pvAnnotations: withUIDAnnotations("older-data-export-uid", testUserPVCUID),
			pvLabels:      correctLabel,
			deNS:          dataExportNamespace,
			deName:        dataExportName,
			expectDEUID:   testDataExportUID,
			expectPVCUID:  testUserPVCUID,
			wantErr:       true,
			wantConflict:  true,
			errContains:   dev1alpha1.AnnotationDataExportUIDKey,
		},
		{
			// The user's claim was recreated under the same name: the PV records the claim we may return
			// the volume to, and it is no longer the one standing in front of us.
			name:          "Source PVC recreated under the same name - error",
			pvAnnotations: withUIDAnnotations(testDataExportUID, "older-user-pvc-uid"),
			pvLabels:      correctLabel,
			deNS:          dataExportNamespace,
			deName:        dataExportName,
			expectDEUID:   testDataExportUID,
			expectPVCUID:  testUserPVCUID,
			wantErr:       true,
			wantConflict:  true,
			errContains:   dev1alpha1.AnnotationUserPVCUIDKey,
		},
		{
			// The orphan sweep works from a deleted parent and can prove no UID; it must still be able to
			// read the PV it is cleaning up.
			name:          "Caller without expectations skips the UID checks - success",
			pvAnnotations: withUIDAnnotations("some-data-export-uid", "some-user-pvc-uid"),
			pvLabels:      correctLabel,
			deNS:          dataExportNamespace,
			deName:        dataExportName,
		},
		{
			// Half an identity is worse than none: it was written by a controller that knows the UID
			// model, so something dropped it. That is corruption, not a pre-UID takeover, and must not
			// silently fall through the legacy door.
			name: "Only the DataExport UID survived - corrupted, not legacy",
			pvAnnotations: withAnnotations(map[string]*string{
				dev1alpha1.AnnotationDataExportUIDKey: ptrTo(string(testDataExportUID)),
			}),
			pvLabels:     correctLabel,
			deNS:         dataExportNamespace,
			deName:       dataExportName,
			expectDEUID:  testDataExportUID,
			expectPVCUID: testUserPVCUID,
			wantErr:      true,
			wantConflict: true,
			errContains:  "incomplete takeover identity",
		},
		{
			name: "Only the source PVC UID survived - corrupted, not legacy",
			pvAnnotations: withAnnotations(map[string]*string{
				dev1alpha1.AnnotationUserPVCUIDKey: ptrTo(string(testUserPVCUID)),
			}),
			pvLabels:     correctLabel,
			deNS:         dataExportNamespace,
			deName:       dataExportName,
			expectDEUID:  testDataExportUID,
			expectPVCUID: testUserPVCUID,
			wantErr:      true,
			wantConflict: true,
			errContains:  "incomplete takeover identity",
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
			info, err := parsePVRecoveryInfo(pv, pvOwnerExpectation{
				DataExportNamespace: tt.deNS,
				DataExportName:      tt.deName,
				DataExportUID:       tt.expectDEUID,
				SourcePVCUID:        tt.expectPVCUID,
			})

			if tt.wantErr {
				require.Error(t, err)
				if tt.wantConflict {
					require.ErrorIs(t, err, ErrPVConflict, "an identity mismatch is a takeover conflict")
				}
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
			UID:       testDataExportUID,
		},
		Spec: dev1alpha1.DataExportSpec{
			TargetRef: dev1alpha1.DataExportTargetRefSpec{
				Name: testUserPVCName,
			},
		},
	}

	userPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: testUserPVCName, Namespace: dataExportNamespace, UID: testUserPVCUID},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pv).Build()
	reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

	err := reconciler.patchPVLabelAnnotationsClaimRef(context.Background(), pv, exportPVC, dataExport, testNames, userPVC, true)
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

	// The takeover identity: names alone cannot tell a recreated object from the original one.
	assert.Equal(t, string(testDataExportUID), updatedPV.Annotations[dev1alpha1.AnnotationDataExportUIDKey])
	assert.Equal(t, string(testUserPVCUID), updatedPV.Annotations[dev1alpha1.AnnotationUserPVCUIDKey])
}

// deTakeoverFixture builds the three live objects of a PVC-target takeover: the user's claim, the
// controller-owned export claim and the PV between them.
func deTakeoverFixture() (*dev1alpha1.DataExport, *corev1.PersistentVolume, *corev1.PersistentVolumeClaim, *corev1.PersistentVolumeClaim) {
	dataExport := &dev1alpha1.DataExport{
		ObjectMeta: metav1.ObjectMeta{Name: dataExportName, Namespace: dataExportNamespace, UID: testDataExportUID},
		Spec: dev1alpha1.DataExportSpec{
			TargetRef: dev1alpha1.DataExportTargetRefSpec{Kind: dev1alpha1.KindPVC, Name: testUserPVCName},
		},
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pv", UID: testPVUID, ResourceVersion: "1"},
		Spec:       corev1.PersistentVolumeSpec{PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete},
	}
	userPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: testUserPVCName, Namespace: dataExportNamespace, UID: testUserPVCUID,
			Annotations: map[string]string{},
		},
		Spec: corev1.PersistentVolumeClaimSpec{VolumeName: pv.Name},
	}
	// Before the takeover the PV is still bound to the user's claim; that binding is what proves the
	// claim in hand is the one the volume is being taken from.
	pv.Spec.ClaimRef = &corev1.ObjectReference{
		Namespace: userPVC.Namespace, Name: userPVC.Name, UID: userPVC.UID,
	}
	exportPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNames.ExportPVCName, Namespace: dataExportNamespace, UID: testExportPVCUID, ResourceVersion: "1",
		},
		Spec: corev1.PersistentVolumeClaimSpec{VolumeName: pv.Name},
	}
	return dataExport, pv, userPVC, exportPVC
}

func assertRecordedTakeover(t *testing.T, de *dev1alpha1.DataExport) {
	t.Helper()
	require.NotNil(t, de.Status.Recovery, "the takeover identity must be persisted with the export status")
	assert.Equal(t, string(testUserPVCUID), de.Status.Recovery.SourcePVCUID)
	assert.Equal(t, string(testExportPVCUID), de.Status.Recovery.ExportPVCUID)
	assert.Equal(t, "test-pv", de.Status.Recovery.PVName)
	assert.Equal(t, string(testPVUID), de.Status.Recovery.PVUID)
}

// TestEnsureExportPVReady_RecordsTakeoverIdentity is the point of step 3: before the volume changes
// hands, the export records which objects it took it from. Without this the controller cannot later
// prove that a same-named claim is the one it may give the volume back to.
func TestEnsureExportPVReady_RecordsTakeoverIdentity(t *testing.T) {
	dataExport, pv, userPVC, exportPVC := deTakeoverFixture()

	fakeClient := newFakeClientWithStatus(t, setupTestScheme(), pv, userPVC, exportPVC)
	reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

	require.NoError(t, reconciler.ensureExportPVReady(context.Background(), pv, exportPVC, testNames, dataExport, testUserPVCName))

	assertRecordedTakeover(t, dataExport)

	updatedPV := &corev1.PersistentVolume{}
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: pv.Name}, updatedPV))
	assert.Equal(t, string(testDataExportUID), updatedPV.Annotations[dev1alpha1.AnnotationDataExportUIDKey])
	assert.Equal(t, string(testUserPVCUID), updatedPV.Annotations[dev1alpha1.AnnotationUserPVCUIDKey])
}

// TestEnsureExportPVReady_RepairsIdentityAfterRestart covers the crash window between the PV patch and
// the status write: the PV is already fully prepared, so the patch path is skipped, and the identity
// would stay missing in status forever if only the patch recorded it.
func TestEnsureExportPVReady_RepairsIdentityAfterRestart(t *testing.T) {
	dataExport, pv, userPVC, exportPVC := deTakeoverFixture()
	pv.Annotations = withUIDAnnotations(testDataExportUID, testUserPVCUID)
	pv.Labels = map[string]string{dev1alpha1.LabelPVDataExporter: "true"}
	pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain
	pv.Spec.ClaimRef = &corev1.ObjectReference{
		Namespace: exportPVC.Namespace, Name: exportPVC.Name, UID: exportPVC.UID,
	}

	fakeClient := newFakeClientWithStatus(t, setupTestScheme(), pv, userPVC, exportPVC)
	reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

	require.NoError(t, reconciler.ensureExportPVReady(context.Background(), pv, exportPVC, testNames, dataExport, testUserPVCName))

	assertRecordedTakeover(t, dataExport)
}

// TestEnsureExportPVReady_RefusesForeignTakeover: a PV carrying somebody else's identity belongs to a
// takeover we know nothing about. Neither the PV nor our own status may be written in that case —
// recording the live identity would put into status exactly the takeover the PV just rejected, and
// status is the write-once evidence recovery will later trust.
func TestEnsureExportPVReady_RefusesForeignTakeover(t *testing.T) {
	for _, tt := range []struct {
		name        string
		annotations map[string]string
		survives    string
		key         string
	}{
		{
			name:        "PV taken over by a same-named predecessor DataExport",
			annotations: withUIDAnnotations("older-data-export-uid", testUserPVCUID),
			survives:    "older-data-export-uid",
			key:         dev1alpha1.AnnotationDataExportUIDKey,
		},
		{
			name:        "PV records a source claim that was since recreated",
			annotations: withUIDAnnotations(testDataExportUID, "older-user-pvc-uid"),
			survives:    "older-user-pvc-uid",
			key:         dev1alpha1.AnnotationUserPVCUIDKey,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dataExport, pv, userPVC, exportPVC := deTakeoverFixture()
			pv.Annotations = tt.annotations
			pv.Labels = map[string]string{dev1alpha1.LabelPVDataExporter: "true"}

			fakeClient := newFakeClientWithStatus(t, setupTestScheme(), pv, userPVC, exportPVC)
			reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

			err := reconciler.ensureExportPVReady(context.Background(), pv, exportPVC, testNames, dataExport, testUserPVCName)
			require.ErrorIs(t, err, ErrPVConflict)

			assert.Nil(t, dataExport.Status.Recovery,
				"a rejected takeover must not be recorded as if it had happened")

			updatedPV := &corev1.PersistentVolume{}
			require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: pv.Name}, updatedPV))
			assert.Equal(t, tt.survives, updatedPV.Annotations[tt.key], "the existing identity must survive")
		})
	}
}

// TestReconcile_ForeignTakeoverPersistsNoIdentity closes the loop through the deferred status write: the
// reconcile fails, and the status write that runs on the way out must not carry a takeover record for a
// volume this export was refused.
func TestReconcile_ForeignTakeoverPersistsNoIdentity(t *testing.T) {
	dataExport, pv, userPVC, _ := deTakeoverFixture()
	dataExport.Status.Conditions = []metav1.Condition{{
		Type: string(common.ConditionReady), Status: metav1.ConditionFalse,
		Reason: string(common.ReasonPending), LastTransitionTime: metav1.NewTime(time.Now()),
	}}
	pv.Annotations = withUIDAnnotations("older-data-export-uid", testUserPVCUID)
	pv.Labels = map[string]string{dev1alpha1.LabelPVDataExporter: "true"}
	pv.Spec.ClaimRef = &corev1.ObjectReference{
		Namespace: userPVC.Namespace, Name: userPVC.Name, UID: userPVC.UID,
	}
	userPVC.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	userPVC.Spec.Resources = corev1.VolumeResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
	}
	volumeMode := corev1.PersistentVolumeFilesystem
	userPVC.Spec.VolumeMode = &volumeMode
	userPVC.Status.Phase = corev1.ClaimBound

	scheme := setupTestScheme()
	require.NoError(t, storagev1.SchemeBuilder.AddToScheme(scheme))

	fakeClient := newFakeClientWithStatus(t, scheme, dataExport, pv, userPVC)
	reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

	_, err := reconciler.Reconcile(context.Background(), deRequest)
	require.ErrorIs(t, err, ErrPVConflict)
	// The name-based owner check passes here (the PV names this very DataExport), so the conflict can
	// only have come from the UID comparison.
	assert.Contains(t, err.Error(), dev1alpha1.AnnotationDataExportUIDKey)

	got := &dev1alpha1.DataExport{}
	require.NoError(t, fakeClient.Get(context.Background(), deRequest.NamespacedName, got))
	assert.Nil(t, got.Status.Recovery, "no takeover record may reach the API for a refused takeover")
	assert.False(t, common.Phase(got.Status.Phase).IsTerminal())
}

// TestEnsureExportPVReady_RefusesToRepointRecordedIdentity: once recorded, the identity is what recovery
// will trust. Silently refreshing it to whatever is live now would make every later comparison agree
// with itself and the loss/mismatch detection meaningless.
func TestEnsureExportPVReady_RefusesToRepointRecordedIdentity(t *testing.T) {
	dataExport, pv, userPVC, exportPVC := deTakeoverFixture()
	dataExport.Status.Recovery = &dev1alpha1.RecoveryStatus{
		SourcePVCUID: string(testUserPVCUID),
		ExportPVCUID: "a-previous-export-claim-uid",
		PVName:       pv.Name,
		PVUID:        string(testPVUID),
	}

	fakeClient := newFakeClientWithStatus(t, setupTestScheme(), pv, userPVC, exportPVC)
	reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

	err := reconciler.ensureExportPVReady(context.Background(), pv, exportPVC, testNames, dataExport, testUserPVCName)
	require.ErrorIs(t, err, ErrPVConflict)
	assert.Equal(t, "a-previous-export-claim-uid", dataExport.Status.Recovery.ExportPVCUID,
		"the recorded identity must not be overwritten by the live one")
}

// TestReconcile_PersistsTakeoverIdentity walks the whole provisioning path once: the identity is only
// useful if it survives the reconcile, i.e. reaches the API through the deferred status write.
func TestReconcile_PersistsTakeoverIdentity(t *testing.T) {
	dataExport, pv, userPVC, _ := deTakeoverFixture()
	dataExport.Status.Conditions = []metav1.Condition{{
		Type: string(common.ConditionReady), Status: metav1.ConditionFalse,
		Reason: string(common.ReasonPending), LastTransitionTime: metav1.NewTime(time.Now()),
	}}
	userPVC.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	userPVC.Spec.Resources = corev1.VolumeResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
	}
	volumeMode := corev1.PersistentVolumeFilesystem
	userPVC.Spec.VolumeMode = &volumeMode
	userPVC.Status.Phase = corev1.ClaimBound
	pv.Spec.ClaimRef = &corev1.ObjectReference{
		Namespace: userPVC.Namespace, Name: userPVC.Name, UID: userPVC.UID,
	}

	// The user PVC detach checks for live VolumeAttachments before taking the volume over.
	scheme := setupTestScheme()
	require.NoError(t, storagev1.SchemeBuilder.AddToScheme(scheme))

	// A ready exporter Deployment keeps this test on the takeover path: creating one would block on a
	// five-minute rollout wait that says nothing about the identity record.
	exportDeploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: testNames.DeployName, Namespace: createTestConfig().ControllerNamespace},
		Spec:       appsv1.DeploymentSpec{Replicas: common.Int32Ptr(1)},
		Status:     appsv1.DeploymentStatus{Replicas: 1, ReadyReplicas: 1, AvailableReplicas: 1},
	}
	dataExport.Status.ServerState = string(common.ServerStateReady)

	fakeClient := newFakeClientWithStatus(t, scheme, dataExport, pv, userPVC, exportDeploy)
	reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

	_, err := reconciler.Reconcile(context.Background(), deRequest)
	require.NoError(t, err)

	got := &dev1alpha1.DataExport{}
	require.NoError(t, fakeClient.Get(context.Background(), deRequest.NamespacedName, got))
	require.NotNil(t, got.Status.Recovery, "the takeover identity must be persisted, not only computed")
	assert.Equal(t, string(testUserPVCUID), got.Status.Recovery.SourcePVCUID)
	assert.Equal(t, pv.Name, got.Status.Recovery.PVName)
	assert.Equal(t, string(testPVUID), got.Status.Recovery.PVUID)
	// ExportPVCUID is not asserted here: the export claim is created during this reconcile and the fake
	// client, unlike an API server, assigns no UID on create. TestEnsureExportPVReady_RecordsTakeoverIdentity
	// covers it against a claim that already has one.

	updatedPV := &corev1.PersistentVolume{}
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: pv.Name}, updatedPV))
	assert.Equal(t, string(testDataExportUID), updatedPV.Annotations[dev1alpha1.AnnotationDataExportUIDKey])
	assert.Equal(t, string(testUserPVCUID), updatedPV.Annotations[dev1alpha1.AnnotationUserPVCUIDKey])
}

// TestEnsureExportPVReady_LegacyTakeoverRecordsNothing is the legacy contract: an export provisioned by
// the previous controller is already past the rebind, so the only thing linking it to a source claim is
// a name. Recording an identity from whatever claim currently holds that name would manufacture evidence
// recovery is meant to verify, so the export keeps running with no identity at all.
func TestEnsureExportPVReady_LegacyTakeoverRecordsNothing(t *testing.T) {
	for _, tt := range []struct {
		name        string
		mutate      func(*corev1.PersistentVolume)
		wantPatched bool
	}{
		{name: "PV already in export-ready shape"},
		{
			// Drift repair still runs for a legacy export; it just must not invent the UID pair.
			name: "PV drifted and gets repaired",
			mutate: func(pv *corev1.PersistentVolume) {
				pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimDelete
			},
			wantPatched: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dataExport, pv, userPVC, exportPVC := deTakeoverFixture()
			pv.Annotations = makeFullAnnotations() // pre-UID controller: no identity annotations at all
			pv.Labels = map[string]string{dev1alpha1.LabelPVDataExporter: "true"}
			pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain
			pv.Spec.ClaimRef = &corev1.ObjectReference{
				Namespace: exportPVC.Namespace, Name: exportPVC.Name, UID: exportPVC.UID,
			}
			if tt.mutate != nil {
				tt.mutate(pv)
			}

			fakeClient := newFakeClientWithStatus(t, setupTestScheme(), pv, userPVC, exportPVC)
			reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

			require.NoError(t, reconciler.ensureExportPVReady(context.Background(), pv, exportPVC, testNames, dataExport, testUserPVCName))
			assert.Nil(t, dataExport.Status.Recovery, "a legacy takeover has no provable identity to record")

			updatedPV := &corev1.PersistentVolume{}
			require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: pv.Name}, updatedPV))
			assert.NotContains(t, updatedPV.Annotations, dev1alpha1.AnnotationDataExportUIDKey)
			assert.NotContains(t, updatedPV.Annotations, dev1alpha1.AnnotationUserPVCUIDKey)
			if tt.wantPatched {
				assert.Equal(t, corev1.PersistentVolumeReclaimRetain, updatedPV.Spec.PersistentVolumeReclaimPolicy,
					"the ordinary repair must still happen")
			}
		})
	}
}

// TestEnsureExportPVReady_UnboundPVIsNotAProof closes the one way a name could still become an identity:
// a PV that lost its claimRef, and a claim recreated under the old name that points back at it. Nothing
// there shows the claim ever owned the volume, so the export runs on unproven rather than recording the
// impostor's UID and presenting it as verified afterwards.
func TestEnsureExportPVReady_UnboundPVIsNotAProof(t *testing.T) {
	dataExport, pv, userPVC, exportPVC := deTakeoverFixture()
	pv.Annotations = makeFullAnnotations()
	pv.Labels = map[string]string{dev1alpha1.LabelPVDataExporter: "true"}
	pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain
	pv.Spec.ClaimRef = nil
	userPVC.Spec.VolumeName = pv.Name

	fakeClient := newFakeClientWithStatus(t, setupTestScheme(), pv, userPVC, exportPVC)
	reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

	require.NoError(t, reconciler.ensureExportPVReady(context.Background(), pv, exportPVC, testNames, dataExport, testUserPVCName))
	assert.Nil(t, dataExport.Status.Recovery)

	updatedPV := &corev1.PersistentVolume{}
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: pv.Name}, updatedPV))
	assert.NotContains(t, updatedPV.Annotations, dev1alpha1.AnnotationDataExportUIDKey)
	assert.NotContains(t, updatedPV.Annotations, dev1alpha1.AnnotationUserPVCUIDKey)
}

// TestEnsureExportPVReady_SnapshotTargetRecordsNothing: a snapshot export provisions its own volume and
// takes nothing away from the user, so there is nothing to give back and no identity to record.
func TestEnsureExportPVReady_SnapshotTargetRecordsNothing(t *testing.T) {
	dataExport, pv, _, exportPVC := deTakeoverFixture()
	snapshotNames := common.NewNames(dev1alpha1.KindVolumeSnapshot, "snap", dataExportNamespace, dataExportName)

	fakeClient := newFakeClientWithStatus(t, setupTestScheme(), pv, exportPVC)
	reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

	require.NoError(t, reconciler.ensureExportPVReady(context.Background(), pv, exportPVC, snapshotNames, dataExport, ""))
	assert.Nil(t, dataExport.Status.Recovery)
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

// orphanFixture builds what the sweep actually finds: a volume still held by the export claim, marked and
// annotated with the identity of a DataExport that no longer exists, and the user's claim waiting for it.
func orphanFixture() (*corev1.PersistentVolume, *corev1.PersistentVolumeClaim) {
	annotations := withUIDAnnotations(testDataExportUID, testUserPVCUID)
	annotations[dev1alpha1.AnnotationOriginalReclaimPolicyKey] = string(corev1.PersistentVolumeReclaimDelete)
	annotations[dev1alpha1.AnnotationUserPVCNamespaceKey] = dataExportNamespace
	annotations[dev1alpha1.AnnotationUserPVCNameKey] = testUserPVCName
	annotations[dev1alpha1.AnnotationStorageManagerNamespaceKey] = dataExportNamespace
	annotations[dev1alpha1.AnnotationStorageManagerNameKey] = dataExportName
	annotations[dev1alpha1.AnnotationPVTargetKindShortKey] = testNames.TargetKindShort
	annotations[dev1alpha1.AnnotationPVHashSuffixKey] = testNames.HashSuffix

	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pv", ResourceVersion: "1", UID: testPVUID,
			Annotations: annotations,
			Labels:      map[string]string{dev1alpha1.LabelPVDataExporter: "true"},
		},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			ClaimRef: &corev1.ObjectReference{
				Namespace: testExportPVCNamespace, Name: testNames.ExportPVCName, UID: testExportPVCUID,
			},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound},
	}
	userPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: testUserPVCName, Namespace: dataExportNamespace, UID: testUserPVCUID, ResourceVersion: "1",
			Annotations: map[string]string{DataExportInProgressKey: "true"},
			Finalizers:  []string{dev1alpha1.StorageManagerFinalizerName},
		},
		Spec:   corev1.PersistentVolumeClaimSpec{VolumeName: "test-pv"},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	return pv, userPVC
}

// TestRemoveOrphanResources_FindsTheInfrastructureThatExists: with the parent gone, the only trustworthy
// account of what was created is the suffix recorded on the volume. A VirtualDisk export is where this
// bites: what was exported is the disk, while the claim the volume goes back to is the disk's backing PVC
// under an entirely different name. The sweep must not identify export infrastructure through that claim.
func TestRemoveOrphanResources_FindsTheInfrastructureThatExists(t *testing.T) {
	const backingPVCName = "disk-a-pvc-9f2c1"

	// The export was made for a VirtualDisk, so its resources are named from the DataExport's identity
	// with the vd kind — never from the backing claim.
	exportNames := common.NewNamesFromShort(dev1alpha1.KindVirtualDiskShort, "disk-a", dataExportNamespace, dataExportName)

	pv, _ := orphanFixture()
	pv.Annotations[dev1alpha1.AnnotationPVTargetKindShortKey] = exportNames.TargetKindShort
	pv.Annotations[dev1alpha1.AnnotationPVHashSuffixKey] = exportNames.HashSuffix
	pv.Annotations[dev1alpha1.AnnotationUserPVCNameKey] = backingPVCName
	pv.Spec.ClaimRef = &corev1.ObjectReference{
		Namespace: testExportPVCNamespace, Name: exportNames.ExportPVCName, UID: testExportPVCUID,
	}

	backingPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: backingPVCName, Namespace: dataExportNamespace, UID: testUserPVCUID, ResourceVersion: "1",
			Annotations: map[string]string{DataExportInProgressKey: "true"},
			Finalizers:  []string{dev1alpha1.StorageManagerFinalizerName},
		},
		Spec:   corev1.PersistentVolumeClaimSpec{VolumeName: pv.Name},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	exportPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: exportNames.ExportPVCName, Namespace: testExportPVCNamespace, UID: testExportPVCUID, ResourceVersion: "1",
		},
		Spec: corev1.PersistentVolumeClaimSpec{VolumeName: pv.Name},
	}
	exportDeploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: exportNames.DeployName, Namespace: testExportPVCNamespace,
		Labels: map[string]string{dev1alpha1.LabelApplicationKey: dev1alpha1.LabelDataExportValue},
	}}
	// Named as if the sweep had identified the export through the backing claim instead of the record.
	decoy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name:      common.NewNamesFromShort(dev1alpha1.KindVirtualDiskShort, "", dataExportNamespace, backingPVCName).DeployName,
		Namespace: testExportPVCNamespace,
		Labels:    map[string]string{dev1alpha1.LabelApplicationKey: dev1alpha1.LabelDataExportValue},
	}}

	fakeClient := newFakeClientWithStatus(t, setupTestScheme(), pv, backingPVC, exportPVC, exportDeploy, decoy)
	reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

	blocked, err := reconciler.removeOrphanResources(context.Background(), dataExportNamespace, dataExportName)
	require.NoError(t, err)
	require.Nil(t, blocked)

	assert.True(t, apierrors.IsNotFound(fakeClient.Get(context.Background(),
		types.NamespacedName{Namespace: testExportPVCNamespace, Name: exportNames.DeployName}, &appsv1.Deployment{})),
		"the deployment named by the recorded identity must go")
	assert.True(t, apierrors.IsNotFound(fakeClient.Get(context.Background(),
		types.NamespacedName{Namespace: testExportPVCNamespace, Name: exportNames.ExportPVCName}, &corev1.PersistentVolumeClaim{})),
		"and so must the export claim named by it")
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Namespace: testExportPVCNamespace, Name: decoy.Name}, &appsv1.Deployment{}),
		"nothing named after the backing claim is any of the sweep's business")

	updatedPV := &corev1.PersistentVolume{}
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: pv.Name}, updatedPV))
	require.NotNil(t, updatedPV.Spec.ClaimRef)
	assert.Equal(t, backingPVCName, updatedPV.Spec.ClaimRef.Name, "the volume goes back to the disk's claim")
	assertPVExportMetadataRemoved(t, updatedPV)
}

// TestRemoveOrphanResources_ReturnsTheVolumeToItsOwner: the parent is gone, so nobody is left to report to
// or to hold a finalizer for — but the user's volume must still come home.
func TestRemoveOrphanResources_ReturnsTheVolumeToItsOwner(t *testing.T) {
	pv, userPVC := orphanFixture()

	fakeClient := newFakeClientWithStatus(t, setupTestScheme(), pv, userPVC)
	reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

	blocked, err := reconciler.removeOrphanResources(context.Background(), dataExportNamespace, dataExportName)
	require.NoError(t, err)
	require.Nil(t, blocked)

	updatedPV := &corev1.PersistentVolume{}
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-pv"}, updatedPV))
	require.NotNil(t, updatedPV.Spec.ClaimRef)
	assert.Equal(t, testUserPVCName, updatedPV.Spec.ClaimRef.Name)
	assert.Equal(t, dataExportNamespace, updatedPV.Spec.ClaimRef.Namespace)
	assertPVExportMetadataRemoved(t, updatedPV)
	assert.Equal(t, corev1.PersistentVolumeReclaimDelete, updatedPV.Spec.PersistentVolumeReclaimPolicy)

	updatedUserPVC := &corev1.PersistentVolumeClaim{}
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: testUserPVCName, Namespace: dataExportNamespace}, updatedUserPVC))
	assert.NotContains(t, updatedUserPVC.Annotations, DataExportInProgressKey)
	assert.NotContains(t, updatedUserPVC.Finalizers, dev1alpha1.StorageManagerFinalizerName)
}

// TestRemoveOrphanResources_BarrierStopsBeforeTheIrreversibleStep: the sweep obeys the same barriers as
// every other path. A pod still holding the export claim means the volume stays exactly where it is.
func TestRemoveOrphanResources_BarrierStopsBeforeTheIrreversibleStep(t *testing.T) {
	pv, userPVC := orphanFixture()
	blocker := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: recoveryBlockerPodName, Namespace: testExportPVCNamespace},
		Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
			Name: "data",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: testNames.ExportPVCName},
			},
		}}},
	}

	fakeClient := newFakeClientWithStatus(t, setupTestScheme(), pv, userPVC, blocker)
	reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

	blocked, err := reconciler.removeOrphanResources(context.Background(), dataExportNamespace, dataExportName)
	require.NoError(t, err, "a barrier is a state to wait on, not a failure")
	require.NotNil(t, blocked)
	assert.Equal(t, "B1", blocked.Name)

	updatedPV := &corev1.PersistentVolume{}
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-pv"}, updatedPV))
	assert.Equal(t, testNames.ExportPVCName, updatedPV.Spec.ClaimRef.Name)
	assert.Contains(t, updatedPV.Labels, dev1alpha1.LabelPVDataExporter,
		"the marks stay, so the next pass finds the volume again")
}

// runRecoveryOn drives the teardown for a takeover known only from the volume's own annotations, which is
// what every pre-record export and every orphan sweep has to work from. An empty expected export UID says
// the caller cannot prove which export owns the takeover — the orphan sweep's position.
func runRecoveryOn(reconciler *DataexportReconciler, pvName string, expectExportUID types.UID) (bool, *recoveryBarrier, error) {
	return reconciler.reconcileLiveExportRecovery(context.Background(), testNames, takeoverRef{
		PVName:        pvName,
		SourceClaim:   types.NamespacedName{Namespace: dataExportNamespace, Name: testUserPVCName},
		DataExportUID: expectExportUID,
	})
}

func requireRecoveryReturnsVolume(t *testing.T, reconciler *DataexportReconciler, pvName string, expectExportUID types.UID) {
	t.Helper()
	_, blocked, err := runRecoveryOn(reconciler, pvName, expectExportUID)
	require.NoError(t, err)
	require.Nil(t, blocked)
}

// TestRecovery_RebindHonoursRecordedIdentity covers the one mutation that cannot be undone: giving
// the volume back. The claim is looked up by name, so when the PV recorded which claim it was taken from,
// that record decides. A same-named claim with a different UID is a different volume owner, and handing
// it a stranger's data is worse than leaving cleanup unfinished for an administrator.
func TestRecovery_RebindHonoursRecordedIdentity(t *testing.T) {
	const pvName = "test-pv"

	newUserPVC := func(uid types.UID) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name: testUserPVCName, Namespace: dataExportNamespace, UID: uid, ResourceVersion: "1",
				Annotations: map[string]string{DataExportInProgressKey: "true"},
				Finalizers:  []string{dev1alpha1.StorageManagerFinalizerName},
			},
			Spec:   corev1.PersistentVolumeClaimSpec{VolumeName: pvName},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
		}
	}
	// The PV is still held by the export claim, so recovery has to rebind it.
	newPV := func(annotations map[string]string) *corev1.PersistentVolume {
		return &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{
				Name: pvName, ResourceVersion: "1", UID: testPVUID,
				Annotations: annotations,
				Labels:      map[string]string{dev1alpha1.LabelPVDataExporter: "true"},
			},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
				ClaimRef: &corev1.ObjectReference{
					Name: testNames.ExportPVCName, Namespace: "test-namespace", UID: testExportPVCUID,
				},
			},
			Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound},
		}
	}

	t.Run("recorded identity matches the live claim - rebinds", func(t *testing.T) {
		pv := newPV(withUIDAnnotations(testDataExportUID, testUserPVCUID))
		userPVC := newUserPVC(testUserPVCUID)

		fakeClient := newFakeClientWithStatus(t, setupTestScheme(), pv, userPVC)
		reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

		requireRecoveryReturnsVolume(t, reconciler, pv.Name, testDataExportUID)

		updatedPV := &corev1.PersistentVolume{}
		require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: pvName}, updatedPV))
		require.NotNil(t, updatedPV.Spec.ClaimRef)
		assert.Equal(t, testUserPVCName, updatedPV.Spec.ClaimRef.Name)
		assertPVExportMetadataRemoved(t, updatedPV)
	})

	t.Run("source claim was recreated - refuses to rebind", func(t *testing.T) {
		pv := newPV(withUIDAnnotations(testDataExportUID, "the-original-claim-uid"))
		userPVC := newUserPVC("a-recreated-claim-uid")

		fakeClient := newFakeClientWithStatus(t, setupTestScheme(), pv, userPVC)
		reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

		_, blocked, err := runRecoveryOn(reconciler, pv.Name, testDataExportUID)
		require.NoError(t, err)
		require.NotNil(t, blocked, "a claim under the same name is a different owner, and the volume waits")
		assert.Equal(t, "B4", blocked.Name)

		updatedPV := &corev1.PersistentVolume{}
		require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: pvName}, updatedPV))
		assert.Equal(t, testNames.ExportPVCName, updatedPV.Spec.ClaimRef.Name, "the volume must not change hands")
		assert.Contains(t, updatedPV.Labels, dev1alpha1.LabelPVDataExporter, "cleanup stays unfinished and retriable")

		updatedUserPVC := &corev1.PersistentVolumeClaim{}
		require.NoError(t, fakeClient.Get(context.Background(),
			types.NamespacedName{Name: testUserPVCName, Namespace: dataExportNamespace}, updatedUserPVC))
		assert.Contains(t, updatedUserPVC.Finalizers, dev1alpha1.StorageManagerFinalizerName,
			"the stranger's claim is left exactly as found")
	})

	t.Run("identity names another export - refuses to rebind", func(t *testing.T) {
		pv := newPV(withUIDAnnotations("some-other-export-uid", testUserPVCUID))
		userPVC := newUserPVC(testUserPVCUID)

		fakeClient := newFakeClientWithStatus(t, setupTestScheme(), pv, userPVC)
		reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

		_, _, err := runRecoveryOn(reconciler, pv.Name, testDataExportUID)
		require.ErrorIs(t, err, ErrPVConflict,
			"a matching claim UID does not authorise an export to undo somebody else's takeover")

		updatedPV := &corev1.PersistentVolume{}
		require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: pvName}, updatedPV))
		assert.Equal(t, testNames.ExportPVCName, updatedPV.Spec.ClaimRef.Name)
	})

	t.Run("orphan sweep cannot judge the export UID and does not pretend to", func(t *testing.T) {
		// The parent is deleted, so nothing can be compared against; the sweep matches PVs by
		// namespace/name and marker instead, and the source claim UID stays enforced.
		pv := newPV(withUIDAnnotations("an-unverifiable-export-uid", testUserPVCUID))
		userPVC := newUserPVC(testUserPVCUID)

		fakeClient := newFakeClientWithStatus(t, setupTestScheme(), pv, userPVC)
		reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

		requireRecoveryReturnsVolume(t, reconciler, pv.Name, "")

		updatedPV := &corev1.PersistentVolume{}
		require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: pvName}, updatedPV))
		assert.Equal(t, testUserPVCName, updatedPV.Spec.ClaimRef.Name)
	})

	t.Run("half an identity survived - refuses to rebind", func(t *testing.T) {
		annotations := makeFullAnnotations()
		annotations[dev1alpha1.AnnotationDataExportUIDKey] = string(testDataExportUID)
		pv := newPV(annotations)
		userPVC := newUserPVC(testUserPVCUID)

		fakeClient := newFakeClientWithStatus(t, setupTestScheme(), pv, userPVC)
		reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

		_, _, err := runRecoveryOn(reconciler, pv.Name, testDataExportUID)
		require.ErrorIs(t, err, ErrPVConflict,
			"a partially written identity is corruption, not a pre-UID takeover, and must not fall back to the name")

		updatedPV := &corev1.PersistentVolume{}
		require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: pvName}, updatedPV))
		assert.Equal(t, testNames.ExportPVCName, updatedPV.Spec.ClaimRef.Name)
	})

	t.Run("legacy takeover without a recorded identity - keeps rebinding by name", func(t *testing.T) {
		pv := newPV(makeFullAnnotations())
		userPVC := newUserPVC("whatever-uid")

		fakeClient := newFakeClientWithStatus(t, setupTestScheme(), pv, userPVC)
		reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

		requireRecoveryReturnsVolume(t, reconciler, pv.Name, testDataExportUID)

		updatedPV := &corev1.PersistentVolume{}
		require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: pvName}, updatedPV))
		assert.Equal(t, testUserPVCName, updatedPV.Spec.ClaimRef.Name,
			"a pre-UID export has no stronger evidence than the name, and stranding it would be worse")
	})
}

// TestRemoveOrphanResources_StopsBeforeRebindingToARecreatedClaim exercises the same rule from the sweep
// side, where the parent is already gone. The infrastructure is still torn down; only the irreversible
// step is withheld, and the PV stays retained and labelled so the attempt is repeated.
func TestRemoveOrphanResources_StopsBeforeRebindingToARecreatedClaim(t *testing.T) {
	const pvName = "test-pv"

	annotations := withUIDAnnotations(testDataExportUID, "the-original-claim-uid")
	annotations[dev1alpha1.AnnotationUserPVCNamespaceKey] = dataExportNamespace
	annotations[dev1alpha1.AnnotationUserPVCNameKey] = testUserPVCName
	annotations[dev1alpha1.AnnotationStorageManagerNamespaceKey] = dataExportNamespace
	annotations[dev1alpha1.AnnotationStorageManagerNameKey] = dataExportName

	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvName, ResourceVersion: "1", UID: testPVUID,
			Annotations: annotations,
			Labels:      map[string]string{dev1alpha1.LabelPVDataExporter: "true"},
		},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			ClaimRef: &corev1.ObjectReference{
				Name: testNames.ExportPVCName, Namespace: "test-namespace", UID: testExportPVCUID,
			},
		},
	}
	recreatedClaim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: testUserPVCName, Namespace: dataExportNamespace, UID: "a-recreated-claim-uid", ResourceVersion: "1",
			Finalizers: []string{dev1alpha1.StorageManagerFinalizerName},
		},
		Spec:   corev1.PersistentVolumeClaimSpec{VolumeName: pvName},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}

	fakeClient := newFakeClientWithStatus(t, setupTestScheme(), pv, recreatedClaim)
	reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

	blocked, err := reconciler.removeOrphanResources(context.Background(), dataExportNamespace, dataExportName)
	require.NoError(t, err)
	require.NotNil(t, blocked)
	assert.Equal(t, "B4", blocked.Name)

	updatedPV := &corev1.PersistentVolume{}
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: pvName}, updatedPV))
	assert.Equal(t, testNames.ExportPVCName, updatedPV.Spec.ClaimRef.Name)
	assert.Equal(t, corev1.PersistentVolumeReclaimRetain, updatedPV.Spec.PersistentVolumeReclaimPolicy,
		"the volume must stay protected while cleanup is unfinished")
	assert.Contains(t, updatedPV.Labels, dev1alpha1.LabelPVDataExporter)
}

// TestRemoveOrphanResources_SnapshotBasedTargetOnlyCleansTheVolume: a snapshot export takes nothing from
// anybody, so there is no binding to restore — only the marks and the reclaim policy we changed.
func TestRemoveOrphanResources_SnapshotBasedTargetOnlyCleansTheVolume(t *testing.T) {
	// A snapshot export detaches nobody, so it records no claim name; everything else is as the takeover
	// left it.
	annotations := makeFullAnnotations()
	delete(annotations, dev1alpha1.AnnotationUserPVCNameKey)

	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test-pv",
			ResourceVersion: "1",
			Annotations:     annotations,
			Labels:          map[string]string{dev1alpha1.LabelPVDataExporter: "true"},
		},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
		},
	}

	fakeClient := newFakeClientWithStatus(t, setupTestScheme(), pv)
	reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

	blocked, err := reconciler.removeOrphanResources(context.Background(), dataExportNamespace, dataExportName)
	require.NoError(t, err)
	require.Nil(t, blocked)

	updatedPV := &corev1.PersistentVolume{}
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-pv"}, updatedPV))
	assertPVExportMetadataRemoved(t, updatedPV)
	assert.Equal(t, corev1.PersistentVolumeReclaimDelete, updatedPV.Spec.PersistentVolumeReclaimPolicy)
}

// TestStopExportConsumers_OnlyDeletesOurDeployment: the Deployment name is generated, so an object under
// it is only ours if it says so. Deleting somebody else's workload because it collided with our naming is
// not a cleanup, and it is the sweep — running without a parent to check against — that is most exposed.
func TestStopExportConsumers_OnlyDeletesOurDeployment(t *testing.T) {
	deployWithLabels := func(labels map[string]string) *appsv1.Deployment {
		return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name: testNames.DeployName, Namespace: testExportPVCNamespace, Labels: labels,
		}}
	}

	tests := []struct {
		name        string
		existing    *appsv1.Deployment
		wantErr     string
		wantDeleted bool
	}{
		{
			name:        "ours - deleted",
			existing:    deployWithLabels(map[string]string{dev1alpha1.LabelApplicationKey: dev1alpha1.LabelDataExportValue}),
			wantDeleted: true,
		},
		{
			name: "nothing there - nothing to stop",
		},
		{
			name:     "no app label - refused",
			existing: deployWithLabels(nil),
			wantErr:  "not managed by data-exporter",
		},
		{
			name:     "somebody else's app label - refused",
			existing: deployWithLabels(map[string]string{dev1alpha1.LabelApplicationKey: "something-else"}),
			wantErr:  "not managed by data-exporter",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(setupTestScheme())
			if tt.existing != nil {
				builder = builder.WithObjects(tt.existing)
			}
			fakeClient := builder.Build()
			reconciler := createTestReconciler(fakeClient, fakeClient, createTestConfig())

			blocked, err := reconciler.stopExportConsumers(context.Background(), testNames, takeoverRef{})

			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				require.NoError(t, fakeClient.Get(context.Background(),
					types.NamespacedName{Name: testNames.DeployName, Namespace: testExportPVCNamespace}, &appsv1.Deployment{}),
					"a workload we do not own is left exactly as found")
				return
			}

			require.NoError(t, err)
			assert.Nil(t, blocked)
			if tt.wantDeleted {
				err = fakeClient.Get(context.Background(),
					types.NamespacedName{Name: testNames.DeployName, Namespace: testExportPVCNamespace}, &appsv1.Deployment{})
				assert.True(t, client.IgnoreNotFound(err) == nil, "deployment should be deleted")
			}
		})
	}
}
