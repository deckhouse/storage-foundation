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
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The VolumeSnapshot fork mirrors state-snapshotter's SnapshotContent.status.data onto
// VolumeSnapshot.status.data so d8 can read the captured-volume descriptor from the namespaced object
// alone. The fork's own README promises that wire shape is byte-identical — and nothing enforced it: the
// mirror kept declaring accessModes for a while after state-snapshotter dropped the field, i.e. it
// published a property no writer could ever fill. These tests turn that promise into a check.
//
// mirrorDataFields is the expected shape, and it is the ONE place the field list is written down in this
// repository. It is transcribed by hand from SnapshotDataBinding in state-snapshotter (the mirror's
// source of truth), because that type lives in another repository and cannot be imported here. The set is
// asserted exactly, in both directions: adding or removing a field on either side fails these tests until
// the source of truth is re-checked and this list is updated with it.
var mirrorDataFields = map[string]int{
	// json name -> protobuf field number in the forked Go type
	"sourceRef":        1,
	"artifactRef":      2,
	"volumeMode":       3,
	"fsType":           4,
	"storageClassName": 6,
	"size":             7,
}

// Number 5 is a deliberate gap: it belonged to accessModes, which was removed before the schema was ever
// released. The remaining numbers MUST keep their values — these tags are part of the type the fork
// carries, and renumbering neighbours would silently change the wire meaning of every field after the gap
// for any consumer that speaks protobuf.
const mirrorRemovedProtobufNumber = 5

const (
	volumeSnapshotCRDFile   = "snapshot.storage.k8s.io_volumesnapshots.yaml"
	volumeSnapshotCRDRUFile = "doc-ru-snapshot.storage.k8s.io_volumesnapshots.yaml"
	volumeSnapshotForkPatch = "003-volumesnapshot-dataimport-fork.patch"
)

// crdStatusDataByVersion returns status.data.properties for every version of the named CRD that declares a
// status.data block, keyed by version name. Versions without one are skipped rather than reported: the
// shipped CRD deliberately carries the mirror on the storage version only.
func crdStatusDataByVersion(t *testing.T, crdFile string) map[string]map[string]interface{} {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "..", "crds", crdFile))
	if err != nil {
		t.Fatalf("read CRD %s: %v", crdFile, err)
	}
	var doc map[string]interface{}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse CRD yaml %s: %v", crdFile, err)
	}

	spec, ok := doc["spec"].(map[string]interface{})
	if !ok {
		t.Fatalf("%s: spec missing", crdFile)
	}
	versions, ok := spec["versions"].([]interface{})
	if !ok {
		t.Fatalf("%s: spec.versions missing", crdFile)
	}

	out := make(map[string]map[string]interface{})
	for _, v := range versions {
		ver, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := ver["name"].(string)
		schema, ok := ver["schema"].(map[string]interface{})
		if !ok {
			continue
		}
		root, ok := schema["openAPIV3Schema"].(map[string]interface{})
		if !ok {
			continue
		}
		props, ok := root["properties"].(map[string]interface{})
		if !ok {
			continue
		}
		status, ok := props["status"].(map[string]interface{})
		if !ok {
			continue
		}
		statusProps, ok := status["properties"].(map[string]interface{})
		if !ok {
			continue
		}
		data, ok := statusProps["data"].(map[string]interface{})
		if !ok {
			continue
		}
		dataProps, ok := data["properties"].(map[string]interface{})
		if !ok {
			t.Fatalf("%s: %s status.data declares no properties: %#v", crdFile, name, data)
		}
		out[name] = dataProps
	}
	return out
}

// TestVolumeSnapshotMirrorCRD_StatusDataFieldSet asserts the shipped CRD and its Russian documentation
// mirror exactly the fields the source of truth has — no more (a property nobody writes, which is what
// accessModes had become) and no fewer (a written field the apiserver would prune away).
func TestVolumeSnapshotMirrorCRD_StatusDataFieldSet(t *testing.T) {
	want := sortedFieldNames(mirrorDataFields)

	inspected := 0
	for _, crdFile := range []string{volumeSnapshotCRDFile, volumeSnapshotCRDRUFile} {
		byVersion := crdStatusDataByVersion(t, crdFile)
		if len(byVersion) == 0 {
			t.Fatalf("%s: no version declares status.data — the mirror is the whole point of this fork", crdFile)
		}
		for version, dataProps := range byVersion {
			got := sortedKeys(dataProps)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("%s (%s): status.data fields = %v, want %v", crdFile, version, got, want)
			}
			inspected++
		}
	}

	t.Logf("CRD status.data blocks inspected: %d (fields per block: %d)", inspected, len(mirrorDataFields))
	if inspected == 0 {
		t.Fatal("inspected no status.data block at all — the check would be vacuously green")
	}
}

