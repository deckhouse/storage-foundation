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
	"fmt"

	"github.com/deckhouse/module-sdk/pkg"
	objectpatch "github.com/deckhouse/module-sdk/pkg/object-patch"
	"github.com/deckhouse/module-sdk/pkg/registry"

	"github.com/deckhouse/storage-foundation/hooks/go/consts"
)

const (
	crdsSnapshot      = "grantCRDs"
	valuesPath        = consts.ModuleName + ".internal.grantsCRDFields"
	definitionCRDName = "grantableclusterresourcedefinitions.multitenancy.deckhouse.io"
	referenceCRDName  = "grantableclusterresourcereferences.multitenancy.deckhouse.io"
	crdsJqFilter      = `{name: .metadata.name, spec: ([.spec.versions[]? | select(.name == "v1alpha1") | .schema.openAPIV3Schema.properties.spec] | first // {})}`
)

var _ = registry.RegisterFunc(
	&pkg.HookConfig{
		OnBeforeHelm: &pkg.OrderedConfig{Order: 10},
		Kubernetes: []pkg.KubernetesConfig{
			{
				Name:         crdsSnapshot,
				APIVersion:   "apiextensions.k8s.io/v1",
				Kind:         "CustomResourceDefinition",
				NameSelector: &pkg.NameSelector{MatchNames: []string{definitionCRDName, referenceCRDName}},
				JqFilter:     crdsJqFilter,
			},
		},
	},
	discoverGrantsCRDFields,
)

type Schema struct {
	Properties map[string]Schema `json:"properties,omitempty"`
	Items      *Schema           `json:"items,omitempty"`
}

type CRD struct {
	Name string `json:"name"`
	Spec Schema `json:"spec"`
}

type Fields struct {
	CatalogFields                       bool `json:"catalogFields"`
	DefaultFromAdditionalAnnotationKeys bool `json:"defaultFromAdditionalAnnotationKeys"`
	FieldPathMessage                    bool `json:"fieldPathMessage"`
	FieldPathMissingPath                bool `json:"fieldPathMissingPath"`
	FieldPathResources                  bool `json:"fieldPathResources"`
}

func (s Schema) has(path ...string) bool {
	cur := s
	for _, step := range path {
		if step == "[]" {
			if cur.Items == nil {
				return false
			}
			cur = *cur.Items
			continue
		}
		next, ok := cur.Properties[step]
		if !ok {
			return false
		}
		cur = next
	}
	return true
}

func Evaluate(crds []CRD) Fields {
	var fields Fields
	for _, crd := range crds {
		switch crd.Name {
		case definitionCRDName:
			fields.CatalogFields = crd.Spec.has("catalogFields")
			fields.DefaultFromAdditionalAnnotationKeys = crd.Spec.has("defaultFrom", "additionalAnnotationKeys")
		case referenceCRDName:
			fields.FieldPathMessage = crd.Spec.has("fieldPaths", "[]", "message")
			fields.FieldPathMissingPath = crd.Spec.has("fieldPaths", "[]", "missingPath")
			fields.FieldPathResources = crd.Spec.has("fieldPaths", "[]", "resources")
		}
	}
	return fields
}

func discoverGrantsCRDFields(_ context.Context, input *pkg.HookInput) error {
	crds, err := objectpatch.UnmarshalToStruct[CRD](input.Snapshots, crdsSnapshot)
	if err != nil {
		return fmt.Errorf("unmarshal %s: %w", crdsSnapshot, err)
	}
	input.Values.Set(valuesPath, Evaluate(crds))
	return nil
}
