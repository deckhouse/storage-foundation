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

package controller

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-lib-utils/accessmodes"
	"github.com/kubernetes-csi/csi-lib-utils/rpc"
	snapclientset "github.com/kubernetes-csi/external-snapshotter/client/v8/clientset/versioned"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	storagelistersv1 "k8s.io/client-go/listers/storage/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	storagev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

// VRRHandler handles VolumeRestoreRequest objects.
//
// VRRHandler is a controller-like component that manages restore operation lifecycle:
// - Creates PV + PVC based on VRR
// - Waits for PVC to become Bound
// - Updates VRR.status on success (Ready=True) or failure (Ready=False)
// - Determines terminal states (Ready/Failed)
//
// VRRHandler owns the restore operation lifecycle and is responsible for
// completing the restore and updating VRR status accordingly.
type VRRHandler struct {
	dynamicClient          dynamic.Interface
	kubeClient             kubernetes.Interface
	csiClient              csi.ControllerClient
	snapshotClient         snapclientset.Interface
	driverName             string
	identity               string
	timeout                time.Duration
	volumeNamePrefix       string
	volumeNameUUIDLength   int
	defaultFSType          string
	scLister               storagelistersv1.StorageClassLister
	eventRecorder          record.EventRecorder
	pluginCapabilities     rpc.PluginCapabilitySet
	controllerCapabilities rpc.ControllerCapabilitySet
}

// NewVRRHandler creates a new VRR handler.
func NewVRRHandler(
	dynamicClient dynamic.Interface,
	kubeClient kubernetes.Interface,
	csiClient csi.ControllerClient,
	snapshotClient snapclientset.Interface,
	driverName string,
	identity string,
	timeout time.Duration,
	volumeNamePrefix string,
	volumeNameUUIDLength int,
	defaultFSType string,
	scLister storagelistersv1.StorageClassLister,
	eventRecorder record.EventRecorder,
	pluginCapabilities rpc.PluginCapabilitySet,
	controllerCapabilities rpc.ControllerCapabilitySet,
) *VRRHandler {
	return &VRRHandler{
		dynamicClient:          dynamicClient,
		kubeClient:             kubeClient,
		csiClient:              csiClient,
		snapshotClient:         snapshotClient,
		driverName:             driverName,
		identity:               identity,
		timeout:                timeout,
		volumeNamePrefix:       volumeNamePrefix,
		volumeNameUUIDLength:   volumeNameUUIDLength,
		defaultFSType:          defaultFSType,
		scLister:               scLister,
		eventRecorder:          eventRecorder,
		pluginCapabilities:     pluginCapabilities,
		controllerCapabilities: controllerCapabilities,
	}
}

