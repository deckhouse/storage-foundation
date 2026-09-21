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

package dataimport

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/common/config"
)

// createPVCFlowControllerNamespace is the ControllerNamespace every fixture in this file uses for the
// internal scratch PVC and the importer Deployment; the CreatePVC target PVC always lives in the
// DataImport's own namespace ("ns", see createPVCImport/boundImportPVC), never here.
const createPVCFlowControllerNamespace = "d8"

// newCreatePVCFlowReconciler builds a DataImportReconciler wired the same way cmd/main.go wires it, so
// tests can drive the real Reconcile end to end for the CreatePVC pipeline. Unlike
// newPopulateDataFlowReconciler this needs no dynamic client: the CreatePVC pipeline never touches
// VolumeCaptureRequest/ObjectKeeper, and none of these tests drive the DataImport's own deletion branch
// (the only caller of the dynamic client on this path, deleteVolumeCaptureRequest).
func newCreatePVCFlowReconciler(
	t *testing.T,
	objs []client.Object,
	interceptorFuncs ...interceptor.Funcs,
) (*DataImportReconciler, client.Client) {
	t.Helper()

	builder := fake.NewClientBuilder().WithScheme(reconcileScheme(t))
	for _, o := range objs {
		if di, ok := o.(*dev1alpha1.DataImport); ok {
			builder = builder.WithStatusSubresource(di)
		}
	}
	builder = builder.WithObjects(objs...)
	for _, f := range interceptorFuncs {
		builder = builder.WithInterceptorFuncs(f)
	}
	c := builder.Build()

	r := &DataImportReconciler{
		Client: c,
		Reader: c,
		Config: &config.Options{ControllerNamespace: createPVCFlowControllerNamespace},
		Now:    func() metav1.Time { return fixedNow },
	}
	return r, c
}

// createPVCFlowTestImmediateStorageClass builds an Immediate-binding StorageClass, the counterpart to
// wffcStorageClass (populate_data_flow_test.go) for the CreatePVC node-hint gate's non-WFC rows.
func createPVCFlowTestImmediateStorageClass(name string) *storagev1.StorageClass {
	mode := storagev1.VolumeBindingImmediate
	return &storagev1.StorageClass{
		ObjectMeta:        metav1.ObjectMeta{Name: name},
		Provisioner:       "csi.example.com",
		VolumeBindingMode: &mode,
	}
}

// createPVCFlowTestTargetPVC builds a live CreatePVC target PVC owned by di, in di's own namespace, shaped
// the way ensureImportPVC's Create response (or a subsequent cached Get) would return it: a real
// StorageClass name, a defaulted VolumeMode, the storage-manager annotations and our finalizer, Pending.
// mutate lets each test shape the one field it needs (storage class, phase, bound volume name, deletion
// timestamp, ...).
func createPVCFlowTestTargetPVC(di *dev1alpha1.DataImport, mutate func(*corev1.PersistentVolumeClaim)) *corev1.PersistentVolumeClaim {
	name := di.Spec.PvcTemplate.Name
	sc := "immediate"
	fs := corev1.PersistentVolumeFilesystem
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: di.Namespace,
			UID:       types.UID(name + "-uid"),
			Annotations: map[string]string{
				dev1alpha1.AnnotationStorageManagerNamespaceKey: di.Namespace,
				dev1alpha1.AnnotationStorageManagerNameKey:      di.Name,
			},
			Finalizers: []string{dev1alpha1.StorageManagerFinalizerName},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
			},
			StorageClassName: &sc,
			VolumeMode:       &fs,
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
	if mutate != nil {
		mutate(pvc)
	}
	return pvc
}

// TestScratchPVCIsShapedFromTheLiveTargetPVC is the source-of-truth guard for the CreatePVC upload phase:
// the scratch PVC must be shaped from the live target PVC the apiserver actually returned, never from
// spec.pvcTemplate, because the template can omit fields the apiserver defaults and can drift from what
// the target ends up with.
func TestScratchPVCIsShapedFromTheLiveTargetPVC(t *testing.T) {
	t.Parallel()

	t.Run("success: apiserver defaults on the target win over an incomplete template", func(t *testing.T) {
		t.Parallel()

		// The DataImport template (via createPVCImport) sets only metadata.name -- no volumeMode, no
		// storageClassName -- exactly what crds/dataimports.yaml permits. The live target PVC below
		// stands in for what the apiserver already defaulted onto it.
		di := createPVCImport("shape-imp-1", "")
		targetPVC := createPVCFlowTestTargetPVC(di, nil) // StorageClassName="immediate", VolumeMode=Filesystem
		sc := createPVCFlowTestImmediateStorageClass("immediate")

		objs := []client.Object{di, targetPVC, sc, exporterImageConfigMap(createPVCFlowControllerNamespace)}
		r, c := newCreatePVCFlowReconciler(t, objs, pendingOnPVCCreateInterceptor())

		_, err := r.Reconcile(context.Background(), diReq(di))
		require.NoError(t, err)

		scratch := &corev1.PersistentVolumeClaim{}
		require.NoError(t, c.Get(context.Background(),
			types.NamespacedName{Namespace: createPVCFlowControllerNamespace, Name: r.names.ImportScratchPVCName}, scratch))
		require.NotNil(t, scratch.Spec.VolumeMode)
		assert.Equal(t, corev1.PersistentVolumeFilesystem, *scratch.Spec.VolumeMode)
		require.NotNil(t, scratch.Spec.StorageClassName)
		assert.Equal(t, "immediate", *scratch.Spec.StorageClassName)
	})

	t.Run("success: a template that disagrees with the live target does not shape the scratch", func(t *testing.T) {
		t.Parallel()

		di := createPVCImport("shape-imp-2", "")
		di.Spec.PvcTemplate.Resources = dev1alpha1.VolumeResourceRequirements{
			Requests: dev1alpha1.ResourceList{dev1alpha1.ResourceStorage: resource.MustParse("20Gi")},
		}
		// The live target PVC (already created, as if by a prior reconcile) requests only 10Gi -- the
		// template disagrees, and the template must lose.
		targetPVC := createPVCFlowTestTargetPVC(di, nil)
		sc := createPVCFlowTestImmediateStorageClass("immediate")

		objs := []client.Object{di, targetPVC, sc, exporterImageConfigMap(createPVCFlowControllerNamespace)}
		r, c := newCreatePVCFlowReconciler(t, objs, pendingOnPVCCreateInterceptor())

		_, err := r.Reconcile(context.Background(), diReq(di))
		require.NoError(t, err)

		scratch := &corev1.PersistentVolumeClaim{}
		require.NoError(t, c.Get(context.Background(),
			types.NamespacedName{Namespace: createPVCFlowControllerNamespace, Name: r.names.ImportScratchPVCName}, scratch))
		gotQty := scratch.Spec.Resources.Requests[corev1.ResourceStorage]
		assert.Equal(t, "10Gi", gotQty.String(), "the scratch PVC must request what the live target PVC requests, not what the template says")
	})
}

