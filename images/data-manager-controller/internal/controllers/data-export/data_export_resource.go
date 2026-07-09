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
	"errors"
	"fmt"
	"log"
	"reflect"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	kubeerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	// dev1alpha1 "hello-world/api/v1alpha1"
	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	. "github.com/deckhouse/storage-foundation/common"
	"github.com/deckhouse/storage-foundation/common/config"
	"github.com/deckhouse/storage-foundation/common/publish"
	virtv1alpha2 "github.com/deckhouse/virtualization/api/core/v1alpha2"
	"github.com/deckhouse/virtualization/api/core/v1alpha2/vdcondition"
)

type DataexportReconciler struct {
	Client client.Client
	Reader client.Reader
	Config *config.Options
	// Dynamic and RESTMapper drive the resource-agnostic snapshot export path (C6): the target leaf is
	// any registered snapshot CR addressed by GroupKind, resolved to its SnapshotContent.dataRef
	// without compiling in domain types.
	Dynamic    dynamic.Interface
	RESTMapper meta.RESTMapper
}

// pvRecoveryInfo holds validated PV annotation data needed for orphan cleanup and idempotency checks.
type pvRecoveryInfo struct {
	UserPVCNamespace      string
	UserPVCName           string
	DataExportNamespace   string
	DataExportName        string
	TargetKindShort       string
	HashSuffix            string
	OriginalReclaimPolicy corev1.PersistentVolumeReclaimPolicy
}

const (
	DataExportInProgressKey        = "storage-foundation.deckhouse.io/data-export-in-progress"
	DataExportRequestAnnotationKey = "storage-foundation.deckhouse.io/data-export-request"

	SeverityWarning = "warning"
	SeverityError   = "error"
)

// Sentinel errors are used as typed markers so callers can distinguish failure
// categories with errors.Is without parsing message strings.
// mutateReadyByErr maps each sentinel to the corresponding Ready condition reason,
// making controller behavior explicit and testable.
var (
	ErrTargetNotFound   = errors.New("target not found")
	ErrTargetNotReady   = errors.New("target not ready")
	ErrPVConflict       = errors.New("pv conflict")
	ErrDeploymentFailed = errors.New("deployment failed")
	ErrCleanupFailed    = errors.New("cleanup failed")
)

