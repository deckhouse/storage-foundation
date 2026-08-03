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

package v1alpha1

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"
)

// recoveryStatusFields are the status.recovery properties the recovery contract depends on. A field
// missing from the CRD is silently pruned by the API server, which would turn a mid-recovery restart
// into an unrecoverable state, so the schema is asserted rather than assumed.
var recoveryStatusFields = []string{"sourcePVCUID", "exportPVCUID", "pvName", "pvUID"}

func TestDataExportStatus_RecoveryContract_JSONRoundTrip(t *testing.T) {
	de := DataExport{
		Status: DataExportImportStatus{
			CleanupReason: "ExportPVCPostRebindLost",
			Recovery: &RecoveryStatus{
				SourcePVCUID: "src-uid",
				ExportPVCUID: "exp-uid",
				PVName:       "pv-a",
				PVUID:        "pv-uid",
			},
		},
	}

	data, err := json.Marshal(&de)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var out DataExport
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Status.CleanupReason != "ExportPVCPostRebindLost" {
		t.Fatalf("status.cleanupReason = %q, want ExportPVCPostRebindLost", out.Status.CleanupReason)
	}
	if out.Status.Recovery == nil {
		t.Fatal("status.recovery must round-trip")
	}
	rec := out.Status.Recovery
	if rec.SourcePVCUID != "src-uid" || rec.ExportPVCUID != "exp-uid" || rec.PVName != "pv-a" || rec.PVUID != "pv-uid" {
		t.Fatalf("recovery mismatch: %#v", rec)
	}
}

