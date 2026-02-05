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
	crdv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	vscmodetesting "github.com/kubernetes-csi/external-snapshotter/v8/pkg/vscmode/testing"
)

// testConfig is the test configuration for sidecar-controller tests.
// These constants match the ones from framework_test.go.
const (
	vscTestDriverName = "csi-mock-plugin"
	vscTestNamespace  = "default"
)

var testConfig = vscmodetesting.TestConfig{
	DriverName:    vscTestDriverName,
	TestNamespace: vscTestNamespace,
}

// newVSCOnlyContent creates a VSC without VolumeSnapshotRef (VSC-only model).
// This is a wrapper around vscmode/testing.NewVSCOnlyContent that uses
// sidecar-controller test configuration.
func newVSCOnlyContent(contentName, snapshotHandle, snapshotClassName, volumeHandle string,
	deletionPolicy crdv1.DeletionPolicy, size *int64, readyToUse *bool,
	withFinalizer bool) *crdv1.VolumeSnapshotContent {
	return vscmodetesting.NewVSCOnlyContent(testConfig, contentName, snapshotHandle, snapshotClassName, volumeHandle, deletionPolicy, size, readyToUse, withFinalizer)
}

// newVSCWithVolumeSnapshotRef creates a VSC with VolumeSnapshotRef (legacy, for backward compatibility tests).
// This is a wrapper around vscmode/testing.NewVSCWithVolumeSnapshotRef that uses
// sidecar-controller test configuration.
func newVSCWithVolumeSnapshotRef(contentName, boundToSnapshotUID, boundToSnapshotName, snapshotHandle, snapshotClassName, volumeHandle string,
	deletionPolicy crdv1.DeletionPolicy, size *int64, readyToUse *bool,
	withFinalizer bool) *crdv1.VolumeSnapshotContent {
	return vscmodetesting.NewVSCWithVolumeSnapshotRef(testConfig, contentName, boundToSnapshotUID, boundToSnapshotName, snapshotHandle, snapshotClassName, volumeHandle, deletionPolicy, size, readyToUse, withFinalizer)
}