func (r *DataexportReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, err error) {
	log.Printf("Start reconciling DataExport resource: %s/%s\n", req.Namespace, req.Name)

	dataExport := &dev1alpha1.DataExport{}
	err = r.Client.Get(ctx, req.NamespacedName, dataExport)
	if err != nil {
		if kubeerrors.IsNotFound(err) {
			// DataExport resource not found, it may have been deleted after the event was received.
			log.Printf("DataExport resource %s/%s not found, checking for orphaned resources\n", req.Namespace, req.Name)
			return ctrl.Result{}, r.removeOrphanResources(ctx, req.Namespace, req.Name)
		}
		return ctrl.Result{}, fmt.Errorf("failed to get DataExport resource from cache: %w", err)
	}

	// Copy the original state before any mutations.
	// The reconcile body mutates dataExport in-memory throughout the entire cycle
	// (conditions, finalizers, status fields, etc.) without persisting intermediate states.
	// The deferred function collects all mutations at the end:
	//   1) mutateReadyByErr - translates reconcile errors into Ready condition reasons
	//   2) updateDataExport - diffs against the original snapshot and persists only real changes
	dataExportOrig := dataExport.DeepCopy()
	defer func() {
		mutateReadyByErr(dataExport, err)
		updateErr := r.updateDataExport(ctx, dataExportOrig, dataExport)
		// Preserve the original reconcile error while also surfacing the update failure.
		// controller-runtime requeues on any non-nil error, so both failures are retried.
		err = errors.Join(err, updateErr)
	}()

	err = r.validateDataExportSpec(ctx, dataExport)
	if err != nil {
		log.Printf("DataExport resource %s/%s spec validation failed: %v\n", req.Namespace, req.Name, err)
		meta.SetStatusCondition(&dataExport.Status.Conditions, metav1.Condition{
			Type:               string(ConditionReady),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: dataExport.Generation,
			Reason:             string(ReasonValidationFailed),
			Message:            err.Error(),
		})
		// Return nil (no requeue): spec validation failure is permanent until the user
		// corrects the spec. The next reconcile will be triggered by a spec change event.
		return ctrl.Result{}, nil
	}

	// Resolve the GroupKind targetRef to a stable short kind (pvc/vd/snap) used for deterministic
	// resource naming and orphan recovery. Classification failures (e.g. a bare VolumeSnapshotContent)
	// are permanent spec errors, surfaced like validation failures (no requeue).
	_, targetKindShort, classifyErr := classifyTargetRef(dataExport.Spec.TargetRef.Group, dataExport.Spec.TargetRef.Kind)
	if classifyErr != nil {
		log.Printf("DataExport resource %s/%s targetRef invalid: %v\n", req.Namespace, req.Name, classifyErr)
		meta.SetStatusCondition(&dataExport.Status.Conditions, metav1.Condition{
			Type:               string(ConditionReady),
			Status:             metav1.ConditionFalse,
			ObservedGeneration: dataExport.Generation,
			Reason:             string(ReasonValidationFailed),
			Message:            classifyErr.Error(),
		})
		return ctrl.Result{}, nil
	}
	generatedNames := NewNamesFromShort(targetKindShort, dataExport.Spec.TargetRef.Name, dataExport.Namespace, dataExport.Name)

	// Case 1: Resource marked for delete
	if dataExport.DeletionTimestamp != nil {
		log.Printf("Case 2: DataExport resource marked for delete")
		err := r.clearDataExportProviding(ctx, dataExport, generatedNames)
		if err != nil {
			if errors.Is(err, ErrInvalidOriginalReclaimPolicy) {
				// PV was labeled as inconsistent (missing or corrupted original reclaimPolicy).
				// Stop reconcile without error or requeue — finalizer stays, admin must investigate.
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, fmt.Errorf("%w: failed to restore configuration before DE: %w", ErrCleanupFailed, err)
		}
		return ctrl.Result{}, nil
	}

	// Case 2: DataExport pod time-to-live has expired
	if meta.IsStatusConditionTrue(dataExport.Status.Conditions, string(ConditionExpired)) {
		log.Printf("Case 2: DataExport pod time-to-live has expired")
		readyCond := meta.FindStatusCondition(dataExport.Status.Conditions, string(ConditionReady))
		// Guard against redundant condition updates: if Ready is already Expired, skip the
		// SetStatusCondition call to avoid a spurious lastTransitionTime bump on every reconcile.
		if readyCond != nil && readyCond.Reason != string(ReasonExpired) {
			meta.SetStatusCondition(&dataExport.Status.Conditions, metav1.Condition{
				Type:               string(ConditionReady),
				Status:             metav1.ConditionFalse,
				Reason:             string(ReasonExpired),
				Message:            "DataExport time to live expired. Please, delete it manually",
				ObservedGeneration: dataExport.Generation,
			})
		}
		err = r.clearDataExportProviding(ctx, dataExport, generatedNames)
		if err != nil {
			if errors.Is(err, ErrInvalidOriginalReclaimPolicy) {
				// PV was labeled as inconsistent (missing or corrupted original reclaimPolicy).
				// Stop reconcile without error or requeue — finalizer stays, admin must investigate.
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, fmt.Errorf("%w: failed to restore configuration before DE: %w", ErrCleanupFailed, err)
		}
		return ctrl.Result{}, nil
	}

	// Case 3: Newly create DataExport resource (has no Condition with type Ready)
	readyCond := meta.FindStatusCondition(dataExport.Status.Conditions, string(ConditionReady))
	switch {
	case readyCond == nil:
		log.Printf("Case 3: DataExport resource newly created")
		// Initialize both conditions so downstream cases (Expired check, mutateReadyByErr)
		// always have a defined state to work with. The finalizer is added here to ensure
		// clearDataExportProviding runs on deletion even if the controller restarts
		// before implementation is complete.
		meta.SetStatusCondition(&dataExport.Status.Conditions, metav1.Condition{
			Type:               string(ConditionReady),
			Status:             metav1.ConditionFalse,
			Reason:             string(ReasonPending),
			Message:            "Started",
			ObservedGeneration: dataExport.Generation,
		})
		meta.SetStatusCondition(&dataExport.Status.Conditions, metav1.Condition{
			Type:               string(ConditionExpired),
			Status:             metav1.ConditionFalse,
			Reason:             string(ReasonPending),
			Message:            "TTL not expired",
			ObservedGeneration: dataExport.Generation,
		})
		EnsureFinalizer(ctx, r.Client, dataExport, dev1alpha1.StorageManagerFinalizerName)
	// Case 4: DataExport resource needs to initial or continue implementation
	case readyCond.Status != metav1.ConditionTrue:
		log.Printf("Case 4: DataExport resource needs to initial or continue implementation")
		err = r.implementDataExportProviding(ctx, dataExport, generatedNames)
		if err != nil {
			if errors.Is(err, ErrPVCValidationFailed) {
				meta.SetStatusCondition(&dataExport.Status.Conditions, metav1.Condition{
					Type:               string(ConditionReady),
					Status:             metav1.ConditionFalse,
					Reason:             string(ReasonValidationFailed),
					Message:            err.Error(),
					ObservedGeneration: dataExport.Generation,
				})
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}
			return ctrl.Result{}, err
		}

	// Case 5: DataExport resource already implemented
	default:
		if err := r.reconcilePodReadyResources(ctx, dataExport, generatedNames); err != nil {
			return ctrl.Result{}, err
		}
		log.Printf("Case 5: DataExport resource providing already implemented")
	}

	return ctrl.Result{}, nil
}

// mutateReadyByErr maps known reconcile errors to Ready condition reasons.
// Called in defer after the reconcile body — translates sentinel errors (e.g. ErrTargetNotFound)
// into user-visible condition reasons without persisting; the caller (updateDataExport) handles persistence.
func mutateReadyByErr(dataExport *dev1alpha1.DataExport, reconcileErr error) {
	if reconcileErr == nil || dataExport == nil {
		return
	}

	// Keep terminal status stable in TTL/deletion cleanup flows.
	// Otherwise Ready can oscillate between Expired and CleanupFailed on each reconcile.
	if errors.Is(reconcileErr, ErrCleanupFailed) {
		readyCond := meta.FindStatusCondition(dataExport.Status.Conditions, string(ConditionReady))
		if readyCond != nil &&
			(readyCond.Reason == string(ReasonExpired) || readyCond.Reason == string(ReasonDeleted)) {
			return
		}
	}

	var reason ConditionReason
	switch {
	case errors.Is(reconcileErr, ErrCleanupFailed):
		reason = ReasonCleanupFailed
	case errors.Is(reconcileErr, ErrTargetNotFound):
		reason = ReasonTargetNotFound
	case errors.Is(reconcileErr, ErrPVConflict):
		reason = ReasonPVConflict
	case errors.Is(reconcileErr, ErrTargetNotReady):
		reason = ReasonTargetNotReady
	case errors.Is(reconcileErr, ErrDeploymentFailed):
		reason = ReasonDeploymentFailed
	default:
		// Keep status unchanged for unknown/transient errors.
		return
	}

	meta.SetStatusCondition(&dataExport.Status.Conditions, metav1.Condition{
		Type:               string(ConditionReady),
		Status:             metav1.ConditionFalse,
		Reason:             string(reason),
		Message:            reconcileErr.Error(),
		ObservedGeneration: dataExport.Generation,
	})
}

// updateDataExport persists all accumulated in-memory mutations (spec, metadata, status) in a single
// deferred call at the end of Reconcile. Splits the write into two API calls: one for the main object
// (spec/finalizers/labels/annotations) and one for the status subresource, because Kubernetes treats
// them as independent resources. Uses deStatusCopy to preserve status across the main Update call,
// since the API server response overwrites the in-memory status with the server-side value.
func (r *DataexportReconciler) updateDataExport(ctx context.Context, dataExportOld, dataExportNew *dev1alpha1.DataExport) error {
	if dataExportNew == nil || dataExportOld == nil {
		return nil
	}

	needStatusUpdate := false
	needUpdate := false
	var deStatusCopy *dev1alpha1.DataExportImportStatus

	// Detect spec/metadata changes by comparing against the pre-reconcile copy
	if !reflect.DeepEqual(dataExportOld.Spec, dataExportNew.Spec) ||
		!reflect.DeepEqual(dataExportOld.Finalizers, dataExportNew.Finalizers) ||
		!reflect.DeepEqual(dataExportOld.Labels, dataExportNew.Labels) ||
		!reflect.DeepEqual(dataExportOld.Annotations, dataExportNew.Annotations) {
		needUpdate = true
	}

	// Save status before the main Update: the API server response will overwrite in-memory status
	if !reflect.DeepEqual(dataExportOld.Status, dataExportNew.Status) {
		needStatusUpdate = true
		deStatusCopy = dataExportNew.Status.DeepCopy()
	}

	// Persist spec/metadata (non-status fields); updates ResourceVersion in dataExportNew
	if needUpdate {
		err := r.Client.Update(ctx, dataExportNew)
		if err != nil {
			return fmt.Errorf("update DataExport failed: %w", err)
		}
	}

	// Restore the saved status (overwritten by Update response) and persist via status subresource
	if needStatusUpdate {
		dataExportNew.Status = *deStatusCopy
		err := r.Client.Status().Update(ctx, dataExportNew)
		if err != nil {
			return fmt.Errorf("update DataExport status failed: %w", err)
		}
	}

	return nil
}

func (r *DataexportReconciler) implementDataExportProviding(ctx context.Context, dataExport *dev1alpha1.DataExport, generatedNames Names) error {
	log.Printf("Start realizing DE for resource %s, userPVC: %s, ttl: %s", dataExport.Name, dataExport.Spec.TargetRef.Name, dataExport.Spec.Ttl)

	var exportPVC *corev1.PersistentVolumeClaim
	// pv is fetched once during exportPVC creation and passed through to ensureExportPVReady
	// to avoid a redundant Get call. It is nil when exportPVC already exists (idempotent restart),
	// in which case ensureExportPVReady fetches PV from exportPVC.Spec.VolumeName.
	var pv *corev1.PersistentVolume
	var userPVCName string

	var err error
	exportPVC, err = r.validateExportPVC(ctx, dataExport, generatedNames.ExportPVCName)
	if err != nil {
		return err
	}

	if exportPVC == nil {
		log.Printf("Export PVC %s not found for DataExport resource %s, creating new one", generatedNames.ExportPVCName, dataExport.Name)

		switch generatedNames.TargetKindShort {
		case dev1alpha1.KindPVCShort:
			log.Printf("Export target kind: %s", generatedNames.TargetKindShort)
			userPVCName = dataExport.Spec.TargetRef.Name
			exportPVC, pv, err = r.getExportPVCFromUserPVC(ctx, dataExport.Namespace, userPVCName, generatedNames.ExportPVCName, dataExport.Name)
			if err != nil {
				return fmt.Errorf("failed to process user PVC export: %w", err)
			}
		case dev1alpha1.KindVirtualDiskShort:
			log.Printf("Export target kind: %s", generatedNames.TargetKindShort)
			exportPVC, pv, userPVCName, err = r.getExportPVCFromUserVirtualDisk(ctx, dataExport, generatedNames)
			if err != nil {
				return fmt.Errorf("failed to process user VirtualDisk export: %w", err)
			}

		case dev1alpha1.KindSnapshotShort:
			// Resource-agnostic snapshot path (C6): any namespaced snapshot leaf (generic VolumeSnapshot,
			// VirtualDiskSnapshot, domain snapshot, ...) is exported the same way — resolve the leaf's
			// SnapshotContent.dataRef and provision the export PVC from the durable artifact via a VRR.
			log.Printf("Export target kind: %s", generatedNames.TargetKindShort)
			exportPVC, err = r.getExportPVCFromSnapshot(ctx, dataExport, generatedNames)
			if err != nil {
				return fmt.Errorf("failed to process snapshot export: %w", err)
			}
		default:
			return fmt.Errorf("unknown export kind: %s", generatedNames.TargetKindShort)
		}

		log.Printf("Export PVC %s created for DataExport resource %s", exportPVC.GetName(), dataExport.Name)
	}

	// TODO: refactor this
	if userPVCName == "" {
		userPVCName, err = r.resolveUserPVCName(ctx, dataExport, generatedNames.TargetKindShort)
		if err != nil {
			return fmt.Errorf("failed to resolve user PVC name: %w", err)
		}
	}

	// Ensure PV has all required annotations/labels for orphan recovery.
	// This is needed for idempotency if controller restarts after exportPVC creation
	// but before PV was patched with tracking annotations.
	// pv may be nil if exportPVC already existed (idempotent restart).
	// ensureExportPVReady handles this case by fetching PV from exportPVC.Spec.VolumeName.
	if err := r.ensureExportPVReady(ctx, pv, exportPVC, generatedNames, dataExport, userPVCName); err != nil {
		return err
	}

	// On an idempotent restart the export PVC comes from cache (validateExportPVC) without a VolumeMode
	// nil-check; for the snapshot path it is provisioned out-of-band by the external-provisioner, so guard
	// against a not-yet-shaped PVC and requeue instead of panicking.
	if exportPVC.Spec.VolumeMode == nil {
		return fmt.Errorf("export PVC %s has no volumeMode yet: %w", exportPVC.Name, ErrTargetNotReady)
	}
	dataExport.Status.VolumeMode = string(*exportPVC.Spec.VolumeMode)

	// create export deployment and wait for running

	var exportDeploy *appsv1.Deployment
	exportDeploy, err = r.validateExportDeploy(ctx, dataExport, generatedNames.DeployName)
	if err != nil {
		return err
	}
	if exportDeploy == nil {
		err = r.createDeployment(ctx, dataExport, exportPVC, generatedNames)
		if err != nil {
			return err
		}
	}

	if err := r.reconcilePublishResources(ctx, dataExport, generatedNames); err != nil {
		return err
	}

	return nil
}

// reconcilePodReadyResources is invoked on every reconcile iteration for a fully
// provisioned DataExport (Case 5) to keep ancillary resources in sync with the
// current spec. It re-evaluates the Publish toggle so that enabling or disabling
// external access takes effect without re-creating the DataExport object.
func (r *DataexportReconciler) reconcilePodReadyResources(ctx context.Context, dataExport *dev1alpha1.DataExport, generatedNames Names) error {
	if err := r.reconcilePublishResources(ctx, dataExport, generatedNames); err != nil {
		return err
	}

	// TODO: Validate deployment
	// TODO: Validate export PVC
	// TODO: Validate CA Secret
	// TODO: Validate PV recovery metadata for PVC/VD targets

	return nil
}

// reconcilePublishResources creates or removes the public Service and Ingress
// based on dataExport.Spec.Publish, keeping dataExport.Status.PublicURL consistent.
// When Publish is true, EnsurePublicURL is called and the resulting URL is written
// to status (skipped if the URL has not changed). When Publish is false, both
// resources are deleted and the status URL is cleared.
func (r *DataexportReconciler) reconcilePublishResources(ctx context.Context, dataExport *dev1alpha1.DataExport, generatedNames Names) error {
	serviceCfg, ingressCfg := r.makePublishConfigs(dataExport, generatedNames)

	if dataExport.Spec.Publish {
		// ensure Service and Ingress exist, get the resulting public URL
		publicURL, err := publish.EnsurePublicURL(ctx, r.Client, r.Reader, serviceCfg, ingressCfg)
		if err != nil {
			meta.SetStatusCondition(&dataExport.Status.Conditions, metav1.Condition{
				Type:               string(ConditionReady),
				Status:             metav1.ConditionFalse,
				Reason:             string(ReasonPublishFailed),
				Message:            err.Error(),
				ObservedGeneration: dataExport.Generation,
			})
			return fmt.Errorf("failed to ensure public URL: %w", err)
		}

		// skip status update if URL hasn't changed
		if dataExport.Status.PublicURL == publicURL {
			return nil
		}

		dataExport.Status.PublicURL = publicURL

		return nil
	}

	// Publish disabled: remove Service and Ingress.
	// DeletePublicResources is idempotent - not-found is not an error.
	if _, err := publish.DeletePublicResources(ctx, r.Client, serviceCfg.ServiceName, ingressCfg.IngressName); err != nil {
		return fmt.Errorf("failed to delete public resources: %w", err)
	}
	dataExport.Status.PublicURL = ""

	return nil
}

// resolveUserPVCName returns the actual user PVC name for the given DataExport.
// For PVC targets, TargetRef.Name is the PVC name directly.
// For VirtualDisk targets, TargetRef.Name is the VD name - the actual PVC name
// is resolved from VirtualDisk.Status.Target.PersistentVolumeClaim.
// For snapshot-based targets, returns empty string (no user PVC to protect).
func (r *DataexportReconciler) resolveUserPVCName(ctx context.Context, dataExport *dev1alpha1.DataExport, targetKindShort string) (string, error) {
	switch targetKindShort {
	case dev1alpha1.KindPVCShort:
		return dataExport.Spec.TargetRef.Name, nil
	case dev1alpha1.KindVirtualDiskShort:
		vd := &virtv1alpha2.VirtualDisk{}
		if err := r.Client.Get(ctx, types.NamespacedName{
			Namespace: dataExport.Namespace,
			Name:      dataExport.Spec.TargetRef.Name,
		}, vd); err != nil {
			return "", fmt.Errorf("failed to get VirtualDisk %s/%s: %w",
				dataExport.Namespace, dataExport.Spec.TargetRef.Name, err)
		}
		pvcName := vd.Status.Target.PersistentVolumeClaim
		if pvcName == "" {
			return "", fmt.Errorf("VirtualDisk %s/%s has no PVC name in status",
				dataExport.Namespace, dataExport.Spec.TargetRef.Name)
		}
		return pvcName, nil
	default:
		return "", nil
	}
}

func (r *DataexportReconciler) makePublishConfigs(dataExport *dev1alpha1.DataExport, generatedNames Names) (publish.HeadlessServiceCfg, publish.IngressCfg) {
	serviceCfg := publish.HeadlessServiceCfg{
		ServiceName:           types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: generatedNames.HeadlessServiceName},
		DeploymentName:        generatedNames.DeployName,
		LabelApplicationValue: dev1alpha1.LabelDataExportValue,
	}
	ingressCfg := publish.IngressCfg{
		IngressName:      types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: generatedNames.IngressResourceName},
		ServiceName:      types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: generatedNames.HeadlessServiceName},
		OriginIngress:    types.NamespacedName{Namespace: r.Config.OriginIngressNamespace, Name: OriginIngressName},
		TargetSecretName: IngressSecretName,
		Path:             fmt.Sprintf("/%s/%s/%s", dataExport.Namespace, generatedNames.TargetKindShort, generatedNames.TargetName),
		CorsAllowMethods: "GET, HEAD, OPTIONS",
	}

	return serviceCfg, ingressCfg
}

