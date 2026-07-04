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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/slok/kubewebhook/v2/pkg/model"
	kwhvalidating "github.com/slok/kubewebhook/v2/pkg/webhook/validating"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

// makeAdmissionReview builds a minimal AdmissionReview for a given operation and namespace.
func makeAdmissionReview(op model.AdmissionReviewOp, namespace string) *model.AdmissionReview {
	return &model.AdmissionReview{
		Operation: op,
		Namespace: namespace,
	}
}

// makeDataExport builds a DataExport with the specified GroupKind targetRef (C6).
func makeDataExport(group, kind, name string) *dev1alpha1.DataExport {
	return &dev1alpha1.DataExport{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-export",
			Namespace: "test-ns",
		},
		Spec: dev1alpha1.DataExportSpec{
			TargetRef: dev1alpha1.DataExportTargetRefSpec{
				Group: group,
				Kind:  kind,
				Name:  name,
			},
		},
	}
}

// assertResult is a helper to check that the ValidatorResult matches expectations.
func assertResult(t *testing.T, got *kwhvalidating.ValidatorResult, wantValid bool, wantMsgContains string) {
	t.Helper()
	if got == nil {
		t.Fatal("ValidatorResult is nil")
	}
	if got.Valid != wantValid {
		t.Errorf("Valid = %v, want %v (message: %q)", got.Valid, wantValid, got.Message)
	}
	if wantMsgContains != "" && !strings.Contains(got.Message, wantMsgContains) {
		t.Errorf("Message = %q, want it to contain %q", got.Message, wantMsgContains)
	}
}

// TestDataExportValidateFunc_NonCreateOperations verifies that UPDATE, DELETE, and other
// non-CREATE operations are unconditionally allowed (the CRD is immutable, so spec changes
// are blocked by the API server itself).
func TestDataExportValidateFunc_NonCreateOperations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		operation model.AdmissionReviewOp
	}{
		{name: "UPDATE operation is allowed without validation", operation: model.OperationUpdate},
		{name: "DELETE operation is allowed without validation", operation: model.OperationDelete},
		{name: "unknown operation is allowed without validation", operation: model.OperationUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			validateFunc := DataExportValidateFunc()
			ar := makeAdmissionReview(tt.operation, "test-ns")
			// Even a forbidden bare VSC must pass on non-CREATE, proving no validation runs.
			de := makeDataExport(snapshotStorageGroup, volumeSnapshotContentKind, "anything")

			got, err := validateFunc(context.Background(), ar, de)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			assertResult(t, got, true, "")
		})
	}
}

// TestDataExportValidateFunc_EmptyTargetRef verifies that a CREATE with an empty kind or
// name in targetRef is rejected with a descriptive validation message.
func TestDataExportValidateFunc_EmptyTargetRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		group  string
		kind   string
		target string
	}{
		{name: "empty kind is denied", group: "", kind: "", target: "some-pvc"},
		{name: "empty name is denied", group: "", kind: "PersistentVolumeClaim", target: ""},
		{name: "both kind and name empty is denied", group: "", kind: "", target: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			validateFunc := DataExportValidateFunc()
			ar := makeAdmissionReview(model.OperationCreate, "test-ns")
			de := makeDataExport(tt.group, tt.kind, tt.target)

			got, err := validateFunc(context.Background(), ar, de)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			assertResult(t, got, false, "targetRef")
		})
	}
}

// TestDataExportValidateFunc_BareVolumeSnapshotContentDenied verifies the security check:
// a bare cluster-scoped VolumeSnapshotContent target is rejected to prevent cross-namespace
// data exfiltration.
func TestDataExportValidateFunc_BareVolumeSnapshotContentDenied(t *testing.T) {
	t.Parallel()

	validateFunc := DataExportValidateFunc()
	ar := makeAdmissionReview(model.OperationCreate, "test-ns")
	de := makeDataExport(snapshotStorageGroup, volumeSnapshotContentKind, "leaked-content")

	got, err := validateFunc(context.Background(), ar, de)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertResult(t, got, false, "VolumeSnapshotContent")
}

// TestDataExportValidateFunc_ValidTargets verifies that well-formed GroupKind targets
// (live and snapshot leaves) are accepted at admission time; existence/readiness is the
// controller's job, not the webhook's.
func TestDataExportValidateFunc_ValidTargets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		group  string
		kind   string
		target string
	}{
		{name: "core PVC is allowed", group: "", kind: "PersistentVolumeClaim", target: "my-pvc"},
		{name: "VirtualDisk is allowed", group: "virtualization.deckhouse.io", kind: "VirtualDisk", target: "my-vd"},
		{name: "VolumeSnapshot leaf is allowed", group: "snapshot.storage.k8s.io", kind: "VolumeSnapshot", target: "my-snap"},
		{name: "VirtualDiskSnapshot leaf is allowed", group: "virtualization.deckhouse.io", kind: "VirtualDiskSnapshot", target: "my-vdsnap"},
		{name: "arbitrary domain snapshot leaf is allowed", group: "example.deckhouse.io", kind: "FancySnapshot", target: "my-fancy"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			validateFunc := DataExportValidateFunc()
			ar := makeAdmissionReview(model.OperationCreate, "test-ns")
			de := makeDataExport(tt.group, tt.kind, tt.target)

			got, err := validateFunc(context.Background(), ar, de)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			assertResult(t, got, true, "")
		})
	}
}

