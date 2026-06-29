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

func TestVolumeCaptureRequestSpec_Target_JSONRoundTrip(t *testing.T) {
	vcr := VolumeCaptureRequest{
		Spec: VolumeCaptureRequestSpec{
			Mode: VolumeCaptureModeSnapshot,
			Target: &VolumeCaptureTarget{
				UID:        "uid-a",
				APIVersion: "v1",
				Kind:       "PersistentVolumeClaim",
				Name:       "data-a",
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
	if out.Spec.Target == nil {
		t.Fatal("spec.target must round-trip")
	}
	if out.Spec.Target.UID != "uid-a" || out.Spec.Target.Name != "data-a" {
		t.Fatalf("target mismatch: %#v", out.Spec.Target)
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	spec := raw["spec"].(map[string]interface{})
	if _, ok := spec["targets"]; ok {
		t.Fatal("legacy spec.targets[] must not appear in JSON")
	}
	target, ok := spec["target"].(map[string]interface{})
	if !ok {
		t.Fatalf("spec.target must be a single object, got %#v", spec["target"])
	}
	// Namespace is omitted from spec.target (the PVC lives in the VCR namespace).
	if _, ok := target["namespace"]; ok {
		t.Fatal("spec.target.namespace must not appear in JSON when empty")
	}
}

func TestVolumeCaptureRequestStatus_DataRef_JSONRoundTrip(t *testing.T) {
	vcr := VolumeCaptureRequest{
		Status: VolumeCaptureRequestStatus{
			DataRef: &VolumeDataBinding{
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
	}

	data, err := json.Marshal(&vcr)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var out VolumeCaptureRequest
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Status.DataRef == nil {
		t.Fatal("status.dataRef must round-trip")
	}
	ref := out.Status.DataRef
	if ref.TargetUID != "uid-a" || ref.Artifact.Name != "snapcontent-a" {
		t.Fatalf("dataRef mismatch: %#v", ref)
	}
	// Namespace is preserved in status.dataRef.target so the binding is self-contained.
	if ref.Target.Namespace != "demo" {
		t.Fatalf("status.dataRef.target.namespace = %q, want %q", ref.Target.Namespace, "demo")
	}
	if ref.Artifact.Kind == "VolumeCaptureRequest" {
		t.Fatal("artifact must not reference an execution request")
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	status := raw["status"].(map[string]interface{})
	if _, ok := status["dataRefs"]; ok {
		t.Fatal("legacy status.dataRefs[] must not appear in JSON")
	}
	if _, ok := status["dataRef"].(map[string]interface{}); !ok {
		t.Fatalf("status.dataRef must be a single object, got %#v", status["dataRef"])
	}
}

func TestVolumeCaptureRequestCRD_SingleTargetSchema(t *testing.T) {
	crdPath := filepath.Join("..", "..", "crds", "internal", "storage.deckhouse.io_volumecapturerequests.yaml")
	data, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("read CRD: %v", err)
	}
	content := string(data)
	for _, forbidden := range []string{
		"persistentVolumeClaimRef:",
		"x-kubernetes-list-type: map",
		"x-kubernetes-list-map-keys",
	} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("CRD must not contain %q (single-target schema)", forbidden)
		}
	}
	for _, required := range []string{
		"target:",
		"dataRef:",
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
	target, ok := specProps["target"].(map[string]interface{})
	if !ok {
		t.Fatalf("spec.target must be an object schema, got %#v", specProps["target"])
	}
	if target["type"] != "object" {
		t.Fatalf("spec.target type: %#v", target["type"])
	}
	if _, ok := specProps["targets"]; ok {
		t.Fatal("spec.targets[] must not exist in CRD")
	}

	statusProps := schema["properties"].(map[string]interface{})["status"].(map[string]interface{})["properties"].(map[string]interface{})
	dataRef, ok := statusProps["dataRef"].(map[string]interface{})
	if !ok {
		t.Fatalf("status.dataRef must be an object schema, got %#v", statusProps["dataRef"])
	}
	if dataRef["type"] != "object" {
		t.Fatalf("status.dataRef type: %#v", dataRef["type"])
	}
	if _, ok := statusProps["dataRefs"]; ok {
		t.Fatal("status.dataRefs[] must not exist in CRD")
	}
}

func TestVolumeCaptureTarget_ZeroValueNotEqualNonZero(t *testing.T) {
	zero := VolumeCaptureTarget{}
	nonZero := VolumeCaptureTarget{UID: "x", APIVersion: "v1", Kind: "PersistentVolumeClaim", Name: "a", Namespace: "ns"}
	if reflect.DeepEqual(zero, nonZero) {
		t.Fatal("expected distinct zero and non-zero targets")
	}
}
