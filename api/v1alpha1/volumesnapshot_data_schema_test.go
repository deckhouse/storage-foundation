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
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestVolumeSnapshotCRD_StatusDataRefSuffix guards the unified status.data wire shape on the
// extended CSI VolumeSnapshot CRD: sourceRef/artifactRef (not the legacy source/artifact keys).
// Drift here silently drops the state-snapshotter mirror patch (apiserver prunes unknown fields,
// then rejects required source/artifact), so VS never gets status.data.
func TestVolumeSnapshotCRD_StatusDataRefSuffix(t *testing.T) {
	crdPath := filepath.Join("..", "..", "crds", "snapshot.storage.k8s.io_volumesnapshots.yaml")
	data, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("read CRD: %v", err)
	}

	var doc map[string]interface{}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse CRD yaml: %v", err)
	}
	versions := doc["spec"].(map[string]interface{})["versions"].([]interface{})
	var dataSchema map[string]interface{}
	for _, v := range versions {
		ver := v.(map[string]interface{})
		if ver["name"] != "v1" {
			continue
		}
		schema := ver["schema"].(map[string]interface{})["openAPIV3Schema"].(map[string]interface{})
		statusProps := schema["properties"].(map[string]interface{})["status"].(map[string]interface{})["properties"].(map[string]interface{})
		var ok bool
		dataSchema, ok = statusProps["data"].(map[string]interface{})
		if !ok {
			t.Fatal("status.data must exist on VolumeSnapshot v1")
		}
		break
	}
	if dataSchema == nil {
		t.Fatal("VolumeSnapshot version v1 not found")
	}

	props, ok := dataSchema["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("status.data.properties missing: %#v", dataSchema["properties"])
	}
	for _, want := range []string{"sourceRef", "artifactRef"} {
		sub, ok := props[want].(map[string]interface{})
		if !ok {
			t.Fatalf("status.data.%s must be an object schema, got %#v", want, props[want])
		}
		subProps, ok := sub["properties"].(map[string]interface{})
		if !ok {
			t.Fatalf("status.data.%s.properties missing", want)
		}
		for _, field := range []string{"apiVersion", "kind", "name"} {
			if _, ok := subProps[field]; !ok {
				t.Fatalf("status.data.%s must declare %s", want, field)
			}
		}
	}
	for _, forbidden := range []string{"source", "artifact"} {
		if _, ok := props[forbidden]; ok {
			t.Fatalf("status.data must not declare legacy key %q (use sourceRef/artifactRef)", forbidden)
		}
	}

	required, ok := dataSchema["required"].([]interface{})
	if !ok {
		t.Fatalf("status.data.required missing: %#v", dataSchema["required"])
	}
	got := make([]string, 0, len(required))
	for _, r := range required {
		got = append(got, r.(string))
	}
	wantRequired := []string{"artifactRef", "sourceRef"}
	if !reflect.DeepEqual(got, wantRequired) {
		t.Fatalf("status.data.required = %v, want %v", got, wantRequired)
	}
}
