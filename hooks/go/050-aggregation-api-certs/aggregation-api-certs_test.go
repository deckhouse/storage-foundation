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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deckhouse/storage-foundation/hooks/go/consts"
)

// LIMIT OF EVERY GUARD IN THIS FILE THAT READS A CHART FILE — run them as
//
//	go test -count=1 ./050-aggregation-api-certs/
//
// The test cache tracks the files a test opens, but only those at or below the module root. These guards
// read templates/ and openapi/, which are ABOVE it, so editing a chart file does not invalidate the
// cached result: a plain `go test` answers `ok (cached)` on a chart that now violates the guard, and only
// `-count=1` sees it. Measured in both directions, not assumed: appending a line to a file the test reads
// from INSIDE the module (hooks/go/main.go) made a plain `go test` re-run, while appending one to
// templates/controller/service.yaml left it `(cached)`.
//
// It is not wired into a runner anywhere, because this repository has none — `go test` is invoked by a
// shared CI template that lives in another project. So the flag has to be typed, and a green run without
// it says nothing about the chart.

// repoFile reads a file of this repository by its path from the repository root. A test runs in its own
// package directory, and this one sits three levels below the root (hooks/go/<hook>).
//
// Reading from outside the module is what makes the caller cache-sensitive; see the note above.
func repoFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", filepath.FromSlash(path)))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// The aggregation layer dials the Service by its in-cluster DNS name and verifies the presented
// certificate against it. A certificate without that name makes every aggregated request fail TLS
// verification and both APIServices report themselves unavailable — with the pod healthy, the routes
// registered and nothing else to point at.
func TestAggregationAPICertHook_CoversTheServiceDNSName(t *testing.T) {
	serviceDNSName := fmt.Sprintf("%s.%s.svc", consts.AggregationAPICertCn, consts.ModuleNamespace)

	found := false
	for _, san := range aggregationAPICertSANs {
		if san == serviceDNSName {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("the certificate does not cover the Service DNS name %q; SANs are %v", serviceDNSName, aggregationAPICertSANs)
	}

	// The fully qualified form is what a client resolving through the cluster search domain asks for.
	qualified := "%CLUSTER_DOMAIN%://" + serviceDNSName
	if !containsString(aggregationAPICertSANs, qualified) {
		t.Fatalf("the certificate does not cover the cluster-domain-qualified name %q; SANs are %v", qualified, aggregationAPICertSANs)
	}

	if aggregationAPICertHookConf.CN != consts.AggregationAPICertCn {
		t.Fatalf("CN: got %q want %q", aggregationAPICertHookConf.CN, consts.AggregationAPICertCn)
	}
	if aggregationAPICertHookConf.Namespace != consts.ModuleNamespace {
		t.Fatalf("namespace: got %q want %q", aggregationAPICertHookConf.Namespace, consts.ModuleNamespace)
	}
	if aggregationAPICertHookConf.SANs == nil {
		t.Fatal("no SAN generator is configured, so the certificate would carry no SANs at all")
	}
}

// The Service the certificate is issued for must be the Service the APIServices send requests to. These
// are three files that have to agree, and nothing but this check makes them.
func TestAggregationAPICertHook_CertifiesTheServiceTheAPIServicesPointAt(t *testing.T) {
	// Reads chart files: needs -count=1 to be meaningful (see the note at the top of this file).

	service := repoFile(t, "templates/controller/service.yaml")
	if !strings.Contains(service, "name: "+consts.AggregationAPICertCn+"\n") {
		t.Fatalf("templates/controller/service.yaml does not define a Service named %q", consts.AggregationAPICertCn)
	}

	apiservice := repoFile(t, "templates/controller/apiservice.yaml")
	// spec.service.name of every APIService, as rendered.
	wantServiceRef := "    name: " + consts.AggregationAPICertCn + "\n"
	if strings.Count(apiservice, wantServiceRef) != 2 {
		t.Fatalf("expected both APIServices to point spec.service.name at %q; found %d occurrences of %q",
			consts.AggregationAPICertCn, strings.Count(apiservice, wantServiceRef), strings.TrimSpace(wantServiceRef))
	}

	// The Service exists for the aggregation layer, and publishing anything else on it has a cost the
	// certificate does not cover: the controller's health endpoints are unauthenticated, and a Service
	// port puts them within reach of every pod in the cluster. The kubelet probes the container port
	// directly and needs no Service entry.
	ports := strings.Count(service, "- port: ")
	if ports != 1 {
		t.Fatalf("templates/controller/service.yaml publishes %d ports, want only the aggregation port 443", ports)
	}
	if !strings.Contains(service, "- port: 443") {
		t.Fatal("templates/controller/service.yaml does not publish port 443, which is what the APIServices address")
	}
}

// The serving pair the pod mounts and the caBundle the APIServices advertise have to come from ONE
// generated value. Two values, or one value and a hand-written CA, and the aggregation layer would be
// asked to trust a CA that did not sign the certificate in use — an outage that looks like a networking
// problem.
func TestAggregationAPICertHook_SecretAndCABundleShareOneValuesKey(t *testing.T) {
	// Reads chart files: needs -count=1 to be meaningful (see the note at the top of this file).

	valuesRef := ".Values." + aggregationAPICertValuesPath

	secret := repoFile(t, "templates/controller/secret.yaml")
	if !strings.Contains(secret, "name: "+aggregationAPISecretName+"\n") {
		t.Fatalf("templates/controller/secret.yaml does not render the Secret %q the hook writes", aggregationAPISecretName)
	}
	for _, key := range []string{"ca", "crt", "key"} {
		if !strings.Contains(secret, valuesRef+"."+key) {
			t.Fatalf("templates/controller/secret.yaml does not take %s from %s", key, valuesRef)
		}
	}

	apiservice := repoFile(t, "templates/controller/apiservice.yaml")
	// Both APIServices plus the guard that keeps them out of the release until the CA exists.
	if got := strings.Count(apiservice, valuesRef+".ca"); got != 3 {
		t.Fatalf("templates/controller/apiservice.yaml references %s.ca %d times, want 3 (two caBundles and the render guard)", valuesRef, got)
	}

	// The Deployment mounts that Secret, or the process has no certificate to serve with.
	deployment := repoFile(t, "templates/controller/deployment.yaml")
	if !strings.Contains(deployment, "secretName: "+aggregationAPISecretName) {
		t.Fatalf("templates/controller/deployment.yaml does not mount the Secret %q", aggregationAPISecretName)
	}

	// An internal value the chart reads must be declared in EVERY values schema. There is one per
	// edition, and the chart is rendered against the schema of the edition being built — so a key added
	// to one file and not the others renders fine where it was added and fails only in the editions
	// nobody looked at.
	schemas, err := filepath.Glob(filepath.Join("..", "..", "..", "openapi", "values*.yaml"))
	if err != nil {
		t.Fatalf("list the values schemas: %v", err)
	}
	if len(schemas) < 2 {
		t.Fatalf("expected several per-edition values schemas, found %v", schemas)
	}
	for _, schema := range schemas {
		data, err := os.ReadFile(schema)
		if err != nil {
			t.Fatalf("read %s: %v", schema, err)
		}
		if !strings.Contains(string(data), consts.AggregationAPICertValuesKey+":") {
			t.Fatalf("%s does not declare the internal value %q; the chart would not render in that edition",
				filepath.Base(schema), consts.AggregationAPICertValuesKey)
		}
	}
}

// A hook is linked into the hooks binary ONLY by a blank import in its main package. Without that line
// this package is never linked in, its init never runs, and the hook simply does not exist — no error,
// no log, and the certificate is never generated, so the APIServices stay unavailable forever. No test of
// the hook's own configuration can see that, which is why this one reads the import list.
func TestAggregationAPICertHook_IsRegisteredInTheHooksBinary(t *testing.T) {
	main, err := os.ReadFile("../main.go")
	if err != nil {
		t.Fatalf("read hooks/go/main.go: %v", err)
	}

	// The import path of THIS package, derived from the module path in go.mod so a rename of either
	// cannot leave the check passing against a stale string.
	gomod, err := os.ReadFile("../go.mod")
	if err != nil {
		t.Fatalf("read hooks/go/go.mod: %v", err)
	}
	modulePath := ""
	for _, line := range strings.Split(string(gomod), "\n") {
		if strings.HasPrefix(line, "module ") {
			modulePath = strings.TrimSpace(strings.TrimPrefix(line, "module "))
			break
		}
	}
	if modulePath == "" {
		t.Fatal("could not read the module path from hooks/go/go.mod")
	}

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	wantImport := fmt.Sprintf(`_ "%s/%s"`, modulePath, filepath.Base(dir))
	if !strings.Contains(string(main), wantImport) {
		t.Fatalf("hooks/go/main.go does not blank-import this hook package; expected the line %s", wantImport)
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