// ensureExportPVReady ensures that the PV backing the exportPVC has all required
// annotations and labels for orphan cleanup and tracking. This method provides
// idempotency when the controller restarts after exportPVC was created but before
// PV annotations were applied.
// If pv is nil (idempotent restart when exportPVC already exists), the PV is fetched
// from exportPVC.Spec.VolumeName.
// userPVCName is the resolved name of the actual user PVC (for VirtualDisk targets,
// this is the underlying PVC name from VD status, not the VD name).
func (r *DataexportReconciler) ensureExportPVReady(ctx context.Context, pv *corev1.PersistentVolume, exportPVC *corev1.PersistentVolumeClaim, generatedNames Names, dataExport *dev1alpha1.DataExport, userPVCName string) error {
	// Skip for snapshot-based exports (VolumeSnapshot, VirtualDiskSnapshot) because they
	// create new PVs via CSI provisioner and don't detach existing user PVCs.
	// TODO: For snapshot-based exports, add polling to wait until PV is provisioned by CSI driver.
	if generatedNames.TargetKindShort != dev1alpha1.KindPVCShort && generatedNames.TargetKindShort != dev1alpha1.KindVirtualDiskShort {
		return nil
	}

	// If PV was not passed (idempotent restart case), fetch it from exportPVC.
	if pv == nil {
		pvName := exportPVC.Spec.VolumeName
		if pvName == "" {
			return fmt.Errorf("export PVC has no volume name")
		}

		pv = &corev1.PersistentVolume{}
		if err := r.Client.Get(ctx, client.ObjectKey{Name: pvName}, pv); err != nil {
			if kubeerrors.IsNotFound(err) {
				return fmt.Errorf("export pvc volume %s not found", pvName)
			}

			log.Printf("Error getting PV %s: %v", pvName, err)
			return err
		}
	}

	if err := r.ensureUserPVCExportingAnnotationAndFinalizer(ctx, dataExport.Namespace, userPVCName); err != nil {
		return err
	}

	// Check if PV already has all required annotations with correct values.
	// The parsed result is not needed - only validation matters
	_, err := parsePVRecoveryInfo(pv, dataExport.Namespace, dataExport.Name)
	infoIsValid := err == nil

	// PV spec must also be in export-ready state:
	// ReclaimPolicy=Retain protects data, ClaimRef binds PV to exportPVC.
	specReady := pv.Spec.PersistentVolumeReclaimPolicy == corev1.PersistentVolumeReclaimRetain &&
		pv.Spec.ClaimRef != nil &&
		pv.Spec.ClaimRef.Name == exportPVC.Name &&
		pv.Spec.ClaimRef.Namespace == exportPVC.Namespace

	// If both annotations and spec are already correct, skip patching
	if infoIsValid && specReady {
		return nil
	}

	if err := r.patchPVLabelAnnotationsClaimRef(ctx, pv, exportPVC, dataExport, generatedNames, userPVCName); err != nil {
		log.Printf("failed to patch PV %s: %v", pv.Name, err)
		return err
	}

	return nil
}

func (r *DataexportReconciler) detachPVC(ctx context.Context, userPVC *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume, exportPVCName, dataExportName string) (*corev1.PersistentVolumeClaim, error) {
	// Check for conflicts before modifying PV
	if err := validatePVNotOwnedByAnotherDataExport(pv, userPVC.Namespace, dataExportName); err != nil {
		return nil, err
	}

	log.Printf("Detaching PVC %s/%s from PV %s", userPVC.Namespace, userPVC.Name, pv.Name)
	// Temporary PVC for dataExport pod
	exportPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      exportPVCName,
			Namespace: r.Config.ControllerNamespace,
			Labels: map[string]string{
				dev1alpha1.LabelApplicationKey: dev1alpha1.LabelDataExportValue,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName:       pv.GetName(),
			VolumeMode:       userPVC.Spec.VolumeMode,
			StorageClassName: userPVC.Spec.StorageClassName,
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOncePod},
			Resources:        userPVC.Spec.Resources,
		},
	}

	log.Printf("Creating export PVC %s/%s for user PVC %s/%s", exportPVC.Namespace, exportPVC.Name, userPVC.Namespace, userPVC.Name)
	err := r.Client.Create(ctx, exportPVC)
	if err != nil {
		return nil, fmt.Errorf("failed to create export PVC: %w", err)
	}

	return exportPVC, nil
}

// patchPVLabelAnnotationsClaimRef patches the PV with:
// - ClaimRef pointing to exportPVC (to bind PV to export PVC)
// - Annotations for tracking original userPVC, DataExport name, and original reclaimPolicy
// - Label for cache filtering (only labeled PVs are cached) and orphan PV discovery
// - ReclaimPolicy set to Retain to protect data if exportPVC is accidentally deleted
func (r *DataexportReconciler) patchPVLabelAnnotationsClaimRef(ctx context.Context, pv *corev1.PersistentVolume, exportPVC *corev1.PersistentVolumeClaim, dataExport *dev1alpha1.DataExport, names Names, userPVCName string) error {
	updatedPv := pv.DeepCopy()

	if len(updatedPv.Annotations) == 0 {
		updatedPv.Annotations = make(map[string]string)
	}

	if len(updatedPv.Labels) == 0 {
		updatedPv.Labels = make(map[string]string)
	}

	updatedPv.Spec.ClaimRef = &corev1.ObjectReference{
		Namespace:       exportPVC.Namespace,
		Name:            exportPVC.Name,
		UID:             exportPVC.UID,
		ResourceVersion: exportPVC.ResourceVersion,
	}

	// Used for tracking original userPVC
	updatedPv.Annotations[dev1alpha1.AnnotationUserPVCNamespaceKey] = dataExport.Namespace
	updatedPv.Annotations[dev1alpha1.AnnotationUserPVCNameKey] = userPVCName

	// This used for exportRequest func
	updatedPv.Annotations[dev1alpha1.AnnotationStorageManagerNamespaceKey] = dataExport.Namespace
	updatedPv.Annotations[dev1alpha1.AnnotationStorageManagerNameKey] = dataExport.Name

	// This used for removing orphan resources
	updatedPv.Annotations[dev1alpha1.AnnotationPVTargetKindShortKey] = names.TargetKindShort
	updatedPv.Annotations[dev1alpha1.AnnotationPVHashSuffixKey] = names.HashSuffix

	// Save original reclaimPolicy before changing it
	updatedPv.Annotations[dev1alpha1.AnnotationOriginalReclaimPolicyKey] = string(pv.Spec.PersistentVolumeReclaimPolicy)

	// Set Retain to protect data during export
	updatedPv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain

	// Add labels for efficient List queries with MatchingLabels
	updatedPv.Labels[dev1alpha1.LabelPVDataExporter] = "true"

	log.Printf("Patching PV %s for attach to export PVC %s/%s", pv.Name, exportPVC.Namespace, exportPVC.Name)
	err := r.Client.Patch(ctx, updatedPv, client.MergeFromWithOptions(pv, client.MergeFromWithOptimisticLock{}))
	if err != nil {
		return fmt.Errorf("failed to patch user PV: %w", err)
	}

	return nil
}

var ErrInvalidOriginalReclaimPolicy = errors.New("invalid original reclaim policy")

