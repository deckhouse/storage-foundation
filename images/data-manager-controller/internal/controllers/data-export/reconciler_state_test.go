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

package dataexport

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDataexportReconcilerHoldsNoRequestState is the precondition for running this controller with
// MaxConcurrentReconciles above 1: the reconciler value is shared by every concurrent reconcile, so a field
// that belongs to the object being reconciled is a data race rather than state.
//
// The list below is the injected dependencies and nothing else. If a field is added, decide which it is: a
// dependency, and it goes in the list; or per-request state, and either it moves into the call chain or the
// concurrency in cmd/main.go has to come back down. The DataImport controller is the cautionary example --
// it keeps the object and its generated names on the struct, which is exactly why its concurrency was left
// at the default.
func TestDataexportReconcilerHoldsNoRequestState(t *testing.T) {
	allowed := map[string]bool{
		"Client": true,
		"Reader": true,
		"Config": true,
		// resource-agnostic snapshot resolution
		"Dynamic":    true,
		"RESTMapper": true,
		// injectable clock for deterministic timestamps in tests
		"Now": true,
	}

	typ := reflect.TypeOf(DataexportReconciler{})
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		assert.Truef(t, allowed[name],
			"DataexportReconciler.%s is shared across concurrent reconciles; if it is per-request state it must not live here (see cmd/main.go, maxConcurrentDataExports)", name)
	}
}
