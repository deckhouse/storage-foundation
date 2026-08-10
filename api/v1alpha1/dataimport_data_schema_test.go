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

package v1alpha1

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"
)

// crdStatusProperties returns status.properties of the named hand-curated CRD at the given served version.
// The dataexports/dataimports CRDs are NOT generated from these Go types (hack/generate_code.sh deletes the
// controller-gen output for them, because they carry a non-standard `download` subresource and CEL rules
// markers cannot express), so a field added to the Go struct reaches the API only if it is also added here
// by hand. That gap is exactly what the tests below watch.
func crdStatusProperties(t *testing.T, crdFile, version string) map[string]interface{} {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "..", "crds", crdFile))
	if err != nil {
		t.Fatalf("read CRD %s: %v", crdFile, err)
	}
	var doc map[string]interface{}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse CRD yaml %s: %v", crdFile, err)
	}

	versions, ok := doc["spec"].(map[string]interface{})["versions"].([]interface{})
	if !ok {
		t.Fatalf("%s: spec.versions missing", crdFile)
	}
	for _, v := range versions {
		ver, ok := v.(map[string]interface{})
		if !ok || ver["name"] != version {
			continue
		}
		schema := ver["schema"].(map[string]interface{})["openAPIV3Schema"].(map[string]interface{})
		status, ok := schema["properties"].(map[string]interface{})["status"].(map[string]interface{})
		if !ok {
			t.Fatalf("%s: status schema missing on %s", crdFile, version)
		}
		props, ok := status["properties"].(map[string]interface{})
		if !ok {
			t.Fatalf("%s: status.properties missing on %s", crdFile, version)
		}
		return props
	}
	t.Fatalf("%s: version %s not found", crdFile, version)
	return nil
}

// TestDataImportCRD_StatusDataDeclaresFSType guards the field that records the filesystem the imported bytes
// were written onto. The apiserver prunes anything the schema does not declare, so an undeclared fsType is
// dropped silently on the status write — and it cannot be re-derived afterwards: the scratch volume is
// destroyed right after capture and the produced artifact carries no filesystem metadata. Undeclared here
// means lost forever, with no error anywhere.
func TestDataImportCRD_StatusDataDeclaresFSType(t *testing.T) {
	statusProps := crdStatusProperties(t, "dataimports.yaml", "v1alpha1")

	data, ok := statusProps["data"].(map[string]interface{})
	if !ok {
		t.Fatal("dataimports.yaml: status.data must exist (it carries the produced artifact)")
	}
	dataProps, ok := data["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("dataimports.yaml: status.data.properties missing: %#v", data["properties"])
	}

	// artifactRef is asserted alongside fsType so that "the schema is fine" cannot mean "the whole data
	// block was replaced by the new field".
	for _, want := range []string{"artifactRef", "fsType"} {
		if _, ok := dataProps[want]; !ok {
			t.Fatalf("dataimports.yaml: status.data must declare %q, got keys %v", want, sortedKeys(dataProps))
		}
	}

	fsType, ok := dataProps["fsType"].(map[string]interface{})
	if !ok {
		t.Fatalf("dataimports.yaml: status.data.fsType must be a schema object, got %#v", dataProps["fsType"])
	}
	if fsType["type"] != "string" {
		t.Fatalf("dataimports.yaml: status.data.fsType type = %v, want string", fsType["type"])
	}
	// Deliberately no enum: the filesystem type is whatever the CSI driver reports (ext4, xfs, btrfs, ...),
	// and an enum here would reject volumes the driver considers perfectly valid.
	if _, ok := fsType["enum"]; ok {
		t.Fatal("dataimports.yaml: status.data.fsType must not be enumerated — the value is driver-defined")
	}
	if desc, _ := fsType["description"].(string); desc == "" {
		t.Fatal("dataimports.yaml: status.data.fsType must be documented (the docs are rendered from the CRD)")
	}

	t.Logf("dataimports.yaml status.data properties inspected: %d", len(dataProps))
	if len(dataProps) == 0 {
		t.Fatal("inspected no status.data properties at all")
	}

	// The Russian documentation is a parallel, hand-written mirror of the same schema (crds/doc-ru-*.yaml)
	// from which the module docs are rendered. A field missing there is not a validation failure anywhere, so
	// nothing but this check notices it.
	ruProps := crdStatusProperties(t, "doc-ru-dataimports.yaml", "v1alpha1")
	ruData, ok := ruProps["data"].(map[string]interface{})
	if !ok {
		t.Fatal("doc-ru-dataimports.yaml: status.data must be documented")
	}
	ruDataProps, ok := ruData["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("doc-ru-dataimports.yaml: status.data.properties missing: %#v", ruData["properties"])
	}
	ruFSType, ok := ruDataProps["fsType"].(map[string]interface{})
	if !ok {
		t.Fatalf("doc-ru-dataimports.yaml: status.data.fsType must be documented, got keys %v", sortedKeys(ruDataProps))
	}
	if desc, _ := ruFSType["description"].(string); desc == "" {
		t.Fatal("doc-ru-dataimports.yaml: status.data.fsType description must not be empty")
	}
}

