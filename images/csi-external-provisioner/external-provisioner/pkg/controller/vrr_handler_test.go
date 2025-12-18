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
	"strings"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/golang/mock/gomock"
	"github.com/kubernetes-csi/csi-lib-utils/rpc"
	"google.golang.org/grpc"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	fakeclientset "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/kubernetes/scheme"
	storagelistersv1 "k8s.io/client-go/listers/storage/v1"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"

	storagev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	crdv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	snapclientset "github.com/kubernetes-csi/external-snapshotter/client/v8/clientset/versioned"
	snapfake "github.com/kubernetes-csi/external-snapshotter/client/v8/clientset/versioned/fake"
)

const (
	vrrTestDriverName      = "test-driver"
	vrrTestIdentity        = "test-identity"
	vrrTestTimeout         = 10 * time.Second
	vrrTestVolumePrefix    = "pvc"
	vrrTestDefaultFSType   = "ext4"
	vrrTestStorageClass    = "test-sc"
	vrrTestTargetNamespace = "test-ns"
	vrrTestTargetPVCName   = "test-pvc"
	vrrTestSnapshotHandle  = "snapshot-handle-123"
	vrrTestVolumeHandle    = "volume-handle-456"
)

func TestHandleVRR_DriverFilter(t *testing.T) {
	tests := []struct {
		name           string
		storageClass   *storagev1.StorageClass
		scListerError  error
		expectedCalled bool
	}{
		{
			name: "matching driver - should process",
			storageClass: &storagev1.StorageClass{
				ObjectMeta:  metav1.ObjectMeta{Name: vrrTestStorageClass},
				Provisioner: vrrTestDriverName,
			},
			expectedCalled: true,
		},
		{
			name: "different driver - should ignore",
			storageClass: &storagev1.StorageClass{
				ObjectMeta:  metav1.ObjectMeta{Name: vrrTestStorageClass},
				Provisioner: "other-driver",
			},
			expectedCalled: false,
		},
		{
			name:           "storage class not found - should ignore",
			scListerError:  apierrors.NewNotFound(storagev1.Resource("storageclass"), vrrTestStorageClass),
			expectedCalled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockCSIClient := &mockControllerClient{}
			mockSCLister := NewMockStorageClassLister(ctrl)

			vrr := createTestVRR("test-vrr", "test-ns", "VolumeSnapshotContent", "test-vsc")
			vrrObj := toUnstructured(t, vrr)

			// Setup StorageClass lister
			if tt.scListerError != nil {
				mockSCLister.errors[vrrTestStorageClass] = tt.scListerError
			} else if tt.storageClass != nil {
				mockSCLister.storageClasses[vrrTestStorageClass] = tt.storageClass
			}

			handler := createTestVRRHandler(mockCSIClient, mockSCLister, nil, nil)

			handler.HandleVRR(context.Background(), vrrObj)

			// Test passes if no panic/error occurs
		})
	}
}

func TestHandleVRR_EmptyStorageClassName(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCSIClient := &mockControllerClient{}
	mockSCLister := NewMockStorageClassLister(ctrl)

	vrr := createTestVRR("test-vrr", "test-ns", "VolumeSnapshotContent", "test-vsc")
	vrr.Spec.StorageClassName = "" // Empty StorageClassName
	vrrObj := toUnstructured(t, vrr)

	kubeClient := fakeclientset.NewSimpleClientset()
	snapshotClient := snapfake.NewSimpleClientset()
	eventRecorder := record.NewFakeRecorder(10)

	handler := createTestVRRHandlerWithClients(mockCSIClient, mockSCLister, kubeClient, snapshotClient, eventRecorder)

	handler.HandleVRR(context.Background(), vrrObj)

	// Verify Event was recorded
	found := false
	select {
	case event := <-eventRecorder.Events:
		if strings.Contains(event, "VRRInvalidSpec") && strings.Contains(event, "StorageClassName") {
			found = true
		}
	default:
		// No event received
	}
	if !found {
		t.Error("Expected Event with reason VRRInvalidSpec about StorageClassName")
	}

	// Verify no PVC was created
	pvcs, err := kubeClient.CoreV1().PersistentVolumeClaims(vrrTestTargetNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failed to list PVCs: %v", err)
	}
	if len(pvcs.Items) > 0 {
		t.Error("No PVC should be created when StorageClassName is empty")
	}
}

func TestHandleVRR_PVCAlreadyExists(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCSIClient := &mockControllerClient{}
	mockSCLister := NewMockStorageClassLister(ctrl)

	vrr := createTestVRR("test-vrr", "test-ns", "VolumeSnapshotContent", "test-vsc")
	vrrObj := toUnstructured(t, vrr)

	sc := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: vrrTestStorageClass},
		Provisioner: vrrTestDriverName,
	}

	existingPVC := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vrrTestTargetPVCName,
			Namespace: vrrTestTargetNamespace,
		},
	}

	kubeClient := fakeclientset.NewSimpleClientset(existingPVC)
	snapshotClient := snapfake.NewSimpleClientset()

	mockSCLister.storageClasses[vrrTestStorageClass] = sc

	handler := createTestVRRHandlerWithClients(mockCSIClient, mockSCLister, kubeClient, snapshotClient, nil)

	// Should not call CSI CreateVolume because PVC already exists
	handler.HandleVRR(context.Background(), vrrObj)

	// Test passes if no CSI call was made (no expectations set)
}

