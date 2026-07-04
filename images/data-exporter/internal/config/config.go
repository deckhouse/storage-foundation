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
package config

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/deckhouse/storage-foundation/common"
)

const (
	VolumeModeFilesystem VolumeMode = "filesystem"
	VolumeModeBlock      VolumeMode = "block"
)

type VolumeMode string

type URLOpt struct {
	DataManagerNamespace       string
	DataManagerTargetKindShort string
	DataManagerTargetName      string
}

type Config struct {
	Port                    string
	Mode                    VolumeMode
	Operation               common.Operation
	Path                    string
	TTLStr                  string
	DataManagerName         string
	DataManagerCASecretName string
	DataManagerServiceName  string
	ControllerNamespace     string
	URLOpt                  URLOpt
	IsServiceOff            bool
}

func (o *Config) Parse() {
	var rootCmd = &cobra.Command{
		RunE: func(_ *cobra.Command, _ []string) error { return nil },
	}

	// Exit after displaying the help information
	rootCmd.SetHelpFunc(func(cmd *cobra.Command, _ []string) {
		cmd.Print(cmd.UsageString())
		os.Exit(0)
	})

	// Add flags
	rootCmd.Flags().StringVarP((*string)(&o.Mode), "mode", "m", "filesystem", "Exporting PV mode (allowed values: 'filesystem' and 'block').")
	rootCmd.Flags().StringVarP(&o.Port, "port", "u", "8080", "File server port on localhost, , e.g. 8080")
	rootCmd.Flags().StringVarP(&o.Path, "path", "p", "/", "Path to exporting PV in container")
	rootCmd.Flags().StringVarP(&o.TTLStr, "ttl", "t", "1m", "Time to live after last request finished, e.g. 5m, 2h45m, 1d")
	rootCmd.Flags().StringVarP(&o.URLOpt.DataManagerNamespace, "data-export-namespace", "s", "default_namespace", "Namespace of DataExport resource-owner")
	rootCmd.Flags().StringVarP(&o.DataManagerName, "data-export-name", "n", "default_name", "Name of DataExport resource-owner")
	rootCmd.Flags().StringVarP(&o.URLOpt.DataManagerTargetKindShort, "export-target-kind-short", "k", "pvc", "Short name of export target kind (e.g. pvc, vs, etc.)")
	rootCmd.Flags().StringVarP(&o.URLOpt.DataManagerTargetName, "export-target-name", "e", "default-target", "Name of export target (e.g. pvc name, vs name, etc.)")
	rootCmd.Flags().StringVarP(&o.DataManagerCASecretName, "data-export-ca-secret-name", "c", "default-ca-secret", "Name of DataExport CA secret (used for TLS)")
	rootCmd.Flags().StringVarP(&o.DataManagerServiceName, "data-export-service-name", "i", "default-service", "Name of DataExport service (used for TLS)")
	rootCmd.Flags().StringVarP(&o.ControllerNamespace, "controller-namespace", "C", "d8-data-exporter", "Namespace of Data Exporter controller")
	rootCmd.Flags().StringVarP((*string)(&o.Operation), "operation", "o", "export", "Operation: 'export' or 'import'")

	if err := rootCmd.Execute(); err != nil {
		// we expect err to be logged already
		os.Exit(1)
	}
}
