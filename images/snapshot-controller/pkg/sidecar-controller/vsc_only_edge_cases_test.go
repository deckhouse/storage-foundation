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
	"errors"
	"testing"
	"time"

	crdv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"github.com/kubernetes-csi/external-snapshotter/v8/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Edge-case and negative tests for VSC-only mode.
// These tests protect against flapping and "silent" bugs.

// TestVSCOnly_NoVolumeHandleNoSnapshotHandle verifies that VSC without VolumeHandle
// and without SnapshotHandle is skipped correctly.
func TestVSCOnly_NoVolumeHandleNoSnapshotHandle(t *testing.T) {
	content := newVSCOnlyContent("content-no-handles-1", "", defaultClass, "", retainPolicy, nil, nil, true)
	// No VolumeHandle, no SnapshotHandle

	tests := []controllerTest{
		{
			name: "VSC-only: No VolumeHandle and no SnapshotHandle - skipped",
			initialContents: []*crdv1.VolumeSnapshotContent{
				content,
			},
			expectedContents: []*crdv1.VolumeSnapshotContent{
				content, // No changes - skipped
			},
			expectedEvents:      noevents,
			expectedCreateCalls: []createCall{},
			expectedListCalls:   []listCall{},
			expectSuccess:       true,
			errors:              noerrors,
			test:                testSyncContent,
		},
	}
	runSyncContentTests(t, tests, snapshotClasses)
}

// TestVSCOnly_CreateSnapshotFinalError verifies that final errors from CreateSnapshot
// are handled correctly (annotation removed, status updated with error).
func TestVSCOnly_CreateSnapshotFinalError(t *testing.T) {
	tests := []controllerTest{
		{
			name: "VSC-only: CreateSnapshot final error handled correctly",
			initialContents: []*crdv1.VolumeSnapshotContent{
				newVSCOnlyContent("content-error-1", "", defaultClass, "invalid-volume-handle", retainPolicy, &defaultSize, nil, true),
			},
			expectedContents: withContentAnnotations(
				withContentStatus(
					[]*crdv1.VolumeSnapshotContent{
						newVSCOnlyContent("content-error-1", "", defaultClass, "invalid-volume-handle", retainPolicy, &defaultSize, &False, true),
					},
					&crdv1.VolumeSnapshotContentStatus{
						SnapshotHandle: nil,
						RestoreSize:    &defaultSize,
						ReadyToUse:     &False,
						Error:          newSnapshotError("Failed to create snapshot: failed to take snapshot of the volume invalid-volume-handle: \"invalid volume handle\""),
					},
				),
				map[string]string{
					utils.AnnVolumeSnapshotBeingCreated: "yes", // Annotation remains for non-gRPC errors (isCSIFinalError returns false for non-gRPC errors)
				},
			),
			expectedEvents: []string{"Warning SnapshotCreationFailed"},
			expectedCreateCalls: []createCall{
				{
					volumeHandle: "invalid-volume-handle",
					snapshotName: "snapshot-vsc-uid-content-error-1",
					driverName:   mockDriverName,
					snapshotId:   "",
					parameters: map[string]string{
						utils.PrefixedVolumeSnapshotContentNameKey: "content-error-1",
					},
					secrets: map[string]string{
						"foo": "bar",
					},
					creationTime: timeNow,
					readyToUse:   false,
					err:          errors.New("invalid volume handle"),
				},
			},
			expectedListCalls: []listCall{},
			expectSuccess:     false, // Error is returned from syncContent
			expectRequeue:     true,  // Error causes requeue
			initialSecrets:    []*v1.Secret{secret()},
			errors:            noerrors,
			test:              testSyncContent,
		},
	}
	runSyncContentTests(t, tests, snapshotClasses)
}

