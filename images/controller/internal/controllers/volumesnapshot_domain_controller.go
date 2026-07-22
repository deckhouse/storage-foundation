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

package controllers

import (
	"context"
	"fmt"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	ssv1alpha1 "github.com/deckhouse/state-snapshotter/api/v1alpha1"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk"
	sfsnapv1 "github.com/deckhouse/storage-foundation/api/snapshot/v1"
	"github.com/deckhouse/storage-foundation/images/controller/pkg/config"
)

const (
	// labelForkProcessed is stamped by the patched external-snapshotter fork (syncUnreadySnapshot) on
	// every VolumeSnapshot it newly reconciles. Its presence is the discriminator that separates NEW,
	// module-managed VolumeSnapshots (adopt as a state-snapshotter domain snapshot) from pre-existing /
	// legacy ones created before this module (never carry the label — left entirely untouched). Import-mode
	// VolumeSnapshots never reach syncUnreadySnapshot and are never labeled.
	labelForkProcessed = "storage-foundation.deckhouse.io/processed"

	// labelSnapshotManaged latches the adoption outcome of the exclude veto, evaluated ONCE on the source
	// PVC at adoption time (design §11.3). "true" => domain-captured (MCR + protocol); "false" => vetoed
	// (a plain CSI snapshot is still created by the fork, but no domain capture happens). It is
	// deliberately scoped to the state-snapshotter group so it does not collide with the storage-foundation
	// "managed" ANNOTATION (snapshotmeta.AnnDeckhouseManaged), which has unrelated CSI-skip semantics.
	labelSnapshotManaged = storagev1alpha1.APIGroup + "/managed"

	managedValueTrue  = "true"
	managedValueFalse = "false"

	// volumeSnapshotDomainRequeueAfter is the in-flight capture poll fallback (the domain also wakes on the
	// core's status writes via the VolumeSnapshot watch).
	volumeSnapshotDomainRequeueAfter = 500 * time.Millisecond

	// volumeSnapshotDomainArtifactRequeueAfter backs off while a required artifact (the source PVC) is
	// missing but may still appear.
	volumeSnapshotDomainArtifactRequeueAfter = 5 * time.Second
)

// VolumeSnapshotDomainReconciler drives state-snapshotter DOMAIN planning for module-managed CSI
// VolumeSnapshots: source-PVC resolution, the exclude-veto adoption latch, the per-snapshot manifest
// capture (MCR over the source PVC), sourceRef publication, and the planning barrier. All Kubernetes
// transport (MCR, owner references, optimistic-locked status patches, the barrier phase) is delegated to
// the snapshot SDK (pkg/snapshotsdk). It never touches the cluster-scoped SnapshotContent (owned by the
// core GenericSnapshotBinderController) and never creates a VolumeCaptureRequest (the data leg is the
// native CSI VolumeSnapshotContent).
type VolumeSnapshotDomainReconciler struct {
	// Client is the manager's cached client, used for source-PVC reads (corev1 lives in the manager scheme).
	Client client.Client
	// APIReader is the manager's uncached reader (reserved for TOCTOU-safe reads).
	APIReader client.Reader
	// SnapClient is a DEDICATED, cache-less client whose scheme maps snapshot.storage.k8s.io/v1
	// VolumeSnapshot to the Deckhouse-extended type and registers the state-snapshotter
	// ManifestCaptureRequest. It MUST NOT be the manager client (whose default scheme maps the same GVK to
	// the upstream external-snapshotter type). All VolumeSnapshot reads/writes and MCR creation go through
	// it, so the SDK sees the extended protocol status fields.
	SnapClient client.Client
	Config     *config.Options
}

