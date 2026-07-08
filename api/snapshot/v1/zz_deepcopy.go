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

// Deepcopy methods for the extended VolumeSnapshot. Hand-written (not controller-gen) because the CRD is
// the CSI VolumeSnapshot and is not generated from this Go type; only the runtime.Object contract is
// needed so the type can be used with a controller-runtime client.

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
)

// DeepCopyInto copies the receiver into out.
func (in *VolumeSnapshot) DeepCopyInto(out *VolumeSnapshot) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopy returns a deep copy of the receiver.
func (in *VolumeSnapshot) DeepCopy() *VolumeSnapshot {
	if in == nil {
		return nil
	}
	out := new(VolumeSnapshot)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *VolumeSnapshot) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto copies the receiver into out.
func (in *VolumeSnapshotList) DeepCopyInto(out *VolumeSnapshotList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		l := make([]VolumeSnapshot, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&l[i])
		}
		out.Items = l
	}
}

// DeepCopy returns a deep copy of the receiver.
func (in *VolumeSnapshotList) DeepCopy() *VolumeSnapshotList {
	if in == nil {
		return nil
	}
	out := new(VolumeSnapshotList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *VolumeSnapshotList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// DeepCopyInto copies the receiver into out.
func (in *VolumeSnapshotSpec) DeepCopyInto(out *VolumeSnapshotSpec) {
	*out = *in
	in.Source.DeepCopyInto(&out.Source)
	if in.VolumeSnapshotClassName != nil {
		v := *in.VolumeSnapshotClassName
		out.VolumeSnapshotClassName = &v
	}
}

// DeepCopyInto copies the receiver into out.
func (in *VolumeSnapshotSource) DeepCopyInto(out *VolumeSnapshotSource) {
	*out = *in
	if in.PersistentVolumeClaimName != nil {
		v := *in.PersistentVolumeClaimName
		out.PersistentVolumeClaimName = &v
	}
	if in.VolumeSnapshotContentName != nil {
		v := *in.VolumeSnapshotContentName
		out.VolumeSnapshotContentName = &v
	}
	if in.Import != nil {
		out.Import = new(VolumeSnapshotImportSource)
	}
}

// DeepCopyInto copies the receiver into out.
func (in *VolumeSnapshotStatus) DeepCopyInto(out *VolumeSnapshotStatus) {
	*out = *in
	if in.CaptureState != nil {
		out.CaptureState = in.CaptureState.DeepCopy()
	}
	if in.ChildrenSnapshotRefs != nil {
		l := make([]storagev1alpha1.SnapshotChildRef, len(in.ChildrenSnapshotRefs))
		copy(l, in.ChildrenSnapshotRefs)
		out.ChildrenSnapshotRefs = l
	}
	if in.SnapshotSource != nil {
		out.SnapshotSource = in.SnapshotSource.DeepCopy()
	}
	if in.Conditions != nil {
		l := make([]metav1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&l[i])
		}
		out.Conditions = l
	}
}