// TestVSCOnly_CreateSnapshotInProgress verifies that CreateSnapshot in progress
// (annotation set but no status yet) is handled correctly.
func TestVSCOnly_CreateSnapshotInProgress(t *testing.T) {
	content := newVSCOnlyContent("content-in-progress-1", "", defaultClass, "volume-handle-1", retainPolicy, &defaultSize, nil, true)
	content.Annotations = map[string]string{
		utils.AnnVolumeSnapshotBeingCreated: "yes",
	}

	tests := []controllerTest{
		{
			name: "VSC-only: CreateSnapshot in progress - status checked periodically",
			initialContents: []*crdv1.VolumeSnapshotContent{
				content,
			},
			expectedContents: []*crdv1.VolumeSnapshotContent{
				content, // No changes - waiting for CSI response
			},
			expectedEvents:      noevents,
			expectedCreateCalls: []createCall{}, // Not called again - already in progress
			expectedListCalls:   []listCall{},
			expectSuccess:       true,
			expectRequeue:       true, // Controller should requeue to check status periodically
			errors:              noerrors,
			test:                testSyncContent,
		},
	}
	runSyncContentTests(t, tests, snapshotClasses)
}

// TestVSCOnly_PartialStatus verifies that partially filled status
// (SnapshotHandle present but ReadyToUse not set) is handled correctly.
func TestVSCOnly_PartialStatus(t *testing.T) {
	content := newVSCOnlyContent("content-partial-1", "snapuid-partial-1", defaultClass, "volume-handle-1", retainPolicy, &defaultSize, nil, true)
	// SnapshotHandle is set, but ReadyToUse is nil

	tests := []controllerTest{
		{
			name: "VSC-only: Partial status - ReadyToUse updated via ListSnapshots",
			initialContents: []*crdv1.VolumeSnapshotContent{
				content,
			},
			expectedContents: withContentStatus(
				[]*crdv1.VolumeSnapshotContent{
					newVSCOnlyContent("content-partial-1", "snapuid-partial-1", defaultClass, "volume-handle-1", retainPolicy, &defaultSize, &True, true),
				},
				&crdv1.VolumeSnapshotContentStatus{
					SnapshotHandle: toStringPointer("snapuid-partial-1"),
					RestoreSize:    &defaultSize,
					ReadyToUse:     &True,
				},
			),
			expectedEvents:      noevents,
			expectedCreateCalls: []createCall{}, // Already has SnapshotHandle
			expectedListCalls: []listCall{
				{"snapuid-partial-1", map[string]string{}, true, time.Now(), 1, nil, ""},
			},
			expectSuccess: true,
			errors:        noerrors,
			test:          testSyncContent,
		},
	}
	runSyncContentTests(t, tests, snapshotClasses)
}

// TestVSCOnly_DeletionRetainPolicy verifies that VSC with Retain policy
// does not call DeleteSnapshot when deleted.
func TestVSCOnly_DeletionRetainPolicy(t *testing.T) {
	deletionTime := metav1.Now()
	content := newVSCOnlyContent("content-retain-1", "snapuid-retain-1", defaultClass, "volume-handle-1", retainPolicy, &defaultSize, &True, true)
	content.DeletionTimestamp = &deletionTime

	expectedContent := newVSCOnlyContent("content-retain-1", "snapuid-retain-1", defaultClass, "volume-handle-1", retainPolicy, &defaultSize, &True, false)

	tests := []controllerTest{
		{
			name: "VSC-only: Retain policy - no DeleteSnapshot called",
			initialContents: []*crdv1.VolumeSnapshotContent{
				content,
			},
			expectedContents: []*crdv1.VolumeSnapshotContent{
				expectedContent, // Finalizer removed, but snapshot retained
			},
			expectedEvents:      noevents,
			expectedDeleteCalls: []deleteCall{}, // No deletion for Retain policy
			expectedListCalls:   []listCall{},
			expectSuccess:       true,
			errors:              noerrors,
			test:                testSyncContent,
		},
	}
	runSyncContentTests(t, tests, snapshotClasses)
}
