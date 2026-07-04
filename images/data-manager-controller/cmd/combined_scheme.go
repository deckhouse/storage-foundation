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

package main

import "k8s.io/apimachinery/pkg/runtime"

// Something that can add to scheme (typically, a scheme builder)
type SchemeAdder interface {
	AddToScheme(scheme *runtime.Scheme) error
}

// Combines multiple scheme adders into one
type CombinedSchemeAdder struct {
	builders []SchemeAdder
}

// Creates new combined scheme adder from list of scheme adders
func NewCombinedSchemeBuilder(builders ...SchemeAdder) *CombinedSchemeAdder {
	return &CombinedSchemeAdder{
		builders: builders,
	}
}

func (b *CombinedSchemeAdder) AddToScheme(scheme *runtime.Scheme) error {
	for _, builder := range b.builders {
		if err := builder.AddToScheme(scheme); err != nil {
			return err
		}
	}

	return nil
}
