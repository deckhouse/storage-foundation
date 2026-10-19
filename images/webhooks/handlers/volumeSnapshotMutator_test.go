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

package handlers

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/slok/kubewebhook/v2/pkg/model"
	kwhmutating "github.com/slok/kubewebhook/v2/pkg/webhook/mutating"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	snapv1 "github.com/deckhouse/storage-foundation/api/snapshot/v1"
)

// TestVolumeSnapshotProvenanceWireLiterals pins the key and the value this webhook writes. Both are read by
// another repository and by objects already living in a cluster, so a rename on this side is a silent
// contract break in both directions: snapshots created by this build would stop being recognised, and the
// reader would go on judging by a key nobody writes any more.
func TestVolumeSnapshotProvenanceWireLiterals(t *testing.T) {
	t.Parallel()

	if provenanceLabelName != "state-snapshotter.deckhouse.io/provenance" {
		t.Fatalf("unexpected provenance key: %s", provenanceLabelName)
	}
	if provenanceUserInCluster != "user-in-cluster" {
		t.Fatalf("unexpected provenance value: %s", provenanceUserInCluster)
	}
}

// ptr returns a pointer to v, for the bool fields of an owner reference.
func ptr[T any](v T) *T { return &v }

// volumeSnapshotWithMeta builds a VolumeSnapshot carrying the given labels and owner references, which is
// everything stateOriginOnCreate looks at.
func volumeSnapshotWithMeta(labels map[string]string, owners []metav1.OwnerReference) *snapv1.VolumeSnapshot {
	return &snapv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "snap",
			Namespace:       "test-ns",
			Labels:          labels,
			OwnerReferences: owners,
		},
	}
}

// TestStateOriginOnCreate enumerates every shape a CREATE payload can arrive in, because each row is a
// different party stating (or not stating) where the object came from:
//
//   - nobody stated anything: the ordinary case, a client creating its own snapshot, and the one this
//     handler exists for;
//   - the unified snapshot write-path already stated its own origin in the payload it is submitting: writing
//     over it would erase the record that this object is a node of a snapshot tree, and the consequence is
//     not cosmetic — the tree stops recognising its own node;
//   - some other value is already there: the same rule, stated in the form that does not depend on knowing
//     which values exist. This handler only ever ADDS a missing statement;
//   - the object arrives already controlled by another controller: it belongs to that controller's
//     lifecycle, and nothing of this stack's vocabulary may appear on it. A non-controller owner reference
//     is somebody's bookkeeping, not a lifecycle, so it does not withhold the statement.
func TestStateOriginOnCreate(t *testing.T) {
	t.Parallel()

	foreignController := metav1.OwnerReference{
		APIVersion: "virtualization.deckhouse.io/v1alpha2",
		Kind:       "VirtualDiskSnapshot",
		Name:       "vds",
		Controller: ptr(true),
	}
	plainReference := metav1.OwnerReference{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Name:       "bookkeeping",
		Controller: ptr(false),
	}

	cases := []struct {
		name        string
		labels      map[string]string
		owners      []metav1.OwnerReference
		wantWritten bool   // whether the call reports having stated an origin
		wantPresent bool   // whether the provenance key exists afterwards
		wantValue   string // its value, when it exists
	}{
		{
			name:        "no labels at all",
			wantWritten: true,
			wantPresent: true,
			wantValue:   provenanceUserInCluster,
		},
		{
			name:        "unrelated labels only",
			labels:      map[string]string{"app": "web"},
			wantWritten: true,
			wantPresent: true,
			wantValue:   provenanceUserInCluster,
		},
		{
			name:        "the tree's own origin already in the payload",
			labels:      map[string]string{provenanceLabelName: "unified-snapshot"},
			wantPresent: true,
			wantValue:   "unified-snapshot",
		},
		{
			name:        "some other origin already in the payload",
			labels:      map[string]string{provenanceLabelName: "somebody-elses-write-path"},
			wantPresent: true,
			wantValue:   "somebody-elses-write-path",
		},
		{
			name:        "the key already there with an empty value",
			labels:      map[string]string{provenanceLabelName: ""},
			wantPresent: true,
			wantValue:   "",
		},
		{
			name:   "already controlled by a foreign controller",
			owners: []metav1.OwnerReference{foreignController},
		},
		{
			name:   "controlled by a foreign controller, listed after a plain reference",
			owners: []metav1.OwnerReference{plainReference, foreignController},
		},
		{
			name:        "a plain owner reference that is not the controller",
			owners:      []metav1.OwnerReference{plainReference},
			wantWritten: true,
			wantPresent: true,
			wantValue:   provenanceUserInCluster,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			snapshot := volumeSnapshotWithMeta(tc.labels, tc.owners)
			written := stateOriginOnCreate(snapshot)
			if written != tc.wantWritten {
				t.Fatalf("stateOriginOnCreate = %v, want %v (labels %#v)", written, tc.wantWritten, snapshot.Labels)
			}

			got, present := snapshot.Labels[provenanceLabelName]
			if present != tc.wantPresent {
				t.Fatalf("provenance key present = %v, want %v (labels %#v)", present, tc.wantPresent, snapshot.Labels)
			}
			if present && got != tc.wantValue {
				t.Fatalf("provenance = %q, want %q (labels %#v)", got, tc.wantValue, snapshot.Labels)
			}

			// Whatever the branch, the rest of the object's labels survive: this handler adds one key and
			// touches nothing else.
			for k, v := range tc.labels {
				if k == provenanceLabelName {
					continue
				}
				if snapshot.Labels[k] != v {
					t.Fatalf("label %q = %q, want %q preserved", k, snapshot.Labels[k], v)
				}
			}
		})
	}
	t.Logf("checked %d CREATE payload shapes", len(cases))
}