// parsePVRecoveryInfo reads and validates all data-export annotations from a PV.
// Returns error if any required annotation is missing, has an invalid value,
// or doesn't match the expected DataExport (deNS/deName).
func parsePVRecoveryInfo(pv *corev1.PersistentVolume, deNS, deName string) (*pvRecoveryInfo, error) {
	// No check userPVC name, because it can be empty (snapshot-based case)
	userPVCName := pv.Annotations[dev1alpha1.AnnotationUserPVCNameKey]
	originalReclaimPolicy := corev1.PersistentVolumeReclaimPolicy(pv.Annotations[dev1alpha1.AnnotationOriginalReclaimPolicyKey])

	// Check valid reclaim policy from annotations
	// Validate originalReclaimPolicy first: this is the only annotation whose absence
	// makes recovery impossible (we cannot restore PV in normal flow (clearDataExportProviding)
	// without knowing the original policy). ErrInvalidOriginalReclaimPolicy is handled specially
	// by callers to stop cleanup and label the PV as inconsistent, while other validation
	// errors allow recovery to proceed.
	switch originalReclaimPolicy {
	case corev1.PersistentVolumeReclaimRetain,
		corev1.PersistentVolumeReclaimDelete,
		corev1.PersistentVolumeReclaimRecycle:
		// Valid policy
	default:
		return nil, fmt.Errorf("%w: invalid PV reclaim policy: %v", ErrInvalidOriginalReclaimPolicy, originalReclaimPolicy)
	}

	if pv.Labels[dev1alpha1.LabelPVDataExporter] != "true" {
		return nil, fmt.Errorf("PV %s has invalid label %s", pv.Name, dev1alpha1.LabelPVDataExporter)
	}

	dataExportName := pv.Annotations[dev1alpha1.AnnotationStorageManagerNameKey]
	if dataExportName != deName {
		return nil, fmt.Errorf("PV %s has invalid annotation %s", pv.Name, dev1alpha1.AnnotationStorageManagerNameKey)
	}

	dataExportNamespace := pv.Annotations[dev1alpha1.AnnotationStorageManagerNamespaceKey]
	if dataExportNamespace != deNS {
		return nil, fmt.Errorf("PV %s has invalid annotation %s", pv.Name, dev1alpha1.AnnotationStorageManagerNamespaceKey)
	}

	userPVCNamespace := pv.Annotations[dev1alpha1.AnnotationUserPVCNamespaceKey]
	if userPVCNamespace != deNS {
		return nil, fmt.Errorf("PV %s has invalid annotation %s", pv.Name, dev1alpha1.AnnotationUserPVCNamespaceKey)
	}

	targetKindShort := pv.Annotations[dev1alpha1.AnnotationPVTargetKindShortKey]
	hashSuffix := pv.Annotations[dev1alpha1.AnnotationPVHashSuffixKey]
	if err := ValidateHashAndTarget(targetKindShort, hashSuffix, deNS, deName); err != nil {
		return nil, err
	}

	return &pvRecoveryInfo{
		UserPVCNamespace:      userPVCNamespace,
		UserPVCName:           userPVCName,
		DataExportNamespace:   dataExportNamespace,
		DataExportName:        dataExportName,
		TargetKindShort:       targetKindShort,
		HashSuffix:            hashSuffix,
		OriginalReclaimPolicy: originalReclaimPolicy,
	}, nil
}

// handleInconsistentPV labels a PV as inconsistent when its data-export annotations
// are corrupted or it is in an unexpected state. The severity label helps administrators
// distinguish between cases:
//   - "warning": ClaimRef is nil or points to a PVC outside the controller namespace
//   - "error": ClaimRef points to a PVC in the controller namespace (likely our export PVC)
func (r *DataexportReconciler) handleInconsistentPV(ctx context.Context, pv *corev1.PersistentVolume) error {
	severity := SeverityWarning
	if pv.Spec.ClaimRef != nil && pv.Spec.ClaimRef.Namespace == r.Config.ControllerNamespace {
		severity = SeverityError
	}

	updatedPV := pv.DeepCopy()
	if len(updatedPV.Labels) == 0 {
		updatedPV.Labels = make(map[string]string)
	}
	updatedPV.Labels[dev1alpha1.LabelPVDataExporterInconsistent] = severity

	log.Printf("Labeling PV %s as inconsistent with severity %q", pv.Name, severity)
	if err := r.Client.Patch(ctx, updatedPV, client.MergeFromWithOptions(pv, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("failed to label PV %s as inconsistent: %w", pv.Name, err)
	}

	return nil
}

func (r *DataexportReconciler) ensureUserPVCExportingAnnotationAndFinalizer(ctx context.Context, namespace, name string) error {
	userPVC := &corev1.PersistentVolumeClaim{}

	userPVCNamespacedName := types.NamespacedName{Namespace: namespace, Name: name}
	err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 3*time.Second, true, func(ctx context.Context) (bool, error) {
		err := r.Client.Get(ctx, userPVCNamespacedName, userPVC)
		if err != nil {
			return false, err
		}

		// set annotation and finalizer
		hasAnnotation := userPVC.Annotations[DataExportInProgressKey] == "true"
		if !hasAnnotation {
			userPVC.Annotations[DataExportInProgressKey] = "true"
		}
		hasFinalizer := ContainsString(userPVC.Finalizers, dev1alpha1.StorageManagerFinalizerName)
		if !hasFinalizer {
			userPVC.Finalizers = append(userPVC.Finalizers, dev1alpha1.StorageManagerFinalizerName)
		}

		if (!hasAnnotation) || (!hasFinalizer) {
			err = r.Client.Update(ctx, userPVC)
			if err != nil {
				// continue attempts
				return false, nil
			}
			return true, nil
		}
		return true, nil
	})
	if err != nil {
		return err
	}

	return nil
}

// restoreOriginalPVState restores PV to its original state after export is complete:
// 1) Restores original reclaimPolicy from annotation (if saved, else return error)
// 2) Removes all storage manager annotations and labels
func (r *DataexportReconciler) restoreOriginalPVState(ctx context.Context, pv *corev1.PersistentVolume) error {
	updatedPV := pv.DeepCopy()

	needUpdate, err := restorePVReclaimPolicy(updatedPV)

	if err != nil {
		return err
	}

	if removePVExportMetadata(updatedPV) {
		needUpdate = true
	}

	if !needUpdate {
		log.Printf("PV %s already in original state, nothing to restore", pv.Name)
		return nil
	}

	log.Printf("Restoring PV %s to original state", pv.Name)
	if err := r.Client.Patch(ctx, updatedPV, client.MergeFromWithOptions(pv, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("failed to restore PV %s: %w", pv.Name, err)
	}

	log.Printf("Successfully restored PV %s to original state", pv.Name)
	return nil
}

// restorePVReclaimPolicy restores PV's reclaimPolicy from annotation.
// Returns true if PV was modified. Returns error if annotation is missing or invalid.
func restorePVReclaimPolicy(pv *corev1.PersistentVolume) (bool, error) {
	originalPolicy, exists := pv.Annotations[dev1alpha1.AnnotationOriginalReclaimPolicyKey]

	if !exists || originalPolicy == "" {
		return false, fmt.Errorf("PV %s does not have %s annotation with original reclaim policy", pv.Name, dev1alpha1.AnnotationOriginalReclaimPolicyKey)
	}

	// Validate the policy value
	switch corev1.PersistentVolumeReclaimPolicy(originalPolicy) {
	case corev1.PersistentVolumeReclaimRetain,
		corev1.PersistentVolumeReclaimDelete,
		corev1.PersistentVolumeReclaimRecycle:
		// Valid policy
	default:
		return false, fmt.Errorf("invalid original reclaimPolicy %q for PV %s", originalPolicy, pv.Name)
	}

	if pv.Spec.PersistentVolumeReclaimPolicy == corev1.PersistentVolumeReclaimPolicy(originalPolicy) {
		return false, nil
	}

	pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimPolicy(originalPolicy)
	log.Printf("Restoring reclaimPolicy %s for PV %s", originalPolicy, pv.Name)

	return true, nil
}

func (r *DataexportReconciler) removeUserPVCExportingAnnotationsAndFinalizer(ctx context.Context, userPVC *corev1.PersistentVolumeClaim) error {
	if userPVC == nil {
		return fmt.Errorf("nil pointer for user PVC")
	}

	annotationsToRemove := []string{
		DataExportInProgressKey,
		DataExportRequestAnnotationKey,
	}

	userPVCNamespacedName := types.NamespacedName{Namespace: userPVC.Namespace, Name: userPVC.Name}
	err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 3*time.Second, true, func(ctx context.Context) (bool, error) {
		err := r.Client.Get(ctx, userPVCNamespacedName, userPVC)
		if err != nil {
			return false, err
		}

		// remove annotation and finalizer
		hasAnnotation := false
		if userPVC.Annotations != nil {
			for _, annotation := range annotationsToRemove {
				if _, exists := userPVC.Annotations[annotation]; exists {
					hasAnnotation = true
					delete(userPVC.Annotations, annotation)
				}
			}
		}

		hasFinalizer := ContainsString(userPVC.Finalizers, dev1alpha1.StorageManagerFinalizerName)
		if hasFinalizer {
			userPVC.Finalizers = RemoveString(userPVC.Finalizers, dev1alpha1.StorageManagerFinalizerName)
		}

		if (hasAnnotation) || (hasFinalizer) {
			err = r.Client.Update(ctx, userPVC)
			if err != nil {
				// continue attempts
				return false, nil
			}
			return true, nil
		}
		return true, nil
	})
	if err != nil {
		return err
	}

	return nil
}

// Check for exportPVC exist and doesn't has status Lost (so has Pending or Bound)
func (r *DataexportReconciler) validateExportPVC(ctx context.Context, dataExport *dev1alpha1.DataExport, exportPVCName string) (*corev1.PersistentVolumeClaim, error) {
	exportPVC := &corev1.PersistentVolumeClaim{}
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: exportPVCName}, exportPVC)
	if err != nil {
		if kubeerrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get exportPVC from cache: %w", err)
	}
	if exportPVC.Status.Phase == corev1.ClaimLost {
		return nil, fmt.Errorf("export PVC for dataExport %s already exists and has status Lost", dataExport.GetName())
	}
	return exportPVC, nil
}

func (r *DataexportReconciler) validateExportDeploy(ctx context.Context, dataExport *dev1alpha1.DataExport, exportDeployName string) (*appsv1.Deployment, error) {
	exportDeploy := &appsv1.Deployment{}
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: exportDeployName}, exportDeploy)
	if err != nil {
		if kubeerrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get export deployment from cache: %w", err)
	}

	err = r.waitingForDeploymentLaunch(ctx, dataExport, exportDeployName)
	if err != nil {
		return nil, err
	}
	return exportDeploy, nil
}