// TestDataExportValidateFunc_ObjectNotDataExport verifies that passing a non-DataExport object
// to the validator results in a denial with an appropriate message.
func TestDataExportValidateFunc_ObjectNotDataExport(t *testing.T) {
	t.Parallel()

	validateFunc := DataExportValidateFunc()
	ar := makeAdmissionReview(model.OperationCreate, "test-ns")

	notADataExport := &metav1.ObjectMeta{Name: "not-a-dataexport"}

	got, err := validateFunc(context.Background(), ar, notADataExport)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	assertResult(t, got, false, "DataExport")
}

// TestNewValidatingWebhookHandler_ReturnsHandler is an integration-level smoke test that
// confirms NewValidatingWebhookHandler returns a non-nil HTTP handler without error.
func TestNewValidatingWebhookHandler_ReturnsHandler(t *testing.T) {
	t.Parallel()

	validatorFunc := func(_ context.Context, _ *model.AdmissionReview, _ metav1.Object) (*kwhvalidating.ValidatorResult, error) {
		return &kwhvalidating.ValidatorResult{Valid: true}, nil
	}

	handler, err := NewValidatingWebhookHandler(validatorFunc, "test-validator", &dev1alpha1.DataExport{}, nil)
	if err != nil {
		t.Fatalf("NewValidatingWebhookHandler returned error: %v", err)
	}
	if handler == nil {
		t.Fatal("NewValidatingWebhookHandler returned nil handler")
	}
}

// TestNewValidatingWebhookHandler_ServeHTTP verifies that the returned HTTP handler responds
// to an admission review request correctly.
func TestNewValidatingWebhookHandler_ServeHTTP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		validResult *kwhvalidating.ValidatorResult
		wantAllowed bool
	}{
		{
			name:        "validator returning valid=true produces allowed response",
			validResult: &kwhvalidating.ValidatorResult{Valid: true},
			wantAllowed: true,
		},
		{
			name:        "validator returning valid=false produces denied response",
			validResult: &kwhvalidating.ValidatorResult{Valid: false, Message: "denied by test"},
			wantAllowed: false,
		},
	}

	makeAdmissionReviewBody := func(t *testing.T) []byte {
		t.Helper()
		de := makeDataExport("", "PersistentVolumeClaim", "my-pvc")
		de.TypeMeta = metav1.TypeMeta{APIVersion: "storage.deckhouse.io/v1alpha1", Kind: "DataExport"}
		objJSON, err := json.Marshal(de)
		if err != nil {
			t.Fatalf("marshal DataExport: %v", err)
		}

		arBody := map[string]interface{}{
			"apiVersion": "admission.k8s.io/v1",
			"kind":       "AdmissionReview",
			"request": map[string]interface{}{
				"uid":       "test-uid",
				"namespace": "test-ns",
				"operation": "CREATE",
				"resource": map[string]interface{}{
					"group":    "storage.deckhouse.io",
					"version":  "v1alpha1",
					"resource": "dataexports",
				},
				"kind": map[string]interface{}{
					"group":   "storage.deckhouse.io",
					"version": "v1alpha1",
					"kind":    "DataExport",
				},
				"object": json.RawMessage(objJSON),
			},
		}
		body, err := json.Marshal(arBody)
		if err != nil {
			t.Fatalf("marshal AdmissionReview: %v", err)
		}
		return body
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			capturedResult := tt.validResult
			validatorFunc := func(_ context.Context, _ *model.AdmissionReview, _ metav1.Object) (*kwhvalidating.ValidatorResult, error) {
				return capturedResult, nil
			}

			webhookHandler, err := NewValidatingWebhookHandler(validatorFunc, "test-validator", &dev1alpha1.DataExport{}, nil)
			if err != nil {
				t.Fatalf("NewValidatingWebhookHandler: %v", err)
			}

			body := makeAdmissionReviewBody(t)
			req := httptest.NewRequest(http.MethodPost, "/dataexport-validate", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rw := httptest.NewRecorder()

			webhookHandler.ServeHTTP(rw, req)

			if rw.Code != http.StatusOK {
				t.Fatalf("HTTP status = %d, want 200; body: %s", rw.Code, rw.Body.String())
			}

			var respAR struct {
				Response struct {
					Allowed bool   `json:"allowed"`
					UID     string `json:"uid"`
				} `json:"response"`
			}
			if err := json.Unmarshal(rw.Body.Bytes(), &respAR); err != nil {
				t.Fatalf("unmarshal response: %v\nbody: %s", err, rw.Body.String())
			}

			if respAR.Response.Allowed != tt.wantAllowed {
				t.Errorf("response.allowed = %v, want %v; body: %s", respAR.Response.Allowed, tt.wantAllowed, rw.Body.String())
			}
		})
	}
}

// TestNewValidatingWebhookHandler_EmptyValidatorID verifies that an empty validator ID causes
// NewWebhook to return an error.
func TestNewValidatingWebhookHandler_EmptyValidatorID(t *testing.T) {
	t.Parallel()

	validatorFunc := func(_ context.Context, _ *model.AdmissionReview, _ metav1.Object) (*kwhvalidating.ValidatorResult, error) {
		return &kwhvalidating.ValidatorResult{Valid: true}, nil
	}

	_, err := NewValidatingWebhookHandler(validatorFunc, "" /* empty ID */, &dev1alpha1.DataExport{}, nil)
	if err == nil {
		t.Error("expected error for empty validator ID, got nil")
	}
}
