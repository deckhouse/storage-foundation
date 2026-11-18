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
	"slices"
	"strings"
	"time"

	"github.com/deckhouse/sds-common-lib/logger"
	"github.com/deckhouse/sds-common-lib/monitoring"
)

const (
	ControllerNamespaceEnv               = "CONTROLLER_NAMESPACE"
	TrustedRequestNamespacesEnv          = "TRUSTED_REQUEST_NAMESPACES"
	HardcodedControllerNS                = "d8-storage-foundation"
	ControllerName                       = "controller"
	DefaultHealthProbeBindAddressEnvName = "HEALTH_PROBE_BIND_ADDRESS"
	DefaultHealthProbeBindAddress        = ":8081"
	RetentionSnapshotTTLEnvName          = "RETENTION_SNAPSHOT_TTL"
	RetentionDetachTTLEnvName            = "RETENTION_DETACH_TTL"
	DefaultRetentionSnapshotTTL          = 24 * time.Hour // Default TTL for snapshot artifacts (IRetainer)
	DefaultRetentionDetachTTL            = 24 * time.Hour // Default TTL for detached PV artifacts (IRetainer)

	// Listen address and serving pair of the aggregated API server. The defaults match what the chart
	// puts in the Pod (the published container port and the mount path of the certificate Secret); the
	// environment variables exist so the two are stated together there rather than only here.
	AggregationAPIAddrEnvName        = "AGGREGATION_API_ADDR"
	AggregationAPITLSCertFileEnvName = "AGGREGATION_API_TLS_CERT_FILE"
	AggregationAPITLSKeyFileEnvName  = "AGGREGATION_API_TLS_KEY_FILE"
	DefaultAggregationAPIAddr        = ":8443"
	DefaultAggregationAPITLSCertFile = "/etc/aggregation-api-certs/tls.crt"
	DefaultAggregationAPITLSKeyFile  = "/etc/aggregation-api-certs/tls.key"
)

type Options struct {
	Loglevel               logger.Verbosity
	HealthProbeBindAddress string
	// MetricsBindAddress is where the manager serves /metrics, injected via
	// METRICS_BIND_ADDRESS from Helm.
	MetricsBindAddress  string
	ControllerNamespace string
	// TrustedRequestNamespaces are the namespaces of other modules whose VolumeRestoreRequests name
	// the namespace their source must belong to in the storage-manager-namespace annotation, as the
	// requests of ControllerNamespace do. Only the module owning such a namespace may write there.
	TrustedRequestNamespaces []string
	Retention                RetentionConfig

	// AggregationAPIAddr is where the module's aggregated API server listens (see internal/domainapi).
	AggregationAPIAddr string
	// AggregationAPITLSCertFile and AggregationAPITLSKeyFile are its serving pair. The aggregation layer
	// validates the presented certificate against the Service DNS name, so this pair must be the one the
	// certificate hook generated for that Service.
	AggregationAPITLSCertFile string
	AggregationAPITLSKeyFile  string
}

type RetentionConfig struct {
	SnapshotTTL time.Duration // TTL for snapshot artifacts (VolumeSnapshotContent) - IRetainer
	DetachTTL   time.Duration // TTL for detached PV artifacts - IRetainer
}

// defaultMetricsBindAddress keeps the endpoint on loopback when the chart says
// nothing: the fleet-wide fallback is the wildcard ":8080", which would answer
// unauthenticated on the pod IP.
const defaultMetricsBindAddress = "127.0.0.1:8080"

func NewConfig() *Options {
	var opts Options

	opts.Loglevel = logger.VerbosityFromEnv(logger.InfoLevel)

	opts.MetricsBindAddress = monitoring.BindAddressFromEnvOr(defaultMetricsBindAddress)

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

	opts.TrustedRequestNamespaces = parseListEnv(TrustedRequestNamespacesEnv)

	// Load retention TTL configuration (for IRetainer - long-lived artifacts)
	opts.Retention.SnapshotTTL = parseDurationEnv(RetentionSnapshotTTLEnvName, DefaultRetentionSnapshotTTL)
	opts.Retention.DetachTTL = parseDurationEnv(RetentionDetachTTLEnvName, DefaultRetentionDetachTTL)

	opts.AggregationAPIAddr = envOrDefault(AggregationAPIAddrEnvName, DefaultAggregationAPIAddr)
	opts.AggregationAPITLSCertFile = envOrDefault(AggregationAPITLSCertFileEnvName, DefaultAggregationAPITLSCertFile)
	opts.AggregationAPITLSKeyFile = envOrDefault(AggregationAPITLSKeyFileEnvName, DefaultAggregationAPITLSKeyFile)

	return &opts
}

// TrustsRequestsIn reports whether a VolumeRestoreRequest in the namespace takes the namespace of its
// source from the storage-manager-namespace annotation instead of its own namespace.
func (o *Options) TrustsRequestsIn(namespace string) bool {
	if namespace == "" {
		return false
	}
	return namespace == o.ControllerNamespace || slices.Contains(o.TrustedRequestNamespaces, namespace)
}

// parseListEnv splits a comma-separated environment variable, dropping empty items.
func parseListEnv(envName string) []string {
	var items []string
	for _, item := range strings.Split(os.Getenv(envName), ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}

// envOrDefault reads an environment variable, falling back to a default when it is unset or empty.
func envOrDefault(envName, defaultValue string) string {
	if value := os.Getenv(envName); value != "" {
		return value
	}
	return defaultValue
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