// TestReconcileCreatePVCWaitsForDefaultStorageClass is the guard on the CreatePVC class gate: a nil
// storageClassName waits (the class may still be assigned retroactively) without ever attempting a
// StorageClass Get, while an empty string or an unresolvable name are retryable lookup failures that DO
// attempt the Get -- so an empty string can never silently collapse into the nil wait.
func TestReconcileCreatePVCWaitsForDefaultStorageClass(t *testing.T) {
	t.Parallel()

	classGetCounter := func() (*int, interceptor.Funcs) {
		count := 0
		return &count, interceptor.Funcs{
			Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if _, ok := obj.(*storagev1.StorageClass); ok {
					count++
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}
	}

	t.Run("success: nil class waits and creates nothing", func(t *testing.T) {
		t.Parallel()

		di := createPVCImport("class-gate-nil", "")
		targetPVC := createPVCFlowTestTargetPVC(di, func(pvc *corev1.PersistentVolumeClaim) {
			pvc.Spec.StorageClassName = nil
		})

		count, countingInterceptor := classGetCounter()
		objs := []client.Object{di, targetPVC, exporterImageConfigMap(createPVCFlowControllerNamespace)}
		r, c := newCreatePVCFlowReconciler(t, objs, countingInterceptor)

		res, err := r.Reconcile(context.Background(), diReq(di))
		require.NoError(t, err)
		assert.Equal(t, dataImportRequeueInterval, res.RequeueAfter)
		assert.Zero(t, *count, "the class gate must never attempt a StorageClass Get for a nil class")

		got := &dev1alpha1.DataImport{}
		require.NoError(t, c.Get(context.Background(), diReq(di).NamespacedName, got))
		assert.Equal(t, string(common.PhasePending), got.Status.Phase)
		ready := meta.FindStatusCondition(got.Status.Conditions, string(common.ConditionReady))
		require.NotNil(t, ready)
		assert.Equal(t, string(common.ReasonPVCCreated), ready.Reason)

		scratchList := &corev1.PersistentVolumeClaimList{}
		require.NoError(t, c.List(context.Background(), scratchList, client.InNamespace(createPVCFlowControllerNamespace)))
		assert.Empty(t, scratchList.Items, "no scratch PVC must be created while waiting for a default class")

		deployList := &appsv1.DeploymentList{}
		require.NoError(t, c.List(context.Background(), deployList))
		assert.Empty(t, deployList.Items)
	})

	t.Run("success: a class assigned later opens the gate", func(t *testing.T) {
		t.Parallel()

		di := createPVCImport("class-gate-later", "")
		targetPVC := createPVCFlowTestTargetPVC(di, func(pvc *corev1.PersistentVolumeClaim) {
			pvc.Spec.StorageClassName = nil
		})

		objs := []client.Object{di, targetPVC, exporterImageConfigMap(createPVCFlowControllerNamespace)}
		r, c := newCreatePVCFlowReconciler(t, objs, pendingOnPVCCreateInterceptor())

		res, err := r.Reconcile(context.Background(), diReq(di))
		require.NoError(t, err)
		assert.Equal(t, dataImportRequeueInterval, res.RequeueAfter)

		// Model the PV controller's retroactive default-class assignment landing on the live target PVC,
		// and the class object appearing.
		live := &corev1.PersistentVolumeClaim{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: targetPVC.Name}, live))
		className := "immediate"
		live.Spec.StorageClassName = &className
		require.NoError(t, c.Update(context.Background(), live))
		require.NoError(t, c.Create(context.Background(), createPVCFlowTestImmediateStorageClass("immediate")))

		_, err = r.Reconcile(context.Background(), diReq(di))
		require.NoError(t, err)

		scratch := &corev1.PersistentVolumeClaim{}
		require.NoError(t, c.Get(context.Background(),
			types.NamespacedName{Namespace: createPVCFlowControllerNamespace, Name: r.names.ImportScratchPVCName}, scratch))
		require.NotNil(t, scratch.Spec.StorageClassName)
		assert.Equal(t, "immediate", *scratch.Spec.StorageClassName)
	})

	t.Run("error: empty class name is a retryable lookup failure, not a wait", func(t *testing.T) {
		t.Parallel()

		di := createPVCImport("class-gate-empty", "")
		targetPVC := createPVCFlowTestTargetPVC(di, func(pvc *corev1.PersistentVolumeClaim) {
			empty := ""
			pvc.Spec.StorageClassName = &empty
		})

		count, countingInterceptor := classGetCounter()
		objs := []client.Object{di, targetPVC, exporterImageConfigMap(createPVCFlowControllerNamespace)}
		r, c := newCreatePVCFlowReconciler(t, objs, countingInterceptor)

		_, err := r.Reconcile(context.Background(), diReq(di))
		require.Error(t, err)
		assert.Greater(t, *count, 0, "an empty class name must actually attempt the StorageClass Get, not silently wait like a nil class")

		got := &dev1alpha1.DataImport{}
		require.NoError(t, c.Get(context.Background(), diReq(di).NamespacedName, got))
		assert.NotEqual(t, string(common.PhaseFailed), got.Status.Phase, "an empty class name is retryable, not terminal, pinning today's main behaviour")
		ready := meta.FindStatusCondition(got.Status.Conditions, string(common.ConditionReady))
		require.NotNil(t, ready)
		assert.Equal(t, string(common.ReasonTargetFailed), ready.Reason)

		scratchList := &corev1.PersistentVolumeClaimList{}
		require.NoError(t, c.List(context.Background(), scratchList, client.InNamespace(createPVCFlowControllerNamespace)))
		assert.Empty(t, scratchList.Items)
	})

	t.Run("error: a named class that does not exist is retryable", func(t *testing.T) {
		t.Parallel()

		di := createPVCImport("class-gate-ghost", "")
		targetPVC := createPVCFlowTestTargetPVC(di, func(pvc *corev1.PersistentVolumeClaim) {
			ghost := "ghost"
			pvc.Spec.StorageClassName = &ghost
		})

		objs := []client.Object{di, targetPVC, exporterImageConfigMap(createPVCFlowControllerNamespace)}
		r, c := newCreatePVCFlowReconciler(t, objs)

		_, err := r.Reconcile(context.Background(), diReq(di))
		require.Error(t, err)

		got := &dev1alpha1.DataImport{}
		require.NoError(t, c.Get(context.Background(), diReq(di).NamespacedName, got))
		assert.NotEqual(t, string(common.PhaseFailed), got.Status.Phase)
		ready := meta.FindStatusCondition(got.Status.Conditions, string(common.ConditionReady))
		require.NotNil(t, ready)
		assert.Equal(t, string(common.ReasonTargetFailed), ready.Reason)

		scratchList := &corev1.PersistentVolumeClaimList{}
		require.NoError(t, c.List(context.Background(), scratchList, client.InNamespace(createPVCFlowControllerNamespace)))
		assert.Empty(t, scratchList.Items)
	})
}

// TestScratchPVCCarriesSelectedNodeOfTheTargetPVC guards the node-hint gate end to end: with a
// WaitForFirstConsumer class, waitForFirstConsumer=true and a selected-node annotation already on the
// target PVC, the scratch PVC is created carrying the same hint, and a later change to the target's own
// annotation is never re-patched onto the (by then possibly already scheduled or bound) scratch PVC.
func TestScratchPVCCarriesSelectedNodeOfTheTargetPVC(t *testing.T) {
	t.Parallel()

	di := createPVCImport("node-hint-imp", "")
	di.Spec.WaitForFirstConsumer = true
	targetPVC := createPVCFlowTestTargetPVC(di, func(pvc *corev1.PersistentVolumeClaim) {
		sc := "wffc"
		pvc.Spec.StorageClassName = &sc
		pvc.Annotations[annSelectedNode] = "node-a"
	})
	sc := wffcStorageClass("wffc")

	objs := []client.Object{di, targetPVC, sc, exporterImageConfigMap(createPVCFlowControllerNamespace)}
	r, c := newCreatePVCFlowReconciler(t, objs, pendingOnPVCCreateInterceptor())

	_, err := r.Reconcile(context.Background(), diReq(di))
	require.NoError(t, err)

	scratch := &corev1.PersistentVolumeClaim{}
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: createPVCFlowControllerNamespace, Name: r.names.ImportScratchPVCName}, scratch))
	assert.Equal(t, "node-a", scratch.Annotations[annSelectedNode])

	// The scheduler may rewrite the target's own annotation later (e.g. after a reschedule); the scratch
	// PVC's hint, stamped only at creation, must not follow it.
	liveTarget := &corev1.PersistentVolumeClaim{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: targetPVC.Name}, liveTarget))
	liveTarget.Annotations[annSelectedNode] = "node-b"
	require.NoError(t, c.Update(context.Background(), liveTarget))

	_, err = r.Reconcile(context.Background(), diReq(di))
	require.NoError(t, err)

	after := &corev1.PersistentVolumeClaim{}
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Namespace: createPVCFlowControllerNamespace, Name: r.names.ImportScratchPVCName}, after))
	assert.Equal(t, "node-a", after.Annotations[annSelectedNode],
		"the scratch PVC's node hint must never be re-patched from a later target-PVC annotation change")
}

// createPVCFlowTestHandoverFixture builds the object graph the refuse-handover-when-bound-elsewhere
// guard needs: a finished-upload CreatePVC import whose scratch PVC is Bound to a
// PersistentVolume, with a target PVC that already exists. mutateTarget/mutatePV let each subtest
// shape the one field under test; every other field is fixed so the fixture always reaches
// reconcileHandover.
func createPVCFlowTestHandoverFixture(
	t *testing.T,
	name string,
	mutateTarget func(*corev1.PersistentVolumeClaim),
	mutatePV func(*corev1.PersistentVolume),
) (r *DataImportReconciler, c client.Client, di *dev1alpha1.DataImport, targetPVC *corev1.PersistentVolumeClaim, scratchPVC *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume) {
	t.Helper()

	di = createPVCImport(name, common.ServerStateFinished)
	targetPVC = createPVCFlowTestTargetPVC(di, func(pvc *corev1.PersistentVolumeClaim) {
		pvc.Status.Phase = corev1.ClaimBound
		if mutateTarget != nil {
			mutateTarget(pvc)
		}
	})

	names := common.NewNames(dev1alpha1.KindPVC, di.Name, di.Namespace, di.Name)
	scratchPVC = boundInternalScratchPVC(names.ImportScratchPVCName, createPVCFlowControllerNamespace)
	scratchPVC.UID = "scratch-uid"
	scratchPVC.Spec.VolumeName = "handover-pv"

	pv = &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "handover-pv"},
		Spec: corev1.PersistentVolumeSpec{
			ClaimRef: &corev1.ObjectReference{
				APIVersion: "v1", Kind: "PersistentVolumeClaim",
				Namespace: scratchPVC.Namespace, Name: scratchPVC.Name, UID: scratchPVC.UID,
			},
		},
	}
	if mutatePV != nil {
		mutatePV(pv)
	}

	objs := []client.Object{di, targetPVC, scratchPVC, pv, exporterImageConfigMap(createPVCFlowControllerNamespace)}
	r, c = newCreatePVCFlowReconciler(t, objs)
	return r, c, di, targetPVC, scratchPVC, pv
}

