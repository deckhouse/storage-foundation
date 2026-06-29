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

package dataimport

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/deckhouse/storage-foundation/common"
)

type DummyContainerCfg struct {
	ConfigMapName types.NamespacedName // Name of ConfigMap to get container image
	PvcName       types.NamespacedName // Name of PVC to mount (required to obtain volume mode)
	ResourceName  types.NamespacedName // DataImport or DataExport resource name
	VolumeName    string               // Volume name to mount (same as in Pod's volumes list!)
}

func MakeDummyContainer(ctx context.Context, client client.Client, cfg DummyContainerCfg) (*corev1.Container, error) {
	logger := log.FromContext(ctx)

	image, err := common.GetImage(ctx, client, cfg.ConfigMapName)
	if err != nil {
		logger.Error(err, "Failed to get container image")
		return nil, err
	}

	pvc := &corev1.PersistentVolumeClaim{}
	err = client.Get(ctx, cfg.PvcName, pvc)
	if err != nil {
		logger.Error(err, "Failed to get PVC")
		return nil, err
	}

	var securityContext *corev1.SecurityContext
	var volumeMounts []corev1.VolumeMount
	var volumeDevices []corev1.VolumeDevice

	switch *pvc.Spec.VolumeMode {
	case corev1.PersistentVolumeBlock:
		securityContext = &corev1.SecurityContext{RunAsUser: &[]int64{0}[0]}
		volumeDevices = []corev1.VolumeDevice{
			{
				Name:       cfg.VolumeName,
				DevicePath: common.BlockMountPath,
			},
		}
	case corev1.PersistentVolumeFilesystem:
		volumeMounts = []corev1.VolumeMount{
			{
				Name:      cfg.VolumeName,
				MountPath: common.FilesystemMountPath,
			},
		}
	}

	// Run the image's real entrypoint (the data-exporter binary) in its no-op "dummy" mode instead of a
	// shell: the distroless image has no /bin/sh, so Command:["sh"] fails with a StartError and the Job
	// churns. The dummy pod only needs to be scheduled with this volume attached to trigger
	// WaitForFirstConsumer binding, then exit cleanly.
	return &corev1.Container{
		Name:            "dummy-consumer",
		Image:           image,
		Args:            []string{"dummy"},
		VolumeMounts:    volumeMounts,
		VolumeDevices:   volumeDevices,
		SecurityContext: securityContext,
	}, nil
}
