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

package config

import (
	"slices"
	"testing"
)

func TestTrustedRequestNamespacesFromEnv(t *testing.T) {
	t.Setenv(ControllerNamespaceEnv, "d8-storage-foundation")
	t.Setenv(TrustedRequestNamespacesEnv, " d8-backup, ,d8-other ")
	opts := NewConfig()
	if want := []string{"d8-backup", "d8-other"}; !slices.Equal(opts.TrustedRequestNamespaces, want) {
		t.Fatalf("TrustedRequestNamespaces = %q, want %q", opts.TrustedRequestNamespaces, want)
	}
	for ns, want := range map[string]bool{
		"d8-storage-foundation": true,
		"d8-backup":             true,
		"d8-other":              true,
		"team-a":                false,
		"":                      false,
	} {
		if got := opts.TrustsRequestsIn(ns); got != want {
			t.Errorf("TrustsRequestsIn(%q) = %v, want %v", ns, got, want)
		}
	}
}

func TestNoTrustedRequestNamespacesByDefault(t *testing.T) {
	t.Setenv(ControllerNamespaceEnv, "d8-storage-foundation")
	t.Setenv(TrustedRequestNamespacesEnv, "")
	opts := NewConfig()
	if len(opts.TrustedRequestNamespaces) != 0 {
		t.Fatalf("TrustedRequestNamespaces = %q, want none", opts.TrustedRequestNamespaces)
	}
	if opts.TrustsRequestsIn("d8-backup") {
		t.Fatal("d8-backup is trusted without TRUSTED_REQUEST_NAMESPACES")
	}
}