// HandleVRR handles a VRR event from informer.
//
// VRR object is treated as immutable input signal; handler reads it once per event and does not cache state.
func (h *VRRHandler) HandleVRR(ctx context.Context, obj any) {
	// Convert dynamic object to typed VRR
	unstructuredObj, ok := obj.(*unstructured.Unstructured)
	if !ok {
		klog.Warningf("Expected *unstructured.Unstructured but got %T", obj)
		return
	}

	vrr := &storagev1alpha1.VolumeRestoreRequest{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(
		unstructuredObj.UnstructuredContent(), vrr); err != nil {
		klog.Errorf("Failed to convert VRR object: %v", err)
		return
	}

	// Early return if StorageClassName is empty (invalid VRR spec)
	if vrr.Spec.StorageClassName == "" {
		h.eventRecorder.Eventf(vrr, v1.EventTypeWarning, "VRRInvalidSpec",
			"StorageClassName is required but not specified")
		klog.Warningf("VRR %s/%s has empty StorageClassName, ignoring", vrr.Namespace, vrr.Name)
		return
	}

	// CRITICAL SECURITY INVARIANT: Driver filter MUST be the first executable step after minimal validation.
	// This prevents external-provisioner from accessing resources (PVC, VRR status) belonging to other CSI drivers
	// in multi-CSI clusters. Without this check, a provisioner could:
	// - Access PVCs in namespaces it shouldn't have access to
	// - Attempt status updates on VRRs it doesn't own
	// - Log information about VRRs from other drivers
	sc, err := h.scLister.Get(vrr.Spec.StorageClassName)
	if err != nil {
		// Not our StorageClass, ignore silently
		return
	}
	if sc.Provisioner != h.driverName {
		// Not our driver, ignore silently
		return
	}

	// Skip already-terminal VRRs (Ready=True or Ready=False)
	// Terminal VRRs should not be reprocessed
	// NOTE: This check happens AFTER driver filter to avoid logging about terminal VRRs from other drivers
	if isTerminalVRR(vrr.Status.Conditions) {
		klog.V(2).Infof("VRR %s/%s is already terminal, skipping", vrr.Namespace, vrr.Name)
		return
	}

	// Check idempotency: if PVC already exists, skip (already processed)
	// NOTE: This check happens AFTER driver filter to avoid accessing PVCs in namespaces we shouldn't access
	targetPVC, err := h.kubeClient.CoreV1().PersistentVolumeClaims(vrr.Spec.TargetNamespace).Get(ctx, vrr.Spec.TargetPVCName, metav1.GetOptions{})
	if err == nil && targetPVC != nil {
		// PVC already exists, skip
		return
	}
	if err != nil && !apierrors.IsNotFound(err) {
		klog.Errorf("Failed to check if PVC exists: %v", err)
		return
	}

	// Process VRR
	if err := h.handleVRRInternal(ctx, vrr); err != nil {
		h.eventRecorder.Eventf(vrr, v1.EventTypeWarning, "VRRRestoreFailed",
			"Failed to restore volume from VRR: %v", err)
		klog.Errorf("Failed to handle VRR %s/%s: %v", vrr.Namespace, vrr.Name, err)
		// Update VRR status to Failed
		if updateErr := h.updateVRRFailed(ctx, vrr, err); updateErr != nil {
			klog.Warningf("Failed to update VRR %s/%s status to Failed: %v", vrr.Namespace, vrr.Name, updateErr)
		}
	}
}

// handleVRRInternal processes VRR based on source type.
func (h *VRRHandler) handleVRRInternal(ctx context.Context, vrr *storagev1alpha1.VolumeRestoreRequest) error {
	switch vrr.Spec.SourceRef.Kind {
	case "VolumeSnapshotContent":
		return h.restoreFromVSC(ctx, vrr)
	case "PersistentVolume":
		return h.restoreFromPV(ctx, vrr)
	default:
		return fmt.Errorf("unsupported source kind: %s", vrr.Spec.SourceRef.Kind)
	}
}

// restoreFromVSC handles restore from VolumeSnapshotContent.
//
// Step 1: Get VSC and extract snapshot handle (isolated contract with snapshot API).
func (h *VRRHandler) restoreFromVSC(ctx context.Context, vrr *storagev1alpha1.VolumeRestoreRequest) error {
	// Get VSC
	vsc, err := h.snapshotClient.SnapshotV1().VolumeSnapshotContents().Get(ctx, vrr.Spec.SourceRef.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("VolumeSnapshotContent %s not found", vrr.Spec.SourceRef.Name)
		}
		return fmt.Errorf("failed to get VolumeSnapshotContent %s: %w", vrr.Spec.SourceRef.Name, err)
	}

	// Check if VSC is being deleted
	if vsc.ObjectMeta.DeletionTimestamp != nil {
		return fmt.Errorf("VolumeSnapshotContent %s is currently being deleted", vsc.Name)
	}

	// Check if VSC is ReadyToUse
	if vsc.Status == nil || vsc.Status.ReadyToUse == nil || !*vsc.Status.ReadyToUse {
		// Check for terminal errors first
		if vsc.Status != nil && vsc.Status.Error != nil && vsc.Status.Error.Message != nil {
			errorMsg := *vsc.Status.Error.Message
			return fmt.Errorf("VolumeSnapshotContent %s has error: %s", vsc.Name, errorMsg)
		}
		// VSC exists but not ready yet - this is not terminal, informer will retry
		return fmt.Errorf("VolumeSnapshotContent %s is not ReadyToUse yet", vsc.Name)
	}

	// Extract snapshot handle
	// CONTRACT: external-provisioner MUST use Status.SnapshotHandle if present;
	// fallback to Spec.Source.SnapshotHandle is allowed only for backward compatibility.
	var snapshotHandle string
	if vsc.Status != nil && vsc.Status.SnapshotHandle != nil {
		snapshotHandle = *vsc.Status.SnapshotHandle
	} else if vsc.Spec.Source.SnapshotHandle != nil {
		// Fallback for backward compatibility only
		snapshotHandle = *vsc.Spec.Source.SnapshotHandle
	} else {
		return fmt.Errorf("snapshot handle not available in VolumeSnapshotContent %s", vsc.Name)
	}

	klog.V(2).Infof("restoreFromVSC: VRR %s/%s, VSC %s, snapshotHandle %s", vrr.Namespace, vrr.Name, vsc.Name, snapshotHandle)

	// Step 2: CSI CreateVolume call (without PV creation yet)
	sc, err := h.scLister.Get(vrr.Spec.StorageClassName)
	if err != nil {
		return fmt.Errorf("failed to get StorageClass %s: %w", vrr.Spec.StorageClassName, err)
	}

	// Generate volume name from VRR UID
	pvName, err := makeVolumeName(h.volumeNamePrefix, string(vrr.ObjectMeta.UID), h.volumeNameUUIDLength)
	if err != nil {
		return fmt.Errorf("failed to generate volume name: %w", err)
	}

	// Check if PV already exists (idempotency: previous attempt may have created PV but not PVC)
	var pv *v1.PersistentVolume
	existingPV, err := h.kubeClient.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
	if err == nil && existingPV != nil {
		// PV already exists - use it and continue to ensure PVC is created
		klog.V(2).Infof("restoreFromVSC: PV %s already exists, skipping CSI CreateVolume and continuing to PVC creation", pvName)
		pv = existingPV
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to check if PV %s exists: %w", pvName, err)
	} else {
		// PV does not exist - proceed with CSI CreateVolume
		// Get default volume capabilities (RWO for now, will be enhanced in step 4.4)
		volumeCaps := []*csi.VolumeCapability{
			{
				AccessType: &csi.VolumeCapability_Mount{
					Mount: &csi.VolumeCapability_MountVolume{
						FsType: h.defaultFSType,
					},
				},
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
				},
			},
		}

		// Get capacity from VSC restore size if available, otherwise use default (1Gi)
		capacityBytes := int64(1024 * 1024 * 1024) // 1Gi default
		if vsc.Status != nil && vsc.Status.RestoreSize != nil {
			capacityBytes = *vsc.Status.RestoreSize
		}

		// Resolve provisioner secret credentials from StorageClass
		// Note: We pass nil for PVC since it doesn't exist yet - secret templates can use PV name
		provisionerSecretRef, err := getSecretReference(provisionerSecretParams, sc.Parameters, pvName, nil)
		if err != nil {
			return fmt.Errorf("failed to get provisioner secret reference: %w", err)
		}
		provisionerCredentials, err := getCredentials(ctx, h.kubeClient, provisionerSecretRef)
		if err != nil {
			return fmt.Errorf("failed to get provisioner credentials: %w", err)
		}

		// Build CreateVolumeRequest
		req := csi.CreateVolumeRequest{
			Name:               pvName,
			Parameters:         sc.Parameters,
			VolumeCapabilities: volumeCaps,
			CapacityRange: &csi.CapacityRange{
				RequiredBytes: capacityBytes,
			},
			VolumeContentSource: &csi.VolumeContentSource{
				Type: &csi.VolumeContentSource_Snapshot{
					Snapshot: &csi.VolumeContentSource_SnapshotSource{
						SnapshotId: snapshotHandle,
					},
				},
			},
			Secrets: provisionerCredentials,
		}

		// Call CSI CreateVolume with timeout
		createCtx, cancel := context.WithTimeout(ctx, h.timeout)
		defer cancel()

		klog.V(2).Infof("restoreFromVSC: calling CSI CreateVolume for VRR %s/%s, volumeName %s", vrr.Namespace, vrr.Name, pvName)
		rep, err := h.csiClient.CreateVolume(createCtx, &req)
		if err != nil {
			return fmt.Errorf("CSI CreateVolume failed: %w", err)
		}

		if rep.Volume == nil {
			return fmt.Errorf("CSI CreateVolume returned nil volume")
		}

		klog.V(2).Infof("restoreFromVSC: CSI CreateVolume succeeded for VRR %s/%s, volumeHandle %s", vrr.Namespace, vrr.Name, rep.Volume.VolumeId)

		// Step 3: Create PV from CSI response
		pv, err = createPVFromCSIResponse(
			rep.Volume,
			sc,
			pvName,
			h.driverName,
			h.identity,
			volumeCaps,
			h.defaultFSType,
		)
		if err != nil {
			return fmt.Errorf("failed to create PV object from CSI response: %w", err)
		}

		// Create PV in Kubernetes
		createdPV, err := h.kubeClient.CoreV1().PersistentVolumes().Create(ctx, pv, metav1.CreateOptions{})
		if err != nil {
			if apierrors.IsAlreadyExists(err) {
				// Race condition: PV was created between Get and Create
				klog.V(2).Infof("restoreFromVSC: PV %s was created concurrently, getting existing PV", pvName)
				existingPV, getErr := h.kubeClient.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
				if getErr != nil {
					return fmt.Errorf("PV %s already exists but failed to get it: %w", pvName, getErr)
				}
				pv = existingPV
			} else {
				return fmt.Errorf("failed to create PV %s: %w", pvName, err)
			}
		} else {
			klog.V(2).Infof("restoreFromVSC: successfully created PV %s for VRR %s/%s", pvName, vrr.Namespace, vrr.Name)
			h.eventRecorder.Eventf(vrr, v1.EventTypeNormal, "VRRPVCreated",
				"Successfully created PV %s from snapshot", pvName)
			pv = createdPV
		}
	}

	// Step 4: Create PVC from VRR spec (always attempt, even if PV already existed)
	if err := h.createPVCFromVRR(ctx, vrr, pv); err != nil {
		return fmt.Errorf("failed to create PVC: %w", err)
	}

	// Step 5: Wait for PVC to become Bound
	if err := h.waitForPVCBound(ctx, vrr); err != nil {
		return fmt.Errorf("failed to wait for PVC Bound: %w", err)
	}

	// Step 6: Update VRR status to Ready=True
	if err := h.updateVRRReady(ctx, vrr); err != nil {
		// Log error but don't fail - status update is best-effort
		klog.Warningf("Failed to update VRR %s/%s status to Ready: %v", vrr.Namespace, vrr.Name, err)
	}

	return nil
}

