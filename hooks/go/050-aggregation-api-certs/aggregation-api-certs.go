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

package hooks_common

import (
	"fmt"

	tlscertificate "github.com/deckhouse/module-sdk/common-hooks/tls-certificate"

	consts "github.com/deckhouse/storage-foundation/hooks/go/consts"
)

// aggregationAPISecretName is the Secret the generated serving pair is stored in and read back from. The
// chart renders a Secret of this very name (templates/controller/secret.yaml) and mounts it into the
// controller Pod.
var aggregationAPISecretName = fmt.Sprintf("%s-https-certs", consts.AggregationAPICertCn)

// aggregationAPICertValuesPath is where the generated crt/key/ca are published. The chart reads the
// serving pair and the APIServices' caBundle from this one path, so the trust the aggregation layer is
// given cannot drift from the certificate the server presents.
var aggregationAPICertValuesPath = fmt.Sprintf("%s.internal.%s", consts.ModuleName, consts.AggregationAPICertValuesKey)

// aggregationAPICertSANs are the names the serving certificate must be valid for.
//
// The load-bearing entry is the third one: an APIService's spec.service resolves to
// <name>.<namespace>.svc, and that is the name the aggregation layer verifies the presented certificate
// against. Without it every aggregated request fails TLS verification and both APIServices report
// themselves unavailable, with nothing wrong anywhere else.
//
// The %CLUSTER_DOMAIN%:// prefix is expanded by the certificate helper into the fully qualified form
// (<name>.<namespace>.svc.<cluster domain>), which is what a client using the cluster's search domain
// ends up requesting.
var aggregationAPICertSANs = []string{
	consts.AggregationAPICertCn,
	fmt.Sprintf("%s.%s", consts.AggregationAPICertCn, consts.ModuleNamespace),
	fmt.Sprintf("%s.%s.svc", consts.AggregationAPICertCn, consts.ModuleNamespace),
	fmt.Sprintf("%%CLUSTER_DOMAIN%%://%s.%s.svc", consts.AggregationAPICertCn, consts.ModuleNamespace),
}

// aggregationAPICertHookConf is the configuration of the hook that generates the serving certificate of
// the module's aggregated API server (the APIServices publishing the VolumeSnapshot subresources).
//
// It is a hook of its own rather than an extra SAN on the admission-webhook certificate, even though both
// are self-signed pairs for the same module: the consumer and the lifecycle differ. The webhook
// certificate's CA is consumed by a webhook configuration's clientConfig.caBundle, this one's by an
// APIService's spec.caBundle, and the two objects are created, deleted and re-rolled independently — a
// shared pair would couple an APIService outage to a webhook change and force one SAN set to cover two
// Services. Keeping them apart also keeps each certificate's SANs to exactly the Service that presents
// it, which is what the aggregation layer verifies.
var aggregationAPICertHookConf = tlscertificate.GenSelfSignedTLSHookConf{
	CommonCACanonicalName: fmt.Sprintf("%s-%s", consts.ModulePluralName, consts.AggregationAPICertCn),
	CN:                    consts.AggregationAPICertCn,
	TLSSecretName:         aggregationAPISecretName,
	Namespace:             consts.ModuleNamespace,
	SANs:                  tlscertificate.DefaultSANs(aggregationAPICertSANs),
	FullValuesPathPrefix:  aggregationAPICertValuesPath,
}

// A hook exists only if it is registered here AND blank-imported from the hooks binary's main package;
// without the import this file is never linked in and the hook silently does not exist. Guarded by
// TestAggregationAPICertHook_IsRegisteredInTheHooksBinary.
var _ = tlscertificate.RegisterInternalTLSHookEM(aggregationAPICertHookConf)
