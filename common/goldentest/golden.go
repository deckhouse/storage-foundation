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

// Package goldentest provides golden-file helpers for snapshot testing of
// generated Kubernetes manifests. The idea: a test renders an object to YAML
// and compares it against a checked-in "golden" file. When the generated
// output changes intentionally, regenerate the golden files and review the diff
// in the commit/MR -- an extra layer that forces a human to confirm "is this
// really the change I wanted?".
//
// Regenerate one package at a time, naming that package:
//
//	go test . -run TestGolden -golden                                  # from common/
//	go test ./internal/controllers/data-export/ -run TestGolden -golden
//
// Not `go test ./... -golden`: the flag is registered by this package's init, so
// it exists only in the test binaries that import it, and every sibling package
// that has tests of its own would reject it as an unknown flag.
package goldentest

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// Update reports whether golden files should be (re)generated instead of
// compared. It is bound to the shared -golden test flag, so every package that
// embeds this framework uses the same flag: `go test ./... -golden`.
var Update bool

// docDelimiter splits a multi-document YAML blob on standalone `---` lines.
var docDelimiter = regexp.MustCompile("(?m)^---$")

func init() {
	flag.BoolVar(&Update, "golden", false, "generate golden files instead of comparing against them")
}

// Mode controls how a golden snapshot is compared with its expected file.
type Mode int

const (
	// PerDocument splits both the actual and expected snapshots into YAML
	// documents and compares them one by one. This gives clearer diffs for
	// snapshots that contain many resources.
	PerDocument Mode = iota
	// WholeDocument compares the whole snapshot as a single YAML document.
	WholeDocument
)

// MarshalYAML renders a Kubernetes object (or any JSON-tagged struct) to YAML
// using sigs.k8s.io/yaml so the output matches `kubectl get -o yaml` field
// naming and ordering.
func MarshalYAML(t testing.TB, obj any) []byte {
	t.Helper()
	data, err := yaml.Marshal(obj)
	require.NoError(t, err, "marshal object to YAML")
	return data
}

// CompareOrUpdate writes got to goldenPath when Update is set, otherwise it
// asserts that got matches the content stored at goldenPath using the given mode.
func CompareOrUpdate(t testing.TB, goldenPath string, got []byte, mode Mode) {
	t.Helper()

	if Update {
		// Recreate the golden directory if it is missing so that deleting it and
		// rerunning with -golden regenerates the fixtures from scratch.
		require.NoError(t, os.MkdirAll(filepath.Dir(goldenPath), 0o755))
		require.NoError(t, os.WriteFile(goldenPath, got, 0o644))
		return
	}

	expB, err := os.ReadFile(goldenPath)
	require.NoErrorf(t, err, "read golden file %s (run tests with -golden to generate it)", goldenPath)

	switch mode {
	case WholeDocument:
		assert.YAMLEq(t, string(expB), string(got))
	default:
		exp := SplitDocuments(expB)
		actual := SplitDocuments(got)
		assert.Equalf(t, len(exp), len(actual),
			"the number of resulting manifests (%d) must match the golden file (%d)", len(actual), len(exp))
		for i := range actual {
			if i >= len(exp) {
				break
			}
			assert.YAMLEqf(t, exp[i], actual[i], "manifest #%d must match the golden file", i)
		}
	}
}

// SplitDocuments splits a multi-document YAML blob on `---` lines, dropping empty documents.
func SplitDocuments(doc []byte) []string {
	split := docDelimiter.Split(string(doc), -1)

	result := make([]string, 0, len(split))
	for i := range split {
		if split[i] != "" {
			result = append(result, split[i])
		}
	}

	return result
}