// TestReconcileCreatePVCRefusesHandoverWhenTargetIsBoundElsewhere guards the precondition that must
// hold before the claimRef handover patch is applied: patching a claimRef onto a PersistentVolume
// whose claim is already bound to a DIFFERENT volume makes the PV controller reclaim (and
// CSI-delete) that volume. The assertion that matters is not the returned error -- it is that the
// handover PV was never written at all.
func TestReconcileCreatePVCRefusesHandoverWhenTargetIsBoundElsewhere(t *testing.T) {
	t.Parallel()

	t.Run("error: target PVC already bound to a different PersistentVolume", func(t *testing.T) {
		t.Parallel()

		r, c, di, _, scratchPVC, pv := createPVCFlowTestHandoverFixture(t, "handover-elsewhere",
			func(pvc *corev1.PersistentVolumeClaim) { pvc.Spec.VolumeName = "someone-elses-pv" },
			nil)

		before := &corev1.PersistentVolume{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: pv.Name}, before))

		_, err := r.Reconcile(context.Background(), diReq(di))
		require.NoError(t, err, "a terminal reconcile error is recorded as phase=Failed, not surfaced as an error-requeue")

		got := &dev1alpha1.DataImport{}
		require.NoError(t, c.Get(context.Background(), diReq(di).NamespacedName, got))
		assert.Equal(t, string(common.PhaseFailed), got.Status.Phase)

		after := &corev1.PersistentVolume{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: pv.Name}, after))
		assert.Equal(t, before.ResourceVersion, after.ResourceVersion,
			"the handover PV must never be written when the target PVC is already bound to a different volume")
		assert.Equal(t, before.Spec.ClaimRef, after.Spec.ClaimRef)

		require.NoError(t, c.Get(context.Background(),
			types.NamespacedName{Namespace: scratchPVC.Namespace, Name: scratchPVC.Name}, &corev1.PersistentVolumeClaim{}),
			"the scratch PVC must be left standing on a refused handover")
	})

	t.Run("success: an interrupted handover with spec.volumeName already naming this PersistentVolume proceeds idempotently", func(t *testing.T) {
		t.Parallel()

		r, c, di, targetPVC, _, pv := createPVCFlowTestHandoverFixture(t, "handover-repeat",
			func(pvc *corev1.PersistentVolumeClaim) { pvc.Spec.VolumeName = "handover-pv" },
			nil)

		_, err := r.Reconcile(context.Background(), diReq(di))
		require.NoError(t, err)

		after := &corev1.PersistentVolume{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: pv.Name}, after))
		require.NotNil(t, after.Spec.ClaimRef)
		assert.Equal(t, targetPVC.Namespace, after.Spec.ClaimRef.Namespace)
		assert.Equal(t, targetPVC.Name, after.Spec.ClaimRef.Name)
	})

	t.Run("error: a PV claimed by a third party is never patched", func(t *testing.T) {
		t.Parallel()

		r, c, di, targetPVC, scratchPVC, pv := createPVCFlowTestHandoverFixture(t, "handover-thirdparty",
			nil,
			func(pv *corev1.PersistentVolume) {
				pv.Spec.ClaimRef = &corev1.ObjectReference{Namespace: "other-ns", Name: "other-pvc"}
			})

		before := &corev1.PersistentVolume{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: pv.Name}, before))

		_, err := r.Reconcile(context.Background(), diReq(di))
		require.NoError(t, err)

		got := &dev1alpha1.DataImport{}
		require.NoError(t, c.Get(context.Background(), diReq(di).NamespacedName, got))
		assert.Equal(t, string(common.PhaseFailed), got.Status.Phase)
		ready := meta.FindStatusCondition(got.Status.Conditions, string(common.ConditionReady))
		require.NotNil(t, ready)
		assert.Contains(t, ready.Message, pv.Name)
		assert.Contains(t, ready.Message, scratchPVC.Name)
		assert.Contains(t, ready.Message, targetPVC.Name)

		after := &corev1.PersistentVolume{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: pv.Name}, after))
		assert.Equal(t, before.ResourceVersion, after.ResourceVersion)
		assert.Equal(t, before.Spec.ClaimRef, after.Spec.ClaimRef)
	})

	t.Run("error: a PV with no claimRef is never patched", func(t *testing.T) {
		t.Parallel()

		r, c, di, _, _, pv := createPVCFlowTestHandoverFixture(t, "handover-noclaim",
			nil,
			func(pv *corev1.PersistentVolume) { pv.Spec.ClaimRef = nil })

		before := &corev1.PersistentVolume{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: pv.Name}, before))

		_, err := r.Reconcile(context.Background(), diReq(di))
		require.NoError(t, err)

		got := &dev1alpha1.DataImport{}
		require.NoError(t, c.Get(context.Background(), diReq(di).NamespacedName, got))
		assert.Equal(t, string(common.PhaseFailed), got.Status.Phase)

		after := &corev1.PersistentVolume{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: pv.Name}, after))
		assert.Equal(t, before.ResourceVersion, after.ResourceVersion)
		assert.Nil(t, after.Spec.ClaimRef)
	})
}

// TestReconcileCreatePVCDoesNotCompleteWithoutUploadFinished guards the UploadFinished term on the
// "target Bound, scratch gone" completion row: a target PVC that carries leftover storage-manager
// annotations from a prior, already garbage-collected DataImport of the same name (cleanupDataImport only
// ever strips StorageManagerFinalizerName, never those annotations) passes the ownership guard and looks
// exactly like a finished handover -- but THIS DataImport never finished an upload. Unlike
// TestReconcile_CreatePVC_TranslatesFinishedAndCompletes, this fixture seeds neither UploadFinished nor
// serverState=Finished, so it cannot be satisfied by that test's "the pod already reported it finished"
// state.
func TestReconcileCreatePVCDoesNotCompleteWithoutUploadFinished(t *testing.T) {
	t.Parallel()

	di := createPVCImport("stale-annotations-imp", "")
	targetPVC := boundImportPVC(di.Name)

	objs := []client.Object{di, targetPVC, exporterImageConfigMap(createPVCFlowControllerNamespace)}
	r, c := newCreatePVCFlowReconciler(t, objs)

	_, err := r.Reconcile(context.Background(), diReq(di))
	require.NoError(t, err, "a terminal reconcile error is recorded as phase=Failed, not surfaced as an error-requeue")

	got := &dev1alpha1.DataImport{}
	require.NoError(t, c.Get(context.Background(), diReq(di).NamespacedName, got))
	assert.Equal(t, string(common.PhaseFailed), got.Status.Phase)
	assert.False(t, meta.IsStatusConditionTrue(got.Status.Conditions, string(common.ConditionCompleted)),
		"a Bound target PVC this import never uploaded into must never be reported Completed")
}

// TestEnsureTargetPVCIssuesNoWriteWhenAlreadyOurs guards the fix that keeps the CreatePVC target-PVC path
// from ever reaching handleExistingPVC on a PVC that is already ours: needsPVCSpecUpdate compares the raw
// (possibly incomplete) template against the live, apiserver-defaulted PVC and reports a spurious diff on
// every reconcile, and once the PVC is Bound the resulting Update is rejected outright ("spec is immutable
// after creation"). Without the fix this test fails on the write count even before the PVC binds, and its
// Bound variant fails outright once it does.
func TestEnsureTargetPVCIssuesNoWriteWhenAlreadyOurs(t *testing.T) {
	t.Parallel()

	countPVCUpdates := func() (*int, interceptor.Funcs) {
		count := 0
		return &count, interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
					count++
				}
				return cl.Update(ctx, obj, opts...)
			},
		}
	}

	t.Run("success: a template omitting volumeMode issues no write against a defaulted, Bound target PVC", func(t *testing.T) {
		t.Parallel()

		di := createPVCImport("no-write-volumemode", "") // PvcTemplate omits VolumeMode
		targetPVC := createPVCFlowTestTargetPVC(di, func(pvc *corev1.PersistentVolumeClaim) {
			pvc.Status.Phase = corev1.ClaimBound
			pvc.Spec.VolumeName = "bound-pv"
		})

		count, countingInterceptor := countPVCUpdates()
		objs := []client.Object{di, targetPVC, exporterImageConfigMap(createPVCFlowControllerNamespace)}
		r, _ := newCreatePVCFlowReconciler(t, objs, countingInterceptor)

		_, err := r.Reconcile(context.Background(), diReq(di))
		require.NoError(t, err)
		assert.Zero(t, *count, "a live target PVC that is already ours must never be routed through handleExistingPVC's spec update")
	})

	t.Run("success: a template omitting storageClassName issues no write against a target PVC carrying the default class", func(t *testing.T) {
		t.Parallel()

		di := createPVCImport("no-write-class", "") // PvcTemplate omits StorageClassName
		targetPVC := createPVCFlowTestTargetPVC(di, func(pvc *corev1.PersistentVolumeClaim) {
			pvc.Status.Phase = corev1.ClaimBound
			pvc.Spec.VolumeName = "bound-pv"
		})
		defaultClass := createPVCFlowTestImmediateStorageClass(*targetPVC.Spec.StorageClassName)

		count, countingInterceptor := countPVCUpdates()
		objs := []client.Object{di, targetPVC, defaultClass, exporterImageConfigMap(createPVCFlowControllerNamespace)}
		r, _ := newCreatePVCFlowReconciler(t, objs, countingInterceptor)

		_, err := r.Reconcile(context.Background(), diReq(di))
		require.NoError(t, err)
		assert.Zero(t, *count)
	})
}