// createPVCFromVRR creates a PersistentVolumeClaim from VRR spec.
//
// CONTRACT:
// - PVC MUST have VolumeName set explicitly (no scheduler binding)
// - PVC MUST NOT have dataSource or dataSourceRef
// - PVC MAY have OwnerReference to ObjectKeeper (from VRR annotation)
// - AlreadyExists is treated as idempotent success
func (h *VRRHandler) createPVCFromVRR(ctx context.Context, vrr *storagev1alpha1.VolumeRestoreRequest, pv *v1.PersistentVolume) error {
	// Build PVC spec
	storageClassName := vrr.Spec.StorageClassName
	pvc := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vrr.Spec.TargetPVCName,
			Namespace: vrr.Spec.TargetNamespace,
		},
		Spec: v1.PersistentVolumeClaimSpec{
			StorageClassName: &storageClassName,
			VolumeName:       pv.Name, // CRITICAL: explicit binding, no scheduler
			AccessModes:      pv.Spec.AccessModes,
			VolumeMode:       pv.Spec.VolumeMode,
			Resources: v1.VolumeResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceStorage: pv.Spec.Capacity[v1.ResourceStorage],
				},
			},
			// CRITICAL: NO dataSource or dataSourceRef
			// This is restore without VolumeSnapshot API
		},
	}

	// Set OwnerReference to ObjectKeeper if annotation exists
	// external-provisioner does NOT search for ObjectKeeper - it trusts VRR controller
	if refStr, ok := vrr.Annotations["storage.deckhouse.io/object-keeper-ref"]; ok && refStr != "" {
		ownerRef, err := parseObjectKeeperRef(refStr)
		if err != nil {
			// Log error but don't fail - VRR controller will handle cleanup
			klog.Warningf("Failed to parse ObjectKeeper ref from VRR %s/%s annotation: %v", vrr.Namespace, vrr.Name, err)
		} else {
			pvc.OwnerReferences = []metav1.OwnerReference{ownerRef}
		}
	}

	// Create PVC in Kubernetes
	_, err := h.kubeClient.CoreV1().PersistentVolumeClaims(vrr.Spec.TargetNamespace).Create(ctx, pvc, metav1.CreateOptions{})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			// PVC already exists - this is idempotent success
			klog.V(2).Infof("createPVCFromVRR: PVC %s/%s already exists, considering success", vrr.Spec.TargetNamespace, vrr.Spec.TargetPVCName)
			return nil
		}
		return fmt.Errorf("failed to create PVC %s/%s: %w", vrr.Spec.TargetNamespace, vrr.Spec.TargetPVCName, err)
	}

	klog.V(2).Infof("createPVCFromVRR: successfully created PVC %s/%s for VRR %s/%s", vrr.Spec.TargetNamespace, vrr.Spec.TargetPVCName, vrr.Namespace, vrr.Name)
	h.eventRecorder.Eventf(vrr, v1.EventTypeNormal, "VRRPVCCreated",
		"Successfully created PVC %s/%s", vrr.Spec.TargetNamespace, vrr.Spec.TargetPVCName)

	return nil
}