func TestDataExportStatus_RecoveryContract_OmittedWhenEmpty(t *testing.T) {
	data, err := json.Marshal(&DataExport{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	status, ok := raw["status"].(map[string]interface{})
	if !ok {
		t.Fatalf("status must be an object, got %#v", raw["status"])
	}
	// An empty discriminator must not serialize: a present-but-empty cleanupReason would be
	// indistinguishable from "recovery required" for a reader that only checks presence.
	if _, ok := status["cleanupReason"]; ok {
		t.Fatal("status.cleanupReason must be omitted when empty")
	}
	if _, ok := status["recovery"]; ok {
		t.Fatal("status.recovery must be omitted when unset")
	}
}

func TestDataTransferCRDs_ExposeRecoveryContract(t *testing.T) {
	for _, crd := range []string{"dataexports.yaml", "dataimports.yaml"} {
		t.Run(crd, func(t *testing.T) {
			statusProps := readCRDStatusProperties(t, filepath.Join("..", "..", "crds", crd))

			cleanupReason, ok := statusProps["cleanupReason"].(map[string]interface{})
			if !ok {
				t.Fatalf("status.cleanupReason must exist, got %#v", statusProps["cleanupReason"])
			}
			if cleanupReason["type"] != "string" {
				t.Fatalf("status.cleanupReason type = %#v, want string", cleanupReason["type"])
			}

			recovery, ok := statusProps["recovery"].(map[string]interface{})
			if !ok {
				t.Fatalf("status.recovery must exist, got %#v", statusProps["recovery"])
			}
			if recovery["type"] != "object" {
				t.Fatalf("status.recovery type = %#v, want object", recovery["type"])
			}
			recoveryProps, ok := recovery["properties"].(map[string]interface{})
			if !ok {
				t.Fatalf("status.recovery must have properties, got %#v", recovery["properties"])
			}
			for _, field := range recoveryStatusFields {
				if _, ok := recoveryProps[field].(map[string]interface{}); !ok {
					t.Fatalf("status.recovery.%s must exist in %s", field, crd)
				}
			}
		})
	}
}

// TestDataTransferCRDs_ExposeManagedResourceReasons covers both kinds: the reasons live in the shared
// common module and DataImport will write them too, so an enum that only DataExport carries would make
// the first DataImport recovery write fail validation — the very failure mode this contract prevents.
func TestDataTransferCRDs_ExposeManagedResourceReasons(t *testing.T) {
	for _, crd := range []string{"dataexports.yaml", "dataimports.yaml"} {
		t.Run(crd, func(t *testing.T) {
			statusProps := readCRDStatusProperties(t, filepath.Join("..", "..", "crds", crd))

			conditions := mustObject(t, statusProps, "status.conditions", "conditions")
			items := mustObject(t, conditions, "status.conditions.items", "items")
			itemProps := mustObject(t, items, "status.conditions.items.properties", "properties")
			reason := mustObject(t, itemProps, "status.conditions[].reason", "reason")

			rawEnum, ok := reason["enum"].([]interface{})
			if !ok {
				t.Fatalf("status.conditions[].reason must have an enum, got %#v", reason["enum"])
			}
			allowed := make(map[string]struct{}, len(rawEnum))
			for _, v := range rawEnum {
				value, ok := v.(string)
				if !ok {
					t.Fatalf("condition reason enum must contain strings, got %#v", v)
				}
				allowed[value] = struct{}{}
			}
			for _, want := range []string{"ManagedResourceLost", "ManagedResourceIdentityMismatch", "CleanupBlocked"} {
				if _, ok := allowed[want]; !ok {
					t.Fatalf("%s condition reason enum missing %q", crd, want)
				}
			}
		})
	}
}

// TestDataTransferCRDs_RussianDocsCoverNewFields keeps the hand-curated translations in step with the
// English CRDs: the doc-ru files are a separate hand-maintained tree, so a new field is easy to add in
// one and forget in the other.
func TestDataTransferCRDs_RussianDocsCoverNewFields(t *testing.T) {
	for _, crd := range []string{"dataexports.yaml", "dataimports.yaml"} {
		t.Run(crd, func(t *testing.T) {
			ru := readCRDStatusProperties(t, filepath.Join("..", "..", "crds", "doc-ru-"+crd))

			if _, ok := ru["cleanupReason"].(map[string]interface{}); !ok {
				t.Fatalf("doc-ru-%s must document status.cleanupReason", crd)
			}
			recovery, ok := ru["recovery"].(map[string]interface{})
			if !ok {
				t.Fatalf("doc-ru-%s must document status.recovery", crd)
			}
			recoveryProps, ok := recovery["properties"].(map[string]interface{})
			if !ok {
				t.Fatalf("doc-ru-%s status.recovery must document its properties", crd)
			}
			documented := make([]string, 0, len(recoveryProps))
			for field := range recoveryProps {
				documented = append(documented, field)
			}
			sort.Strings(documented)
			want := append([]string(nil), recoveryStatusFields...)
			sort.Strings(want)
			if len(documented) != len(want) {
				t.Fatalf("doc-ru-%s documents %v, want exactly %v", crd, documented, want)
			}
			for i := range want {
				if documented[i] != want[i] {
					t.Fatalf("doc-ru-%s documents %v, want exactly %v", crd, documented, want)
				}
			}
		})
	}
}

func readCRDStatusProperties(t *testing.T, path string) map[string]interface{} {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]interface{}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	spec := mustObject(t, doc, path+" spec", "spec")
	versions, ok := spec["versions"].([]interface{})
	if !ok || len(versions) == 0 {
		t.Fatalf("%s spec.versions must be a non-empty list, got %#v", path, spec["versions"])
	}
	version, ok := versions[0].(map[string]interface{})
	if !ok {
		t.Fatalf("%s spec.versions[0] must be an object, got %#v", path, versions[0])
	}
	schemaWrapper := mustObject(t, version, path+" spec.versions[0].schema", "schema")
	schema := mustObject(t, schemaWrapper, path+" openAPIV3Schema", "openAPIV3Schema")
	schemaProps := mustObject(t, schema, path+" schema properties", "properties")
	status := mustObject(t, schemaProps, path+" status schema", "status")
	return mustObject(t, status, path+" status properties", "properties")
}

// mustObject reads a nested mapping and fails with the schema path instead of panicking on a type
// assertion, so a restructured CRD reports where it diverged rather than a bare interface conversion.
func mustObject(t *testing.T, parent map[string]interface{}, describe, key string) map[string]interface{} {
	t.Helper()

	value, ok := parent[key].(map[string]interface{})
	if !ok {
		t.Fatalf("%s must be an object, got %#v", describe, parent[key])
	}
	return value
}