func TestRestoreFromVSC_HappyPath(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCSIClient := &mockControllerClient{}
	mockSCLister := NewMockStorageClassLister(ctrl)

	vrr := createTestVRR("test-vrr", "test-ns", "VolumeSnapshotContent", "test-vsc")
	sc := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: vrrTestStorageClass},
		Provisioner: vrrTestDriverName,
		Parameters:  map[string]string{},
	}

	vsc := &crdv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "test-vsc"},
		Spec: crdv1.VolumeSnapshotContentSpec{
			Driver: "test-driver",
		},
		Status: &crdv1.VolumeSnapshotContentStatus{
			ReadyToUse:     boolPtr(true),
			SnapshotHandle: stringPtr(vrrTestSnapshotHandle),
			RestoreSize:    int64Ptr(1024 * 1024 * 1024),
		},
	}

	kubeClient := fakeclientset.NewSimpleClientset()
	snapshotClient := snapfake.NewSimpleClientset(vsc)
	eventRecorder := record.NewFakeRecorder(10)

	mockSCLister.storageClasses[vrrTestStorageClass] = sc

	mockCSIClient.createVolumeFunc = func(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
		// Verify request
		if req.VolumeContentSource == nil {
			t.Error("VolumeContentSource should be set")
		}
		snapshotSource, ok := req.VolumeContentSource.Type.(*csi.VolumeContentSource_Snapshot)
		if !ok {
			t.Error("VolumeContentSource should be Snapshot type")
		}
		if snapshotSource.Snapshot.SnapshotId != vrrTestSnapshotHandle {
			t.Errorf("Expected snapshot handle %s, got %s", vrrTestSnapshotHandle, snapshotSource.Snapshot.SnapshotId)
		}
		return &csi.CreateVolumeResponse{
			Volume: &csi.Volume{
				VolumeId:      "new-volume-id",
				CapacityBytes: 1024 * 1024 * 1024,
			},
		}, nil
	}

	handler := createTestVRRHandlerWithClients(mockCSIClient, mockSCLister, kubeClient, snapshotClient, eventRecorder)
	addVRRToDynamicClient(t, handler, vrr)

	err := handler.restoreFromVSC(context.Background(), vrr)
	if err != nil {
		t.Fatalf("restoreFromVSC failed: %v", err)
	}

	// Verify PV was created
	pvs, err := kubeClient.CoreV1().PersistentVolumes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failed to list PVs: %v", err)
	}
	if len(pvs.Items) != 1 {
		t.Errorf("Expected 1 PV, got %d", len(pvs.Items))
	}

	// Verify PVC was created
	pvcs, err := kubeClient.CoreV1().PersistentVolumeClaims(vrrTestTargetNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failed to list PVCs: %v", err)
	}
	if len(pvcs.Items) != 1 {
		t.Errorf("Expected 1 PVC, got %d", len(pvcs.Items))
	}
	if pvcs.Items[0].Spec.VolumeName != pvs.Items[0].Name {
		t.Errorf("PVC VolumeName should match PV name")
	}
}

func TestRestoreFromVSC_VSCNotFound(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCSIClient := &mockControllerClient{}
	mockSCLister := NewMockStorageClassLister(ctrl)

	vrr := createTestVRR("test-vrr", "test-ns", "VolumeSnapshotContent", "non-existent-vsc")
	sc := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: vrrTestStorageClass},
		Provisioner: vrrTestDriverName,
	}

	kubeClient := fakeclientset.NewSimpleClientset()
	snapshotClient := snapfake.NewSimpleClientset() // Empty - VSC doesn't exist
	eventRecorder := record.NewFakeRecorder(10)

	mockSCLister.storageClasses[vrrTestStorageClass] = sc

	handler := createTestVRRHandlerWithClients(mockCSIClient, mockSCLister, kubeClient, snapshotClient, eventRecorder)
	addVRRToDynamicClient(t, handler, vrr)

	err := handler.restoreFromVSC(context.Background(), vrr)
	if err == nil {
		t.Fatal("Expected error when VSC not found")
	}
	if !contains(err.Error(), "not found") {
		t.Errorf("Expected 'not found' error, got: %v", err)
	}
}

