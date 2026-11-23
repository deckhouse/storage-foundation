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

package metrics

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/deckhouse/storage-foundation/api/v1alpha1"
)

// snapshotClassAnnotation is the StorageClass annotation that names the VolumeSnapshotClass the VCR controller
// takes snapshots with.
const snapshotClassAnnotation = "storage.deckhouse.io/volumesnapshotclass"

// storageClassLabels holds the label values that one StorageClass gives a volume.
type storageClassLabels struct {
	provisioner   string
	snapshotClass string
}

// vcrVolume holds the labels of a VolumeCaptureRequest that describe its source volume.
type vcrVolume struct {
	driver        string
	storageClass  string
	snapshotClass string
}

// storageClasses returns the label values of every StorageClass in the cluster by its name.
func storageClasses(ctx context.Context, reader client.Reader) (map[string]storageClassLabels, error) {
	list := new(storagev1.StorageClassList)
	if err := reader.List(ctx, list); err != nil {
		return nil, err
	}

	classes := make(map[string]storageClassLabels, len(list.Items))
	for i := range list.Items {
		sc := &list.Items[i]

		snapshotClass := sc.Annotations[snapshotClassAnnotation]
		// The VCR controller takes no snapshot on a StorageClass that does not name a VolumeSnapshotClass.
		if snapshotClass == "" {
			snapshotClass = labelNone
		}

		classes[sc.Name] = storageClassLabels{provisioner: sc.Provisioner, snapshotClass: snapshotClass}
	}

	return classes, nil
}

// vcrVolumeLabels returns the volume labels of vcr, read from its source PVC. A label that cannot be determined
// is unknown, and a Detach request has no VolumeSnapshotClass.
func vcrVolumeLabels(
	ctx context.Context,
	reader client.Reader,
	vcr *v1alpha1.VolumeCaptureRequest,
	classes map[string]storageClassLabels,
) vcrVolume {
	volume := sourceVolumeLabels(ctx, reader, vcr, classes)
	if vcr.Spec.Mode == v1alpha1.VolumeCaptureModeDetach {
		volume.snapshotClass = labelNone
	}

	return volume
}

// sourceVolumeLabels returns the volume labels of the source PVC of vcr, all unknown if that PVC cannot be read.
func sourceVolumeLabels(
	ctx context.Context,
	reader client.Reader,
	vcr *v1alpha1.VolumeCaptureRequest,
	classes map[string]storageClassLabels,
) vcrVolume {
	unknown := vcrVolume{driver: labelUnknown, storageClass: labelUnknown, snapshotClass: labelUnknown}
	if vcr.Spec.Target == nil {
		return unknown
	}

	pvc := new(corev1.PersistentVolumeClaim)
	// spec.target has no namespace: the PVC is in the namespace of the request.
	err := reader.Get(ctx, client.ObjectKey{Namespace: vcr.Namespace, Name: vcr.Spec.Target.Name}, pvc)
	if apierrors.IsNotFound(err) {
		return unknown
	}
	if err != nil {
		// V(1): a broken cache would repeat this for every request on every scrape.
		log.FromContext(ctx).V(1).Info("get the PVC of a VolumeCaptureRequest, using unknown labels",
			"namespace", vcr.Namespace, "name", vcr.Name, "err", err)
		return unknown
	}

	// A PVC recreated under the same name is not the source PVC.
	if string(pvc.UID) != vcr.Spec.Target.UID {
		return unknown
	}

	var scName string
	if pvc.Spec.StorageClassName != nil {
		scName = *pvc.Spec.StorageClassName
	}
	class, ok := classes[scName]
	if !ok {
		scName = labelUnknown
		class = storageClassLabels{provisioner: labelUnknown, snapshotClass: labelUnknown}
	}

	driver, err := pvcDriver(ctx, reader, pvc, class.provisioner)
	if err != nil {
		log.FromContext(ctx).V(1).Info("get the PV of a VolumeCaptureRequest, using unknown driver",
			"namespace", vcr.Namespace, "name", vcr.Name, "err", err)
		driver = labelUnknown
	}

	return vcrVolume{driver: driver, storageClass: scName, snapshotClass: class.snapshotClass}
}

// pvcDriver returns the CSI driver of the PV bound to pvc, or provisioner while pvc has no PV.
func pvcDriver(
	ctx context.Context,
	reader client.Reader,
	pvc *corev1.PersistentVolumeClaim,
	provisioner string,
) (string, error) {
	if pvc.Spec.VolumeName == "" {
		return provisioner, nil
	}

	pv := new(corev1.PersistentVolume)
	err := reader.Get(ctx, client.ObjectKey{Name: pvc.Spec.VolumeName}, pv)
	if apierrors.IsNotFound(err) {
		return provisioner, nil
	}
	if err != nil {
		return "", err
	}

	if pv.Spec.CSI == nil {
		return labelUnknown, nil
	}

	return pv.Spec.CSI.Driver, nil
}