// mirrorTypeBlockRE matches the forked Go struct as it appears inside the patch (added lines only). The
// patch carries two copies — the authoritative ./client one and its vendor/ mirror — and both must agree.
var mirrorTypeBlockRE = regexp.MustCompile(`(?ms)^\+type VolumeSnapshotDataBinding struct \{\n(.*?)^\+\}$`)

// mirrorTypeFieldRE matches one added struct field line and captures its json and protobuf tags.
var mirrorTypeFieldRE = regexp.MustCompile("^\\+\t(\\w+) +\\S+ +`json:\"([^\"]+)\" +protobuf:\"([^\"]+)\"`$")

// TestVolumeSnapshotForkPatch_MirrorTypeFieldSet reads the fork patch as text, because that is the only
// form the type exists in inside this repository: the Go file itself lives in the upstream tree the werf
// build clones. It pins the field set against the same list the CRD is checked against, so type and schema
// cannot drift apart, and it pins the protobuf numbering of the survivors.
func TestVolumeSnapshotForkPatch_MirrorTypeFieldSet(t *testing.T) {
	path := filepath.Join("..", "..", "images", "snapshot-controller", "patches", volumeSnapshotForkPatch)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fork patch %s: %v", path, err)
	}
	patch := string(raw)

	blocks := mirrorTypeBlockRE.FindAllStringSubmatch(patch, -1)
	// Two copies is the contract of this patch (see images/snapshot-controller/patches/README.md): it edits
	// both ./client/apis/... and vendor/.../client/v8/apis/..., and an edit applied to one copy only would
	// leave local -mod=vendor builds disagreeing with the image build.
	if len(blocks) != 2 {
		t.Fatalf("%s: found %d VolumeSnapshotDataBinding declarations, want 2 (./client and vendor/ copies)", volumeSnapshotForkPatch, len(blocks))
	}
	if blocks[0][1] != blocks[1][1] {
		t.Fatalf("%s: the ./client and vendor/ copies of VolumeSnapshotDataBinding differ", volumeSnapshotForkPatch)
	}

	fields := map[string]int{}
	for _, line := range strings.Split(blocks[0][1], "\n") {
		m := mirrorTypeFieldRE.FindStringSubmatch(line)
		if m == nil {
			continue // comment or marker line inside the struct
		}
		jsonName := strings.Split(m[2], ",")[0]
		protoParts := strings.Split(m[3], ",")
		if len(protoParts) < 2 {
			t.Fatalf("field %s: unparsable protobuf tag %q", m[1], m[3])
		}
		number := 0
		for _, r := range protoParts[1] {
			if r < '0' || r > '9' {
				t.Fatalf("field %s: protobuf tag %q has no field number", m[1], m[3])
			}
			number = number*10 + int(r-'0')
		}
		fields[jsonName] = number
	}

	gotNames := sortedFieldNames(fields)
	wantNames := sortedFieldNames(mirrorDataFields)
	if strings.Join(gotNames, ",") != strings.Join(wantNames, ",") {
		t.Fatalf("%s: VolumeSnapshotDataBinding json fields = %v, want %v", volumeSnapshotForkPatch, gotNames, wantNames)
	}
	for name, wantNumber := range mirrorDataFields {
		if fields[name] != wantNumber {
			t.Errorf("%s: field %s has protobuf number %d, want %d (neighbours must not be renumbered)",
				volumeSnapshotForkPatch, name, fields[name], wantNumber)
		}
		if fields[name] == mirrorRemovedProtobufNumber {
			t.Errorf("%s: field %s reuses protobuf number %d, which belonged to the removed accessModes",
				volumeSnapshotForkPatch, name, mirrorRemovedProtobufNumber)
		}
	}

	// The deepcopy the patch carries is generated output: with every remaining field a value type, the whole
	// body is the plain struct copy. A per-field block here would mean a reference-typed field came back
	// without the checks above noticing the shape change.
	if strings.Contains(patch, "AccessModes") {
		t.Errorf("%s: still mentions AccessModes; the mirror must not carry a field state-snapshotter does not publish", volumeSnapshotForkPatch)
	}

	t.Logf("fork patch: %d copies of the mirrored type, %d fields each, %d bytes of patch", len(blocks), len(fields), len(patch))
	if len(fields) == 0 {
		t.Fatal("parsed no fields at all — the check would be vacuously green")
	}
}

// sortedFieldNames returns the json field names in a stable order so failures name the same set every run.
func sortedFieldNames(fields map[string]int) []string {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