// TestScratchPVCDeletedOnlyAfterClaimLost guards the ordering rule for the scratch-PVC delete: the
// scratch PVC survives while it is still Bound, and is deleted only once it is BOTH observed
// ClaimLost AND the target PVC is Bound to the handed-over volume -- waiting for ClaimLost alone
// would race the binder and could delete the scratch PVC before the target PVC is confirmed to
// hold the data.
func TestScratchPVCDeletedOnlyAfterClaimLost(t *testing.T) {
	t.Parallel()

	newFixture := func(t *testing.T, name string, scratchPhase, targetPhase corev1.PersistentVolumeClaimPhase) (*DataImportReconciler, client.Client, *dev1alpha1.DataImport, string) {
		t.Helper()

		di := createPVCImport(name, common.ServerStateFinished)
		targetPVC := createPVCFlowTestTargetPVC(di, func(pvc *corev1.PersistentVolumeClaim) {
			pvc.Status.Phase = targetPhase
			pvc.Spec.VolumeName = "handed-over-pv"
		})

		names := common.NewNames(dev1alpha1.KindPVC, di.Name, di.Namespace, di.Name)
		scratchPVC := boundInternalScratchPVC(names.ImportScratchPVCName, createPVCFlowControllerNamespace)
		scratchPVC.Status.Phase = scratchPhase
		scratchPVC.Spec.VolumeName = "handed-over-pv"

		pv := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "handed-over-pv"},
			Spec: corev1.PersistentVolumeSpec{
				ClaimRef: &corev1.ObjectReference{
					APIVersion: "v1", Kind: "PersistentVolumeClaim",
					Namespace: targetPVC.Namespace, Name: targetPVC.Name, UID: targetPVC.UID,
				},
			},
		}

		objs := []client.Object{di, targetPVC, scratchPVC, pv, exporterImageConfigMap(createPVCFlowControllerNamespace)}
		r, c := newCreatePVCFlowReconciler(t, objs)
		return r, c, di, names.ImportScratchPVCName
	}

	t.Run("success: scratch PVC survives while still Bound", func(t *testing.T) {
		t.Parallel()

		r, c, di, scratchName := newFixture(t, "scratch-survives-bound", corev1.ClaimBound, corev1.ClaimBound)

		_, err := r.Reconcile(context.Background(), diReq(di))
		require.NoError(t, err)

		require.NoError(t, c.Get(context.Background(),
			types.NamespacedName{Namespace: createPVCFlowControllerNamespace, Name: scratchName}, &corev1.PersistentVolumeClaim{}),
			"the scratch PVC must survive while it is still Bound")
	})

	t.Run("success: scratch PVC deleted once ClaimLost and the target PVC is Bound", func(t *testing.T) {
		t.Parallel()

		r, c, di, scratchName := newFixture(t, "scratch-deleted", corev1.ClaimLost, corev1.ClaimBound)

		_, err := r.Reconcile(context.Background(), diReq(di))
		require.NoError(t, err)

		getErr := c.Get(context.Background(), types.NamespacedName{Namespace: createPVCFlowControllerNamespace, Name: scratchName}, &corev1.PersistentVolumeClaim{})
		require.Error(t, getErr)
		assert.True(t, apierrors.IsNotFound(getErr))

		got := &dev1alpha1.DataImport{}
		require.NoError(t, c.Get(context.Background(), diReq(di).NamespacedName, got))
		assert.True(t, meta.IsStatusConditionTrue(got.Status.Conditions, string(common.ConditionCompleted)))
	})

	t.Run("error: scratch PVC ClaimLost but the target PVC is still Pending must not be deleted -- ordering guard", func(t *testing.T) {
		t.Parallel()

		r, c, di, scratchName := newFixture(t, "scratch-ordering-guard", corev1.ClaimLost, corev1.ClaimPending)

		_, err := r.Reconcile(context.Background(), diReq(di))
		require.NoError(t, err)

		require.NoError(t, c.Get(context.Background(),
			types.NamespacedName{Namespace: createPVCFlowControllerNamespace, Name: scratchName}, &corev1.PersistentVolumeClaim{}),
			"the scratch PVC must survive until the target PVC itself is observed Bound -- deleting earlier races the binder")
	})
}