func TestRestoreFromVSC_PVAlreadyExists_PVCCreated(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCSIClient := &mockControllerClient{}
	mockSCLister := NewMockStorageClassLister(ctrl)

	vrr := createTestVRR("test-vrr", "test-ns", "VolumeSnapshotContent", "test-vsc")
	sc := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: vrrTestStorageClass},
		Provisioner: vrrTestDriverName,
		Parameters:  map[string]string{},
	}

	vsc := &crdv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "test-vsc"},
		Spec: crdv1.VolumeSnapshotContentSpec{
			Driver: "test-driver",
		},
		Status: &crdv1.VolumeSnapshotContentStatus{
			ReadyToUse:     boolPtr(true),
			SnapshotHandle: stringPtr(vrrTestSnapshotHandle),
			RestoreSize:    int64Ptr(1024 * 1024 * 1024),
		},
	}

	// Generate PV name that will be used
	pvName, err := makeVolumeName(vrrTestVolumePrefix, string(vrr.ObjectMeta.UID), -1)
	if err != nil {
		t.Fatalf("Failed to generate PV name: %v", err)
	}

	// Pre-create PV with the expected name (simulating previous partial execution)
	existingPV := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvName},
		Spec: v1.PersistentVolumeSpec{
			AccessModes: []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
			Capacity: v1.ResourceList{
				v1.ResourceStorage: resource.MustParse("1Gi"),
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				CSI: &v1.CSIPersistentVolumeSource{
					Driver:       vrrTestDriverName,
					VolumeHandle: "existing-volume-id",
					FSType:       "ext4",
				},
			},
		},
	}

	kubeClient := fakeclientset.NewSimpleClientset(existingPV)
	snapshotClient := snapfake.NewSimpleClientset(vsc)
	eventRecorder := record.NewFakeRecorder(10)

	mockSCLister.storageClasses[vrrTestStorageClass] = sc

	mockCSIClient.createVolumeFunc = func(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
		// CSI CreateVolume should not be called when PV already exists
		t.Error("CSI CreateVolume should not be called when PV already exists")
		return nil, nil
	}

	handler := createTestVRRHandlerWithClients(mockCSIClient, mockSCLister, kubeClient, snapshotClient, eventRecorder)
	addVRRToDynamicClient(t, handler, vrr)

	err = handler.restoreFromVSC(context.Background(), vrr)
	if err != nil {
		t.Fatalf("restoreFromVSC failed: %v", err)
	}

	// Verify PVC was created despite PV already existing
	pvcs, err := kubeClient.CoreV1().PersistentVolumeClaims(vrrTestTargetNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failed to list PVCs: %v", err)
	}
	if len(pvcs.Items) != 1 {
		t.Errorf("Expected 1 PVC, got %d", len(pvcs.Items))
	}
	if pvcs.Items[0].Spec.VolumeName != pvName {
		t.Errorf("Expected PVC VolumeName %s, got %s", pvName, pvcs.Items[0].Spec.VolumeName)
	}
}

func TestRestoreFromVSC_VSCNotReady(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCSIClient := &mockControllerClient{}
	mockSCLister := NewMockStorageClassLister(ctrl)

	vrr := createTestVRR("test-vrr", "test-ns", "VolumeSnapshotContent", "test-vsc")
	sc := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: vrrTestStorageClass},
		Provisioner: vrrTestDriverName,
	}

	vsc := &crdv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: "test-vsc"},
		Spec: crdv1.VolumeSnapshotContentSpec{
			Driver: "test-driver",
		},
		Status: &crdv1.VolumeSnapshotContentStatus{
			ReadyToUse: boolPtr(false), // Not ready
		},
	}

	kubeClient := fakeclientset.NewSimpleClientset()
	snapshotClient := snapfake.NewSimpleClientset(vsc)
	eventRecorder := record.NewFakeRecorder(10)

	mockSCLister.storageClasses[vrrTestStorageClass] = sc

	handler := createTestVRRHandlerWithClients(mockCSIClient, mockSCLister, kubeClient, snapshotClient, eventRecorder)
	addVRRToDynamicClient(t, handler, vrr)

	err := handler.restoreFromVSC(context.Background(), vrr)
	if err == nil {
		t.Fatal("Expected error when VSC not ready")
	}
	if !contains(err.Error(), "not ReadyToUse") {
		t.Errorf("Expected 'not ReadyToUse' error, got: %v", err)
	}
}

