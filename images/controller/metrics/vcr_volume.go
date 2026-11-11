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

// storageClassProvisioners returns the provisioner of every StorageClass in the cluster by its name.
func storageClassProvisioners(ctx context.Context, apiReader client.Reader) (map[string]string, error) {
	list := new(storagev1.StorageClassList)
	if err := apiReader.List(ctx, list); err != nil {
		return nil, err
	}

	provisioners := make(map[string]string, len(list.Items))
	for i := range list.Items {
		provisioners[list.Items[i].Name] = list.Items[i].Provisioner
	}

	return provisioners, nil
}

// vcrVolumeLabels returns the driver and storage_class labels of vcr, read from its source PVC.
// A label that cannot be determined is unknown.
func vcrVolumeLabels(
	ctx context.Context,
	reader client.Reader,
	vcr *v1alpha1.VolumeCaptureRequest,
	provisioners map[string]string,
) (driver, scName string) {
	if vcr.Spec.Target == nil {
		return labelUnknown, labelUnknown
	}

	pvc := new(corev1.PersistentVolumeClaim)
	// spec.target has no namespace: the PVC is in the namespace of the request.
	err := reader.Get(ctx, client.ObjectKey{Namespace: vcr.Namespace, Name: vcr.Spec.Target.Name}, pvc)
	if apierrors.IsNotFound(err) {
		return labelUnknown, labelUnknown
	}
	if err != nil {
		// V(1): a broken cache would repeat this for every request on every scrape.
		log.FromContext(ctx).V(1).Info("get the PVC of a VolumeCaptureRequest, using unknown labels",
			"namespace", vcr.Namespace, "name", vcr.Name, "err", err)
		return labelUnknown, labelUnknown
	}

	// A PVC recreated under the same name is not the source PVC.
	if string(pvc.UID) != vcr.Spec.Target.UID {
		return labelUnknown, labelUnknown
	}

	if pvc.Spec.StorageClassName != nil {
		scName = *pvc.Spec.StorageClassName
	}
	provisioner, ok := provisioners[scName]
	if !ok {
		scName, provisioner = labelUnknown, labelUnknown
	}

	driver, err = pvcDriver(ctx, reader, pvc, provisioner)
	if err != nil {
		log.FromContext(ctx).V(1).Info("get the PV of a VolumeCaptureRequest, using unknown driver",
			"namespace", vcr.Namespace, "name", vcr.Name, "err", err)
		return labelUnknown, scName
	}

	return driver, scName
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
