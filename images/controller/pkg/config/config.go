/*
Copyright 2024 Flant JSC

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
	"log"
	"os"
	"time"

	"github.com/deckhouse/storage-foundation/images/controller/pkg/logger"
)

const (
	LogLevelEnvName                      = "LOG_LEVEL"
	ControllerNamespaceEnv               = "CONTROLLER_NAMESPACE"
	HardcodedControllerNS                = "d8-storage-foundation"
	ControllerName                       = "controller"
	DefaultHealthProbeBindAddressEnvName = "HEALTH_PROBE_BIND_ADDRESS"
	DefaultHealthProbeBindAddress        = ":8081"
	RetentionSnapshotTTLEnvName          = "RETENTION_SNAPSHOT_TTL"
	RetentionDetachTTLEnvName            = "RETENTION_DETACH_TTL"
	DefaultRetentionSnapshotTTL          = 24 * time.Hour // Default TTL for snapshot artifacts (IRetainer)
	DefaultRetentionDetachTTL            = 24 * time.Hour // Default TTL for detached PV artifacts (IRetainer)
)

type Options struct {
	Loglevel               logger.Verbosity
	HealthProbeBindAddress string
	ControllerNamespace    string
	Retention              RetentionConfig
}

type RetentionConfig struct {
	SnapshotTTL time.Duration // TTL for snapshot artifacts (VolumeSnapshotContent) - IRetainer
	DetachTTL   time.Duration // TTL for detached PV artifacts - IRetainer
}

func NewConfig() *Options {
	var opts Options

	loglevel := os.Getenv(LogLevelEnvName)
	if loglevel == "" {
		opts.Loglevel = logger.DebugLevel
	} else {
		opts.Loglevel = logger.Verbosity(loglevel)
	}

	opts.HealthProbeBindAddress = os.Getenv(DefaultHealthProbeBindAddressEnvName)
	if opts.HealthProbeBindAddress == "" {
		opts.HealthProbeBindAddress = DefaultHealthProbeBindAddress
	}

	opts.ControllerNamespace = os.Getenv(ControllerNamespaceEnv)
	if opts.ControllerNamespace == "" {
		namespace, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
		if err != nil {
			log.Printf("Failed to get namespace from filesystem: %v", err)
			log.Printf("Using hardcoded namespace: %s", HardcodedControllerNS)
			opts.ControllerNamespace = HardcodedControllerNS
		} else {
			log.Printf("Got namespace from filesystem: %s", string(namespace))
			opts.ControllerNamespace = string(namespace)
		}
	}

	// Load retention TTL configuration (for IRetainer - long-lived artifacts)
	opts.Retention.SnapshotTTL = parseDurationEnv(RetentionSnapshotTTLEnvName, DefaultRetentionSnapshotTTL)
	opts.Retention.DetachTTL = parseDurationEnv(RetentionDetachTTLEnvName, DefaultRetentionDetachTTL)

	return &opts
}

// parseDurationEnv parses duration from environment variable or returns default
func parseDurationEnv(envName string, defaultValue time.Duration) time.Duration {
	envValue := os.Getenv(envName)
	if envValue == "" {
		return defaultValue
	}
	duration, err := time.ParseDuration(envValue)
	if err != nil {
		log.Printf("Failed to parse %s: %v, using default: %v", envName, err, defaultValue)
		return defaultValue
	}
	return duration
}