func TestRestoreFromPV_HappyPath(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCSIClient := &mockControllerClient{}
	mockSCLister := NewMockStorageClassLister(ctrl)

	vrr := createTestVRR("test-vrr", "test-ns", "PersistentVolume", "source-pv")
	sc := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: vrrTestStorageClass},
		Provisioner: vrrTestDriverName,
		Parameters:  map[string]string{},
	}

	sourcePV := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "source-pv"},
		Spec: v1.PersistentVolumeSpec{
			AccessModes: []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
			Capacity: v1.ResourceList{
				v1.ResourceStorage: resource.MustParse("1Gi"),
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				CSI: &v1.CSIPersistentVolumeSource{
					Driver:       vrrTestDriverName,
					VolumeHandle: vrrTestVolumeHandle,
					FSType:       "ext4",
				},
			},
		},
	}

	kubeClient := fakeclientset.NewSimpleClientset(sourcePV)
	snapshotClient := snapfake.NewSimpleClientset()
	eventRecorder := record.NewFakeRecorder(10)

	mockSCLister.storageClasses[vrrTestStorageClass] = sc

	mockCSIClient.createVolumeFunc = func(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
		// Verify request
		if req.VolumeContentSource == nil {
			t.Error("VolumeContentSource should be set")
		}
		volumeSource, ok := req.VolumeContentSource.Type.(*csi.VolumeContentSource_Volume)
		if !ok {
			t.Error("VolumeContentSource should be Volume type")
		}
		if volumeSource.Volume.VolumeId != vrrTestVolumeHandle {
			t.Errorf("Expected volume handle %s, got %s", vrrTestVolumeHandle, volumeSource.Volume.VolumeId)
		}
		return &csi.CreateVolumeResponse{
			Volume: &csi.Volume{
				VolumeId:      "new-volume-id",
				CapacityBytes: 1024 * 1024 * 1024,
			},
		}, nil
	}

	handler := createTestVRRHandlerWithClients(mockCSIClient, mockSCLister, kubeClient, snapshotClient, eventRecorder)
	addVRRToDynamicClient(t, handler, vrr)

	err := handler.restoreFromPV(context.Background(), vrr)
	if err != nil {
		t.Fatalf("restoreFromPV failed: %v", err)
	}

	// Verify PV was created
	pvs, err := kubeClient.CoreV1().PersistentVolumes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failed to list PVs: %v", err)
	}
	if len(pvs.Items) != 2 { // Source PV + new PV
		t.Errorf("Expected 2 PVs, got %d", len(pvs.Items))
	}

	// Verify PVC was created
	pvcs, err := kubeClient.CoreV1().PersistentVolumeClaims(vrrTestTargetNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failed to list PVCs: %v", err)
	}
	if len(pvcs.Items) != 1 {
		t.Errorf("Expected 1 PVC, got %d", len(pvcs.Items))
	}
}

func TestRestoreFromPV_PVAlreadyExists_PVCCreated(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCSIClient := &mockControllerClient{}
	mockSCLister := NewMockStorageClassLister(ctrl)

	vrr := createTestVRR("test-vrr", "test-ns", "PersistentVolume", "source-pv")
	sc := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: vrrTestStorageClass},
		Provisioner: vrrTestDriverName,
		Parameters:  map[string]string{},
	}

	sourcePV := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "source-pv"},
		Spec: v1.PersistentVolumeSpec{
			AccessModes: []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
			Capacity: v1.ResourceList{
				v1.ResourceStorage: resource.MustParse("1Gi"),
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				CSI: &v1.CSIPersistentVolumeSource{
					Driver:       vrrTestDriverName,
					VolumeHandle: vrrTestVolumeHandle,
					FSType:       "ext4",
				},
			},
		},
	}

	// Generate PV name that will be used (same as handler does)
	pvName, err := makeVolumeName(vrrTestVolumePrefix, string(vrr.ObjectMeta.UID), -1)
	if err != nil {
		t.Fatalf("Failed to generate PV name: %v", err)
	}

	// Pre-create PV with the expected name (simulating previous partial execution)
	existingPV := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvName},
		Spec: v1.PersistentVolumeSpec{
			AccessModes: []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
			Capacity: v1.ResourceList{
				v1.ResourceStorage: resource.MustParse("1Gi"),
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				CSI: &v1.CSIPersistentVolumeSource{
					Driver:       vrrTestDriverName,
					VolumeHandle: "existing-volume-id",
					FSType:       "ext4",
				},
			},
		},
	}

	kubeClient := fakeclientset.NewSimpleClientset(sourcePV, existingPV)
	snapshotClient := snapfake.NewSimpleClientset()
	eventRecorder := record.NewFakeRecorder(10)

	mockSCLister.storageClasses[vrrTestStorageClass] = sc

	mockCSIClient.createVolumeFunc = func(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
		// CSI CreateVolume should not be called when PV already exists
		t.Error("CSI CreateVolume should not be called when PV already exists")
		return nil, nil
	}

	handler := createTestVRRHandlerWithClients(mockCSIClient, mockSCLister, kubeClient, snapshotClient, eventRecorder)
	addVRRToDynamicClient(t, handler, vrr)

	err = handler.restoreFromPV(context.Background(), vrr)
	if err != nil {
		t.Fatalf("restoreFromPV failed: %v", err)
	}

	// Verify PVC was created despite PV already existing
	pvcs, err := kubeClient.CoreV1().PersistentVolumeClaims(vrrTestTargetNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failed to list PVCs: %v", err)
	}
	if len(pvcs.Items) != 1 {
		t.Errorf("Expected 1 PVC, got %d", len(pvcs.Items))
	}
	if pvcs.Items[0].Spec.VolumeName != pvName {
		t.Errorf("Expected PVC VolumeName %s, got %s", pvName, pvcs.Items[0].Spec.VolumeName)
	}
}

