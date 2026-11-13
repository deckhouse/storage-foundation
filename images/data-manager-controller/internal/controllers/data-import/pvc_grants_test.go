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
	corev1 "k8s.io/api/core/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
)

const grantsDenial = `admission webhook "cluster-objects-grants-validator.multitenancy.deckhouse.io" denied the request: ` +
	`[multitenancy] PersistentVolumeClaim "target" references "restricted" which is not available to project "team". ` +
	`Ask the cluster administrator to grant it.`

func TestNeedsPVCSpecUpdate_UnsetStorageClassAcceptsTheAssignedClass(t *testing.T) {
	q := resource.MustParse("1Gi")
	assigned := "project-default"
	existing := pvcRequesting(q, func(p *corev1.PersistentVolumeClaim) { p.Spec.StorageClassName = &assigned })

	assert.False(t, needsPVCSpecUpdate(existing, pvcRequesting(q)),
		"a template without a class must accept the class the admission chain assigned")
}

func TestReconcileCreatePVCReportsADeniedTargetPVC(t *testing.T) {
	t.Parallel()

	di := createPVCImport("denied-target", "")
	class := "restricted"
	di.Spec.PvcTemplate.StorageClassName = &class

	creates := 0
	deny := interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if pvc, ok := obj.(*corev1.PersistentVolumeClaim); ok && pvc.Namespace == di.Namespace {
				creates++
				return kubeerrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumeclaims"}, pvc.Name, errors.New(grantsDenial))
			}
			return cl.Create(ctx, obj, opts...)
		},
	}
	objs := []client.Object{di, exporterImageConfigMap(createPVCFlowControllerNamespace)}
	r, c := newCreatePVCFlowReconciler(t, objs, deny)

	_, err := r.Reconcile(context.Background(), diReq(di))
	require.Error(t, err, "a denied target PVC is retried: the policy can still grant the class")
	assert.Equal(t, 1, creates)

	got := &dev1alpha1.DataImport{}
	require.NoError(t, c.Get(context.Background(), diReq(di).NamespacedName, got))
	ready := meta.FindStatusCondition(got.Status.Conditions, string(common.ConditionReady))
	require.NotNil(t, ready, "the denial must be reported on the DataImport")
	assert.Equal(t, string(common.ReasonTargetFailed), ready.Reason)
	assert.Contains(t, ready.Message, "not available to project")
	assert.NotEqual(t, string(common.PhaseFailed), got.Status.Phase)
}

func TestReconcileCreatePVCKeepsTheClassTheAdmissionChainAssigned(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		requested *string
	}{
		{name: "template without a class", requested: nil},
		{name: "template naming a class the project lost", requested: ptrTo("revoked")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			di := createPVCImport("assigned-class", "")
			di.Spec.PvcTemplate.StorageClassName = tc.requested
			assigned := "project-default"
			target := createPVCFlowTestTargetPVC(di, func(pvc *corev1.PersistentVolumeClaim) {
				pvc.Spec.StorageClassName = &assigned
			})

			updates := 0
			countUpdates := interceptor.Funcs{
				Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
					if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
						updates++
					}
					return cl.Update(ctx, obj, opts...)
				},
				Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if pvc, ok := obj.(*corev1.PersistentVolumeClaim); ok {
						pvc.Status.Phase = corev1.ClaimPending
					}
					return cl.Create(ctx, obj, opts...)
				},
			}
			objs := []client.Object{di, target, createPVCFlowTestImmediateStorageClass(assigned), exporterImageConfigMap(createPVCFlowControllerNamespace)}
			r, c := newCreatePVCFlowReconciler(t, objs, countUpdates)

			for range 3 {
				_, err := r.Reconcile(context.Background(), diReq(di))
				require.NoError(t, err)
			}
			assert.Zero(t, updates, "the target PVC must keep the class it was admitted with")

			live := &corev1.PersistentVolumeClaim{}
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: di.Namespace, Name: target.Name}, live))
			require.NotNil(t, live.Spec.StorageClassName)
			assert.Equal(t, assigned, *live.Spec.StorageClassName)

			scratch := &corev1.PersistentVolumeClaim{}
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: createPVCFlowControllerNamespace, Name: r.names.ImportScratchPVCName}, scratch))
			require.NotNil(t, scratch.Spec.StorageClassName)
			assert.Equal(t, assigned, *scratch.Spec.StorageClassName, "the scratch volume follows the class of the target PVC")
		})
	}
}

func ptrTo[T any](v T) *T { return &v }
