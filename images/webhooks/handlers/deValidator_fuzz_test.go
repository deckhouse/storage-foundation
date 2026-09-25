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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	"github.com/deckhouse/sds-common-lib/logger"
	"github.com/deckhouse/sds-common-lib/logger/kwhlogger"
	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

// FuzzDataExportValidateFunc drives the real admission-webhook HTTP handler (JSON decode ->
// DataExportValidateFunc) end to end with an arbitrary DataExport.spec.targetRef, mirroring the
// data-exporter FuzzHandler approach: fuzz the entrypoint, not a single helper, so decode and validation
// bugs are caught together.
//
// It also asserts the one decision the validator must never get wrong whatever it is fed: a bare
// cluster-scoped VolumeSnapshotContent is refused. That target has no tenant boundary, so admitting one
// would let a DataExport in any namespace read a snapshot belonging to another.
func FuzzDataExportValidateFunc(f *testing.F) {
	const create = "CREATE"

	// group, kind, name of the target, plus the admission operation and namespace.
	f.Add(create, "default", "", dev1alpha1.KindPVC, "existing-pvc")
	f.Add(create, "default", "", dev1alpha1.KindPVC, "missing-pvc")
	f.Add(create, "", "", dev1alpha1.KindPVC, "existing-pvc")
	f.Add(create, "default", dev1alpha1.GroupSnapshotStorage, "VolumeSnapshot", "some-vs")
	f.Add(create, "default", dev1alpha1.GroupVirtualization, "VirtualDisk", "some-vd")
	f.Add(create, "default", dev1alpha1.GroupVirtualization, "VirtualDiskSnapshot", "some-vds")
	// The forbidden target, and the near-misses that must NOT be treated as it.
	f.Add(create, "default", dev1alpha1.GroupSnapshotStorage, dev1alpha1.KindVolumeSnapshotContent, "some-vsc")
	f.Add(create, "default", "", dev1alpha1.KindVolumeSnapshotContent, "some-vsc")
	f.Add(create, "default", dev1alpha1.GroupSnapshotStorage, strings.ToLower(dev1alpha1.KindVolumeSnapshotContent), "some-vsc")
	f.Add(create, "default", strings.ToUpper(dev1alpha1.GroupSnapshotStorage), dev1alpha1.KindVolumeSnapshotContent, "some-vsc")
	f.Add(create, "default", dev1alpha1.GroupSnapshotStorage+" ", dev1alpha1.KindVolumeSnapshotContent, "some-vsc")
	f.Add(create, "default", "", "", "existing-pvc")
	f.Add(create, "default", "", dev1alpha1.KindPVC, "")
	f.Add(create, "default", "", "Pod", "existing-pvc")
	f.Add(create, "default", "", strings.ToLower(dev1alpha1.KindPVC), "existing-pvc")
	f.Add("UPDATE", "default", "", dev1alpha1.KindPVC, "existing-pvc")
	f.Add("DELETE", "default", "", dev1alpha1.KindPVC, "existing-pvc")
	f.Add("CONNECT", "default", "", dev1alpha1.KindPVC, "existing-pvc")
	f.Add("", "", "", "", "")
	f.Add(create, "kube-system", "", dev1alpha1.KindPVC, "existing-pvc")
	f.Add(create, "default", "", dev1alpha1.KindPVC, "../../etc/passwd")
	f.Add(create, "default", "", dev1alpha1.KindPVC, strings.Repeat("a", 300))
	f.Add(create, strings.Repeat("n", 300), "", dev1alpha1.KindPVC, "existing-pvc")
	f.Add(create, "default", "", strings.Repeat("K", 300), "existing-pvc")
	f.Add(create, "default", strings.Repeat("g", 300), dev1alpha1.KindPVC, "existing-pvc")
	f.Add(create, "нэймспейс", "", dev1alpha1.KindPVC, "имя")
	f.Add(create, "default", "", dev1alpha1.KindPVC, " abcdйцукен///⚺⪣⿓␜ⅷⱦ⣠")
	f.Add(create, "default", "", "VirtualDisk\x00", "some-vd")
	f.Add(create, "default\n", "", dev1alpha1.KindPVC, "existing-pvc")

	handler, err := NewValidatingWebhookHandler(DataExportValidateFunc(), "fuzz-de-validator", &dev1alpha1.DataExport{}, kwhlogger.New(logger.NewNop()))
	if err != nil {
		f.Fatalf("failed to build webhook handler: %v", err)
	}

	f.Fuzz(func(t *testing.T, operation, namespace, group, kind, name string) {
		dataExport := &dev1alpha1.DataExport{
			TypeMeta:   metav1.TypeMeta{Kind: "DataExport", APIVersion: dev1alpha1.APIGroup + "/" + dev1alpha1.APIVersion},
			ObjectMeta: metav1.ObjectMeta{Name: "fuzz-de", Namespace: namespace},
			Spec: dev1alpha1.DataExportSpec{
				TargetRef: dev1alpha1.DataExportTargetRefSpec{Group: group, Kind: kind, Name: name},
			},
		}
		objRaw, err := json.Marshal(dataExport)
		if err != nil {
			t.Fatalf("failed to marshal DataExport: %v", err)
		}

		ar := &admissionv1.AdmissionReview{
			TypeMeta: metav1.TypeMeta{Kind: "AdmissionReview", APIVersion: "admission.k8s.io/v1"},
			Request: &admissionv1.AdmissionRequest{
				UID:       types.UID("fuzz-uid"),
				Operation: admissionv1.Operation(operation),
				Namespace: namespace,
				Object:    runtime.RawExtension{Raw: objRaw},
			},
		}
		body, err := json.Marshal(ar)
		if err != nil {
			t.Fatalf("failed to marshal AdmissionReview: %v", err)
		}

		req := httptest.NewRequest(http.MethodPost, "/dataexport-validate", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("unexpected status code: %d, body: %s", rr.Code, rr.Body.String())
		}

		var resp admissionv1.AdmissionReview
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("handler returned a body that is not a valid AdmissionReview: %v", err)
		}
		if resp.Response == nil {
			t.Fatal("handler returned an AdmissionReview with no response")
		}

		// The security boundary, checked on every input rather than only on the seeds: an exact
		// group+kind match on the cluster-scoped VolumeSnapshotContent must never be admitted on CREATE.
		// Only CREATE is examined because that is the only operation the validator inspects.
		if admissionv1.Operation(operation) == admissionv1.Create &&
			group == dev1alpha1.GroupSnapshotStorage && kind == dev1alpha1.KindVolumeSnapshotContent {
			if resp.Response.Allowed {
				t.Fatalf("a bare cluster-scoped %s was admitted (group %q, name %q)",
					dev1alpha1.KindVolumeSnapshotContent, group, name)
			}
		}
	})
}
