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

// Regression tests against upstream snapshot-controller logic.
// These tests ensure that VSC-only changes do not break upstream behavior.
// All tests in this file verify upstream-compatible behavior.

// TestLegacyVSC_WithVolumeSnapshotRef verifies that legacy VSC with VolumeSnapshotRef
// works identically to upstream (not affected by VSC-only logic).
func TestLegacyVSC_WithVolumeSnapshotRef(t *testing.T) {
	tests := []controllerTest{
		{
			name: "Legacy VSC with VolumeSnapshotRef: upstream-compatible behavior",
			initialContents: []*crdv1.VolumeSnapshotContent{
				newVSCWithVolumeSnapshotRef("content-legacy-1", "snapuid-1", "snap-1", "", defaultClass, "volume-handle-1", retainPolicy, &defaultSize, nil, true),
			},
			expectedContents: withContentAnnotations(
				withContentStatus(
					[]*crdv1.VolumeSnapshotContent{
						newVSCWithVolumeSnapshotRef("content-legacy-1", "snapuid-1", "snap-1", "snapuid-legacy-1", defaultClass, "volume-handle-1", retainPolicy, &defaultSize, &True, true),
					},
					&crdv1.VolumeSnapshotContentStatus{
						SnapshotHandle: toStringPointer("snapuid-legacy-1"),
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
					snapshotName: "snapshot-snapuid-1", // Uses VolumeSnapshotRef.UID
					driverName:   mockDriverName,
					snapshotId:   "snapuid-legacy-1",
					parameters: map[string]string{
						utils.PrefixedVolumeSnapshotNameKey:        "snap-1",
						utils.PrefixedVolumeSnapshotNamespaceKey:   testNamespace,
						utils.PrefixedVolumeSnapshotContentNameKey: "content-legacy-1",
					},
					creationTime: timeNow,
					readyToUse:   true,
					size:         defaultSize,
				},
			},
			expectedListCalls: []listCall{{"snapuid-legacy-1", map[string]string{}, true, time.Now(), 1, nil, ""}},
			expectSuccess:     true,
			errors:            noerrors,
			test:              testSyncContent,
		},
	}
	runSyncContentTests(t, tests, snapshotClasses)
}

// TestPreProvisionedSnapshot_UpstreamCompatible verifies that pre-provisioned snapshots
// (Source.SnapshotHandle != nil, VolumeSnapshotRef == "") work identically to upstream.
func TestPreProvisionedSnapshot_UpstreamCompatible(t *testing.T) {
	content := &crdv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "content-pre-provisioned-1",
			UID:             types.UID("content-pre-provisioned-1-uid"),
			ResourceVersion: "1",
			Finalizers:      []string{utils.VolumeSnapshotContentFinalizer},
		},
		Spec: crdv1.VolumeSnapshotContentSpec{
			Driver:         mockDriverName,
			DeletionPolicy: retainPolicy,
			Source: crdv1.VolumeSnapshotContentSource{
				SnapshotHandle: toStringPointer("pre-provisioned-handle"),
			},
			// VolumeSnapshotRef is empty for pre-provisioned (upstream behavior)
			VolumeSnapshotRef: v1.ObjectReference{},
		},
		Status: &crdv1.VolumeSnapshotContentStatus{},
	}

	tests := []controllerTest{
		{
			name: "Pre-provisioned snapshot: upstream-compatible behavior",
			initialContents: []*crdv1.VolumeSnapshotContent{
				content,
			},
			expectedContents: withContentAnnotations(
				withContentStatus(
					[]*crdv1.VolumeSnapshotContent{
						content,
					},
					&crdv1.VolumeSnapshotContentStatus{
						SnapshotHandle: toStringPointer("pre-provisioned-handle"),
						RestoreSize:    &defaultSize,
						ReadyToUse:     &True,
					},
				),
				map[string]string{},
			),
			expectedEvents:      noevents,
			expectedCreateCalls: []createCall{}, // No CreateSnapshot for pre-provisioned
			expectedListCalls: []listCall{
				{"pre-provisioned-handle", map[string]string{}, true, time.Now(), 1, nil, ""},
			},
			expectSuccess: true,
			errors:        noerrors,
			test:          testSyncContent,
		},
	}
	runSyncContentTests(t, tests, snapshotClasses)
}

// TestGroupSnapshot_VSCOnlyNotInterfering verifies that VSC-only logic does not interfere
// with group snapshot handling (sidecar should skip CreateSnapshot for group members).
func TestGroupSnapshot_VSCOnlyNotInterfering(t *testing.T) {
	content := newVSCOnlyContent("content-group-1", "", defaultClass, "volume-handle-1", retainPolicy, &defaultSize, nil, true)
	content.Annotations = map[string]string{
		utils.VolumeGroupSnapshotHandleAnnotation: "group-handle-1",
	}

	tests := []controllerTest{
		{
			name: "Group snapshot member: VSC-only logic does not interfere",
			initialContents: []*crdv1.VolumeSnapshotContent{
				content,
			},
			expectedContents: []*crdv1.VolumeSnapshotContent{
				content, // No changes expected - group snapshot handled separately
			},
			expectedEvents:      noevents,
			expectedCreateCalls: []createCall{}, // Group snapshot member - no CreateSnapshot
			expectedListCalls:   []listCall{},
			expectSuccess:       true,
			errors:              noerrors,
			test:                testSyncContent,
		},
	}
	runSyncContentTests(t, tests, snapshotClasses)
}