func TestRestoreFromPV_PVAndPVCAAlreadyExist(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCSIClient := &mockControllerClient{}
	mockSCLister := NewMockStorageClassLister(ctrl)

	vrr := createTestVRR("test-vrr", "test-ns", "PersistentVolume", "source-pv")
	sc := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: vrrTestStorageClass},
		Provisioner: vrrTestDriverName,
		Parameters:  map[string]string{},
	}

	sourcePV := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "source-pv"},
		Spec: v1.PersistentVolumeSpec{
			AccessModes: []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
			Capacity: v1.ResourceList{
				v1.ResourceStorage: resource.MustParse("1Gi"),
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				CSI: &v1.CSIPersistentVolumeSource{
					Driver:       vrrTestDriverName,
					VolumeHandle: vrrTestVolumeHandle,
					FSType:       "ext4",
				},
			},
		},
	}

	// Generate PV name
	pvName, err := makeVolumeName(vrrTestVolumePrefix, string(vrr.ObjectMeta.UID), -1)
	if err != nil {
		t.Fatalf("Failed to generate PV name: %v", err)
	}

	// Pre-create both PV and PVC (simulating complete previous execution)
	existingPV := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: pvName},
		Spec: v1.PersistentVolumeSpec{
			AccessModes: []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
			Capacity: v1.ResourceList{
				v1.ResourceStorage: resource.MustParse("1Gi"),
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				CSI: &v1.CSIPersistentVolumeSource{
					Driver:       vrrTestDriverName,
					VolumeHandle: "existing-volume-id",
					FSType:       "ext4",
				},
			},
		},
	}

	existingPVC := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vrrTestTargetPVCName,
			Namespace: vrrTestTargetNamespace,
		},
		Spec: v1.PersistentVolumeClaimSpec{
			VolumeName: pvName,
		},
		Status: v1.PersistentVolumeClaimStatus{
			Phase: v1.ClaimBound, // PVC is already Bound
		},
	}

	kubeClient := fakeclientset.NewSimpleClientset(sourcePV, existingPV, existingPVC)
	snapshotClient := snapfake.NewSimpleClientset()
	eventRecorder := record.NewFakeRecorder(10)

	mockSCLister.storageClasses[vrrTestStorageClass] = sc

	handler := createTestVRRHandlerWithClients(mockCSIClient, mockSCLister, kubeClient, snapshotClient, eventRecorder)
	addVRRToDynamicClient(t, handler, vrr)

	err = handler.restoreFromPV(context.Background(), vrr)
	if err != nil {
		t.Fatalf("restoreFromPV failed: %v", err)
	}

	// Verify no new PVCs were created
	pvcs, err := kubeClient.CoreV1().PersistentVolumeClaims(vrrTestTargetNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failed to list PVCs: %v", err)
	}
	if len(pvcs.Items) != 1 {
		t.Errorf("Expected 1 PVC, got %d", len(pvcs.Items))
	}
}

func TestRestoreFromPV_ForeignDriver(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCSIClient := &mockControllerClient{}
	mockSCLister := NewMockStorageClassLister(ctrl)

	vrr := createTestVRR("test-vrr", "test-ns", "PersistentVolume", "source-pv")
	sourcePV := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "source-pv"},
		Spec: v1.PersistentVolumeSpec{
			PersistentVolumeSource: v1.PersistentVolumeSource{
				CSI: &v1.CSIPersistentVolumeSource{
					Driver:       "other-driver", // Different driver
					VolumeHandle: vrrTestVolumeHandle,
				},
			},
		},
	}

	kubeClient := fakeclientset.NewSimpleClientset(sourcePV)
	snapshotClient := snapfake.NewSimpleClientset()
	eventRecorder := record.NewFakeRecorder(10)

	sc := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: vrrTestStorageClass},
		Provisioner: vrrTestDriverName,
	}
	mockSCLister.storageClasses[vrrTestStorageClass] = sc

	handler := createTestVRRHandlerWithClients(mockCSIClient, mockSCLister, kubeClient, snapshotClient, eventRecorder)

	err := handler.restoreFromPV(context.Background(), vrr)
	if err == nil {
		t.Fatal("Expected error when PV has different driver")
	}
	if !contains(err.Error(), "different CSI driver") {
		t.Errorf("Expected 'different CSI driver' error, got: %v", err)
	}
}

