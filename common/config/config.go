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
	"log"
	"os"
	"time"

	"github.com/deckhouse/sds-common-lib/logger"
	"github.com/deckhouse/sds-common-lib/monitoring"
	"github.com/deckhouse/storage-foundation/common"
)

const (
	ControllerNamespaceEnv               = "CONTROLLER_NAMESPACE"
	HardcodedControllerNS                = "d8-storage-foundation"
	ControllerName                       = "d8-controller"
	DefaultHealthProbeBindAddressEnvName = "HEALTH_PROBE_BIND_ADDRESS"
	DefaultHealthProbeBindAddress        = ":8081"
	DefaultRequeueStorageClassInterval   = 10
	HAModeEnvName                        = "HA_MODE"
	LeaderElectionID                     = "storage-foundation.deckhouse.io"
	OriginIngressNamespaceEnv            = "ORIGIN_INGRESS_NAMESPACE"
)

type Options struct {
	Loglevel                    logger.Verbosity
	RequeueStorageClassInterval time.Duration
	HealthProbeBindAddress      string
	// MetricsBindAddress is where the manager serves /metrics, injected via
	// METRICS_BIND_ADDRESS from Helm.
	MetricsBindAddress     string
	ControllerNamespace    string
	OriginIngressNamespace string
	LeaderElection         bool
	LeaderElectionID       string
}

// defaultMetricsBindAddress keeps the endpoint on loopback when the chart says
// nothing: the kube-rbac-proxy of the same pod is what publishes it. The fleet-wide
// fallback is the wildcard ":8080", which would both answer unauthenticated on the
// pod IP and collide with the proxy, since the two carry the same port number.
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

	opts.OriginIngressNamespace = os.Getenv(OriginIngressNamespaceEnv)
	if opts.OriginIngressNamespace == "" {
		opts.OriginIngressNamespace = common.OriginIngressNamespace
	}

	opts.RequeueStorageClassInterval = DefaultRequeueStorageClassInterval

	if os.Getenv(HAModeEnvName) == "true" {
		opts.LeaderElection = true
		opts.LeaderElectionID = LeaderElectionID
	}

	return &opts
}