// TestStateOriginOnCreateIsIdempotent pins what a second pass must do, which is nothing. The origin is
// stamped on CREATE only, so a second pass over the same object is not an ordinary event — but a webhook
// whose second call differs from its first is one whose result depends on how many times the API server
// happened to call it, and that is never the intended contract.
func TestStateOriginOnCreateIsIdempotent(t *testing.T) {
	t.Parallel()

	snapshot := volumeSnapshotWithMeta(nil, nil)
	if !stateOriginOnCreate(snapshot) {
		t.Fatal("the first pass must state the origin of an unclaimed snapshot")
	}
	first := snapshot.Labels[provenanceLabelName]

	if stateOriginOnCreate(snapshot) {
		t.Fatalf("the second pass must write nothing, labels=%#v", snapshot.Labels)
	}
	if got := snapshot.Labels[provenanceLabelName]; got != first {
		t.Fatalf("provenance changed on the second pass: %q -> %q", first, got)
	}
	if len(snapshot.Labels) != 1 {
		t.Fatalf("the second pass added labels: %#v", snapshot.Labels)
	}
}

// reviewVS runs a request through the real kubewebhook pipeline and returns the spec.mode its patch writes
// ("" if untouched) and the paths the patch changes.
func reviewVS(t *testing.T, op model.AdmissionReviewOp, oldRaw, newRaw string) (string, []string, error) {
	t.Helper()

	wh, err := kwhmutating.NewWebhook(kwhmutating.WebhookConfig{
		ID:      "vs",
		Obj:     new(snapv1.VolumeSnapshot),
		Mutator: kwhmutating.MutatorFunc(VolumeSnapshotMutate),
	})
	if err != nil {
		t.Fatalf("build the webhook: %v", err)
	}

	res, err := wh.Review(context.Background(), model.AdmissionReview{
		Operation:    op,
		OldObjectRaw: []byte(oldRaw),
		NewObjectRaw: []byte(newRaw),
	})
	if err != nil {
		return "", nil, err
	}

	resp, ok := res.(*model.MutatingAdmissionResponse)
	if !ok {
		t.Fatalf("unexpected response type %T", res)
	}

	var ops []struct {
		Path  string          `json:"path"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(resp.JSONPatchPatch, &ops); err != nil {
		t.Fatalf("decode the patch: %v", err)
	}

	mode := ""
	paths := make([]string, 0, len(ops))
	for _, o := range ops {
		paths = append(paths, o.Path)
		if o.Path != "/spec/mode" {
			continue
		}
		if err := json.Unmarshal(o.Value, &mode); err != nil {
			t.Fatalf("decode the patched spec.mode: %v", err)
		}
	}
	return mode, paths, nil
}

// TestVolumeSnapshotMutateMode pins how spec.mode is filled, including the upstream-client update that the
// CRD immutability rule used to reject for an import-mode snapshot.
func TestVolumeSnapshotMutateMode(t *testing.T) {
	t.Parallel()

	// upstreamUpdate is what a client built on the upstream type sends: no mode, an empty source, a finalizer.
	const upstreamUpdate = `{"metadata":{"finalizers":["provisioner.storage.kubernetes.io/volumesnapshot-as-source-protection"]},` +
		`"spec":{"source":{}},"status":{"readyToUse":true}}`

	tests := []struct {
		name     string
		op       model.AdmissionReviewOp
		oldRaw   string
		newRaw   string
		wantMode string
		wantErr  bool
	}{
		{
			name:     "success: upstream-client update keeps Import",
			op:       model.OperationUpdate,
			oldRaw:   `{"spec":{"mode":"Import"}}`,
			newRaw:   upstreamUpdate,
			wantMode: snapv1.VolumeSnapshotModeImport,
		},
		{
			name:     "success: upstream-client update keeps Capture",
			op:       model.OperationUpdate,
			oldRaw:   `{"spec":{"mode":"Capture","source":{}}}`,
			newRaw:   upstreamUpdate,
			wantMode: snapv1.VolumeSnapshotModeCapture,
		},
		{
			name:     "success: update of an object stored without mode gets Capture",
			op:       model.OperationUpdate,
			oldRaw:   `{"spec":{"source":{}}}`,
			newRaw:   upstreamUpdate,
			wantMode: snapv1.VolumeSnapshotModeCapture,
		},
		{
			name:   "success: explicit mode is left for the CRD rule",
			op:     model.OperationUpdate,
			oldRaw: `{"spec":{"mode":"Import"}}`,
			newRaw: `{"spec":{"mode":"Capture","source":{}}}`,
		},
		{
			name:     "success: create without mode gets Capture",
			op:       model.OperationCreate,
			newRaw:   `{"spec":{"source":{"volumeSnapshotContentName":"vsc"}}}`,
			wantMode: snapv1.VolumeSnapshotModeCapture,
		},
		{
			name:    "error: malformed stored object",
			op:      model.OperationUpdate,
			oldRaw:  `not json`,
			newRaw:  upstreamUpdate,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mode, paths, err := reviewVS(t, tt.op, tt.oldRaw, tt.newRaw)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("review: %v", err)
			}
			if mode != tt.wantMode {
				t.Fatalf("spec.mode = %q, want %q", mode, tt.wantMode)
			}
			for _, p := range paths {
				if strings.HasPrefix(p, "/metadata/labels") || strings.HasPrefix(p, "/metadata/finalizers") {
					t.Fatalf("patch changes %s: only a PVC-source create may get labels, finalizers are never touched", p)
				}
			}
		})
	}
}

// TestVolumeSnapshotMatchCondition evaluates the matchCondition read from the chart template, so a change to
// the expression is checked against the requests the mutator must and must not see.
func TestVolumeSnapshotMatchCondition(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "templates", "webhooks", "webhook.yaml"))
	if err != nil {
		t.Fatalf("read the webhook template: %v", err)
	}
	m := regexp.MustCompile(`name: pvc-source-create-or-missing-mode\s+expression: ("(?:[^"\\]|\\.)*")`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("matchCondition pvc-source-create-or-missing-mode not found in the template")
	}
	expr, err := strconv.Unquote(string(m[1]))
	if err != nil {
		t.Fatalf("unquote the expression: %v", err)
	}

	env, err := cel.NewEnv(cel.Variable("object", cel.DynType), cel.Variable("request", cel.DynType))
	if err != nil {
		t.Fatalf("build the CEL environment: %v", err)
	}
	ast, iss := env.Compile(expr)
	if iss.Err() != nil {
		t.Fatalf("compile the expression: %v", iss.Err())
	}
	prg, err := env.Program(ast)
	if err != nil {
		t.Fatalf("plan the expression: %v", err)
	}

	tests := []struct {
		name   string
		op     string
		object string
		want   bool
	}{
		{name: "success: PVC-source create reaches the webhook", op: "CREATE", object: `{"spec":{"mode":"Capture","source":{"persistentVolumeClaimName":"pvc"}}}`, want: true},
		{name: "success: create without mode but with a source reaches the webhook", op: "CREATE", object: `{"spec":{"source":{}}}`, want: true},
		{name: "success: create with neither mode nor source is left to the schema", op: "CREATE", object: `{"spec":{}}`},
		{name: "success: import create skips the webhook", op: "CREATE", object: `{"spec":{"mode":"Import"}}`},
		{name: "success: update without mode reaches the webhook", op: "UPDATE", object: `{"spec":{"source":{}}}`, want: true},
		{name: "success: update with mode skips the webhook", op: "UPDATE", object: `{"spec":{"mode":"Import"}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// CEL takes the request as untyped data, the way the API server hands it over.
			object := map[string]any{}
			if err := json.Unmarshal([]byte(tt.object), &object); err != nil {
				t.Fatalf("decode the object: %v", err)
			}
			out, _, err := prg.Eval(map[string]any{"object": object, "request": map[string]any{"operation": tt.op}})
			if err != nil {
				t.Fatalf("evaluate the expression: %v", err)
			}
			got, ok := out.Value().(bool)
			if !ok {
				t.Fatalf("the expression returned %v, not a bool", out.Value())
			}
			if got != tt.want {
				t.Fatalf("reaches the webhook = %v, want %v", got, tt.want)
			}
		})
	}
}