// AddVolumeSnapshotDomainControllerToManager wires the VolumeSnapshot domain reconciler. It builds the
// dedicated scheme + cache-less client for the extended type and registers a watch on the upstream CSI
// VolumeSnapshot (present in the manager scheme) so the reconciler wakes on both spec changes and the
// core's status writes.
func AddVolumeSnapshotDomainControllerToManager(mgr ctrl.Manager, cfg *config.Options) error {
	extScheme := apiruntime.NewScheme()
	if err := sfsnapv1.AddToScheme(extScheme); err != nil {
		return fmt.Errorf("register extended VolumeSnapshot into dedicated scheme: %w", err)
	}
	if err := ssv1alpha1.AddToScheme(extScheme); err != nil {
		return fmt.Errorf("register ManifestCaptureRequest into dedicated scheme: %w", err)
	}

	snapClient, err := client.New(mgr.GetConfig(), client.Options{
		Scheme: extScheme,
		Mapper: mgr.GetRESTMapper(),
	})
	if err != nil {
		return fmt.Errorf("build dedicated snapshot client: %w", err)
	}

	reconciler := &VolumeSnapshotDomainReconciler{
		Client:     mgr.GetClient(),
		APIReader:  mgr.GetAPIReader(),
		SnapClient: snapClient,
		Config:     cfg,
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("volumesnapshot-domain").
		For(&snapshotv1.VolumeSnapshot{}).
		Complete(reconciler)
}

func (r *VolumeSnapshotDomainReconciler) capture() snapshotsdk.CaptureSDK {
	return snapshotsdk.New(r.SnapClient, r.SnapClient, snapshotsdk.NewStorageFoundationProvider(r.SnapClient))
}

func (r *VolumeSnapshotDomainReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("volumeSnapshot", req.NamespacedName)
	ctx = log.IntoContext(ctx, logger)

	vs := &sfsnapv1.VolumeSnapshot{}
	if err := r.SnapClient.Get(ctx, req.NamespacedName, vs); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Deletion is handled by the CSI lifecycle / ObjectKeeper (no finalizers here).
	if vs.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	// (1) Fork-label discriminator: only NEW, module-managed VolumeSnapshots are domain objects.
	if vs.Labels[labelForkProcessed] == "" {
		logger.V(1).Info("skip: VolumeSnapshot is not fork-managed (no processed label)")
		return ctrl.Result{}, nil
	}

	// (2) Import mode (spec.mode: Import — parity with every other snapshot kind): capture is off (the
	// source PVC may be absent); the core materializes content from uploaded manifests. Domain planning
	// is trivially complete.
	if vs.Spec.Mode == sfsnapv1.VolumeSnapshotModeImport {
		logger.V(1).Info("skip: import-mode VolumeSnapshot")
		return ctrl.Result{}, nil
	}

	// (3) Pre-provisioned (static) binding: no live PVC source to capture.
	pvcName := vs.Spec.Source.PersistentVolumeClaimName
	if pvcName == nil || *pvcName == "" {
		logger.V(1).Info("skip: pre-provisioned VolumeSnapshot (no source PVC)")
		return ctrl.Result{}, nil
	}

	// (4) Adoption veto latch: evaluate the exclude veto on the source PVC exactly once and record the
	// outcome. A vetoed snapshot stays a plain CSI snapshot with no domain capture.
	managed, err := r.ensureManagedLatch(ctx, vs, *pvcName)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !managed {
		logger.V(1).Info("skip: VolumeSnapshot vetoed by exclude label on source PVC")
		return ctrl.Result{}, nil
	}

	// (5) Capture protocol (manifest leg only; data leg is the native CSI VolumeSnapshotContent).
	adapter := volumeSnapshotAdapter{snap: vs}
	sdk := r.capture()

	pvc := &corev1.PersistentVolumeClaim{}
	if getErr := r.Client.Get(ctx, types.NamespacedName{Namespace: vs.Namespace, Name: *pvcName}, pvc); getErr != nil {
		if !apierrors.IsNotFound(getErr) {
			return ctrl.Result{}, getErr
		}
		// The source PVC disappeared after adoption: surface an actionable Ready=False (it may reappear).
		if perr := sdk.Reject(ctx, adapter, snapshotsdk.FailSpec{
			Reason:  snapshotsdk.Reason(storagev1alpha1.ReasonArtifactMissing),
			Message: fmt.Sprintf("source PersistentVolumeClaim %q not found", *pvcName),
			Requeue: true,
		}); perr != nil {
			return ctrl.Result{}, perr
		}
		return ctrl.Result{RequeueAfter: volumeSnapshotDomainArtifactRequeueAfter}, nil
	}

	// Publish the captured source PVC's full reference (top-level status.sourceRef) so d8-cli can
	// rebuild the import-mode source. Not part of the readiness formula.
	if err := sdk.PublishSnapshotSource(ctx, adapter, snapshotsdk.SnapshotSource{
		APIVersion: corev1.SchemeGroupVersion.String(),
		Kind:       "PersistentVolumeClaim",
		Name:       pvc.Name,
		Namespace:  pvc.Namespace,
		UID:        pvc.UID,
	}); err != nil {
		return ctrl.Result{}, err
	}

	// Manifest leg: capture the source PVC object. The data leg is the native CSI VolumeSnapshotContent
	// (observed and latched by the core), so there is no EnsureVolumeCapture / VolumeCaptureRequest here.
	if err := sdk.EnsureManifestCapture(ctx, adapter, snapshotsdk.ManifestCaptureSpec{Targets: []snapshotsdk.ManifestTarget{{
		APIVersion: corev1.SchemeGroupVersion.String(),
		Kind:       "PersistentVolumeClaim",
		Name:       pvc.Name,
	}}}); err != nil {
		return ctrl.Result{}, err
	}

	// Barrier 1 (Planned): for a data leaf, planning is complete once its MCR is created and published.
	if err := sdk.MarkPlanned(ctx, adapter); err != nil {
		return ctrl.Result{}, err
	}

	// Barrier 2 (Finished): switch on the SDK-derived capture outcome. A VolumeSnapshot is a data leaf, so
	// it confirms consistency immediately once all declared legs (manifest + native-CSI data) are captured.
	switch outcome := snapshotsdk.CoreCaptureOutcome(adapter); outcome.Outcome {
	case snapshotsdk.CaptureOutcomeCaptured:
		return ctrl.Result{}, sdk.ConfirmConsistent(ctx, adapter)
	case snapshotsdk.CaptureOutcomeFailed:
		return ctrl.Result{}, sdk.Reject(ctx, adapter, snapshotsdk.FailSpec{
			Reason:  snapshotsdk.Reason(outcome.Reason),
			Message: outcome.Message,
		})
	default:
		return ctrl.Result{RequeueAfter: volumeSnapshotDomainRequeueAfter}, nil
	}
}

// ensureManagedLatch returns whether the VolumeSnapshot is domain-managed, latching the exclude-veto
// outcome on first observation. The veto is checked on the SOURCE PVC's exclude label. A missing PVC at
// adoption cannot be vetoed by an absent label, so it latches managed=true and lets the capture leg
// surface a recoverable ArtifactMissing instead. The latch is monotonic (evaluated exactly once).
func (r *VolumeSnapshotDomainReconciler) ensureManagedLatch(ctx context.Context, vs *sfsnapv1.VolumeSnapshot, pvcName string) (bool, error) {
	if v, ok := vs.Labels[labelSnapshotManaged]; ok {
		return v == managedValueTrue, nil
	}

	vetoed := false
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: vs.Namespace, Name: pvcName}, pvc); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, err
		}
	} else if _, excluded := pvc.Labels[snapshotsdk.ExcludeLabelKey]; excluded {
		vetoed = true
	}

	desired := managedValueTrue
	if vetoed {
		desired = managedValueFalse
	}

	effective, err := r.latchManagedLabel(ctx, client.ObjectKeyFromObject(vs), desired)
	if err != nil {
		return false, err
	}
	if vs.Labels == nil {
		vs.Labels = map[string]string{}
	}
	vs.Labels[labelSnapshotManaged] = effective
	return effective == managedValueTrue, nil
}