// parseObjectKeeperRef parses ObjectKeeper reference from annotation string.
//
// Expected format: "apiVersion|kind|name|uid"
// Example: "deckhouse.io/v1alpha1|ObjectKeeper|ret-pvc-abc123|def456"
//
// external-provisioner does NOT validate existence - it trusts VRR controller.
func parseObjectKeeperRef(refStr string) (metav1.OwnerReference, error) {
	parts := strings.Split(refStr, "|")
	if len(parts) != 4 {
		return metav1.OwnerReference{}, fmt.Errorf("invalid ObjectKeeper ref format: expected 'apiVersion|kind|name|uid', got %d parts", len(parts))
	}

	apiVersion, kind, name, uid := parts[0], parts[1], parts[2], parts[3]
	if apiVersion == "" || kind == "" || name == "" || uid == "" {
		return metav1.OwnerReference{}, fmt.Errorf("invalid ObjectKeeper ref: empty fields not allowed")
	}

	controller := true
	return metav1.OwnerReference{
		APIVersion: apiVersion,
		Kind:       kind,
		Name:       name,
		UID:        types.UID(uid),
		Controller: &controller,
	}, nil
}

// restoreFromPV handles restore from PersistentVolume (clone operation).
//
// Step 1: Get source PV and extract volume handle (isolated contract with PV API).
func (h *VRRHandler) restoreFromPV(ctx context.Context, vrr *storagev1alpha1.VolumeRestoreRequest) error {
	// Get source PV
	sourcePV, err := h.kubeClient.CoreV1().PersistentVolumes().Get(ctx, vrr.Spec.SourceRef.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("PersistentVolume %s not found", vrr.Spec.SourceRef.Name)
		}
		return fmt.Errorf("failed to get PersistentVolume %s: %w", vrr.Spec.SourceRef.Name, err)
	}

	// Check if PV is being deleted
	if sourcePV.ObjectMeta.DeletionTimestamp != nil {
		return fmt.Errorf("PersistentVolume %s is currently being deleted", sourcePV.Name)
	}

	// Check if PV has CSI source
	if sourcePV.Spec.CSI == nil {
		return fmt.Errorf("PersistentVolume %s does not have CSI source", sourcePV.Name)
	}

	// Check if PV driver matches our driver
	if sourcePV.Spec.CSI.Driver != h.driverName {
		return fmt.Errorf("PersistentVolume %s is handled by a different CSI driver (%s), expected %s", sourcePV.Name, sourcePV.Spec.CSI.Driver, h.driverName)
	}

	// Extract volume handle
	volumeHandle := sourcePV.Spec.CSI.VolumeHandle
	if volumeHandle == "" {
		return fmt.Errorf("PersistentVolume %s has empty VolumeHandle", sourcePV.Name)
	}

	klog.V(2).Infof("restoreFromPV: VRR %s/%s, source PV %s, volumeHandle %s", vrr.Namespace, vrr.Name, sourcePV.Name, volumeHandle)

	// Step 2: CSI CreateVolume call (clone operation)
	sc, err := h.scLister.Get(vrr.Spec.StorageClassName)
	if err != nil {
		return fmt.Errorf("failed to get StorageClass %s: %w", vrr.Spec.StorageClassName, err)
	}

	// Generate volume name from VRR UID
	pvName, err := makeVolumeName(h.volumeNamePrefix, string(vrr.ObjectMeta.UID), h.volumeNameUUIDLength)
	if err != nil {
		return fmt.Errorf("failed to generate volume name: %w", err)
	}

	// Check if PV already exists (idempotency: previous attempt may have created PV but not PVC)
	var pv *v1.PersistentVolume
	existingPV, err := h.kubeClient.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
	if err == nil && existingPV != nil {
		// PV already exists - use it and continue to ensure PVC is created
		klog.V(2).Infof("restoreFromPV: PV %s already exists, skipping CSI CreateVolume and continuing to PVC creation", pvName)
		pv = existingPV
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to check if PV %s exists: %w", pvName, err)
	} else {
		// PV does not exist - proceed with CSI CreateVolume
		// Get volume capabilities from source PV
		volumeCaps, err := h.getVolumeCapabilitiesFromPV(sourcePV, sc)
		if err != nil {
			return fmt.Errorf("failed to get volume capabilities from PV: %w", err)
		}

		// Get capacity from source PV
		capacityBytes := int64(1024 * 1024 * 1024) // 1Gi default
		if capacity, ok := sourcePV.Spec.Capacity[v1.ResourceStorage]; ok {
			capacityBytes = capacity.Value()
		}

		// Resolve provisioner secret credentials from StorageClass
		// Note: We pass nil for PVC since it doesn't exist yet - secret templates can use PV name
		provisionerSecretRef, err := getSecretReference(provisionerSecretParams, sc.Parameters, pvName, nil)
		if err != nil {
			return fmt.Errorf("failed to get provisioner secret reference: %w", err)
		}
		provisionerCredentials, err := getCredentials(ctx, h.kubeClient, provisionerSecretRef)
		if err != nil {
			return fmt.Errorf("failed to get provisioner credentials: %w", err)
		}

		// Build CreateVolumeRequest for clone
		req := csi.CreateVolumeRequest{
			Name:               pvName,
			Parameters:         sc.Parameters,
			VolumeCapabilities: volumeCaps,
			CapacityRange: &csi.CapacityRange{
				RequiredBytes: capacityBytes,
			},
			VolumeContentSource: &csi.VolumeContentSource{
				Type: &csi.VolumeContentSource_Volume{
					Volume: &csi.VolumeContentSource_VolumeSource{
						VolumeId: volumeHandle,
					},
				},
			},
			Secrets: provisionerCredentials,
		}

		// Call CSI CreateVolume with timeout
		createCtx, cancel := context.WithTimeout(ctx, h.timeout)
		defer cancel()

		klog.V(2).Infof("restoreFromPV: calling CSI CreateVolume (clone) for VRR %s/%s, volumeName %s", vrr.Namespace, vrr.Name, pvName)
		rep, err := h.csiClient.CreateVolume(createCtx, &req)
		if err != nil {
			return fmt.Errorf("CSI CreateVolume (clone) failed: %w", err)
		}

		if rep.Volume == nil {
			return fmt.Errorf("CSI CreateVolume returned nil volume")
		}

		klog.V(2).Infof("restoreFromPV: CSI CreateVolume (clone) succeeded for VRR %s/%s, volumeHandle %s", vrr.Namespace, vrr.Name, rep.Volume.VolumeId)

		// Step 3: Create PV from CSI response
		pv, err = createPVFromCSIResponse(
			rep.Volume,
			sc,
			pvName,
			h.driverName,
			h.identity,
			volumeCaps,
			h.getFSTypeFromPV(sourcePV),
		)
		if err != nil {
			return fmt.Errorf("failed to create PV object from CSI response: %w", err)
		}

		// Create PV in Kubernetes
		createdPV, err := h.kubeClient.CoreV1().PersistentVolumes().Create(ctx, pv, metav1.CreateOptions{})
		if err != nil {
			if apierrors.IsAlreadyExists(err) {
				// Race condition: PV was created between Get and Create
				klog.V(2).Infof("restoreFromPV: PV %s was created concurrently, getting existing PV", pvName)
				existingPV, getErr := h.kubeClient.CoreV1().PersistentVolumes().Get(ctx, pvName, metav1.GetOptions{})
				if getErr != nil {
					return fmt.Errorf("PV %s already exists but failed to get it: %w", pvName, getErr)
				}
				pv = existingPV
			} else {
				return fmt.Errorf("failed to create PV %s: %w", pvName, err)
			}
		} else {
			klog.V(2).Infof("restoreFromPV: successfully created PV %s for VRR %s/%s", pvName, vrr.Namespace, vrr.Name)
			h.eventRecorder.Eventf(vrr, v1.EventTypeNormal, "VRRPVCreated",
				"Successfully created PV %s from PersistentVolume %s", pvName, sourcePV.Name)
			pv = createdPV
		}
	}

	// Step 4: Create PVC from VRR spec (always attempt, even if PV already existed)
	if err := h.createPVCFromVRR(ctx, vrr, pv); err != nil {
		return fmt.Errorf("failed to create PVC: %w", err)
	}

	// Step 5: Wait for PVC to become Bound
	if err := h.waitForPVCBound(ctx, vrr); err != nil {
		return fmt.Errorf("failed to wait for PVC Bound: %w", err)
	}

	// Step 6: Update VRR status to Ready=True
	if err := h.updateVRRReady(ctx, vrr); err != nil {
		// Log error but don't fail - status update is best-effort
		klog.Warningf("Failed to update VRR %s/%s status to Ready: %v", vrr.Namespace, vrr.Name, err)
	}

	return nil
}