// Delete export deployment and export PVC (if exists)
// Patch PV for attach it back to user's PVC
// Delete finalizer: storage-foundation.deckhouse.io/data-exporter-controller
func (r *DataexportReconciler) clearDataExportProviding(ctx context.Context, dataExport *dev1alpha1.DataExport, generatedNames Names) error {
	log.Printf("Start recovering configuration before Dataexport %s", dataExport.GetName())

	// Remove deployment if exist
	log.Printf("Deleting export deployment %s/%s", r.Config.ControllerNamespace, generatedNames.DeployName)
	exportDeploy := &appsv1.Deployment{}
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: generatedNames.DeployName}, exportDeploy)
	if !kubeerrors.IsNotFound(err) { // if not found - do nothing
		if err != nil {
			return fmt.Errorf("failed to get export deployment from cache: %w", err)
		}
		err = r.Client.Delete(ctx, exportDeploy)
		if err != nil {
			return fmt.Errorf("error deletion export deployment: %w", err)
		}
		log.Printf("Export deployment %s deleted", generatedNames.DeployName)
	}

	// Delete exportPVC
	log.Printf("Deleting export PVC %s/%s", r.Config.ControllerNamespace, generatedNames.ExportPVCName)
	exportPVC := &corev1.PersistentVolumeClaim{}
	err = r.Client.Get(ctx, types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: generatedNames.ExportPVCName}, exportPVC)
	if err != nil {
		if kubeerrors.IsNotFound(err) {
			log.Printf("Export PVC %s not found, nothing to delete. Continuing...", generatedNames.ExportPVCName)
		} else {
			return fmt.Errorf("failed to get export PVC from cache: %w", err)
		}
	} else {
		err = r.Client.Delete(ctx, exportPVC)
		if err != nil {
			return fmt.Errorf("error deletion export PVC: %w", err)
		}
		log.Printf("Export PVC %s deleted", generatedNames.ExportPVCName)
	}

	switch generatedNames.TargetKindShort {
	case dev1alpha1.KindPVCShort:
		log.Printf("Export target kind: %s", generatedNames.TargetKindShort)
		// detach PVC from DataExport pod and patch PV for attach it back to user's PVC
		userPVCNamespace := dataExport.Namespace
		userPVCName := dataExport.Spec.TargetRef.Name
		err := r.recoverUserPVC(ctx, userPVCNamespace, userPVCName, dataExport.Name, nil)
		if err != nil {
			return fmt.Errorf("failed to return PVC to user: %w", err)
		}

	case dev1alpha1.KindVirtualDiskShort:
		log.Printf("Export target kind: %s", generatedNames.TargetKindShort)
		err := r.recoverUserVirtualDisk(ctx, dataExport)
		if err != nil {
			return fmt.Errorf("failed to clear virtual disk: %w", err)
		}

	case dev1alpha1.KindSnapshotShort:
		// Snapshot export creates no user-PVC detach and no clone VS; the only owned out-of-band object
		// is the VolumeRestoreRequest that provisioned the export PVC. The export PVC itself is deleted
		// above; deleting the VRR releases the restore record (it also self-expires by TTL).
		log.Printf("Export target kind: %s", generatedNames.TargetKindShort)
		if err := r.deleteVolumeRestoreRequest(ctx, dataExport, generatedNames); err != nil {
			return fmt.Errorf("failed to clear snapshot export VolumeRestoreRequest: %w", err)
		}

	default:
		return fmt.Errorf("unknown export kind: %s", generatedNames.TargetKindShort)
	}

	// delete service and ingress resource
	// delete unconditionally: reconcilePublishResources handles the Publish toggle
	// during normal reconciliation, but if the user disables Publish and deletes
	// the DataExport in the same reconcile gap, we reach here without the toggle
	// being processed. DeletePublicResources is idempotent (not-found is not an error).
	if _, err := publish.DeletePublicResources(
		ctx,
		r.Client,
		types.NamespacedName{
			Namespace: r.Config.ControllerNamespace,
			Name:      generatedNames.HeadlessServiceName,
		},
		types.NamespacedName{
			Namespace: r.Config.ControllerNamespace,
			Name:      generatedNames.IngressResourceName,
		},
	); err != nil {
		return err
	}

	// delete CA secret
	log.Printf("Deleting CA secret for export target %s", generatedNames.TargetKindShort)
	dataExporterCASecret := &corev1.Secret{}
	err = r.Client.Get(ctx, types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: generatedNames.CASecretName}, dataExporterCASecret)
	if !kubeerrors.IsNotFound(err) { // if not found - do nothing
		if err != nil {
			return fmt.Errorf("failed to get CA secret from cache: %w", err)
		}
		err = r.Client.Delete(ctx, dataExporterCASecret)
		if err != nil {
			return fmt.Errorf("error deletion CA secret: %w", err)
		}
		log.Printf("CA secret %s deleted", generatedNames.CASecretName)
	}

	// Delete DataExoport finalizer
	if len(dataExport.Finalizers) > 0 {
		dataExport.Finalizers = []string{}
	}

	log.Printf("Recovering configuration before Dataexport %s finished", dataExport.GetName())
	return nil
}

var ErrPVCValidationFailed = errors.New("PVC validation failed")

// Validate PV for attach to DataExport pod:
// - PVC in working state: has status "Bound" and PV has references to PVC
// - PVC detached from consumering pods: no pods, using PVC, no volumeAttachment with status "True"
func (r *DataexportReconciler) validateUserPVCAndGetPV(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (*corev1.PersistentVolume, error) {
	pvName := pvc.Spec.VolumeName
	if pvName == "" {
		return nil, fmt.Errorf("%w: user's PVC %s does not contain VolumeName", ErrPVCValidationFailed, pvc.GetName())
	}

	pv := &corev1.PersistentVolume{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: pvName}, pv); err != nil {
		return nil, fmt.Errorf("failed to get PV %s: %w", pvName, err)
	}

	// Check condition: PVC has status "Bound" and PV has references to PVC
	if err := validateUserPVCInWorkingState(pvc, pv); err != nil {
		return nil, err
	}

	// Check condition: no pods, using PVC
	// All pods in namespace
	podList := &corev1.PodList{}
	if err := r.Reader.List(ctx, podList, client.InNamespace(pvc.Namespace)); err != nil {
		return nil, fmt.Errorf("failed to get podList: %w", err)
	}

	// Check for pods, using required PVC
	targetPVC := pvc.GetName()
	var foundPods []corev1.Pod
	for _, pod := range podList.Items {
		for _, volume := range pod.Spec.Volumes {
			if (volume.PersistentVolumeClaim != nil) && (volume.PersistentVolumeClaim.ClaimName == targetPVC) {
				foundPods = append(foundPods, pod)
			}
		}
	}
	if len(foundPods) > 0 {
		foundPodsNames := make([]string, 0, len(foundPods))
		for _, pod := range foundPods {
			foundPodsNames = append(foundPodsNames, pod.GetName())
		}
		return nil, fmt.Errorf("%w: user's PVC isn't free because it's being occupied by pods %s", ErrPVCValidationFailed, strings.Join(foundPodsNames, ", "))
	}

	// Check condition: no volumeAttachment with status "True"
	// Get volumeAttachmentList
	vaList := &storagev1.VolumeAttachmentList{}
	if err := r.Client.List(ctx, vaList, client.InNamespace(pvc.Namespace)); err != nil {
		return nil, fmt.Errorf("failed to get volumeAttachmentList: %w", err)
	}

	// Find volumeAttachment for target PV
	targetPV := pv.GetName()
	var foundVAs []storagev1.VolumeAttachment
	for _, va := range vaList.Items {
		if (va.Spec.Source.PersistentVolumeName != nil) && (*va.Spec.Source.PersistentVolumeName == targetPV) && (va.Status.Attached) {
			foundVAs = append(foundVAs, va)
		}
	}
	if len(foundVAs) > 0 {
		foundVANames := make([]string, 0, len(foundVAs))
		for _, foundVA := range foundVAs {
			foundVANames = append(foundVANames, foundVA.GetName())
		}

		return nil, fmt.Errorf("%w: user's PV not free because has volumeAttachments with status True: %s", ErrPVCValidationFailed, strings.Join(foundVANames, ", "))
	}

	return pv, nil
}

// Validate user's PVC+PV for attach to DataExport pod:
// - PVC has status "Bound"
// - PV has references to PVC
func validateUserPVCInWorkingState(pvc *corev1.PersistentVolumeClaim, pv *corev1.PersistentVolume) error {
	// Check condition: PVC has status "Bound"
	if pvc.Status.Phase != corev1.ClaimBound {
		return fmt.Errorf("%w: user's PVC has status: %s, but expected status Bound", ErrPVCValidationFailed, pvc.Status.Phase)
	}

	// Check: PVC.Spec.VolumeName matches PV.Name
	if pvc.Spec.VolumeName == "" || pvc.Spec.VolumeName != pv.Name {
		return fmt.Errorf("%w: PVC volumeName %s does not match PV name %s",
			ErrPVCValidationFailed, pvc.Spec.VolumeName, pv.Name)
	}

	// Check condition: PV has references to PVC
	if pv.Spec.ClaimRef == nil {
		return fmt.Errorf("%w: user's PV does not contain ClaimRef", ErrPVCValidationFailed)
	}

	// Check ClaimRef: Name + Namespace must match
	ref := pv.Spec.ClaimRef
	if ref.Name != pvc.Name || ref.Namespace != pvc.Namespace {
		return fmt.Errorf("%w: PV ClaimRef does not match user's PVC (pvc=%s/%s, claimRef=%s/%s)",
			ErrPVCValidationFailed,
			pvc.Namespace, pvc.Name,
			ref.Namespace, ref.Name)
	}

	// Check UID if exists
	if pvc.UID != "" && ref.UID != "" && pvc.UID != ref.UID {
		return fmt.Errorf("%w: PV ClaimRef UID mismatch (pvc uid=%s, claimRef uid=%s)",
			ErrPVCValidationFailed, pvc.UID, ref.UID)
	}

	return nil
}

// TODO: remove copy-paste, reuse common/deployment.go
func (r *DataexportReconciler) createDeployment(ctx context.Context, dataExport *dev1alpha1.DataExport, exportPVC *corev1.PersistentVolumeClaim, generatedNames Names) error {
	// Get dataExport image name from configMap
	cm := &corev1.ConfigMap{}
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: CongigMapName}, cm)
	if err != nil {
		return fmt.Errorf("failed to get ComfigMap from cache: %w", err)
	}
	dataExportPodImageName := cm.Data["image"]

	// Create deployment
	exportDeploy := appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Deployment",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      generatedNames.DeployName,
			Namespace: r.Config.ControllerNamespace,
			Labels: map[string]string{
				dev1alpha1.LabelApplicationKey: dev1alpha1.LabelDataExportValue,
			},
		},
		Spec: makeExportDeploySpec(exportPVC.Spec.VolumeMode, dataExport, dataExportPodImageName, dataExport.Spec.Ttl, r.Config.ControllerNamespace, generatedNames),
	}

	err = r.Client.Create(ctx, &exportDeploy)
	if err != nil {
		return fmt.Errorf("failed to create DataExport deployment: %w", err)
	}
	log.Printf("DataExport deployment created!")

	err = r.waitingForDeploymentLaunch(ctx, dataExport, generatedNames.DeployName)
	if err != nil {
		return err
	}

	return nil
}