// TestDataExportCRD_StatusDeclaresNoDataBlock is the leak guard for the SHARED status type. DataImport and
// DataExport both serialize DataExportImportStatus, so status.data — and every field under it, fsType
// included — is structurally reachable from a DataExport as well. What keeps it out of the export API is this
// CRD declaring no status.data at all, which is correct: an export streams a live volume out, produces no
// artifact and owns no scratch volume whose filesystem could be observed. A `data` block appearing here means
// import-only fields have leaked into the export's published API.
func TestDataExportCRD_StatusDeclaresNoDataBlock(t *testing.T) {
	statusProps := crdStatusProperties(t, "dataexports.yaml", "v1alpha1")

	if _, ok := statusProps["data"]; ok {
		t.Fatalf("dataexports.yaml: status must not declare a data block; an export produces no artifact (status keys: %v)",
			sortedKeys(statusProps))
	}

	t.Logf("dataexports.yaml status properties inspected: %d", len(statusProps))
	if len(statusProps) == 0 {
		t.Fatal("inspected no status properties at all — an empty schema would pass the check above vacuously")
	}
}

// TestDataExportImportData_FSTypeWireForm pins the wire contract of the new field: the JSON name consumers
// read (state-snapshotter copies status.data.fsType into SnapshotContent.status.data.fsType) and its
// omitempty behaviour. fsType is optional, so an unobserved filesystem type must be absent from the wire
// rather than present as an empty string that a consumer could mistake for a real value.
func TestDataExportImportData_FSTypeWireForm(t *testing.T) {
	encoded, err := json.Marshal(DataExportImportData{
		ArtifactRef: &DataArtifactReference{APIVersion: "snapshot.storage.k8s.io/v1", Kind: "VolumeSnapshotContent", Name: "vsc-a"},
		FsType:      "xfs",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var wire map[string]interface{}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatalf("unmarshal into map: %v", err)
	}
	if wire["fsType"] != "xfs" {
		t.Fatalf("wire key fsType = %v, want %q (got object %s)", wire["fsType"], "xfs", encoded)
	}

	var decoded DataExportImportData
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}
	if decoded.FsType != "xfs" {
		t.Fatalf("round-tripped FsType = %q, want %q", decoded.FsType, "xfs")
	}
	if decoded.ArtifactRef == nil || decoded.ArtifactRef.Name != "vsc-a" {
		t.Fatalf("round-trip lost the artifact reference: %#v", decoded.ArtifactRef)
	}

	empty, err := json.Marshal(DataExportImportData{})
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	if string(empty) != "{}" {
		t.Fatalf("an unobserved filesystem type must not appear on the wire, got %s", empty)
	}
}

// sortedKeys returns the map keys in a stable order, so a failure message names the same set every run.
func sortedKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