// createPVFromCSIResponse creates a PersistentVolume from CSI CreateVolume response.
//
// This function is extracted from csiProvisioner logic to be reusable for VRR restore operations.
// It creates a PV object with proper annotations, topology, and volume attributes.
func createPVFromCSIResponse(
	volume *csi.Volume,
	sc *storagev1.StorageClass,
	pvName string,
	driverName string,
	identity string,
	volumeCaps []*csi.VolumeCapability,
	fsType string,
) (*v1.PersistentVolume, error) {
	respCap := volume.GetCapacityBytes()
	if respCap == 0 {
		// According to CSI spec, capacity = 0 means unknown, use default
		respCap = 1024 * 1024 * 1024 // 1Gi default
	}

	// Build volume attributes
	volumeAttributes := map[string]string{
		"storage.kubernetes.io/csiProvisionerIdentity": identity,
	}
	if volume.VolumeContext != nil {
		maps.Copy(volumeAttributes, volume.VolumeContext)
	}

	// Determine access modes from volume capabilities
	accessModes := []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce} // Default
	if len(volumeCaps) > 0 {
		csiMode := volumeCaps[0].GetAccessMode().GetMode()
		switch csiMode {
		case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER:
			accessModes = []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce}
		case csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:
			accessModes = []v1.PersistentVolumeAccessMode{v1.ReadWriteMany}
		case csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY:
			accessModes = []v1.PersistentVolumeAccessMode{v1.ReadOnlyMany}
		}
	}

	// Determine volume mode from capabilities
	var volumeMode *v1.PersistentVolumeMode
	if len(volumeCaps) > 0 {
		if volumeCaps[0].GetBlock() != nil {
			blockMode := v1.PersistentVolumeBlock
			volumeMode = &blockMode
		} else {
			fsMode := v1.PersistentVolumeFilesystem
			volumeMode = &fsMode
		}
	}

	// Determine if PV should be read-only
	pvReadOnly := false
	if len(volumeCaps) == 1 && volumeCaps[0].GetAccessMode().GetMode() == csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY {
		pvReadOnly = true
	}

	// Create CSI PV source
	csiPVSource := &v1.CSIPersistentVolumeSource{
		Driver:           driverName,
		VolumeHandle:     volume.VolumeId,
		VolumeAttributes: volumeAttributes,
		ReadOnly:         pvReadOnly,
	}

	// Set FSType if not block volume
	if volumeMode == nil || *volumeMode == v1.PersistentVolumeFilesystem {
		csiPVSource.FSType = fsType
	}

	// Create PV object
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvName,
		},
		Spec: v1.PersistentVolumeSpec{
			AccessModes:  accessModes,
			MountOptions: sc.MountOptions,
			Capacity: v1.ResourceList{
				v1.ResourceStorage: bytesToQuantity(respCap),
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				CSI: csiPVSource,
			},
			VolumeMode: volumeMode,
		},
	}

	// Set deletion secret annotations (empty for VRR - no deletion secrets)
	metav1.SetMetaDataAnnotation(&pv.ObjectMeta, "volume.kubernetes.io/provisioner-deletion-secret-name", "")
	metav1.SetMetaDataAnnotation(&pv.ObjectMeta, "volume.kubernetes.io/provisioner-deletion-secret-namespace", "")

	// Set reclaim policy from StorageClass
	if sc.ReclaimPolicy != nil {
		pv.Spec.PersistentVolumeReclaimPolicy = *sc.ReclaimPolicy
	}

	// Set topology/node affinity if available
	if len(volume.AccessibleTopology) > 0 {
		pv.Spec.NodeAffinity = GenerateVolumeNodeAffinity(volume.AccessibleTopology)
	}

	return pv, nil
}

