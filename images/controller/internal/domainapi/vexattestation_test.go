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

package domainapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This package is why the controller binary links golang.org/x/crypto at all: the aggregated API server
// pulls it in through k8s.io/apiserver/pkg/server. That brings in an advisory against an openpgp
// subpackage the binary does not compile, and suppressing it takes an attestation attached TO THIS IMAGE.
//
// Where the predicate lives is the part that is easy to get wrong, and getting it wrong is silent: an
// attestation on the wrong image leaves the scan reporting the finding while a file in the repository
// explains why it is not a problem. The two halves — a predicate at the path the build looks for, and the
// build actually asking for one — are checked here together, because either alone is inert.
//
// Unlike the chart guards elsewhere in this repository, these reads need no -count=1: both files sit at
// this module's root, and the test cache does track files inside the module.
func TestControllerImage_CarriesItsOwnVEXAttestation(t *testing.T) {
	// From internal/domainapi to the module root, which is also the image directory.
	const imageDir = "../.."

	// The advisory the predicate is allowed to suppress, restated here on purpose. Nothing in the
	// repository derives it, so a guard can only pin it: an edit that swaps the id keeps a well-formed
	// predicate that suppresses a DIFFERENT finding, leaving the real one reported while the file next to
	// it looks like it was handled. Suppressing something else is then a deliberate change to both sides.
	const expectedAdvisory = "GO-2026-5932"
	// The module whose advisory this is. Its VERSION is not restated — it is read from go.mod below,
	// because it moves on its own: any bump of a k8s.io dependency can carry x/crypto with it, and a
	// predicate pinned to the old version silently stops matching what is in the image.
	const suppressedModule = "golang.org/x/crypto"

	predicate := filepath.Join(imageDir, "known_vulnerabilities.vex")
	raw, err := os.ReadFile(predicate)
	if err != nil {
		t.Fatalf("the controller image has no VEX predicate of its own (%v); the root one is attached to the bundle image, which does not carry this binary", err)
	}

	var doc struct {
		Statements []struct {
			Vulnerability struct {
				Name string `json:"name"`
			} `json:"vulnerability"`
			Products []struct {
				ID string `json:"@id"`
			} `json:"products"`
			Status          string `json:"status"`
			Justification   string `json:"justification"`
			ImpactStatement string `json:"impact_statement"`
		} `json:"statements"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the VEX predicate is not valid JSON, so the attestation step would fail: %v", err)
	}
	if len(doc.Statements) == 0 {
		t.Fatal("the VEX predicate carries no statements")
	}

	wantVersion := moduleVersionFromGoMod(t, filepath.Join(imageDir, "go.mod"), suppressedModule)

	var found bool
	for _, st := range doc.Statements {
		for _, product := range st.Products {
			if !strings.Contains(product.ID, suppressedModule) {
				continue
			}
			found = true
			if st.Vulnerability.Name != expectedAdvisory {
				t.Fatalf("the %s statement suppresses %q, want %q; suppressing a different advisory leaves the real one reported",
					suppressedModule, st.Vulnerability.Name, expectedAdvisory)
			}
			if st.Status != "not_affected" {
				t.Fatalf("the %s statement has status %q, want not_affected", suppressedModule, st.Status)
			}
			if st.Justification != "vulnerable_code_not_in_execute_path" {
				t.Fatalf("the %s statement is justified as %q, want vulnerable_code_not_in_execute_path",
					suppressedModule, st.Justification)
			}
			// The version in the purl has to be the version this binary actually links, or the
			// suppression stops matching what the scanner finds in the image.
			wantPurl := "pkg:golang/" + suppressedModule + "@" + wantVersion
			if product.ID != wantPurl {
				t.Fatalf("the predicate covers %q while go.mod resolves %s to %s; want the purl %q",
					product.ID, suppressedModule, wantVersion, wantPurl)
			}
			// The statement has to be about THIS binary. A copy of another image's wording would claim
			// something true elsewhere and unverified here.
			if !strings.Contains(st.ImpactStatement, "internal/domainapi") {
				t.Fatalf("the impact statement does not name the import path that brings x/crypto into this binary: %q", st.ImpactStatement)
			}
		}
	}
	if !found {
		t.Fatalf("the VEX predicate says nothing about %s, which this binary links", suppressedModule)
	}

	// The predicate is inert unless the image build asks for an attestation, and the request has to name
	// THIS image. The template attaches the attestation to whatever image it is handed, and other images
	// in this repository legitimately hand it a name built with printf — copying that shape here would
	// attach the attestation to a non-final build artifact and leave the published image without one,
	// with the predicate still sitting next to it.
	build, err := os.ReadFile(filepath.Join(imageDir, "werf.inc.yaml"))
	if err != nil {
		t.Fatalf("read the image build definition: %v", err)
	}
	if !strings.Contains(string(build), `include "vex mitigation"`) {
		t.Fatal("the controller image build does not attach a VEX attestation, so the predicate next to it is never used")
	}
	const wantAddressee = `include "vex mitigation" (list $ $.ImageName)`
	if !strings.Contains(string(build), wantAddressee) {
		t.Fatalf("the attestation is not attached to this image itself; the build must contain %s", wantAddressee)
	}
}

// moduleVersionFromGoMod returns the version go.mod resolves for a module path. It reads the require
// lines directly rather than shelling out, so the guard needs no module downloads to run.
func moduleVersionFromGoMod(t *testing.T, path, modulePath string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 && fields[0] == modulePath && strings.HasPrefix(fields[1], "v") {
			return fields[1]
		}
	}
	t.Fatalf("%s does not require %s, so the predicate covers a module this binary does not link", path, modulePath)
	return ""
}
