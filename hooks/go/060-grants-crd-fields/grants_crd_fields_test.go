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

package grantscrdfields

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/deckhouse/module-sdk/pkg/jq"
	"sigs.k8s.io/yaml"
)

func snapshotOf(t *testing.T, file string) CRD {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	var obj map[string]any
	if err := yaml.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("decode %s: %v", file, err)
	}
	query, err := jq.NewQuery(crdsJqFilter)
	if err != nil {
		t.Fatalf("compile the jq filter: %v", err)
	}
	res, err := query.FilterObject(context.Background(), obj)
	if err != nil {
		t.Fatalf("filter %s: %v", file, err)
	}
	var crd CRD
	if err := json.Unmarshal([]byte(res.String()), &crd); err != nil {
		t.Fatalf("decode the filtered %s: %v", file, err)
	}
	return crd
}

func TestEvaluateShippedCRDs(t *testing.T) {
	cases := map[string]Fields{
		"release-1.77": {},
		"release-1.78": {
			CatalogFields:      true,
			FieldPathMessage:   true,
			FieldPathResources: true,
		},
		"main": {
			CatalogFields:                       true,
			DefaultFromAdditionalAnnotationKeys: true,
			FieldPathMessage:                    true,
			FieldPathMissingPath:                true,
			FieldPathResources:                  true,
		},
	}
	for release, want := range cases {
		t.Run(release, func(t *testing.T) {
			files, err := filepath.Glob(filepath.Join("testdata", release, "*.yaml"))
			if err != nil || len(files) != 2 {
				t.Fatalf("fixtures of %s: %v %v", release, files, err)
			}
			var crds []CRD
			for _, file := range files {
				crds = append(crds, snapshotOf(t, file))
			}
			if got := Evaluate(crds); got != want {
				t.Fatalf("Evaluate = %+v, want %+v", got, want)
			}
		})
	}
}

func TestEvaluateWithoutCRDs(t *testing.T) {
	if got := Evaluate(nil); got != (Fields{}) {
		t.Fatalf("Evaluate(nil) = %+v, want no fields", got)
	}
}

func TestEvaluatePatchedCatalogFields(t *testing.T) {
	crd := CRD{
		Name: definitionCRDName,
		Spec: Schema{Properties: map[string]Schema{"catalogFields": {Items: &Schema{}}}},
	}
	if got := Evaluate([]CRD{crd}); got != (Fields{CatalogFields: true}) {
		t.Fatalf("Evaluate = %+v, want only catalogFields", got)
	}
}

func TestEvaluateCRDWithoutTheVersion(t *testing.T) {
	query, err := jq.NewQuery(crdsJqFilter)
	if err != nil {
		t.Fatalf("compile the jq filter: %v", err)
	}
	obj := map[string]any{
		"metadata": map[string]any{"name": definitionCRDName},
		"spec":     map[string]any{"versions": []any{map[string]any{"name": "v1beta1"}}},
	}
	res, err := query.FilterObject(context.Background(), obj)
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	var crd CRD
	if err := json.Unmarshal([]byte(res.String()), &crd); err != nil {
		t.Fatalf("decode %s: %v", res.String(), err)
	}
	if got := Evaluate([]CRD{crd}); got != (Fields{}) {
		t.Fatalf("Evaluate = %+v, want no fields", got)
	}
}