// getVolumeCapabilitiesFromPV converts PV AccessModes and VolumeMode to CSI VolumeCapabilities.
func (h *VRRHandler) getVolumeCapabilitiesFromPV(pv *v1.PersistentVolume, sc *storagev1.StorageClass) ([]*csi.VolumeCapability, error) {
	supportsSingleNodeMultiWriter := h.controllerCapabilities[csi.ControllerServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER]

	// Convert AccessModes to CSI access mode
	accessModes := pv.Spec.AccessModes
	if len(accessModes) == 0 {
		// Default to RWO if no access modes specified
		accessModes = []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce}
	}

	csiAccessMode, err := accessmodes.ToCSIAccessMode(accessModes, supportsSingleNodeMultiWriter)
	if err != nil {
		return nil, fmt.Errorf("failed to convert AccessModes to CSI access mode: %w", err)
	}

	// Determine access type (Block or Mount)
	isBlock := pv.Spec.VolumeMode != nil && *pv.Spec.VolumeMode == v1.PersistentVolumeBlock

	if isBlock {
		return []*csi.VolumeCapability{
			{
				AccessType: getAccessTypeBlock(),
				AccessMode: &csi.VolumeCapability_AccessMode{
					Mode: csiAccessMode,
				},
			},
		}, nil
	}

	// Mount volume - use FSType from PV or default
	fsType := h.getFSTypeFromPV(pv)
	mountFlags := []string{}
	if sc != nil {
		mountFlags = sc.MountOptions
	}

	return []*csi.VolumeCapability{
		{
			AccessType: getAccessTypeMount(fsType, mountFlags),
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csiAccessMode,
			},
		},
	}, nil
}