func (r *DataexportReconciler) waitingForDeploymentLaunch(ctx context.Context, dataExport *dev1alpha1.DataExport, deployName string) error {
	// waiting for deployment launch
	err := wait.PollUntilContextTimeout(ctx, 5*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		deploy := &appsv1.Deployment{}

		err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: deployName}, deploy)
		if err != nil {
			if kubeerrors.IsNotFound(err) {
				// Deployment not created yet, continie waiting
				return false, nil
			}
			return false, err
		}

		// Check deployment conditions
		for _, condition := range deploy.Status.Conditions {
			if condition.Type == appsv1.DeploymentProgressing && condition.Status == corev1.ConditionFalse {
				return false, fmt.Errorf("deployment %q is stuck: %s: %w", deployName, condition.Message, ErrDeploymentFailed)
			}
			if condition.Type == appsv1.DeploymentReplicaFailure && condition.Status == corev1.ConditionTrue {
				return false, fmt.Errorf("deployment %q has replica failure: %s: %w", deployName, condition.Message, ErrDeploymentFailed)
			}
		}

		// Check for all replicas started
		if deploy.Status.AvailableReplicas == *deploy.Spec.Replicas {
			readyCond := meta.FindStatusCondition(dataExport.Status.Conditions, string(ConditionReady))
			if readyCond != nil && readyCond.Reason != string(ReasonExpired) {
				meta.SetStatusCondition(&dataExport.Status.Conditions, metav1.Condition{
					Type:               string(ConditionReady),
					Status:             metav1.ConditionTrue,
					Reason:             string(ReasonPodReady),
					Message:            "Pod is ready and export started",
					ObservedGeneration: dataExport.Generation,
				})
			}
			return true, nil
		}

		log.Printf("Waiting for deployment %q: %d/%d replicas available\n",
			deployName,
			deploy.Status.AvailableReplicas,
			*deploy.Spec.Replicas,
		)
		return false, nil
	})
	if err != nil && !errors.Is(err, ErrDeploymentFailed) {
		return fmt.Errorf("timed out waiting for deployment %q to become ready: %w: %w", deployName, ErrDeploymentFailed, err)
	}
	return err
}

func makeExportDeploySpec(pvMode *corev1.PersistentVolumeMode, dataExport *dev1alpha1.DataExport, dataExportPodImageName, ttl, controllerNamespace string, generatedNames Names) appsv1.DeploymentSpec {
	deploySpec := appsv1.DeploymentSpec{
		Replicas: Int32Ptr(1),
		Selector: &metav1.LabelSelector{
			MatchLabels: map[string]string{
				dev1alpha1.LabelApplicationKey:                  dev1alpha1.LabelDataExportValue,
				dev1alpha1.LabelStorageManagerDeploymentNameKey: generatedNames.DeployName,
			},
		},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					dev1alpha1.LabelApplicationKey:                  dev1alpha1.LabelDataExportValue,
					dev1alpha1.LabelStorageManagerDeploymentNameKey: generatedNames.DeployName,
				},
				Annotations: map[string]string{
					dev1alpha1.AnnotationStorageManagerNamespaceKey: dataExport.Namespace,
					dev1alpha1.AnnotationStorageManagerNameKey:      dataExport.Name,
				},
			},
			Spec: corev1.PodSpec{
				ServiceAccountName: ServiceAccountServer,
				ImagePullSecrets:   []corev1.LocalObjectReference{{Name: ImagePullSecretsName}},
				Containers:         makeContainerList(pvMode, dataExportPodImageName, ttl, dataExport.Namespace, dataExport.Name, controllerNamespace, generatedNames),
				Volumes:            makeVolumeList(generatedNames.ExportPVCName),
			},
		},
	}
	return deploySpec
}

func makeContainerList(pvMode *corev1.PersistentVolumeMode, image, ttl, dataExportNamespace, dataExportName, controllerNamespace string, generatedNames Names) []corev1.Container {
	var containers []corev1.Container
	portArg := fmt.Sprintf("--port=%d", FileServerPort)
	ttlArg := fmt.Sprintf("--ttl=%s", ttl)
	dataExportNamespaceArg := fmt.Sprintf("--data-export-namespace=%s", dataExportNamespace)
	dataExportNameArg := fmt.Sprintf("--data-export-name=%s", dataExportName)
	exportTargetKindShortArg := fmt.Sprintf("--export-target-kind-short=%s", generatedNames.TargetKindShort)
	exportTargetNameArg := fmt.Sprintf("--export-target-name=%s", generatedNames.TargetName)
	dataExportCASecretNameArg := fmt.Sprintf("--data-export-ca-secret-name=%s", generatedNames.CASecretName)
	dataExportServiceNameArg := fmt.Sprintf("--data-export-service-name=%s", generatedNames.HeadlessServiceName)
	controllerNamespaceArg := fmt.Sprintf("--controller-namespace=%s", controllerNamespace)

	switch *pvMode {
	case corev1.PersistentVolumeBlock:
		var rootUser int64
		securityContext := corev1.SecurityContext{RunAsUser: &rootUser}
		containers = []corev1.Container{
			{
				Name:  "data-exporter",
				Image: image,
				Args:  []string{"--mode=block", "--path=/mnt/block-storage", portArg, ttlArg, dataExportNamespaceArg, dataExportNameArg, exportTargetKindShortArg, exportTargetNameArg, dataExportCASecretNameArg, dataExportServiceNameArg, controllerNamespaceArg},
				Env: []corev1.EnvVar{
					{
						Name: "POD_IP",
						ValueFrom: &corev1.EnvVarSource{
							FieldRef: &corev1.ObjectFieldSelector{
								FieldPath: "status.podIP",
							},
						},
					},
				},
				VolumeDevices: []corev1.VolumeDevice{
					{
						Name:       "export-pvc",
						DevicePath: "/mnt/block-storage",
					},
				},
				Ports:           []corev1.ContainerPort{{Name: "http", ContainerPort: FileServerPort}},
				SecurityContext: &securityContext,
			},
		}

	case corev1.PersistentVolumeFilesystem:
		var rootUser int64
		securityContext := corev1.SecurityContext{RunAsUser: &rootUser}
		containers = []corev1.Container{
			{
				Name:  "data-exporter",
				Image: image,
				Args:  []string{"--mode=filesystem", "--path=/mnt/filesystem-storage", portArg, ttlArg, dataExportNamespaceArg, dataExportNameArg, exportTargetKindShortArg, exportTargetNameArg, dataExportCASecretNameArg, dataExportServiceNameArg, controllerNamespaceArg},
				Env: []corev1.EnvVar{
					{
						Name: "POD_IP",
						ValueFrom: &corev1.EnvVarSource{
							FieldRef: &corev1.ObjectFieldSelector{
								FieldPath: "status.podIP",
							},
						},
					},
				},
				VolumeMounts: []corev1.VolumeMount{
					{
						Name:      "export-pvc",
						MountPath: "/mnt/filesystem-storage",
					},
				},
				Ports:           []corev1.ContainerPort{{Name: "http", ContainerPort: FileServerPort}},
				SecurityContext: &securityContext,
			},
		}
	}
	return containers
}

func makeVolumeList(pvcName string) []corev1.Volume {
	volumes := []corev1.Volume{
		{
			Name: "export-pvc",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvcName,
					ReadOnly:  true,
				},
			},
		},
	}
	return volumes
}

func (r *DataexportReconciler) getExportPVCFromUserPVC(ctx context.Context, userPVCNameSpace, userPVCName, exportPVCName, dataExportName string) (*corev1.PersistentVolumeClaim, *corev1.PersistentVolume, error) {
	// Get user PVC // TODO: check the PV name in Lost state
	userPVC := &corev1.PersistentVolumeClaim{}
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: userPVCNameSpace, Name: userPVCName}, userPVC)
	if err != nil {
		if kubeerrors.IsNotFound(err) {
			return nil, nil, fmt.Errorf("user PVC %s/%s: %w: %w", userPVCNameSpace, userPVCName, ErrTargetNotFound, err)
		}
		return nil, nil, err
	}

	pv, err := r.validateUserPVCAndGetPV(ctx, userPVC)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to validate user PVC: %w", err)
	}

	if pv == nil {
		return nil, nil, fmt.Errorf("user PVC %s does not have a valid PersistentVolume", userPVC.GetName())
	}

	// Create export PVC
	exportPVC, err := r.detachPVC(ctx, userPVC, pv, exportPVCName, dataExportName)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to detach user PVC: %w", err)
	}

	return exportPVC, pv, nil
}

// recoverUserPVC restores the PV binding back to the user's PVC after export cleanup.
// pv may be nil when called from clearDataExportProviding (normal deletion flow),
// in which case PV is fetched via userPVC.Spec.VolumeName.
// pv is non-nil when called from removeOrphanResources, where it was already fetched
// during the PV list scan.
func (r *DataexportReconciler) recoverUserPVC(ctx context.Context, userPVCNamespace, userPVCName, dataExportName string, pv *corev1.PersistentVolume) error {
	log.Printf("Recovering user PVC %s/%s", userPVCNamespace, userPVCName)
	userPVC := &corev1.PersistentVolumeClaim{}
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: userPVCNamespace, Name: userPVCName}, userPVC)
	if err != nil {
		if kubeerrors.IsNotFound(err) {
			log.Printf("User PVC %s not found, nothing to recover", userPVCName)
			return nil // Nothing to recover, PVC does not exist
		}
		return fmt.Errorf("failed to get user PVC %s/%s: %w", userPVCNamespace, userPVCName, err)
	}

	// PV was not passed by caller (normal deletion flow via clearDataExportProviding).
	// Fetch it from userPVC.Spec.VolumeName.
	if pv == nil {
		pvName := userPVC.Spec.VolumeName
		if pvName == "" {
			return fmt.Errorf("user PVC %s does not have a valid PersistentVolume name in spec", userPVC.GetName())
		}

		pv = &corev1.PersistentVolume{}
		err = r.Client.Get(ctx, types.NamespacedName{Name: pvName}, pv)
		if err != nil {
			return fmt.Errorf("failed to get PV %s for user PVC %s: %w", pvName, userPVC.GetName(), err)
		}
	}

	// If the original reclaimPolicy annotation is missing or corrupted, recovery is impossible —
	// we cannot restore PV to its pre-export state without knowing the original policy.
	// Label the PV as inconsistent and stop. The finalizer stays on DataExport,
	// preventing its deletion until admin resolves the issue.
	if _, parseErr := parsePVRecoveryInfo(pv, userPVCNamespace, dataExportName); errors.Is(parseErr, ErrInvalidOriginalReclaimPolicy) {
		if labelErr := r.handleInconsistentPV(ctx, pv); labelErr != nil {
			return fmt.Errorf("failed to label inconsistent PV %s: %w", pv.Name, labelErr)
		}
		return fmt.Errorf("inconsistent PV %s: %w", pv.Name, ErrInvalidOriginalReclaimPolicy)
	}

	// Check if PV is in working state
	err = validateUserPVCInWorkingState(userPVC, pv)
	if err != nil {
		log.Printf("User PVC %s is not in working state: %v. Attempting to recover...", userPVC.GetName(), err)
		// Patch PV to change ClaimRef to user PVC
		updatedPV := pv.DeepCopy()
		updatedPV.Spec.ClaimRef = &corev1.ObjectReference{
			Namespace:       userPVC.Namespace,
			Name:            userPVC.Name,
			UID:             userPVC.UID,
			ResourceVersion: userPVC.ResourceVersion,
		}

		err = r.Client.Patch(ctx, updatedPV, client.MergeFromWithOptions(pv, client.MergeFromWithOptimisticLock{}))
		if err != nil {
			return fmt.Errorf("recover user PVC: failed to patch user PV: %w", err)
		}
		log.Printf("User PVC %s recovered successfully", userPVC.GetName())

		// Update pv with the patched version (including new ResourceVersion from API server)
		// so that restoreOriginalPVState below uses the correct ResourceVersion for optimistic locking.
		pv = updatedPV.DeepCopy()
	}

	// If we reach here, user PVC is in working state. TODO: check if user PVC not in lost state
	err = r.removeUserPVCExportingAnnotationsAndFinalizer(ctx, userPVC)
	if err != nil {
		return err
	}

	log.Printf("User PVC %s is in working state, ready to be used again", userPVC.GetName())

	// Remove annotations and labels from PV, and restore original reclaimPolicy.
	// NOTE: This is intentionally a separate PV patch from ClaimRef rebinding above.
	// We cannot combine them because PV labels must be removed LAST:
	// - If we remove labels in the first patch and finalizer removal fails,
	//   we lose the ability to retry (no labels = no reconcile trigger).
	// - We cannot remove finalizers before rebinding PV because that would
	//   leave userPVC unprotected while PV is still bound to exportPVC.
	if err := r.restoreOriginalPVState(ctx, pv); err != nil {
		return fmt.Errorf("failed to remove annotations/labels from PV: %w", err)
	}

	log.Printf("Successfully removed annotations and labels from PV: %s", pv.GetName())
	return nil
}

