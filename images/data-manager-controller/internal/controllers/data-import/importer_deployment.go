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

package dataimport

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
)

// importerVolumeName is the name of the Pod volume the internal scratch PVC is mounted under, referenced
// both by the volume definition and by the server container's VolumeMounts/VolumeDevices.
const importerVolumeName = "pvc"

// ensureImporterDeployment brings up the upload server: a Deployment in the controller namespace mounting
// the internal scratch PVC. Created before the PVC is bound on purpose — it is the PVC's first consumer.
func (r *DataImportReconciler) ensureImporterDeployment(ctx context.Context, pvc *corev1.PersistentVolumeClaim) error {
	if pvc.Spec.VolumeMode == nil {
		return fmt.Errorf("internal scratch PVC %s/%s has no volume mode set", pvc.Namespace, pvc.Name)
	}

	serverContainerCfg := r.getServerContainerCfg(pvc)

	server, err := common.MakeServerContainer(ctx, r.Client, serverContainerCfg)
	if err != nil {
		return fmt.Errorf("make importer server container: %w", err)
	}

	podSpec := r.getPodSpec(pvc, server)

	return common.EnsureDeployment(ctx, r.Client, r.getDeploymentCfg(podSpec))
}

func (r *DataImportReconciler) getServerContainerCfg(pvc *corev1.PersistentVolumeClaim) common.ServerContainerCfg {
	return common.ServerContainerCfg{
		ConfigMapName: types.NamespacedName{
			Namespace: r.Config.ControllerNamespace,
			Name:      common.CongigMapName,
		},
		ResourceName:        types.NamespacedName{Namespace: r.dataImport.Namespace, Name: r.dataImport.Name},
		VolumeName:          importerVolumeName,
		VolumeMode:          *pvc.Spec.VolumeMode,
		Ttl:                 r.dataImport.Spec.Ttl,
		ServerMode:          common.ServerModeImport,
		ControllerNamespace: r.Config.ControllerNamespace,
		Names:               r.names,
	}
}

func (r *DataImportReconciler) getPodSpec(pvc *corev1.PersistentVolumeClaim, server *corev1.Container) corev1.PodSpec {
	volumes := common.MakeVolumes(importerVolumeName, pvc.Name, false)

	return corev1.PodSpec{
		ServiceAccountName: common.ServiceAccountServer,
		ImagePullSecrets:   []corev1.LocalObjectReference{{Name: common.ImagePullSecretsName}},
		Containers:         []corev1.Container{*server},
		Volumes:            volumes,
	}
}

func (r *DataImportReconciler) getDeploymentCfg(podSpec corev1.PodSpec) common.DeploymentCfg {
	return common.DeploymentCfg{
		PodSpec: podSpec,
		DeploymentName: types.NamespacedName{
			Namespace: r.Config.ControllerNamespace,
			Name:      r.names.DeployName,
		},
		ResourceName:          types.NamespacedName{Namespace: r.dataImport.Namespace, Name: r.dataImport.Name},
		LabelApplicationValue: dev1alpha1.LabelDataImportValue,
	}
}

// stopImporter deletes the upload server and reports whether it is fully stopped (Deployment gone AND
// no pod left). Capturing the volume while the importer still holds the mount would be less consistent
// than the pre-existing populator flow, which waited for ClaimLost before tearing the Deployment down.
func (r *DataImportReconciler) stopImporter(ctx context.Context) (stopped bool, err error) {
	// common.DeleteDeployment already logs internally; do not also log here before returning.
	if _, err := common.DeleteDeployment(ctx, r.Client, r.Config.ControllerNamespace, r.names.DeployName); err != nil {
		return false, err
	}
	return r.importerPodsGone(ctx)
}

// importerPodsGone reports whether every importer pod for this DataImport has fully terminated.
// Pod-object absence is the strongest available "the volume was unmounted, and therefore flushed"
// signal: images/data-exporter performs no explicit fsync anywhere, so consistency of the captured
// volume rests entirely on the kubelet's unmount, which completes before the Pod object is removed.
func (r *DataImportReconciler) importerPodsGone(ctx context.Context) (bool, error) {
	pods := new(corev1.PodList)
	if err := r.Client.List(ctx, pods,
		client.InNamespace(r.Config.ControllerNamespace),
		client.MatchingLabels{
			dev1alpha1.LabelApplicationKey:                  dev1alpha1.LabelDataImportValue,
			dev1alpha1.LabelStorageManagerDeploymentNameKey: r.names.DeployName,
		},
	); err != nil {
		return false, fmt.Errorf("list importer pods: %w", err)
	}

	return len(pods.Items) == 0, nil
}