func TestCreatePVCFromVRR_AlreadyExists(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCSIClient := &mockControllerClient{}
	mockSCLister := NewMockStorageClassLister(ctrl)

	vrr := createTestVRR("test-vrr", "test-ns", "VolumeSnapshotContent", "test-vsc")
	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pv"},
		Spec: v1.PersistentVolumeSpec{
			AccessModes: []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
			Capacity: v1.ResourceList{
				v1.ResourceStorage: resource.MustParse("1Gi"),
			},
		},
	}

	existingPVC := &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vrrTestTargetPVCName,
			Namespace: vrrTestTargetNamespace,
		},
	}

	kubeClient := fakeclientset.NewSimpleClientset(existingPVC)
	snapshotClient := snapfake.NewSimpleClientset()
	eventRecorder := record.NewFakeRecorder(10)

	handler := createTestVRRHandlerWithClients(mockCSIClient, mockSCLister, kubeClient, snapshotClient, eventRecorder)

	// Should succeed (idempotent)
	err := handler.createPVCFromVRR(context.Background(), vrr, pv)
	if err != nil {
		t.Fatalf("createPVCFromVRR should succeed when PVC already exists: %v", err)
	}
}

func TestCreatePVCFromVRR_WithOwnerReference(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCSIClient := &mockControllerClient{}
	mockSCLister := NewMockStorageClassLister(ctrl)

	vrr := createTestVRR("test-vrr", "test-ns", "VolumeSnapshotContent", "test-vsc")
	vrr.Annotations = map[string]string{
		"storage.deckhouse.io/object-keeper-ref": "deckhouse.io/v1alpha1|ObjectKeeper|test-ok|test-uid",
	}

	pv := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pv"},
		Spec: v1.PersistentVolumeSpec{
			AccessModes: []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
			Capacity: v1.ResourceList{
				v1.ResourceStorage: resource.MustParse("1Gi"),
			},
		},
	}

	kubeClient := fakeclientset.NewSimpleClientset()
	snapshotClient := snapfake.NewSimpleClientset()
	eventRecorder := record.NewFakeRecorder(10)

	handler := createTestVRRHandlerWithClients(mockCSIClient, mockSCLister, kubeClient, snapshotClient, eventRecorder)

	err := handler.createPVCFromVRR(context.Background(), vrr, pv)
	if err != nil {
		t.Fatalf("createPVCFromVRR failed: %v", err)
	}

	// Verify PVC was created with OwnerReference
	pvc, err := kubeClient.CoreV1().PersistentVolumeClaims(vrrTestTargetNamespace).Get(context.Background(), vrrTestTargetPVCName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Failed to get PVC: %v", err)
	}
	if len(pvc.OwnerReferences) != 1 {
		t.Errorf("Expected 1 OwnerReference, got %d", len(pvc.OwnerReferences))
	}
	if pvc.OwnerReferences[0].Name != "test-ok" {
		t.Errorf("Expected OwnerReference name 'test-ok', got %s", pvc.OwnerReferences[0].Name)
	}
}

func TestCreatePVFromCSIResponse_MULTI_NODE_READER_ONLY(t *testing.T) {
	volume := &csi.Volume{
		VolumeId:      "test-volume-id",
		CapacityBytes: 1024 * 1024 * 1024,
	}
	sc := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: vrrTestStorageClass},
		Provisioner: vrrTestDriverName,
	}
	volumeCaps := []*csi.VolumeCapability{
		{
			AccessType: getAccessTypeMount("ext4", nil),
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
			},
		},
	}

	pv, err := createPVFromCSIResponse(
		volume,
		sc,
		"test-pv",
		vrrTestDriverName,
		vrrTestIdentity,
		volumeCaps,
		"ext4",
	)
	if err != nil {
		t.Fatalf("createPVFromCSIResponse failed: %v", err)
	}

	// Verify PV is marked as ReadOnly
	if !pv.Spec.PersistentVolumeSource.CSI.ReadOnly {
		t.Error("PV should be marked as ReadOnly for MULTI_NODE_READER_ONLY")
	}

	// Verify AccessModes are ReadOnlyMany
	if len(pv.Spec.AccessModes) != 1 || pv.Spec.AccessModes[0] != v1.ReadOnlyMany {
		t.Errorf("Expected AccessModes [ReadOnlyMany], got %v", pv.Spec.AccessModes)
	}
}

