/*
Copyright 2019 The Kubernetes Authors.

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

package sidecar_controller

import (
	"testing"
	"time"

	crdv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"github.com/kubernetes-csi/external-snapshotter/v8/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Contract tests for VSC-only mode.
// These tests verify behavior, not implementation details.

// TestVSCOnlyContract_Basic verifies basic VSC-only behavior:
// 1. VSC without VolumeSnapshotRef is accepted
// 2. CreateSnapshot is called
// 3. SnapshotHandle appears in status
func TestVSCOnlyContract_Basic(t *testing.T) {
	tests := []controllerTest{
		{
			name: "VSC-only basic: CreateSnapshot called, SnapshotHandle set",
			initialContents: []*crdv1.VolumeSnapshotContent{
				newVSCOnlyContent("content-vsc-only-1", "", defaultClass, "volume-handle-1", retainPolicy, &defaultSize, nil, true),
			},
			expectedContents: withContentAnnotations(
				withContentStatus(
					[]*crdv1.VolumeSnapshotContent{
						newVSCOnlyContent("content-vsc-only-1", "snapuid-vsc-only-1", defaultClass, "volume-handle-1", retainPolicy, &defaultSize, &True, true),
					},
					&crdv1.VolumeSnapshotContentStatus{
						SnapshotHandle: toStringPointer("snapuid-vsc-only-1"),
						RestoreSize:    &defaultSize,
						ReadyToUse:     &True,
					},
				),
				map[string]string{},
			),
			expectedEvents: noevents,
			expectedCreateCalls: []createCall{
				{
					volumeHandle: "volume-handle-1",
					snapshotName: "snapshot-vsc-uid-content-vsc-only-1",
					driverName:   mockDriverName,
					snapshotId:   "snapuid-vsc-only-1",
					parameters: map[string]string{
						utils.PrefixedVolumeSnapshotContentNameKey: "content-vsc-only-1",
					},
					creationTime: timeNow,
					readyToUse:   true,
					size:         defaultSize,
				},
			},
			expectedListCalls: []listCall{{"snapuid-vsc-only-1", map[string]string{}, true, time.Now(), 1, nil, ""}},
			expectSuccess:     true,
			errors:            noerrors,
			test:              testSyncContent,
		},
	}
	runSyncContentTests(t, tests, snapshotClasses)
}

// TestVSCOnlyContract_Lifecycle verifies complete VSC-only lifecycle:
// 1. CreateSnapshot called once
// 2. SnapshotHandle appears in status
// 3. ReadyToUse correctly set
func TestVSCOnlyContract_Lifecycle(t *testing.T) {
	tests := []controllerTest{
		{
			name: "VSC-only lifecycle: CreateSnapshot -> Status -> ReadyToUse",
			initialContents: []*crdv1.VolumeSnapshotContent{
				newVSCOnlyContent("content-lifecycle-1", "", defaultClass, "volume-handle-1", retainPolicy, &defaultSize, nil, true),
			},
			expectedContents: withContentAnnotations(
				withContentStatus(
					[]*crdv1.VolumeSnapshotContent{
						newVSCOnlyContent("content-lifecycle-1", "snapuid-lifecycle-1", defaultClass, "volume-handle-1", retainPolicy, &defaultSize, &True, true),
					},
					&crdv1.VolumeSnapshotContentStatus{
						SnapshotHandle: toStringPointer("snapuid-lifecycle-1"),
						RestoreSize:    &defaultSize,
						ReadyToUse:     &True,
					},
				),
				map[string]string{},
			),
			expectedEvents: noevents,
			expectedCreateCalls: []createCall{
				{
					volumeHandle: "volume-handle-1",
					snapshotName: "snapshot-vsc-uid-content-lifecycle-1",
					driverName:   mockDriverName,
					snapshotId:   "snapuid-lifecycle-1",
					parameters: map[string]string{
						utils.PrefixedVolumeSnapshotContentNameKey: "content-lifecycle-1",
					},
					creationTime: timeNow,
					readyToUse:   true,
					size:         defaultSize,
				},
			},
			expectedListCalls: []listCall{{"snapuid-lifecycle-1", map[string]string{}, true, time.Now(), 1, nil, ""}},
			expectSuccess:     true,
			errors:            noerrors,
			test:              testSyncContent,
		},
	}
	runSyncContentTests(t, tests, snapshotClasses)
}

// TestVSCOnlyContract_Deletion verifies VSC-only deletion behavior:
// 1. DeleteSnapshot called only for Delete policy
// 2. Finalizer removed after successful deletion
func TestVSCOnlyContract_Deletion(t *testing.T) {
	deletionTime := metav1.Now()
	content := newVSCOnlyContent("content-delete-1", "snapuid-delete-1", defaultClass, "volume-handle-1", deletePolicy, &defaultSize, &True, true)
	content.DeletionTimestamp = &deletionTime

	// After deletion, SnapshotHandle is cleared by clearVolumeContentStatus
	expectedContent := newVSCOnlyContent("content-delete-1", "", defaultClass, "volume-handle-1", deletePolicy, nil, nil, false)

	tests := []controllerTest{
		{
			name: "VSC-only deletion: DeleteSnapshot called, finalizer removed",
			initialContents: []*crdv1.VolumeSnapshotContent{
				content,
			},
			expectedContents: []*crdv1.VolumeSnapshotContent{
				expectedContent,
			},
			expectedEvents: noevents,
			expectedDeleteCalls: []deleteCall{
				{
					snapshotID: "snapuid-delete-1",
					secrets:    map[string]string{},
					err:        nil,
				},
			},
			expectedListCalls: []listCall{},
			expectSuccess:     true,
			errors:            noerrors,
			test:              testSyncContent,
		},
	}
	runSyncContentTests(t, tests, snapshotClasses)
}

// TestVSCOnlyContract_NoVolumeSnapshot verifies that no VolumeSnapshot objects
// are created or required for VSC-only mode.
func TestVSCOnlyContract_NoVolumeSnapshot(t *testing.T) {
	tests := []controllerTest{
		{
			name: "VSC-only: No VolumeSnapshot required or created",
			initialContents: []*crdv1.VolumeSnapshotContent{
				newVSCOnlyContent("content-no-vs-1", "", defaultClass, "volume-handle-1", retainPolicy, &defaultSize, nil, true),
			},
			expectedContents: withContentAnnotations(
				withContentStatus(
					[]*crdv1.VolumeSnapshotContent{
						newVSCOnlyContent("content-no-vs-1", "snapuid-no-vs-1", defaultClass, "volume-handle-1", retainPolicy, &defaultSize, &True, true),
					},
					&crdv1.VolumeSnapshotContentStatus{
						SnapshotHandle: toStringPointer("snapuid-no-vs-1"),
						RestoreSize:    &defaultSize,
						ReadyToUse:     &True,
					},
				),
				map[string]string{},
			),
			expectedEvents: noevents,
			expectedCreateCalls: []createCall{
				{
					volumeHandle: "volume-handle-1",
					snapshotName: "snapshot-vsc-uid-content-no-vs-1",
					driverName:   mockDriverName,
					snapshotId:   "snapuid-no-vs-1",
					parameters: map[string]string{
						utils.PrefixedVolumeSnapshotContentNameKey: "content-no-vs-1",
					},
					creationTime: timeNow,
					readyToUse:   true,
					size:         defaultSize,
				},
			},
			expectedListCalls: []listCall{{"snapuid-no-vs-1", map[string]string{}, true, time.Now(), 1, nil, ""}},
			expectSuccess:     true,
			errors:            noerrors,
			test:              testSyncContent,
		},
	}
	runSyncContentTests(t, tests, snapshotClasses)
}

// TestVSCOnlyContract_RecreateVSC verifies that when a VSC is deleted and recreated
// with the same name but different UID, a new snapshot is created with a different snapshotName.
// This is a real scenario in backup/restore workflows.
func TestVSCOnlyContract_RecreateVSC(t *testing.T) {
	// First VSC with UID "vsc-uid-content-recreate-1"
	content1 := newVSCOnlyContent("content-recreate-1", "", defaultClass, "volume-handle-1", retainPolicy, &defaultSize, nil, true)
	// Verify UID is set correctly
	if string(content1.UID) != "vsc-uid-content-recreate-1" {
		t.Fatalf("Expected UID vsc-uid-content-recreate-1, got %s", content1.UID)
	}

	// Second VSC with same name but different UID
	content2 := &crdv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "content-recreate-1",
			UID:             types.UID("vsc-uid-content-recreate-1-new"), // Different UID
			ResourceVersion: "1",
		},
		Spec: crdv1.VolumeSnapshotContentSpec{
			Driver:                  vscTestDriverName,
			DeletionPolicy:          retainPolicy,
			VolumeSnapshotRef:       v1.ObjectReference{}, // Empty - VSC-only
			VolumeSnapshotClassName: &defaultClass,
			Source: crdv1.VolumeSnapshotContentSource{
				VolumeHandle: toStringPointer("volume-handle-1"),
			},
		},
		Status: &crdv1.VolumeSnapshotContentStatus{},
	}
	content2.ObjectMeta.Finalizers = []string{utils.VolumeSnapshotContentFinalizer}

	tests := []controllerTest{
		{
			name: "VSC-only recreate: New VSC with same name but different UID creates new snapshot",
			initialContents: []*crdv1.VolumeSnapshotContent{
				content2, // Only the new VSC
			},
			expectedContents: withContentAnnotations(
				withContentStatus(
					[]*crdv1.VolumeSnapshotContent{
						func() *crdv1.VolumeSnapshotContent {
							c := content2.DeepCopy()
							c.Status.SnapshotHandle = toStringPointer("snapuid-recreate-1")
							c.Status.RestoreSize = &defaultSize
							c.Status.ReadyToUse = &True
							return c
						}(),
					},
					&crdv1.VolumeSnapshotContentStatus{
						SnapshotHandle: toStringPointer("snapuid-recreate-1"),
						RestoreSize:    &defaultSize,
						ReadyToUse:     &True,
					},
				),
				map[string]string{},
			),
			expectedEvents: noevents,
			expectedCreateCalls: []createCall{
				{
					volumeHandle: "volume-handle-1",
					snapshotName: "snapshot-vsc-uid-content-recreate-1-new", // Different snapshotName based on new UID
					driverName:   mockDriverName,
					snapshotId:   "snapuid-recreate-1",
					parameters: map[string]string{
						utils.PrefixedVolumeSnapshotContentNameKey: "content-recreate-1",
					},
					creationTime: timeNow,
					readyToUse:   true,
					size:         defaultSize,
				},
			},
			expectedListCalls: []listCall{{"snapuid-recreate-1", map[string]string{}, true, time.Now(), 1, nil, ""}},
			expectSuccess:     true,
			errors:            noerrors,
			test:              testSyncContent,
		},
	}
	runSyncContentTests(t, tests, snapshotClasses)
}