// latchManagedLabel writes the managed label under optimistic lock, but only if it is not already set
// (monotonic). It returns the EFFECTIVE label value (a concurrent writer's value wins if it latched first).
func (r *VolumeSnapshotDomainReconciler) latchManagedLabel(ctx context.Context, key client.ObjectKey, desired string) (string, error) {
	var effective string
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur := &sfsnapv1.VolumeSnapshot{}
		if err := r.SnapClient.Get(ctx, key, cur); err != nil {
			return err
		}
		if v, ok := cur.Labels[labelSnapshotManaged]; ok {
			effective = v
			return nil
		}
		base := cur.DeepCopy()
		if cur.Labels == nil {
			cur.Labels = map[string]string{}
		}
		cur.Labels[labelSnapshotManaged] = desired
		// A VolumeSnapshot adopted as a domain-managed node (managed=true) is a protected tree node: stamp
		// the authoritative delete-protection state in the SAME patch that latches adoption, i.e. before any
		// graph edge is published (delete-protection-contract.md §6.1/§8.1). A vetoed (managed=false)
		// VolumeSnapshot is a plain CSI snapshot, not a tree node, and MUST NOT be protected.
		if desired == managedValueTrue {
			storagev1alpha1.StampDeleteProtected(cur)
		}
		if err := r.SnapClient.Patch(ctx, cur, client.MergeFrom(base)); err != nil {
			return err
		}
		effective = desired
		return nil
	})
	return effective, err
}