func TestCreatePVFromCSIResponse_AccessibleTopology(t *testing.T) {
	volume := &csi.Volume{
		VolumeId:      "test-volume-id",
		CapacityBytes: 1024 * 1024 * 1024,
		AccessibleTopology: []*csi.Topology{
			{
				Segments: map[string]string{
					"topology.kubernetes.io/zone": "us-west-1a",
					"topology.kubernetes.io/rack": "rack-1",
				},
			},
		},
	}
	sc := &storagev1.StorageClass{
		ObjectMeta:  metav1.ObjectMeta{Name: vrrTestStorageClass},
		Provisioner: vrrTestDriverName,
	}
	volumeCaps := []*csi.VolumeCapability{
		{
			AccessType: getAccessTypeMount("ext4", nil),
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	}

	pv, err := createPVFromCSIResponse(
		volume,
		sc,
		"test-pv",
		vrrTestDriverName,
		vrrTestIdentity,
		volumeCaps,
		"ext4",
	)
	if err != nil {
		t.Fatalf("createPVFromCSIResponse failed: %v", err)
	}

	// Verify NodeAffinity is set from AccessibleTopology
	if pv.Spec.NodeAffinity == nil {
		t.Error("PV should have NodeAffinity set from AccessibleTopology")
	}
	if pv.Spec.NodeAffinity.Required == nil {
		t.Error("PV NodeAffinity.Required should not be nil")
	}
	if len(pv.Spec.NodeAffinity.Required.NodeSelectorTerms) == 0 {
		t.Error("PV should have at least one NodeSelectorTerm")
	}
}

// Helper functions

func createTestVRR(name, namespace, sourceKind, sourceName string) *storagev1alpha1.VolumeRestoreRequest {
	return &storagev1alpha1.VolumeRestoreRequest{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID("test-uid-" + name),
		},
		Spec: storagev1alpha1.VolumeRestoreRequestSpec{
			SourceRef: storagev1alpha1.ObjectReference{
				Kind: sourceKind,
				Name: sourceName,
			},
			TargetNamespace:  vrrTestTargetNamespace,
			TargetPVCName:    vrrTestTargetPVCName,
			StorageClassName: vrrTestStorageClass,
		},
	}
}

func toUnstructured(t *testing.T, obj runtime.Object) *unstructured.Unstructured {
	unstructuredObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		t.Fatalf("Failed to convert to unstructured: %v", err)
	}
	return &unstructured.Unstructured{Object: unstructuredObj}
}

func createTestVRRHandler(csiClient csi.ControllerClient, scLister storagelistersv1.StorageClassLister, kubeClient kubernetes.Interface, snapshotClient snapclientset.Interface) *VRRHandler {
	if kubeClient == nil {
		kubeClient = fakeclientset.NewSimpleClientset()
	}
	if snapshotClient == nil {
		snapshotClient = snapfake.NewSimpleClientset()
	}
	eventRecorder := record.NewFakeRecorder(10)

	// Create fake dynamic client for VRR status updates
	utilruntime.Must(storagev1alpha1.AddToScheme(scheme.Scheme))
	dynamicClient := fake.NewSimpleDynamicClient(scheme.Scheme)

	return NewVRRHandler(
		dynamicClient,
		kubeClient,
		csiClient,
		snapshotClient,
		vrrTestDriverName,
		vrrTestIdentity,
		vrrTestTimeout,
		vrrTestVolumePrefix,
		-1, // volumeNameUUIDLength
		vrrTestDefaultFSType,
		scLister,
		eventRecorder,
		rpc.PluginCapabilitySet{},
		rpc.ControllerCapabilitySet{},
	)
}

func createTestVRRHandlerWithClients(csiClient csi.ControllerClient, scLister storagelistersv1.StorageClassLister, kubeClient kubernetes.Interface, snapshotClient snapclientset.Interface, eventRecorder record.EventRecorder) *VRRHandler {
	if eventRecorder == nil {
		eventRecorder = record.NewFakeRecorder(10)
	}

	// Add reactor to fake client to automatically set PVC Phase to Bound when VolumeName is set
	// This mimics real Kubernetes behavior where PVC becomes Bound when VolumeName is set
	if fakeClient, ok := kubeClient.(*fakeclientset.Clientset); ok {
		// Reactor for Get: return PVC with Bound phase if VolumeName is set
		fakeClient.PrependReactor("get", "persistentvolumeclaims", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			getAction := action.(k8stesting.GetAction)
			obj, err := fakeClient.Tracker().Get(action.GetResource(), action.GetNamespace(), getAction.GetName())
			if err != nil {
				return false, nil, err
			}
			pvc := obj.(*v1.PersistentVolumeClaim).DeepCopy()
			// If PVC has VolumeName set, automatically set Phase to Bound
			if pvc.Spec.VolumeName != "" && pvc.Status.Phase != v1.ClaimBound {
				pvc.Status.Phase = v1.ClaimBound
			}
			return false, pvc, nil
		})
		// Reactor for Create: set Phase to Bound immediately if VolumeName is set
		fakeClient.PrependReactor("create", "persistentvolumeclaims", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
			createAction := action.(k8stesting.CreateAction)
			obj := createAction.GetObject().(*v1.PersistentVolumeClaim)
			// If PVC has VolumeName set, automatically set Phase to Bound
			if obj.Spec.VolumeName != "" {
				obj.Status.Phase = v1.ClaimBound
			}
			return false, nil, nil
		})
	}

	// Create fake dynamic client for VRR status updates
	// Add VRR to scheme for dynamic client
	utilruntime.Must(storagev1alpha1.AddToScheme(scheme.Scheme))
	dynamicClient := fake.NewSimpleDynamicClient(scheme.Scheme)

	handler := NewVRRHandler(
		dynamicClient,
		kubeClient,
		csiClient,
		snapshotClient,
		vrrTestDriverName,
		vrrTestIdentity,
		vrrTestTimeout,
		vrrTestVolumePrefix,
		-1, // volumeNameUUIDLength
		vrrTestDefaultFSType,
		scLister,
		eventRecorder,
		rpc.PluginCapabilitySet{},
		rpc.ControllerCapabilitySet{},
	)

	return handler
}