// TestReconcileCreatePVCResumesFromEachState is table-driven over the CreatePVC idempotent-resume state
// matrix: every state is re-derived from the live objects on each pass, never from a persisted status
// flag, so each row asserts both the next action and that a repeat reconcile against the same live state
// issues no further writes.
func TestReconcileCreatePVCResumesFromEachState(t *testing.T) {
	t.Parallel()

	type row struct {
		name        string
		buildObjs   func(di *dev1alpha1.DataImport) []client.Object
		wantErr     bool
		wantPhase   common.Phase
		wantRequeue bool
		assertExtra func(t *testing.T, c client.Client, r *DataImportReconciler, di *dev1alpha1.DataImport)
	}

	rows := []row{
		{
			name: "target PVC missing is created, requeue",
			buildObjs: func(_ *dev1alpha1.DataImport) []client.Object {
				return []client.Object{exporterImageConfigMap(createPVCFlowControllerNamespace)}
			},
			wantPhase:   common.PhasePending,
			wantRequeue: true,
			assertExtra: func(t *testing.T, c client.Client, _ *DataImportReconciler, di *dev1alpha1.DataImport) {
				require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: di.Name}, &corev1.PersistentVolumeClaim{}))
			},
		},
		{
			name: "target PVC ours and Pending, scratch missing, WFC class with no selected-node waits",
			buildObjs: func(di *dev1alpha1.DataImport) []client.Object {
				di.Spec.WaitForFirstConsumer = true
				targetPVC := createPVCFlowTestTargetPVC(di, func(pvc *corev1.PersistentVolumeClaim) {
					sc := "wffc"
					pvc.Spec.StorageClassName = &sc
				})
				return []client.Object{targetPVC, wffcStorageClass("wffc"), exporterImageConfigMap(createPVCFlowControllerNamespace)}
			},
			wantPhase:   common.PhasePending,
			wantRequeue: true,
			assertExtra: func(t *testing.T, c client.Client, _ *DataImportReconciler, _ *dev1alpha1.DataImport) {
				scratchList := &corev1.PersistentVolumeClaimList{}
				require.NoError(t, c.List(context.Background(), scratchList, client.InNamespace(createPVCFlowControllerNamespace)))
				assert.Empty(t, scratchList.Items)
			},
		},
		{
			name: "target PVC ours and Pending, Immediate class opens the upload phase",
			buildObjs: func(di *dev1alpha1.DataImport) []client.Object {
				targetPVC := createPVCFlowTestTargetPVC(di, nil)
				return []client.Object{targetPVC, createPVCFlowTestImmediateStorageClass("immediate"), exporterImageConfigMap(createPVCFlowControllerNamespace)}
			},
			wantPhase:   common.PhasePending,
			wantRequeue: true,
			assertExtra: func(t *testing.T, c client.Client, r *DataImportReconciler, _ *dev1alpha1.DataImport) {
				require.NoError(t, c.Get(context.Background(),
					types.NamespacedName{Namespace: createPVCFlowControllerNamespace, Name: r.names.ImportScratchPVCName}, &corev1.PersistentVolumeClaim{}))
			},
		},
		{
			name: "UploadFinished=True, importer not fully stopped requeues without handing over",
			buildObjs: func(di *dev1alpha1.DataImport) []client.Object {
				di.Status.ServerState = string(common.ServerStateFinished)
				targetPVC := createPVCFlowTestTargetPVC(di, nil)
				names := common.NewNames(dev1alpha1.KindPVC, di.Name, di.Namespace, di.Name)
				scratchPVC := boundInternalScratchPVC(names.ImportScratchPVCName, createPVCFlowControllerNamespace)
				pod := importerPodFixture("importer-pod", createPVCFlowControllerNamespace, names.DeployName)
				return []client.Object{targetPVC, scratchPVC, pod, exporterImageConfigMap(createPVCFlowControllerNamespace)}
			},
			wantPhase:   common.PhaseReady,
			wantRequeue: true,
		},
		{
			name: "scratch Bound, importer stopped, PV claimed by scratch, target unbound -- claimRef is patched to the target",
			buildObjs: func(di *dev1alpha1.DataImport) []client.Object {
				di.Status.ServerState = string(common.ServerStateFinished)
				targetPVC := createPVCFlowTestTargetPVC(di, nil)
				names := common.NewNames(dev1alpha1.KindPVC, di.Name, di.Namespace, di.Name)
				scratchPVC := boundInternalScratchPVC(names.ImportScratchPVCName, createPVCFlowControllerNamespace)
				scratchPVC.UID = "scratch-uid"
				scratchPVC.Spec.VolumeName = "matrix-pv"
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{Name: "matrix-pv"},
					Spec: corev1.PersistentVolumeSpec{ClaimRef: &corev1.ObjectReference{
						Namespace: scratchPVC.Namespace, Name: scratchPVC.Name, UID: scratchPVC.UID,
					}},
				}
				return []client.Object{targetPVC, scratchPVC, pv, exporterImageConfigMap(createPVCFlowControllerNamespace)}
			},
			wantPhase:   common.PhaseReady,
			wantRequeue: true,
			assertExtra: func(t *testing.T, c client.Client, _ *DataImportReconciler, di *dev1alpha1.DataImport) {
				pv := &corev1.PersistentVolume{}
				require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "matrix-pv"}, pv))
				require.NotNil(t, pv.Spec.ClaimRef)
				assert.Equal(t, di.Name, pv.Spec.ClaimRef.Name)
			},
		},
		{
			name: "scratch Bound, importer stopped, target already bound to some other PV is terminal with no patch",
			buildObjs: func(di *dev1alpha1.DataImport) []client.Object {
				di.Status.ServerState = string(common.ServerStateFinished)
				targetPVC := createPVCFlowTestTargetPVC(di, func(pvc *corev1.PersistentVolumeClaim) {
					pvc.Status.Phase = corev1.ClaimBound
					pvc.Spec.VolumeName = "other-pv"
				})
				names := common.NewNames(dev1alpha1.KindPVC, di.Name, di.Namespace, di.Name)
				scratchPVC := boundInternalScratchPVC(names.ImportScratchPVCName, createPVCFlowControllerNamespace)
				scratchPVC.UID = "scratch-uid"
				scratchPVC.Spec.VolumeName = "matrix-pv"
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{Name: "matrix-pv"},
					Spec: corev1.PersistentVolumeSpec{ClaimRef: &corev1.ObjectReference{
						Namespace: scratchPVC.Namespace, Name: scratchPVC.Name, UID: scratchPVC.UID,
					}},
				}
				return []client.Object{targetPVC, scratchPVC, pv, exporterImageConfigMap(createPVCFlowControllerNamespace)}
			},
			wantPhase: common.PhaseFailed,
			assertExtra: func(t *testing.T, c client.Client, _ *DataImportReconciler, _ *dev1alpha1.DataImport) {
				pv := &corev1.PersistentVolume{}
				require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "matrix-pv"}, pv))
				require.NotNil(t, pv.Spec.ClaimRef)
				assert.Equal(t, "scratch-uid", string(pv.Spec.ClaimRef.UID), "the PV must not be repatched onto the target")
			},
		},
		{
			name: "scratch Bound, importer stopped, PV claimed by neither PVC is terminal with no patch",
			buildObjs: func(di *dev1alpha1.DataImport) []client.Object {
				di.Status.ServerState = string(common.ServerStateFinished)
				targetPVC := createPVCFlowTestTargetPVC(di, nil)
				names := common.NewNames(dev1alpha1.KindPVC, di.Name, di.Namespace, di.Name)
				scratchPVC := boundInternalScratchPVC(names.ImportScratchPVCName, createPVCFlowControllerNamespace)
				scratchPVC.Spec.VolumeName = "matrix-pv"
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{Name: "matrix-pv"},
					Spec:       corev1.PersistentVolumeSpec{ClaimRef: &corev1.ObjectReference{Namespace: "other-ns", Name: "other-pvc"}},
				}
				return []client.Object{targetPVC, scratchPVC, pv, exporterImageConfigMap(createPVCFlowControllerNamespace)}
			},
			wantPhase: common.PhaseFailed,
			assertExtra: func(t *testing.T, c client.Client, _ *DataImportReconciler, _ *dev1alpha1.DataImport) {
				pv := &corev1.PersistentVolume{}
				require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "matrix-pv"}, pv))
				require.NotNil(t, pv.Spec.ClaimRef)
				assert.Equal(t, "other-pvc", pv.Spec.ClaimRef.Name)
			},
		},
		{
			name: "PV claimed by target, scratch still present but not yet ClaimLost keeps waiting",
			buildObjs: func(di *dev1alpha1.DataImport) []client.Object {
				di.Status.ServerState = string(common.ServerStateFinished)
				targetPVC := createPVCFlowTestTargetPVC(di, func(pvc *corev1.PersistentVolumeClaim) {
					pvc.Spec.VolumeName = "matrix-pv" // still Pending
				})
				names := common.NewNames(dev1alpha1.KindPVC, di.Name, di.Namespace, di.Name)
				scratchPVC := boundInternalScratchPVC(names.ImportScratchPVCName, createPVCFlowControllerNamespace)
				scratchPVC.Spec.VolumeName = "matrix-pv"
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{Name: "matrix-pv"},
					Spec: corev1.PersistentVolumeSpec{ClaimRef: &corev1.ObjectReference{
						Namespace: targetPVC.Namespace, Name: targetPVC.Name, UID: targetPVC.UID,
					}},
				}
				return []client.Object{targetPVC, scratchPVC, pv, exporterImageConfigMap(createPVCFlowControllerNamespace)}
			},
			wantPhase:   common.PhaseReady,
			wantRequeue: true,
			assertExtra: func(t *testing.T, c client.Client, _ *DataImportReconciler, di *dev1alpha1.DataImport) {
				names := common.NewNames(dev1alpha1.KindPVC, di.Name, di.Namespace, di.Name)
				require.NoError(t, c.Get(context.Background(),
					types.NamespacedName{Namespace: createPVCFlowControllerNamespace, Name: names.ImportScratchPVCName}, &corev1.PersistentVolumeClaim{}))
			},
		},
		{
			name: "target Bound, scratch gone, UploadFinished=True completes",
			buildObjs: func(di *dev1alpha1.DataImport) []client.Object {
				di.Status.ServerState = string(common.ServerStateFinished)
				return []client.Object{boundImportPVC(di.Name), exporterImageConfigMap(createPVCFlowControllerNamespace)}
			},
			wantPhase: common.PhaseCompleted,
		},
		{
			name: "target Bound, scratch gone, UploadFinished not True is terminal",
			buildObjs: func(di *dev1alpha1.DataImport) []client.Object {
				return []client.Object{boundImportPVC(di.Name), exporterImageConfigMap(createPVCFlowControllerNamespace)}
			},
			wantPhase: common.PhaseFailed,
		},
		{
			name: "UploadFinished=True, scratch gone, target still Pending is terminal, never restarted",
			buildObjs: func(di *dev1alpha1.DataImport) []client.Object {
				di.Status.ServerState = string(common.ServerStateFinished)
				targetPVC := createPVCFlowTestTargetPVC(di, nil)
				return []client.Object{targetPVC, exporterImageConfigMap(createPVCFlowControllerNamespace)}
			},
			wantPhase: common.PhaseFailed,
			assertExtra: func(t *testing.T, c client.Client, _ *DataImportReconciler, _ *dev1alpha1.DataImport) {
				scratchList := &corev1.PersistentVolumeClaimList{}
				require.NoError(t, c.List(context.Background(), scratchList, client.InNamespace(createPVCFlowControllerNamespace)))
				assert.Empty(t, scratchList.Items, "the data is gone; a new scratch PVC must never be created to restart it")
			},
		},
		{
			name: "target PVC exists and is not ours is terminal",
			buildObjs: func(di *dev1alpha1.DataImport) []client.Object {
				foreign := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: di.Name, Namespace: di.Namespace}}
				return []client.Object{foreign, exporterImageConfigMap(createPVCFlowControllerNamespace)}
			},
			wantPhase: common.PhaseFailed,
		},
		{
			name: "import already terminal (Completed), target PVC missing does nothing",
			buildObjs: func(di *dev1alpha1.DataImport) []client.Object {
				di.Status.Phase = string(common.PhaseCompleted)
				di.Status.CompletionTimestamp = &fixedNow
				meta.SetStatusCondition(&di.Status.Conditions, metav1.Condition{
					Type: string(common.ConditionCompleted), Status: metav1.ConditionTrue, Reason: string(common.ReasonCompleted),
				})
				return []client.Object{exporterImageConfigMap(createPVCFlowControllerNamespace)}
			},
			wantPhase: common.PhaseCompleted,
			assertExtra: func(t *testing.T, c client.Client, _ *DataImportReconciler, di *dev1alpha1.DataImport) {
				getErr := c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: di.Name}, &corev1.PersistentVolumeClaim{})
				require.Error(t, getErr)
				assert.True(t, apierrors.IsNotFound(getErr), "a terminal import must never recreate a target PVC it no longer has")
			},
		},
	}

	for i, tt := range rows {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			di := createPVCImport(rowImportName(i), "")
			objs := append([]client.Object{di}, tt.buildObjs(di)...)

			// Writes are counted from the very first reconcile onward (a fake.Client's interceptors can
			// only be installed at build time); the counter is reset just before the repeat pass below, so
			// only that second pass's writes are asserted against. Every non-terminal reconcile with
			// Publish=false unconditionally re-attempts deleting the (already-absent) publish
			// Service/Ingress -- a pre-existing, deliberate property of this controller (an unconditional
			// Delete is cheaper and safer than a Get-then-Delete), and orthogonal to the CreatePVC
			// resume-matrix property this test guards -- so Service/Ingress writes are never counted here.
			// A freshly created PersistentVolumeClaim also needs its status.phase stamped Pending the way
			// the real apiserver's binder controller would (pendingOnPVCCreateInterceptor's own job),
			// since internalPVCStatus classifies an empty phase as Failed.
			var writes int
			countingInterceptor := interceptor.Funcs{
				Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if err := cl.Create(ctx, obj, opts...); err != nil {
						return err
					}
					switch o := obj.(type) {
					case *corev1.Service, *networkingv1.Ingress:
						return nil
					case *corev1.PersistentVolumeClaim:
						writes++
						if o.Status.Phase == "" {
							o.Status.Phase = corev1.ClaimPending
							return cl.Status().Update(ctx, o)
						}
					default:
						writes++
					}
					return nil
				},
				Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if _, ok := obj.(*corev1.Service); !ok {
						if _, ok := obj.(*networkingv1.Ingress); !ok {
							writes++
						}
					}
					return cl.Update(ctx, obj, opts...)
				},
				Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					writes++
					return cl.Patch(ctx, obj, patch, opts...)
				},
				Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					switch obj.(type) {
					case *corev1.Service, *networkingv1.Ingress:
						// deletePublish's unconditional Service/Ingress delete, see the comment above.
						return cl.Delete(ctx, obj, opts...)
					case *appsv1.Deployment:
						// stopImporter's unconditional Deployment delete: called on every pass while the
						// handover waits on the importer to stop, cheap and safe against an already-absent
						// Deployment by the same "unconditional delete" convention as deletePublish.
						return cl.Delete(ctx, obj, opts...)
					default:
						writes++
						return cl.Delete(ctx, obj, opts...)
					}
				},
			}

			r, c := newCreatePVCFlowReconciler(t, objs, countingInterceptor)

			res, err := r.Reconcile(context.Background(), diReq(di))
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if tt.wantRequeue {
				assert.Equal(t, dataImportRequeueInterval, res.RequeueAfter)
			} else {
				assert.Zero(t, res.RequeueAfter)
			}

			got := &dev1alpha1.DataImport{}
			require.NoError(t, c.Get(context.Background(), diReq(di).NamespacedName, got))
			assert.Equal(t, string(tt.wantPhase), got.Status.Phase)

			if tt.assertExtra != nil {
				tt.assertExtra(t, c, r, di)
			}

			// No duplicate writes: a repeat reconcile against the same live state must not re-issue any
			// typed-client write, and must return the same Result.
			writes = 0
			res2, err := r.Reconcile(context.Background(), diReq(di))
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, res, res2, "a repeat reconcile against unchanged state must return the same Result")
			assert.Zero(t, writes, "a repeat reconcile against unchanged state must issue no further writes")
		})
	}
}

// rowImportName gives each TestReconcileCreatePVCResumesFromEachState row its own stable DataImport/target
// PVC name, so rows never collide inside the shared fake client scheme validation.
func rowImportName(i int) string {
	return "matrix-row-" + string(rune('a'+i))
}