// getFSTypeFromPV extracts FSType from PV, with fallback to default.
func (h *VRRHandler) getFSTypeFromPV(pv *v1.PersistentVolume) string {
	if pv.Spec.CSI != nil && pv.Spec.CSI.FSType != "" {
		return pv.Spec.CSI.FSType
	}
	return h.defaultFSType
}

// waitForPVCBound waits for PVC to become Bound with timeout.
// Returns error if PVC does not become Bound within the timeout period.
//
// NOTE: PVC Bound waiting is implemented via polling (wait.PollUntilContextCancel),
// not informer-based watching, to:
// - avoid maintaining additional informers
// - keep VRRHandler logic self-contained
// - reduce memory footprint
//
// Polling interval and timeout are bounded and configurable.
func (h *VRRHandler) waitForPVCBound(ctx context.Context, vrr *storagev1alpha1.VolumeRestoreRequest) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()

	// Poll every 1 second until PVC is Bound or timeout
	err := wait.PollUntilContextCancel(timeoutCtx, 1*time.Second, true, func(ctx context.Context) (bool, error) {
		pvc, err := h.kubeClient.CoreV1().PersistentVolumeClaims(vrr.Spec.TargetNamespace).Get(ctx, vrr.Spec.TargetPVCName, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				// PVC not found yet - continue polling
				return false, nil
			}
			// Other error - stop polling
			return false, fmt.Errorf("failed to get PVC %s/%s: %w", vrr.Spec.TargetNamespace, vrr.Spec.TargetPVCName, err)
		}

		// Check if PVC is Bound
		if pvc.Status.Phase == v1.ClaimBound {
			return true, nil
		}

		// Continue polling
		return false, nil
	})

	if err != nil {
		if err == context.DeadlineExceeded {
			return fmt.Errorf("timeout waiting for PVC %s/%s to become Bound", vrr.Spec.TargetNamespace, vrr.Spec.TargetPVCName)
		}
		return err
	}

	klog.V(2).Infof("waitForPVCBound: PVC %s/%s is now Bound", vrr.Spec.TargetNamespace, vrr.Spec.TargetPVCName)
	return nil
}

