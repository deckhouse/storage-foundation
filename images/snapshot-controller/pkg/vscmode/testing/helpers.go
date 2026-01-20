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

package testing

import (
	crdv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"github.com/kubernetes-csi/external-snapshotter/v8/pkg/utils"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// TestConfig contains test configuration constants.
// These can be overridden by test packages that import vscmode/testing.
type TestConfig struct {
	DriverName    string
	TestNamespace string
}

// DefaultTestConfig returns default test configuration.
func DefaultTestConfig() TestConfig {
	return TestConfig{
		DriverName:    "csi-mock-plugin",
		TestNamespace: "default",
	}
}

// NewVSCOnlyContent creates a VSC without VolumeSnapshotRef (VSC-only model).
// Used by contract, compatibility, and edge-case tests.
func NewVSCOnlyContent(config TestConfig, contentName, snapshotHandle, snapshotClassName, volumeHandle string,
	deletionPolicy crdv1.DeletionPolicy, size *int64, readyToUse *bool,
	withFinalizer bool) *crdv1.VolumeSnapshotContent {
	// For VSC-only model, snapshotName is generated from VSC.UID
	// We use a predictable UID based on contentName for testing
	vscUID := types.UID("vsc-uid-" + contentName)
	content := &crdv1.VolumeSnapshotContent{
		ObjectMeta: metav1.ObjectMeta{
			Name:            contentName,
			UID:             vscUID,
			ResourceVersion: "1",
		},
		Spec: crdv1.VolumeSnapshotContentSpec{
			Driver:         config.DriverName,
			DeletionPolicy: deletionPolicy,
			// VolumeSnapshotRef is empty - VSC-only model
			VolumeSnapshotRef: v1.ObjectReference{},
		},
		Status: &crdv1.VolumeSnapshotContentStatus{},
	}

	if snapshotClassName != "" {
		content.Spec.VolumeSnapshotClassName = &snapshotClassName
	}

	if volumeHandle != "" {
		content.Spec.Source = crdv1.VolumeSnapshotContentSource{
			VolumeHandle: &volumeHandle,
		}
	}

	if snapshotHandle != "" {
		content.Status.SnapshotHandle = &snapshotHandle
	}

	if size != nil {
		content.Status.RestoreSize = size
	}

	if readyToUse != nil {
		content.Status.ReadyToUse = readyToUse
	}

	if withFinalizer {
		content.ObjectMeta.Finalizers = append(content.ObjectMeta.Finalizers, utils.VolumeSnapshotContentFinalizer)
	}

	return content
}

// NewVSCWithVolumeSnapshotRef creates a VSC with VolumeSnapshotRef (legacy, for backward compatibility tests).
// Used by compatibility tests to verify upstream behavior.
func NewVSCWithVolumeSnapshotRef(config TestConfig, contentName, boundToSnapshotUID, boundToSnapshotName, snapshotHandle, snapshotClassName, volumeHandle string,
	deletionPolicy crdv1.DeletionPolicy, size *int64, readyToUse *bool,
	withFinalizer bool) *crdv1.VolumeSnapshotContent {
	content := NewVSCOnlyContent(config, contentName, snapshotHandle, snapshotClassName, volumeHandle, deletionPolicy, size, readyToUse, withFinalizer)

	if boundToSnapshotName != "" {
		content.Spec.VolumeSnapshotRef = v1.ObjectReference{
			Kind:       "VolumeSnapshot",
			APIVersion: "snapshot.storage.k8s.io/v1",
			UID:        types.UID(boundToSnapshotUID),
			Namespace:  config.TestNamespace,
			Name:       boundToSnapshotName,
		}
	}

	return content
}