// TestReconcileCreatePVCCompletesOnlyAfterTargetPVCBound guards the Completed=True gate directly: it must
// never flip while the target PVC is not yet Bound, nor while the scratch PVC is still standing (even once
// the handover PV already names the target).
func TestReconcileCreatePVCCompletesOnlyAfterTargetPVCBound(t *testing.T) {
	t.Parallel()

	t.Run("success: target PVC Pending never completes", func(t *testing.T) {
		t.Parallel()

		di := createPVCImport("no-complete-pending", common.ServerStateFinished)
		targetPVC := createPVCFlowTestTargetPVC(di, nil) // Pending by default

		objs := []client.Object{di, targetPVC, exporterImageConfigMap(createPVCFlowControllerNamespace)}
		r, c := newCreatePVCFlowReconciler(t, objs)

		_, err := r.Reconcile(context.Background(), diReq(di))
		require.NoError(t, err)

		got := &dev1alpha1.DataImport{}
		require.NoError(t, c.Get(context.Background(), diReq(di).NamespacedName, got))
		assert.False(t, meta.IsStatusConditionTrue(got.Status.Conditions, string(common.ConditionCompleted)))
	})

	t.Run("success: scratch PVC still present (not yet ClaimLost) never completes", func(t *testing.T) {
		t.Parallel()

		di := createPVCImport("no-complete-scratch-present", common.ServerStateFinished)
		targetPVC := createPVCFlowTestTargetPVC(di, func(pvc *corev1.PersistentVolumeClaim) {
			pvc.Status.Phase = corev1.ClaimBound
			pvc.Spec.VolumeName = "handed-over-pv"
		})
		names := common.NewNames(dev1alpha1.KindPVC, di.Name, di.Namespace, di.Name)
		scratchPVC := boundInternalScratchPVC(names.ImportScratchPVCName, createPVCFlowControllerNamespace)
		scratchPVC.Status.Phase = corev1.ClaimBound // NOT yet ClaimLost
		scratchPVC.Spec.VolumeName = "handed-over-pv"
		pv := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "handed-over-pv"},
			Spec: corev1.PersistentVolumeSpec{ClaimRef: &corev1.ObjectReference{
				Namespace: targetPVC.Namespace, Name: targetPVC.Name, UID: targetPVC.UID,
			}},
		}

		objs := []client.Object{di, targetPVC, scratchPVC, pv, exporterImageConfigMap(createPVCFlowControllerNamespace)}
		r, c := newCreatePVCFlowReconciler(t, objs)

		_, err := r.Reconcile(context.Background(), diReq(di))
		require.NoError(t, err)

		got := &dev1alpha1.DataImport{}
		require.NoError(t, c.Get(context.Background(), diReq(di).NamespacedName, got))
		assert.False(t, meta.IsStatusConditionTrue(got.Status.Conditions, string(common.ConditionCompleted)))

		require.NoError(t, c.Get(context.Background(),
			types.NamespacedName{Namespace: createPVCFlowControllerNamespace, Name: names.ImportScratchPVCName}, &corev1.PersistentVolumeClaim{}),
			"the scratch PVC must survive until it is observed ClaimLost")
	})
}

// createPVCFlowTestCountingWriteInterceptor builds an interceptor.Funcs that increments *writes on every
// Create/Update/Patch/Delete issued through the typed client, regardless of object kind.
func createPVCFlowTestCountingWriteInterceptor(writes *int) interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			*writes++
			return cl.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			*writes++
			return cl.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			*writes++
			return cl.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			*writes++
			return cl.Delete(ctx, obj, opts...)
		},
	}
}

// createPVCFlowTestCountingPVCWriteInterceptor builds an interceptor.Funcs that increments *writes only on
// a Create/Update/Patch/Delete of a *corev1.PersistentVolumeClaim. Every reconcile with Publish=false
// unconditionally re-attempts deleting the (already-absent) publish Service/Ingress -- that churn is a
// pre-existing, deliberate property of this controller (an unconditional Delete is cheaper and safer than
// a Get-then-Delete, see teardownImportInfra's own doc comment) and is orthogonal to whatever this test
// guards, so it must never be counted as a "duplicate write" here.
func createPVCFlowTestCountingPVCWriteInterceptor(writes *int) interceptor.Funcs {
	countPVC := func(obj client.Object) {
		if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
			*writes++
		}
	}
	return interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			countPVC(obj)
			return cl.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			countPVC(obj)
			return cl.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			countPVC(obj)
			return cl.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			countPVC(obj)
			return cl.Delete(ctx, obj, opts...)
		},
	}
}

// TestReconcileCreatePVCIsIdempotent mirrors TestReconcile_PopulateData_Idempotent for the CreatePVC
// pipeline: a completed import issues no further writes on a repeat reconcile, and a settled Failed import
// whose target PVC is not being deleted costs the release step's one cached Get and nothing else.
func TestReconcileCreatePVCIsIdempotent(t *testing.T) {
	t.Parallel()

	t.Run("success: a completed import issues no writes on the second reconcile", func(t *testing.T) {
		t.Parallel()

		di := createPVCImport("idempotent-completed", common.ServerStateFinished)
		targetPVC := boundImportPVC(di.Name)

		var writes int
		objs := []client.Object{di, targetPVC, exporterImageConfigMap(createPVCFlowControllerNamespace)}
		r, c := newCreatePVCFlowReconciler(t, objs, createPVCFlowTestCountingWriteInterceptor(&writes))

		res1, err := r.Reconcile(context.Background(), diReq(di))
		require.NoError(t, err)

		got := &dev1alpha1.DataImport{}
		require.NoError(t, c.Get(context.Background(), diReq(di).NamespacedName, got))
		assert.True(t, meta.IsStatusConditionTrue(got.Status.Conditions, string(common.ConditionCompleted)))

		writes = 0
		res2, err := r.Reconcile(context.Background(), diReq(di))
		require.NoError(t, err)
		assert.Equal(t, res1, res2)
		assert.Zero(t, writes, "a completed CreatePVC import must issue no writes on a repeat reconcile")
	})

	t.Run("success: a settled Failed state issues no writes beyond the release step's cached Get", func(t *testing.T) {
		t.Parallel()

		di := createPVCImport("idempotent-failed", "")
		di.Status.Phase = string(common.PhaseFailed)
		di.Status.CompletionTimestamp = &fixedNow
		pvcFinalizerTestSeedFailedConditions(di)
		targetPVC := boundImportPVC(di.Name) // Bound, no finalizer of ours, not being deleted

		var writes int
		objs := []client.Object{di, targetPVC, exporterImageConfigMap(createPVCFlowControllerNamespace)}
		r, _ := newCreatePVCFlowReconciler(t, objs, createPVCFlowTestCountingWriteInterceptor(&writes))

		_, err := r.Reconcile(context.Background(), diReq(di))
		require.NoError(t, err)
		assert.Zero(t, writes, "a settled Failed import whose target PVC is not being deleted must cost one cached Get and no write")
	})
}

// TestReconcileCreatePVCKeepsUserNamespaceFreeOfServiceObjects proves the "unconditionally" half of the
// no-service-objects rule by running every StorageClass-binding-mode x waitForFirstConsumer combination:
// after every reconcile the user's namespace holds nothing but the DataImport and the target PVC.
func TestReconcileCreatePVCKeepsUserNamespaceFreeOfServiceObjects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                 string
		importName           string
		waitForFirstConsumer bool
		className            string
		buildClass           func(name string) *storagev1.StorageClass
		seedSelectedNode     bool
	}{
		{name: "wffc=false, Immediate class", importName: "clean-ns-1", waitForFirstConsumer: false, className: "immediate", buildClass: createPVCFlowTestImmediateStorageClass},
		{name: "wffc=false, WaitForFirstConsumer class", importName: "clean-ns-2", waitForFirstConsumer: false, className: "wffc", buildClass: wffcStorageClass},
		{name: "wffc=true, Immediate class", importName: "clean-ns-3", waitForFirstConsumer: true, className: "immediate", buildClass: createPVCFlowTestImmediateStorageClass},
		{name: "wffc=true, WaitForFirstConsumer class", importName: "clean-ns-4", waitForFirstConsumer: true, className: "wffc", buildClass: wffcStorageClass, seedSelectedNode: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			di := createPVCImport(tt.importName, "")
			di.Spec.WaitForFirstConsumer = tt.waitForFirstConsumer

			targetPVC := createPVCFlowTestTargetPVC(di, func(pvc *corev1.PersistentVolumeClaim) {
				className := tt.className
				pvc.Spec.StorageClassName = &className
				if tt.seedSelectedNode {
					pvc.Annotations[annSelectedNode] = "node-a"
				}
			})
			sc := tt.buildClass(tt.className)

			objs := []client.Object{di, targetPVC, sc, exporterImageConfigMap(createPVCFlowControllerNamespace)}
			r, c := newCreatePVCFlowReconciler(t, objs, pendingOnPVCCreateInterceptor())

			assertUserNamespaceClean := func() {
				pvcList := &corev1.PersistentVolumeClaimList{}
				require.NoError(t, c.List(context.Background(), pvcList, client.InNamespace(di.Namespace)))
				require.Len(t, pvcList.Items, 1, "the user namespace must hold exactly the target PVC")
				assert.Equal(t, targetPVC.Name, pvcList.Items[0].Name)

				deployList := &appsv1.DeploymentList{}
				require.NoError(t, c.List(context.Background(), deployList, client.InNamespace(di.Namespace)))
				assert.Empty(t, deployList.Items)

				podList := &corev1.PodList{}
				require.NoError(t, c.List(context.Background(), podList, client.InNamespace(di.Namespace)))
				assert.Empty(t, podList.Items)

				jobList := &batchv1.JobList{}
				require.NoError(t, c.List(context.Background(), jobList, client.InNamespace(di.Namespace)))
				assert.Empty(t, jobList.Items)

				svcList := &corev1.ServiceList{}
				require.NoError(t, c.List(context.Background(), svcList, client.InNamespace(di.Namespace)))
				assert.Empty(t, svcList.Items)

				ingressList := &networkingv1.IngressList{}
				require.NoError(t, c.List(context.Background(), ingressList, client.InNamespace(di.Namespace)))
				assert.Empty(t, ingressList.Items)

				secretList := &corev1.SecretList{}
				require.NoError(t, c.List(context.Background(), secretList, client.InNamespace(di.Namespace)))
				assert.Empty(t, secretList.Items)
			}

			_, err := r.Reconcile(context.Background(), diReq(di))
			require.NoError(t, err)
			assertUserNamespaceClean()

			_, err = r.Reconcile(context.Background(), diReq(di))
			require.NoError(t, err)
			assertUserNamespaceClean()
		})
	}
}