// addVRRToDynamicClient adds VRR to fake dynamic client for status update tests
func addVRRToDynamicClient(t *testing.T, handler *VRRHandler, vrr *storagev1alpha1.VolumeRestoreRequest) {
	if fakeDynamicClient, ok := handler.dynamicClient.(*fake.FakeDynamicClient); ok {
		vrrObj := toUnstructured(t, vrr)
		_ = fakeDynamicClient.Tracker().Add(vrrObj)
	}
}

func boolPtr(b bool) *bool {
	return &b
}

func stringPtr(s string) *string {
	return &s
}

func int64Ptr(i int64) *int64 {
	return &i
}

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}

// Mock interfaces for testing
// Note: We use gomock to generate mocks, but for simplicity in tests we'll use a simple wrapper

type mockControllerClient struct {
	createVolumeFunc func(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error)
}

func (m *mockControllerClient) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest, opts ...grpc.CallOption) (*csi.CreateVolumeResponse, error) {
	if m.createVolumeFunc != nil {
		return m.createVolumeFunc(ctx, req)
	}
	return nil, nil
}

func (m *mockControllerClient) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest, opts ...grpc.CallOption) (*csi.DeleteVolumeResponse, error) {
	return nil, nil
}

func (m *mockControllerClient) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest, opts ...grpc.CallOption) (*csi.ControllerPublishVolumeResponse, error) {
	return nil, nil
}

func (m *mockControllerClient) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest, opts ...grpc.CallOption) (*csi.ControllerUnpublishVolumeResponse, error) {
	return nil, nil
}

func (m *mockControllerClient) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest, opts ...grpc.CallOption) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	return nil, nil
}

func (m *mockControllerClient) ListVolumes(ctx context.Context, req *csi.ListVolumesRequest, opts ...grpc.CallOption) (*csi.ListVolumesResponse, error) {
	return nil, nil
}

func (m *mockControllerClient) GetCapacity(ctx context.Context, req *csi.GetCapacityRequest, opts ...grpc.CallOption) (*csi.GetCapacityResponse, error) {
	return nil, nil
}

func (m *mockControllerClient) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest, opts ...grpc.CallOption) (*csi.ControllerGetCapabilitiesResponse, error) {
	return nil, nil
}

func (m *mockControllerClient) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest, opts ...grpc.CallOption) (*csi.CreateSnapshotResponse, error) {
	return nil, nil
}

func (m *mockControllerClient) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest, opts ...grpc.CallOption) (*csi.DeleteSnapshotResponse, error) {
	return nil, nil
}

func (m *mockControllerClient) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest, opts ...grpc.CallOption) (*csi.ListSnapshotsResponse, error) {
	return nil, nil
}

func (m *mockControllerClient) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest, opts ...grpc.CallOption) (*csi.ControllerExpandVolumeResponse, error) {
	return nil, nil
}

func (m *mockControllerClient) ControllerModifyVolume(ctx context.Context, req *csi.ControllerModifyVolumeRequest, opts ...grpc.CallOption) (*csi.ControllerModifyVolumeResponse, error) {
	return nil, nil
}

func (m *mockControllerClient) GetSnapshot(ctx context.Context, req *csi.GetSnapshotRequest, opts ...grpc.CallOption) (*csi.GetSnapshotResponse, error) {
	return nil, nil
}

func (m *mockControllerClient) ControllerGetVolume(ctx context.Context, req *csi.ControllerGetVolumeRequest, opts ...grpc.CallOption) (*csi.ControllerGetVolumeResponse, error) {
	return nil, nil
}

type mockStorageClassLister struct {
	storageClasses map[string]*storagev1.StorageClass
	errors         map[string]error
}

func NewMockStorageClassLister(ctrl *gomock.Controller) *mockStorageClassLister {
	return &mockStorageClassLister{
		storageClasses: make(map[string]*storagev1.StorageClass),
		errors:         make(map[string]error),
	}
}

func (m *mockStorageClassLister) Get(name string) (*storagev1.StorageClass, error) {
	if err, ok := m.errors[name]; ok {
		return nil, err
	}
	sc, ok := m.storageClasses[name]
	if !ok {
		return nil, apierrors.NewNotFound(storagev1.Resource("storageclass"), name)
	}
	return sc, nil
}

func (m *mockStorageClassLister) List(selector labels.Selector) ([]*storagev1.StorageClass, error) {
	result := make([]*storagev1.StorageClass, 0, len(m.storageClasses))
	for _, sc := range m.storageClasses {
		result = append(result, sc)
	}
	return result, nil
}
