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

package common

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/deckhouse/sds-common-lib/logger"
)

type ServerContainerCfg struct {
	ConfigMapName types.NamespacedName // Name of ConfigMap to get container image
	ResourceName  types.NamespacedName // DataImport or DataExport resource name
	VolumeName    string               // Volume name to mount (same as in Pod's volumes list!)
	VolumeMode    corev1.PersistentVolumeMode
	// Server parameters
	Ttl                 string
	ServerMode          ServerMode
	Names               Names
	ControllerNamespace string
	// LogLevel is handed to the server as LOG_LEVEL; see ServerEnv.
	LogLevel logger.Verbosity
}

func MakeServerContainer(ctx context.Context, client client.Client, cfg ServerContainerCfg) (*corev1.Container, error) {
	logger := log.FromContext(ctx)

	image, err := GetImage(ctx, client, cfg.ConfigMapName)
	if err != nil {
		logger.Error(err, "Failed to get container image")
		return nil, err
	}

	portArg := fmt.Sprintf("--port=%d", FileServerPort)
	ttlArg := fmt.Sprintf("--ttl=%s", cfg.Ttl)
	// TODO: unify names
	dataExportNamespaceArg := fmt.Sprintf("--data-export-namespace=%s", cfg.ResourceName.Namespace)
	dataExportNameArg := fmt.Sprintf("--data-export-name=%s", cfg.ResourceName.Name)
	exportTargetKindShortArg := fmt.Sprintf("--export-target-kind-short=%s", cfg.Names.TargetKindShort)
	exportTargetNameArg := fmt.Sprintf("--export-target-name=%s", cfg.Names.TargetName)
	dataExportCASecretNameArg := fmt.Sprintf("--data-export-ca-secret-name=%s", cfg.Names.CASecretName)
	dataExportServiceNameArg := fmt.Sprintf("--data-export-service-name=%s", cfg.Names.HeadlessServiceName)
	controllerNamespaceArg := fmt.Sprintf("--controller-namespace=%s", cfg.ControllerNamespace)
	importExportMode := fmt.Sprintf("--operation=%s", cfg.ServerMode)

	var mode string
	var path string
	var volumeMounts []corev1.VolumeMount
	var volumeDevices []corev1.VolumeDevice

	switch cfg.VolumeMode {
	case corev1.PersistentVolumeBlock:
		mode = "block"
		path = BlockMountPath
		volumeDevices = []corev1.VolumeDevice{
			{
				Name:       cfg.VolumeName,
				DevicePath: path,
			},
		}
	case corev1.PersistentVolumeFilesystem:
		mode = "filesystem"
		path = FilesystemMountPath
		volumeMounts = []corev1.VolumeMount{
			{
				Name:      cfg.VolumeName,
				MountPath: path,
			},
		}
	}

	args := []string{
		fmt.Sprintf("--mode=%s", mode),
		fmt.Sprintf("--path=%s", path),
		portArg,
		ttlArg,
		dataExportNamespaceArg,
		dataExportNameArg,
		exportTargetKindShortArg,
		exportTargetNameArg,
		dataExportCASecretNameArg,
		dataExportServiceNameArg,
		controllerNamespaceArg,
		importExportMode,
	}

	return &corev1.Container{
		Name:            "server",
		Image:           image,
		Args:            args,
		Env:             ServerEnv(cfg.LogLevel),
		VolumeMounts:    volumeMounts,
		VolumeDevices:   volumeDevices,
		Ports:           []corev1.ContainerPort{{Name: "http", ContainerPort: FileServerPort}},
		SecurityContext: ServerSecurityContext(cfg.ServerMode, cfg.VolumeMode),
	}, nil
}

// ServerEnv is the environment of the exporter and importer server container.
//
// LOG_LEVEL carries the controller's own verbosity: the server pods have no Helm template of their own, so
// the controller is the only thing that can pass the module's log level on to them. An empty logLevel leaves
// the variable unset, and the server falls back to its default.
func ServerEnv(logLevel logger.Verbosity) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{
			Name: "POD_IP",
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{
					FieldPath: "status.podIP",
				},
			},
		},
	}

	if logLevel != "" {
		env = append(env, corev1.EnvVar{Name: logger.EnvLogLevel, Value: string(logLevel)})
	}

	return env
}

func ServerSecurityContext(serverMode ServerMode, volumeMode corev1.PersistentVolumeMode) *corev1.SecurityContext {
	var add []corev1.Capability
	if volumeMode == corev1.PersistentVolumeFilesystem {
		if serverMode == ServerModeImport {
			add = []corev1.Capability{"CHOWN", "DAC_OVERRIDE", "FOWNER"}
		} else {
			add = []corev1.Capability{"DAC_OVERRIDE"}
		}
	}

	return &corev1.SecurityContext{
		RunAsUser:                &[]int64{0}[0],
		RunAsNonRoot:             &[]bool{false}[0],
		ReadOnlyRootFilesystem:   &[]bool{true}[0],
		AllowPrivilegeEscalation: &[]bool{false}[0],
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
			Add:  add,
		},
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}