// TestReconcileCreatePVCRejectsForeignTargetPVCName guards the ownership-guard write side, not just the
// message: a pre-existing PVC named after the template that carries no storage-manager annotations for
// this DataImport fails terminally, and its .Spec is never touched.
func TestReconcileCreatePVCRejectsForeignTargetPVCName(t *testing.T) {
	t.Parallel()

	di := createPVCImport("foreign-pvc-imp", "")
	foreign := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: di.Name, Namespace: di.Namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}},
		},
	}

	objs := []client.Object{di, foreign, exporterImageConfigMap(createPVCFlowControllerNamespace)}
	r, c := newCreatePVCFlowReconciler(t, objs)

	before := &corev1.PersistentVolumeClaim{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: di.Name}, before))

	_, err := r.Reconcile(context.Background(), diReq(di))
	require.NoError(t, err)

	got := &dev1alpha1.DataImport{}
	require.NoError(t, c.Get(context.Background(), diReq(di).NamespacedName, got))
	assert.Equal(t, string(common.PhaseFailed), got.Status.Phase)

	after := &corev1.PersistentVolumeClaim{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: di.Name}, after))
	assert.Equal(t, before.Spec, after.Spec, "a stranger's PVC .Spec must never be modified by the ownership guard")
}

// TestReconcileCreatePVCTargetPVCDeletedByUser guards the two-pass finalizer-release design end to end.
// Every Terminating fixture is built the same way the fake client actually produces one: Create the PVC
// carrying our finalizer, then Delete it (the fake client sets DeletionTimestamp and keeps the object,
// since a finalizer is still present).
func TestReconcileCreatePVCTargetPVCDeletedByUser(t *testing.T) {
	t.Parallel()

	t.Run("success: first pass fails terminally and leaves the PVC untouched", func(t *testing.T) {
		t.Parallel()

		di := createPVCImport("deleted-first-pass", "")
		var pvcWrites int
		countingInterceptor := interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
					pvcWrites++
				}
				return cl.Update(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
					pvcWrites++
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
					pvcWrites++
				}
				return cl.Delete(ctx, obj, opts...)
			},
		}

		objs := []client.Object{di, exporterImageConfigMap(createPVCFlowControllerNamespace)}
		r, c := newCreatePVCFlowReconciler(t, objs, countingInterceptor)

		pvc := boundImportPVC(di.Name)
		pvc.Namespace = di.Namespace
		pvc.Finalizers = []string{dev1alpha1.StorageManagerFinalizerName}
		require.NoError(t, c.Create(context.Background(), pvc))
		require.NoError(t, c.Delete(context.Background(), pvc))

		pvcWrites = 0
		_, err := r.Reconcile(context.Background(), diReq(di))
		require.NoError(t, err)

		got := &dev1alpha1.DataImport{}
		require.NoError(t, c.Get(context.Background(), diReq(di).NamespacedName, got))
		assert.Equal(t, string(common.PhaseFailed), got.Status.Phase)
		ready := meta.FindStatusCondition(got.Status.Conditions, string(common.ConditionReady))
		require.NotNil(t, ready)
		assert.Equal(t, string(common.ReasonTargetFailed), ready.Reason)
		assert.Contains(t, ready.Message, pvc.Name)
		completed := meta.FindStatusCondition(got.Status.Conditions, string(common.ConditionCompleted))
		require.NotNil(t, completed)
		assert.Equal(t, metav1.ConditionFalse, completed.Status)
		assert.Equal(t, string(common.ReasonFailed), completed.Reason)

		gotPVC := &corev1.PersistentVolumeClaim{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: pvc.Name}, gotPVC))
		assert.NotNil(t, gotPVC.DeletionTimestamp)
		assert.Contains(t, gotPVC.Finalizers, dev1alpha1.StorageManagerFinalizerName)
		assert.Zero(t, pvcWrites, "the observing pass must write nothing to the target PVC")
	})

	t.Run("success: second pass releases only our finalizer", func(t *testing.T) {
		t.Parallel()

		di := createPVCImport("deleted-second-pass", "")

		var pvcWrites int
		countingInterceptor := interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
					pvcWrites++
				}
				return cl.Update(ctx, obj, opts...)
			},
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
					pvcWrites++
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
		}

		objs := []client.Object{di, exporterImageConfigMap(createPVCFlowControllerNamespace)}
		r, c := newCreatePVCFlowReconciler(t, objs, countingInterceptor)

		pvc := boundImportPVC(di.Name)
		pvc.Namespace = di.Namespace
		pvc.Finalizers = []string{dev1alpha1.StorageManagerFinalizerName, "other.example.com/keep-me"}
		require.NoError(t, c.Create(context.Background(), pvc))
		require.NoError(t, c.Delete(context.Background(), pvc))

		_, err := r.Reconcile(context.Background(), diReq(di)) // pass 1: observes the deletion, fails terminally
		require.NoError(t, err)

		pvcWrites = 0
		res, err := r.Reconcile(context.Background(), diReq(di)) // pass 2: releases our finalizer
		require.NoError(t, err)
		assert.Equal(t, ctrl.Result{}, res)
		assert.Equal(t, 1, pvcWrites, "exactly one PVC write releases our finalizer")

		got := &corev1.PersistentVolumeClaim{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: pvc.Name}, got))
		assert.Equal(t, []string{"other.example.com/keep-me"}, got.Finalizers)
	})

	t.Run("error: a failed status write on the first pass keeps the finalizer and never recreates the PVC", func(t *testing.T) {
		t.Parallel()

		di := createPVCImport("deleted-crash-window", "")

		var subResourceUpdateCalls int
		injectedErr := errors.New("injected transient status-write failure")
		countingInterceptor := interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, cl client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				subResourceUpdateCalls++
				if subResourceUpdateCalls == 1 {
					return injectedErr
				}
				return cl.SubResource(subResourceName).Update(ctx, obj, opts...)
			},
		}

		objs := []client.Object{di, exporterImageConfigMap(createPVCFlowControllerNamespace)}
		r, c := newCreatePVCFlowReconciler(t, objs, countingInterceptor)

		pvc := boundImportPVC(di.Name)
		pvc.Namespace = di.Namespace
		pvc.Finalizers = []string{dev1alpha1.StorageManagerFinalizerName}
		require.NoError(t, c.Create(context.Background(), pvc))
		require.NoError(t, c.Delete(context.Background(), pvc))

		// Pass 1: the observing reconcile fails to persist status (injected error).
		_, err := r.Reconcile(context.Background(), diReq(di))
		require.Error(t, err)

		stillThere := &corev1.PersistentVolumeClaim{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: pvc.Name}, stillThere))
		assert.Contains(t, stillThere.Finalizers, dev1alpha1.StorageManagerFinalizerName,
			"a failed status write must not have released the finalizer -- Failed was never actually persisted")

		// Pass 2: the injected failure only fires once; Failed is now persisted for the first time.
		_, err = r.Reconcile(context.Background(), diReq(di))
		require.NoError(t, err)

		got := &dev1alpha1.DataImport{}
		require.NoError(t, c.Get(context.Background(), diReq(di).NamespacedName, got))
		assert.Equal(t, string(common.PhaseFailed), got.Status.Phase)

		stillThere = &corev1.PersistentVolumeClaim{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: pvc.Name}, stillThere))
		assert.Contains(t, stillThere.Finalizers, dev1alpha1.StorageManagerFinalizerName,
			"Failed was only just persisted on this pass -- the release step runs on the NEXT pass, never the one that first persists it")

		// Pass 3: Failed is the persisted phase entering this pass, so the release step runs.
		_, err = r.Reconcile(context.Background(), diReq(di))
		require.NoError(t, err)

		getErr := c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: pvc.Name}, &corev1.PersistentVolumeClaim{})
		require.Error(t, getErr, "the fake client finalizes (deletes) the PVC once its last finalizer is removed")
		assert.True(t, apierrors.IsNotFound(getErr))

		// Pass 4: nothing is ever recreated for a terminal import whose target PVC the user deleted.
		_, err = r.Reconcile(context.Background(), diReq(di))
		require.NoError(t, err)

		getErr = c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: pvc.Name}, &corev1.PersistentVolumeClaim{})
		require.Error(t, getErr)
		assert.True(t, apierrors.IsNotFound(getErr), "a terminal phase must never recreate the PVC the user deleted")

		scratchList := &corev1.PersistentVolumeClaimList{}
		require.NoError(t, c.List(context.Background(), scratchList, client.InNamespace(createPVCFlowControllerNamespace)))
		assert.Empty(t, scratchList.Items, "no scratch PVC must ever be created for this import")

		deployList := &appsv1.DeploymentList{}
		require.NoError(t, c.List(context.Background(), deployList))
		assert.Empty(t, deployList.Items, "no importer Deployment must ever be created for this import")
	})

	t.Run("success: a PVC deleted after the import completed keeps our finalizer", func(t *testing.T) {
		t.Parallel()

		di := createPVCImport("deleted-after-completed", "")
		di.Status.Phase = string(common.PhaseCompleted)
		di.Status.CompletionTimestamp = &fixedNow
		meta.SetStatusCondition(&di.Status.Conditions, metav1.Condition{
			Type: string(common.ConditionReady), Status: metav1.ConditionFalse,
			Reason: string(common.ReasonCompleted), Message: "Data import completed",
		})
		meta.SetStatusCondition(&di.Status.Conditions, metav1.Condition{
			Type: string(common.ConditionCompleted), Status: metav1.ConditionTrue,
			Reason: string(common.ReasonCompleted), Message: "Data import completed into PVC deleted-after-completed",
		})

		var pvcWrites int
		objs := []client.Object{di, exporterImageConfigMap(createPVCFlowControllerNamespace)}
		r, c := newCreatePVCFlowReconciler(t, objs, createPVCFlowTestCountingPVCWriteInterceptor(&pvcWrites))

		pvc := boundImportPVC(di.Name)
		pvc.Namespace = di.Namespace
		pvc.Finalizers = []string{dev1alpha1.StorageManagerFinalizerName}
		require.NoError(t, c.Create(context.Background(), pvc))
		require.NoError(t, c.Delete(context.Background(), pvc))

		pvcWrites = 0
		_, err := r.Reconcile(context.Background(), diReq(di))
		require.NoError(t, err)

		got := &dev1alpha1.DataImport{}
		require.NoError(t, c.Get(context.Background(), diReq(di).NamespacedName, got))
		assert.Equal(t, string(common.PhaseCompleted), got.Status.Phase)
		require.NotNil(t, got.Status.CompletionTimestamp)
		assert.True(t, fixedNow.Time.Equal(got.Status.CompletionTimestamp.Time), "completionTimestamp must be unchanged")

		gotPVC := &corev1.PersistentVolumeClaim{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: pvc.Name}, gotPVC))
		assert.Contains(t, gotPVC.Finalizers, dev1alpha1.StorageManagerFinalizerName)
		assert.Zero(t, pvcWrites, "a PVC deleted after Completed must not be touched -- the release step is scoped to Failed only")
	})

	t.Run("success: a PVC deleted after the import expired keeps our finalizer", func(t *testing.T) {
		t.Parallel()

		di := createPVCImport("deleted-after-expired", common.ServerStateIdleExpired)
		di.Status.Phase = string(common.PhaseExpired)
		di.Status.CompletionTimestamp = &fixedNow
		meta.SetStatusCondition(&di.Status.Conditions, metav1.Condition{
			Type: string(common.ConditionReady), Status: metav1.ConditionFalse,
			Reason: string(common.ReasonExpired), Message: "DataImport idle timeout expired",
		})
		meta.SetStatusCondition(&di.Status.Conditions, metav1.Condition{
			Type: string(common.ConditionCompleted), Status: metav1.ConditionFalse,
			Reason: string(common.ReasonExpired), Message: "DataImport idle timeout expired before completion",
		})

		var pvcWrites int
		objs := []client.Object{di, exporterImageConfigMap(createPVCFlowControllerNamespace)}
		r, c := newCreatePVCFlowReconciler(t, objs, createPVCFlowTestCountingPVCWriteInterceptor(&pvcWrites))

		pvc := boundImportPVC(di.Name)
		pvc.Namespace = di.Namespace
		pvc.Finalizers = []string{dev1alpha1.StorageManagerFinalizerName}
		require.NoError(t, c.Create(context.Background(), pvc))
		require.NoError(t, c.Delete(context.Background(), pvc))

		pvcWrites = 0
		_, err := r.Reconcile(context.Background(), diReq(di))
		require.NoError(t, err)

		got := &dev1alpha1.DataImport{}
		require.NoError(t, c.Get(context.Background(), diReq(di).NamespacedName, got))
		assert.Equal(t, string(common.PhaseExpired), got.Status.Phase)

		gotPVC := &corev1.PersistentVolumeClaim{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: pvc.Name}, gotPVC))
		assert.Contains(t, gotPVC.Finalizers, dev1alpha1.StorageManagerFinalizerName)
		assert.Zero(t, pvcWrites, "the Expired branch's teardown must never touch the target PVC")
	})

	t.Run("success: a terminating PVC that belongs to another import is not touched", func(t *testing.T) {
		t.Parallel()

		di := createPVCImport("not-mine-terminating", "")

		var pvcWrites int
		objs := []client.Object{di, exporterImageConfigMap(createPVCFlowControllerNamespace)}
		r, c := newCreatePVCFlowReconciler(t, objs, createPVCFlowTestCountingPVCWriteInterceptor(&pvcWrites))

		pvc := boundImportPVC(di.Name)
		pvc.Namespace = di.Namespace
		pvc.Annotations[dev1alpha1.AnnotationStorageManagerNameKey] = "some-other-import"
		pvc.Finalizers = []string{dev1alpha1.StorageManagerFinalizerName}
		require.NoError(t, c.Create(context.Background(), pvc))
		require.NoError(t, c.Delete(context.Background(), pvc))

		pvcWrites = 0
		_, err := r.Reconcile(context.Background(), diReq(di))
		require.NoError(t, err)

		got := &dev1alpha1.DataImport{}
		require.NoError(t, c.Get(context.Background(), diReq(di).NamespacedName, got))
		assert.Equal(t, string(common.PhaseFailed), got.Status.Phase)

		gotPVC := &corev1.PersistentVolumeClaim{}
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: pvc.Name}, gotPVC))
		assert.Equal(t, []string{dev1alpha1.StorageManagerFinalizerName}, gotPVC.Finalizers,
			"a stranger's terminating PVC must keep every finalizer untouched, including one that happens to carry our own name")
		assert.Zero(t, pvcWrites)
	})
}