func (r *DataexportReconciler) getExportPVCFromUserVirtualDisk(ctx context.Context, dataExport *dev1alpha1.DataExport, generatedNames Names) (*corev1.PersistentVolumeClaim, *corev1.PersistentVolume, string, error) {
	userNamespace := dataExport.Namespace
	userVirtualDiskName := dataExport.Spec.TargetRef.Name

	log.Printf("Processing user VirtualDisk %s/%s export for DataExport resource %s", userNamespace, userVirtualDiskName, dataExport.Name)

	userPVCName, err := r.prepareVirtualDiskForExportAndGetPVCName(ctx, userNamespace, userVirtualDiskName)
	if err != nil {
		return nil, nil, "", fmt.Errorf("failed to prepare user VirtualDisk %s/%s for export: %w", userNamespace, userVirtualDiskName, err)
	}

	userVirtualDiskReadyForExport, err := r.isVirtualDiskReadyForExport(ctx, userNamespace, userVirtualDiskName)
	if err != nil {
		return nil, nil, "", fmt.Errorf("failed to check if VirtualDisk %s/%s is ready for export: %w", userNamespace, userVirtualDiskName, err)
	}

	if !userVirtualDiskReadyForExport {
		return nil, nil, "", fmt.Errorf("%w: VirtualDisk %s/%s is not ready for export", ErrTargetNotReady, userNamespace, userVirtualDiskName)
	}

	log.Printf("User VirtualDisk %s/%s is ready, using PVC %s/%s for export", userNamespace, userVirtualDiskName, userNamespace, userPVCName)

	exportPVC, pv, err := r.getExportPVCFromUserPVC(ctx, userNamespace, userPVCName, generatedNames.ExportPVCName, dataExport.Name)
	return exportPVC, pv, userPVCName, err
}

func (r *DataexportReconciler) prepareVirtualDiskForExportAndGetPVCName(ctx context.Context, namespace, virtualDiskName string) (string, error) {
	log.Printf("Preparing user VirtualDisk %s/%s for export", namespace, virtualDiskName)

	// Fetch the VirtualDisk resource
	virtualDisk := &virtv1alpha2.VirtualDisk{}
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: virtualDiskName}, virtualDisk)
	if err != nil {
		if kubeerrors.IsNotFound(err) {
			return "", fmt.Errorf("VirtualDisk %s/%s: %w: %w", namespace, virtualDiskName, ErrTargetNotFound, err)
		}
		return "", fmt.Errorf("failed to get VirtualDisk %s/%s: %w", namespace, virtualDiskName, err)
	}

	// Check if the VirtualDisk has a valid PersistentVolumeClaim in status
	pvcName := virtualDisk.Status.Target.PersistentVolumeClaim
	if pvcName == "" {
		return "", fmt.Errorf("user VirtualDisk %s/%s does not have a valid PersistentVolumeClaim in status: %w", namespace, virtualDiskName, ErrTargetNotReady)
	}

	// Fetch the PVC to ensure it exists and is in a valid state
	pvc := &corev1.PersistentVolumeClaim{}
	err = r.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: pvcName}, pvc)
	if err != nil {
		return "", fmt.Errorf("failed to get PVC %s/%s for user VirtualDisk %s/%s: %w", namespace, pvcName, namespace, virtualDiskName, err)
	}

	// Ensure the PVC has the DataExportRequest annotation
	err = r.ensureAnnotationsOnPVC(ctx, pvc, map[string]string{
		DataExportRequestAnnotationKey: "true",
	})
	if err != nil {
		return "", fmt.Errorf("failed to ensure DataExportRequest annotation %s on PVC %s/%s: %w", DataExportRequestAnnotationKey, namespace, pvcName, err)
	}

	return pvcName, nil
}

// Check if the VirtualDisk is already in use for export

func (r *DataexportReconciler) isVirtualDiskReadyForExport(ctx context.Context, namespace, virtualDiskName string) (bool, error) {
	log.Printf("Checking if user VirtualDisk %s/%s is ready for export", namespace, virtualDiskName)

	err := wait.PollUntilContextTimeout(ctx, 1*time.Second, 3*time.Second, true, func(ctx context.Context) (bool, error) {
		// Re-fetch the VirtualDisk to ensure we have the latest state
		virtualDisk := &virtv1alpha2.VirtualDisk{}
		err := r.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: virtualDiskName}, virtualDisk)
		if err != nil {
			return false, fmt.Errorf("failed to get user VirtualDisk %s/%s while checking readiness: %w", namespace, virtualDiskName, err)
		}
		if virtualDisk.Status.Conditions == nil {
			return false, nil // Conditions not set yet, keep waiting
		}

		isReady := false
		isReadyForExport := false

		for _, condition := range virtualDisk.Status.Conditions {
			if condition.Type == vdcondition.ReadyType.String() {
				isReady = isVDReady(virtualDisk, condition)
			}
			if condition.Type == vdcondition.InUseType.String() {
				if condition.Status == metav1.ConditionTrue &&
					condition.Reason == "UsedForDataExport" && // TODO: get rid of this hardcoded reason
					virtualDisk.Generation == condition.ObservedGeneration {
					isReadyForExport = true
				}
			}
		}

		if isReady && isReadyForExport {
			log.Printf("VirtualDisk %s/%s is ready for export", namespace, virtualDiskName)
			return true, nil
		}

		log.Printf("VirtualDisk %s/%s is not ready for export yet, waiting...", namespace, virtualDiskName)
		return false, nil
	})
	if err != nil {
		return false, fmt.Errorf("failed to check if user VirtualDisk %s/%s is ready for export: %w", namespace, virtualDiskName, err)
	}

	log.Printf("User VirtualDisk %s/%s is ready for export", namespace, virtualDiskName)
	return true, nil
}

func (r *DataexportReconciler) recoverUserVirtualDisk(ctx context.Context, dataExport *dev1alpha1.DataExport) error {
	log.Printf("Recovering VirtualDisk for DataExport resource %s/%s", dataExport.Namespace, dataExport.Name)
	userNamespace := dataExport.Namespace
	userVirtualDiskName := dataExport.Spec.TargetRef.Name
	userVirtualDisk := &virtv1alpha2.VirtualDisk{}
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: userNamespace, Name: userVirtualDiskName}, userVirtualDisk)
	if err != nil {
		if kubeerrors.IsNotFound(err) {
			log.Printf("User VirtualDisk %s/%s not found, nothing to recover", userNamespace, userVirtualDiskName)
			return nil // Nothing to recover, VirtualDisk does not exist
		}
		return fmt.Errorf("failed to get user VirtualDisk %s/%s: %w", userNamespace, userVirtualDiskName, err)
	}

	userPVCName := userVirtualDisk.Status.Target.PersistentVolumeClaim
	// err := r.removeAnnotationFromPVCIfNeeded(ctx, userNamespace, userPVCName, DataExportRequestAnnotationKey)
	// if err != nil {
	// 	return fmt.Errorf("failed to remove %s annotation from user PVC %s/%s: %w", DataExportRequestAnnotationKey, userNamespace, userPVCName, err)
	// }

	return r.recoverUserPVC(ctx, userNamespace, userPVCName, dataExport.Name, nil)
}

func (r *DataexportReconciler) ensureAnnotationsOnPVC(ctx context.Context, pvc *corev1.PersistentVolumeClaim, annotationsToAdd map[string]string) error {
	if pvc == nil {
		return fmt.Errorf("PVC is nil, cannot ensure annotations")
	}

	if pvc.Annotations == nil {
		pvc.Annotations = make(map[string]string)
	}

	needUpdate := false

	for key, value := range annotationsToAdd {
		if pvc.Annotations[key] != value {
			pvc.Annotations[key] = value
			needUpdate = true
		}
	}

	if needUpdate {
		err := r.Client.Update(ctx, pvc)
		if err != nil {
			return fmt.Errorf("failed to update PVC %s/%s with annotation %+v: %w", pvc.Namespace, pvc.Name, annotationsToAdd, err)
		}
	}

	return nil
}

func isVDReady(vd *virtv1alpha2.VirtualDisk, condition metav1.Condition) bool {
	if vd.Generation != condition.ObservedGeneration {
		return false
	}

	switch condition.Status {
	case metav1.ConditionTrue:
		return true
	case metav1.ConditionFalse:
		// VD is ready for us if condition Reason is "Exporting"
		return condition.Reason == "Exporting" // TODO: get rid of this hardcoded reason
	default:
		return false
	}
}