// updateVRRReady updates VRR status to Ready=True with completion timestamp.
func (h *VRRHandler) updateVRRReady(ctx context.Context, vrr *storagev1alpha1.VolumeRestoreRequest) error {
	return h.updateVRRStatus(ctx, vrr, metav1.ConditionTrue, storagev1alpha1.ConditionReasonCompleted, fmt.Sprintf("PVC %s/%s restored successfully", vrr.Spec.TargetNamespace, vrr.Spec.TargetPVCName))
}

// updateVRRFailed updates VRR status to Ready=False with error message.
func (h *VRRHandler) updateVRRFailed(ctx context.Context, vrr *storagev1alpha1.VolumeRestoreRequest, err error) error {
	return h.updateVRRStatus(ctx, vrr, metav1.ConditionFalse, storagev1alpha1.ConditionReasonRestoreFailed, err.Error())
}

// updateVRRStatus updates VRR status using dynamic client with retry-on-conflict.
func (h *VRRHandler) updateVRRStatus(ctx context.Context, vrr *storagev1alpha1.VolumeRestoreRequest, status metav1.ConditionStatus, reason, message string) error {
	// Get GVR for VolumeRestoreRequest
	gvr := schema.GroupVersionResource{
		Group:    storagev1alpha1.GroupVersion.Group,
		Version:  storagev1alpha1.GroupVersion.Version,
		Resource: "volumerestorerequests",
	}

	// Retry on conflict
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		// Get current VRR
		unstructuredVRR, err := h.dynamicClient.Resource(gvr).Namespace(vrr.Namespace).Get(ctx, vrr.Name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				// VRR deleted - nothing to update
				return nil
			}
			return fmt.Errorf("failed to get VRR %s/%s: %w", vrr.Namespace, vrr.Name, err)
		}

		// Convert to typed object
		var currentVRR storagev1alpha1.VolumeRestoreRequest
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(unstructuredVRR.Object, &currentVRR); err != nil {
			return fmt.Errorf("failed to convert VRR from unstructured: %w", err)
		}

		// Update status
		now := metav1.Now()
		if currentVRR.Status.CompletionTimestamp == nil {
			currentVRR.Status.CompletionTimestamp = &now
		}

		// Set condition
		setSingleCondition(&currentVRR.Status.Conditions, metav1.Condition{
			Type:               storagev1alpha1.ConditionTypeReady,
			Status:             status,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: now,
		})

		// Set TargetPVCRef on success
		if status == metav1.ConditionTrue {
			currentVRR.Status.TargetPVCRef = &storagev1alpha1.ObjectReference{
				Name:      vrr.Spec.TargetPVCName,
				Namespace: vrr.Spec.TargetNamespace,
			}
		}

		// Convert back to unstructured
		unstructuredStatus, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&currentVRR.Status)
		if err != nil {
			return fmt.Errorf("failed to convert VRR status to unstructured: %w", err)
		}

		// Update status subresource
		// NOTE: metadata reused from fetched object to preserve resourceVersion
		_, err = h.dynamicClient.Resource(gvr).Namespace(vrr.Namespace).UpdateStatus(ctx, &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": unstructuredVRR.GetAPIVersion(),
				"kind":       unstructuredVRR.GetKind(),
				"metadata":   unstructuredVRR.Object["metadata"],
				"status":     unstructuredStatus,
			},
		}, metav1.UpdateOptions{})

		if err != nil {
			if apierrors.IsNotFound(err) {
				// VRR deleted - nothing to update
				return nil
			}
			return err
		}

		klog.V(2).Infof("updateVRRStatus: successfully updated VRR %s/%s status: Ready=%v, reason=%s", vrr.Namespace, vrr.Name, status, reason)
		return nil
	})
}

// setSingleCondition sets a condition, removing any existing condition of the same type first.
func setSingleCondition(conds *[]metav1.Condition, cond metav1.Condition) {
	meta.RemoveStatusCondition(conds, cond.Type)
	meta.SetStatusCondition(conds, cond)
}

// isTerminalVRR checks if VRR is in terminal state (Ready=True or Ready=False).
func isTerminalVRR(conditions []metav1.Condition) bool {
	cond := meta.FindStatusCondition(conditions, storagev1alpha1.ConditionTypeReady)
	return cond != nil && (cond.Status == metav1.ConditionTrue || cond.Status == metav1.ConditionFalse)
}