// TestReconcileCreatePVCDoesNotRestartAfterScratchVolumeIsLost guards the "data uploaded, never handed
// over, then lost" terminal row: UploadFinished=True with the scratch PVC gone and the target PVC still
// Pending must fail terminally rather than silently starting a fresh upload that recreates bytes nobody
// asked for again.
func TestReconcileCreatePVCDoesNotRestartAfterScratchVolumeIsLost(t *testing.T) {
	t.Parallel()

	di := createPVCImport("scratch-lost", common.ServerStateFinished)
	targetPVC := createPVCFlowTestTargetPVC(di, nil) // Pending, no scratch PVC seeded

	objs := []client.Object{di, targetPVC, exporterImageConfigMap(createPVCFlowControllerNamespace)}
	r, c := newCreatePVCFlowReconciler(t, objs)

	_, err := r.Reconcile(context.Background(), diReq(di))
	require.NoError(t, err)

	got := &dev1alpha1.DataImport{}
	require.NoError(t, c.Get(context.Background(), diReq(di).NamespacedName, got))
	assert.Equal(t, string(common.PhaseFailed), got.Status.Phase)
	ready := meta.FindStatusCondition(got.Status.Conditions, string(common.ConditionReady))
	require.NotNil(t, ready)
	assert.Contains(t, ready.Message, targetPVC.Name)

	scratchList := &corev1.PersistentVolumeClaimList{}
	require.NoError(t, c.List(context.Background(), scratchList, client.InNamespace(createPVCFlowControllerNamespace)))
	assert.Empty(t, scratchList.Items, "no scratch PVC must be recreated for data that was never handed over")

	deployList := &appsv1.DeploymentList{}
	require.NoError(t, c.List(context.Background(), deployList))
	assert.Empty(t, deployList.Items)
}

// TestReconcileCreatePVCAdoptsALegacyTargetPVC covers the no-migration-code claim end to end: a target PVC
// that already carries a foreign populate-target-protection finalizer alongside ours, plus an existing
// importer Deployment mounting a legacy prime-<uid> claim, is adopted -- the ownership guard passes on the
// PVC's storage-manager annotations alone, the Deployment is updated to mount our own scratch PVC, and the
// flow proceeds. The foreign finalizer is asserted to survive untouched, pinning the accepted leak as
// deliberate rather than a missing feature.
func TestReconcileCreatePVCAdoptsALegacyTargetPVC(t *testing.T) {
	t.Parallel()

	const legacyFinalizer = "storage-foundation.deckhouse.io/populate-target-protection"

	di := createPVCImport("adopt-legacy", "")
	targetPVC := createPVCFlowTestTargetPVC(di, func(pvc *corev1.PersistentVolumeClaim) {
		pvc.Finalizers = append(pvc.Finalizers, legacyFinalizer)
	})

	names := common.NewNames(dev1alpha1.KindPVC, di.Name, di.Namespace, di.Name)
	legacyDeploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      names.DeployName,
			Namespace: createPVCFlowControllerNamespace,
			Labels:    map[string]string{dev1alpha1.LabelApplicationKey: dev1alpha1.LabelDataImportValue},
			Annotations: map[string]string{
				dev1alpha1.AnnotationStorageManagerNamespaceKey: di.Namespace,
				dev1alpha1.AnnotationStorageManagerNameKey:      di.Name,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: common.Int32Ptr(1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
				dev1alpha1.LabelApplicationKey:                  dev1alpha1.LabelDataImportValue,
				dev1alpha1.LabelStorageManagerDeploymentNameKey: names.DeployName,
			}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					dev1alpha1.LabelApplicationKey:                  dev1alpha1.LabelDataImportValue,
					dev1alpha1.LabelStorageManagerDeploymentNameKey: names.DeployName,
				}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "server", Image: "legacy-image"}},
					Volumes: []corev1.Volume{{
						Name:         importerVolumeName,
						VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "prime-legacy-uid"}},
					}},
				},
			},
		},
	}

	sc := createPVCFlowTestImmediateStorageClass(*targetPVC.Spec.StorageClassName)
	objs := []client.Object{di, targetPVC, legacyDeploy, sc, exporterImageConfigMap(createPVCFlowControllerNamespace)}
	r, c := newCreatePVCFlowReconciler(t, objs, pendingOnPVCCreateInterceptor())

	_, err := r.Reconcile(context.Background(), diReq(di))
	require.NoError(t, err)

	gotDeploy := &appsv1.Deployment{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: createPVCFlowControllerNamespace, Name: names.DeployName}, gotDeploy))
	require.Len(t, gotDeploy.Spec.Template.Spec.Volumes, 1)
	require.NotNil(t, gotDeploy.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim)
	assert.Equal(t, names.ImportScratchPVCName, gotDeploy.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName,
		"the legacy Deployment must be adopted and re-pointed at our own scratch PVC")

	gotPVC := &corev1.PersistentVolumeClaim{}
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: targetPVC.Name}, gotPVC))
	assert.Contains(t, gotPVC.Finalizers, legacyFinalizer, "the accepted leak is left untouched -- neither removed nor re-added")
	assert.Contains(t, gotPVC.Finalizers, dev1alpha1.StorageManagerFinalizerName)
}
