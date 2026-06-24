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
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestVolumeCaptureRequestSpec_Targets_JSONRoundTrip(t *testing.T) {
	vcr := VolumeCaptureRequest{
		Spec: VolumeCaptureRequestSpec{
			Mode: VolumeCaptureModeSnapshot,
			Targets: []VolumeCaptureTarget{
				{
					UID:        "uid-a",
					APIVersion: "v1",
					Kind:       "PersistentVolumeClaim",
					Namespace:  "demo",
					Name:       "data-a",
				},
				{
					UID:        "uid-b",
					APIVersion: "v1",
					Kind:       "PersistentVolumeClaim",
					Namespace:  "demo",
					Name:       "data-b",
				},
			},
		},
	}

	data, err := json.Marshal(&vcr)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var out VolumeCaptureRequest
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Spec.Targets) != 2 {
		t.Fatalf("targets len: got %d want 2", len(out.Spec.Targets))
	}
	if out.Spec.Targets[0].UID != "uid-a" || out.Spec.Targets[1].Name != "data-b" {
		t.Fatalf("targets mismatch: %#v", out.Spec.Targets)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	spec := raw["spec"].(map[string]interface{})
	if _, ok := spec["persistentVolumeClaimRef"]; ok {
		t.Fatal("persistentVolumeClaimRef must not appear in JSON")
	}
	targets := spec["targets"].([]interface{})
	if len(targets) != 2 {
		t.Fatalf("raw targets len: got %d want 2", len(targets))
	}
}

func TestVolumeCaptureRequestStatus_DataRefs_JSONRoundTrip(t *testing.T) {
	vcr := VolumeCaptureRequest{
		Status: VolumeCaptureRequestStatus{
			DataRefs: []VolumeDataBinding{
				{
					TargetUID: "uid-a",
					Target: VolumeCaptureTarget{
						UID:        "uid-a",
						APIVersion: "v1",
						Kind:       "PersistentVolumeClaim",
						Namespace:  "demo",
						Name:       "data-a",
					},
					Artifact: VolumeDataArtifactRef{
						APIVersion: "snapshot.storage.k8s.io/v1",
						Kind:       "VolumeSnapshotContent",
						Name:       "snapcontent-a",
					},
				},
			},
		},
	}

	data, err := json.Marshal(&vcr)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var out VolumeCaptureRequest
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Status.DataRefs) != 1 {
		t.Fatalf("dataRefs len: got %d want 1", len(out.Status.DataRefs))
	}
	ref := out.Status.DataRefs[0]
	if ref.TargetUID != "uid-a" || ref.Artifact.Name != "snapcontent-a" {
		t.Fatalf("dataRefs mismatch: %#v", ref)
	}
	if ref.Artifact.Kind == "VolumeCaptureRequest" {
		t.Fatal("artifact must not reference an execution request")
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	status := raw["status"].(map[string]interface{})
	if _, ok := status["dataRef"]; ok {
		t.Fatal("dataRef must not appear in JSON")
	}
}

func TestVolumeCaptureRequestCRD_MapListSemantics(t *testing.T) {
	crdPath := filepath.Join("..", "..", "crds", "storage.deckhouse.io_volumecapturerequests.yaml")
	data, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("read CRD: %v", err)
	}
	content := string(data)
	for _, forbidden := range []string{"persistentVolumeClaimRef:", "dataRef:", "volumeSnapshotClassName:"} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("CRD must not contain %q", forbidden)
		}
	}
	for _, required := range []string{
		"x-kubernetes-list-type: map",
		"x-kubernetes-list-map-keys",
		"targets:",
		"dataRefs:",
		"targetUID:",
	} {
		if !strings.Contains(content, required) {
			t.Fatalf("CRD missing %q", required)
		}
	}

	var doc map[string]interface{}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse CRD yaml: %v", err)
	}
	versions := doc["spec"].(map[string]interface{})["versions"].([]interface{})
	schema := versions[0].(map[string]interface{})["schema"].(map[string]interface{})["openAPIV3Schema"].(map[string]interface{})
	specProps := schema["properties"].(map[string]interface{})["spec"].(map[string]interface{})["properties"].(map[string]interface{})
	targets := specProps["targets"].(map[string]interface{})
	if targets["x-kubernetes-list-type"] != "map" {
		t.Fatalf("spec.targets list-type: %#v", targets["x-kubernetes-list-type"])
	}
	mapKeys, ok := targets["x-kubernetes-list-map-keys"].([]interface{})
	if !ok || len(mapKeys) != 1 || mapKeys[0].(string) != "uid" {
		t.Fatalf("spec.targets map keys: %#v", targets["x-kubernetes-list-map-keys"])
	}

	statusProps := schema["properties"].(map[string]interface{})["status"].(map[string]interface{})["properties"].(map[string]interface{})
	dataRefs := statusProps["dataRefs"].(map[string]interface{})
	if dataRefs["x-kubernetes-list-type"] != "map" {
		t.Fatalf("status.dataRefs list-type: %#v", dataRefs["x-kubernetes-list-type"])
	}
	dataMapKeys := dataRefs["x-kubernetes-list-map-keys"].([]interface{})
	if len(dataMapKeys) != 1 || dataMapKeys[0].(string) != "targetUID" {
		t.Fatalf("status.dataRefs map keys: %#v", dataRefs["x-kubernetes-list-map-keys"])
	}
}

func TestVolumeCaptureTarget_ZeroValueNotEqualNonZero(t *testing.T) {
	zero := VolumeCaptureTarget{}
	nonZero := VolumeCaptureTarget{UID: "x", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "a", Namespace: "ns"}
	if reflect.DeepEqual(zero, nonZero) {
		t.Fatal("expected distinct zero and non-zero targets")
	}
}

// Controller validation TODO (PR-F-2): reject duplicate spec.targets[].uid at runtime if apiserver
// does not enforce map-list keys on create in all code paths.