// validateDataExportSpec performs cheap, permanent-until-spec-change validation. With the GroupKind
// targetRef (C6) there is no kind allowlist: classifyTargetRef rejects structurally invalid / forbidden
// targets (e.g. a bare VolumeSnapshotContent), and only the live-VirtualDisk path needs a CRD presence
// pre-check (the snapshot path is generic and surfaces missing targets through Ready conditions).
func (r *DataexportReconciler) validateDataExportSpec(ctx context.Context, dataExport *dev1alpha1.DataExport) error {
	cat, _, err := classifyTargetRef(dataExport.Spec.TargetRef.Group, dataExport.Spec.TargetRef.Kind)
	if err != nil {
		return err
	}

	if cat == categoryLiveVirtualDisk && !r.isVirtualDiskCRDExists(ctx) {
		return fmt.Errorf("CRD %s does not exist in the cluster", virtv1alpha2.VirtualDiskKind)
	}

	return nil
}

func (r *DataexportReconciler) isVirtualDiskCRDExists(ctx context.Context) bool {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	err := r.Reader.Get(ctx, types.NamespacedName{Name: dev1alpha1.VirtualDiskCRDName}, crd)
	if err != nil {
		if kubeerrors.IsNotFound(err) {
			return false
		}
		log.Printf("Error checking for VirtualDisk CRD existence: %v", err)
		return false
	}
	return true
}

// validatePVNotOwnedByAnotherDataExport checks if the PV is already being used
// by a different DataExport. This prevents conflicts when multiple DataExports
// attempt to export the same PVC simultaneously. Returns an error if the PV
// has storage manager annotations pointing to a different DataExport.
func validatePVNotOwnedByAnotherDataExport(pv *corev1.PersistentVolume, expectedNamespace, expectedName string) error {
	annotations := pv.Annotations

	if len(annotations) == 0 {
		return nil
	}

	currentName, hasName := annotations[dev1alpha1.AnnotationStorageManagerNameKey]
	currentNamespace, hasNS := annotations[dev1alpha1.AnnotationStorageManagerNamespaceKey]

	if !hasNS && !hasName {
		return nil
	}

	if !hasNS || !hasName {
		return fmt.Errorf("PV %s has inconsistent storage manager annotations: %w", pv.Name, ErrPVConflict)
	}

	if currentName != expectedName || currentNamespace != expectedNamespace {
		return fmt.Errorf("PV %s is already owned by DataExport %s/%s: %w", pv.Name, currentNamespace, currentName, ErrPVConflict)
	}

	return nil
}

// removePVExportMetadata removes storage manager annotations and labels from PV.
// Returns true if any metadata was removed
func removePVExportMetadata(updatePV *corev1.PersistentVolume) bool {
	changed := false
	keysToRemove := []string{
		dev1alpha1.AnnotationUserPVCNamespaceKey,
		dev1alpha1.AnnotationUserPVCNameKey,
		dev1alpha1.AnnotationStorageManagerNamespaceKey,
		dev1alpha1.AnnotationStorageManagerNameKey,
		dev1alpha1.AnnotationPVTargetKindShortKey,
		dev1alpha1.AnnotationPVHashSuffixKey,
		dev1alpha1.AnnotationOriginalReclaimPolicyKey,
	}

	// Remove annotations
	for _, key := range keysToRemove {
		if _, exists := updatePV.Annotations[key]; exists {
			delete(updatePV.Annotations, key)
			changed = true
		}
	}

	// Remove labels
	if _, exists := updatePV.Labels[dev1alpha1.LabelPVDataExporter]; exists {
		delete(updatePV.Labels, dev1alpha1.LabelPVDataExporter)
		changed = true
	}

	return changed
}

// removeOrphanResources handles cleanup when a DataExport is deleted while the controller
// was down. It finds orphaned PVs by label, reads recovery info from annotations, then:
// 1) Deletes orphaned deployment
// 2) Deletes orphaned exportPVC
// 3) Recovers userPVC (restores PV binding and original reclaimPolicy)
// This is triggered by the PV watch when DataExport is not found during reconciliation.
func (r *DataexportReconciler) removeOrphanResources(ctx context.Context, dataExportNamespace, dataExportName string) error {
	log.Printf("Starting cleanup of orphaned resources for deleted DataExport %s/%s", dataExportNamespace, dataExportName)

	// List PVs with data-exporter label. The cache now contains all PVs (no label filter),
	// so we must use MatchingLabels to filter only PVs managed by DataExport.
	pvList := &corev1.PersistentVolumeList{}
	if err := r.Client.List(ctx, pvList, client.MatchingLabels{dev1alpha1.LabelPVDataExporter: "true"}); err != nil {
		return fmt.Errorf("failed to list PVs for orphan cleanup: %w", err)
	}

	for i := range pvList.Items {
		pv := &pvList.Items[i]

		// skip, its not orphaned PV
		if pv.Annotations[dev1alpha1.AnnotationStorageManagerNameKey] != dataExportName ||
			pv.Annotations[dev1alpha1.AnnotationStorageManagerNamespaceKey] != dataExportNamespace {
			continue
		}

		log.Printf("Found orphaned PV %s for deleted DataExport %s/%s", pv.Name, dataExportNamespace, dataExportName)

		// Validate and extract recovery info from PV annotations.
		// If annotations are corrupted, label the PV as inconsistent and stop reconcile
		// instead of attempting cleanup with wrong values.
		pvInfo, err := parsePVRecoveryInfo(pv, dataExportNamespace, dataExportName)
		if err != nil {
			log.Printf("PV %s has inconsistent state: %v", pv.Name, err)
			if labelErr := r.handleInconsistentPV(ctx, pv); labelErr != nil {
				return fmt.Errorf("failed to label inconsistent PV %s: %w", pv.Name, labelErr)
			}
			return nil // Stop reconcile without error
		}

		// 1) Delete orphaned deployment
		if err := r.deleteOrphanedDeployment(ctx, pvInfo.TargetKindShort, pvInfo.HashSuffix); err != nil {
			return fmt.Errorf("failed to delete orphaned deployment: %w", err)
		}

		// 2) Delete orphaned exportPVC
		if err := r.deleteOrphanedExportPVCs(ctx, pvInfo.TargetKindShort, pvInfo.HashSuffix); err != nil {
			return fmt.Errorf("failed to delete orphaned export PVCs: %w", err)
		}

		// 3) Recover userPVC
		if err := r.recoverOrphanedUserPVC(ctx, pv, pvInfo.UserPVCNamespace, pvInfo.UserPVCName, dataExportName); err != nil {
			return fmt.Errorf("failed to recover user PVC: %w", err)
		}

		// One DataExport operates on exactly one PV, so we stop after finding the matching one
		break
	}

	log.Printf("cleanup complete: %s/%s", dataExportNamespace, dataExportName)
	return nil
}

// recoverOrphanedUserPVC recovers the user PVC after orphan cleanup.
// For snapshot-based exports (VolumeSnapshot, VirtualDiskSnapshot), userPVCNamespace and
// userPVCName are empty because no user PVC was detached — we only need to clean PV annotations.
// For PVC/VirtualDisk exports, we restore the PV binding to the original user PVC.
func (r *DataexportReconciler) recoverOrphanedUserPVC(ctx context.Context, pv *corev1.PersistentVolume, userPVCNamespace, userPVCName, dataExportName string) error {
	// For snapshot-based exports, no user PVC was detached, so we only clean up PV annotations
	if userPVCNamespace == "" && userPVCName == "" {
		log.Printf("No user PVC to recover (snapshot-based export), cleaning up PV annotations only")

		if err := r.restoreOriginalPVState(ctx, pv); err != nil {
			return fmt.Errorf("failed to remove storage manager annotations and labels from PV %s: %w", pv.Name, err)
		}

		return nil
	}

	if err := r.recoverUserPVC(ctx, userPVCNamespace, userPVCName, dataExportName, pv); err != nil {
		return fmt.Errorf("failed to recover user PVC %s/%s: %w", userPVCNamespace, userPVCName, err)
	}

	return nil
}

// deleteOrphanedDeployment deletes the data-exporter deployment that was created
// for the now-deleted DataExport. Uses targetRef and hashSuffix from PV annotations
// to reconstruct the deployment name.
func (r *DataexportReconciler) deleteOrphanedDeployment(ctx context.Context, targetKindShort, hashSuffix string) error {
	deployName := DeployNameForHash(targetKindShort, hashSuffix)
	deploy := &appsv1.Deployment{}

	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: deployName}, deploy); err != nil {
		if kubeerrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get deployment: %w", err)
	}

	label, exists := deploy.Labels[dev1alpha1.LabelApplicationKey]
	if !exists || label != dev1alpha1.LabelDataExportValue {
		return fmt.Errorf("deployment %s/%s is not managed by data-exporter: missing or invalid app label", r.Config.ControllerNamespace, deployName)
	}

	if _, err := DeleteDeployment(ctx, r.Client, r.Config.ControllerNamespace, deployName); err != nil {
		return fmt.Errorf("failed to delete orphaned deployment %s/%s: %w", r.Config.ControllerNamespace, deployName, err)
	}

	return nil
}

// deleteOrphanedExportPVCs deletes the temporary export PVC that was created
// for the now-deleted DataExport. Uses targetRef and hashSuffix from PV annotations
// to reconstruct the PVC name.
func (r *DataexportReconciler) deleteOrphanedExportPVCs(ctx context.Context, targetKindShort, hashSuffix string) error {
	exportPVCName := ExportPVCNameForHash(targetKindShort, hashSuffix)
	exportPVC := &corev1.PersistentVolumeClaim{}

	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Config.ControllerNamespace, Name: exportPVCName}, exportPVC); err != nil {
		if kubeerrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get export PVC %s/%s: %w", r.Config.ControllerNamespace, exportPVCName, err)
	}

	label, exists := exportPVC.Labels[dev1alpha1.LabelApplicationKey]
	if !exists || label != dev1alpha1.LabelDataExportValue {
		return fmt.Errorf("export PVC %s/%s is not managed by data-exporter: missing or invalid app label", r.Config.ControllerNamespace, exportPVCName)
	}

	if err := r.Client.Delete(ctx, exportPVC); err != nil {
		if kubeerrors.IsNotFound(err) {
			return nil
		}

		return fmt.Errorf("failed to delete orphaned exportPVC %s/%s: %w", r.Config.ControllerNamespace, exportPVCName, err)
	}

	return nil
}
