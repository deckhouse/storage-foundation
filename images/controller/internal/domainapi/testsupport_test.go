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

package domainapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	sfsnapv1 "github.com/deckhouse/storage-foundation/api/snapshot/v1"
)

const (
	testNamespace = "app-ns"
	testVSName    = "pvc-snap"
	testContent   = "content-pvc-snap"
	testVSCName   = "snapcontent-durable"
	testPVCName   = "data"
	testPVCUID    = "pvc-uid-1"
)

// fakeReader answers the two reads the facade makes: the addressed VolumeSnapshot (unstructured, because
// the served fields are CSI-native status the module's typed view deliberately omits) and the
// cluster-scoped SnapshotContent. It is hand-written rather than a generic fake so a VolumeSnapshot in a
// test keeps EXACTLY the fields it was given: a fake that round-trips through the module's typed view
// would silently drop status.readyToUse and status.boundSnapshotContentName, which is what these tests
// are about.
type fakeReader struct {
	volumeSnapshots map[string]*unstructured.Unstructured
	contents        map[string]*storagev1alpha1.SnapshotContent
}

func newFakeReader() *fakeReader {
	return &fakeReader{
		volumeSnapshots: map[string]*unstructured.Unstructured{},
		contents:        map[string]*storagev1alpha1.SnapshotContent{},
	}
}

func (f *fakeReader) withVolumeSnapshot(vs *unstructured.Unstructured) *fakeReader {
	f.volumeSnapshots[vs.GetNamespace()+"/"+vs.GetName()] = vs
	return f
}

func (f *fakeReader) withContent(content *storagev1alpha1.SnapshotContent) *fakeReader {
	f.contents[content.Name] = content
	return f
}

func (f *fakeReader) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	switch target := obj.(type) {
	case *unstructured.Unstructured:
		stored, ok := f.volumeSnapshots[key.Namespace+"/"+key.Name]
		if !ok {
			return apierrors.NewNotFound(
				schema.GroupResource{Group: sfsnapv1.GroupName, Resource: "volumesnapshots"}, key.Name)
		}
		stored.DeepCopyInto(target)
		return nil
	case *storagev1alpha1.SnapshotContent:
		stored, ok := f.contents[key.Name]
		if !ok {
			return apierrors.NewNotFound(
				storagev1alpha1.SchemeGroupVersion.WithResource("snapshotcontents").GroupResource(), key.Name)
		}
		stored.DeepCopyInto(target)
		return nil
	default:
		return fmt.Errorf("fakeReader: unexpected object type %T", obj)
	}
}

func (f *fakeReader) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return fmt.Errorf("fakeReader: List is not part of the facade's contract")
}

// fakeCore stands in for the snapshot core's content layer.
type fakeCore struct {
	manifests []byte
	fetchErr  error

	uploadCode int
	uploadBody []byte
	uploadErr  error

	// uploadedFor and uploadedManifests record what the facade forwarded, so a test can assert that it
	// forwards manifests ONLY and addresses the content it resolved.
	uploadedFor       string
	uploadedManifests json.RawMessage
	uploadCalls       int
}

func (c *fakeCore) ContentManifestsRaw(_ context.Context, contentName string) ([]byte, error) {
	if c.fetchErr != nil {
		return nil, c.fetchErr
	}
	c.uploadedFor = contentName
	return c.manifests, nil
}

func (c *fakeCore) UploadContentManifests(_ context.Context, contentName string, manifests json.RawMessage) (int, []byte, error) {
	c.uploadCalls++
	c.uploadedFor = contentName
	c.uploadedManifests = manifests
	if c.uploadErr != nil {
		return 0, nil, c.uploadErr
	}
	return c.uploadCode, c.uploadBody, nil
}

// volumeSnapshotOption mutates a test VolumeSnapshot.
type volumeSnapshotOption func(*unstructured.Unstructured)

// newVolumeSnapshot builds a VolumeSnapshot in the state a restore is meant to succeed from: ready,
// bound to a durable content and to a node content.
func newVolumeSnapshot(opts ...volumeSnapshotOption) *unstructured.Unstructured {
	vs := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": sfsnapv1.SchemeGroupVersion.String(),
		"kind":       KindVolumeSnapshot,
		"metadata": map[string]interface{}{
			"name":      testVSName,
			"namespace": testNamespace,
			"uid":       "vs-uid-1",
		},
		"spec": map[string]interface{}{
			"source": map[string]interface{}{"persistentVolumeClaimName": testPVCName},
		},
		"status": map[string]interface{}{
			"readyToUse":                     true,
			"boundVolumeSnapshotContentName": testVSCName,
			"boundSnapshotContentName":       testContent,
		},
	}}
	for _, opt := range opts {
		opt(vs)
	}
	return vs
}

func withStatusField(value interface{}, fields ...string) volumeSnapshotOption {
	return func(vs *unstructured.Unstructured) {
		path := append([]string{"status"}, fields...)
		if err := unstructured.SetNestedField(vs.Object, value, path...); err != nil {
			panic(err)
		}
	}
}

func withoutStatusField(fields ...string) volumeSnapshotOption {
	return func(vs *unstructured.Unstructured) {
		unstructured.RemoveNestedField(vs.Object, append([]string{"status"}, fields...)...)
	}
}

func withImportMode() volumeSnapshotOption {
	return func(vs *unstructured.Unstructured) {
		if err := unstructured.SetNestedField(vs.Object, sfsnapv1.VolumeSnapshotModeImport, "spec", "mode"); err != nil {
			panic(err)
		}
	}
}

func withDeletionTimestamp() volumeSnapshotOption {
	return func(vs *unstructured.Unstructured) {
		now := metav1.Now()
		vs.SetDeletionTimestamp(&now)
	}
}

// contentOption mutates a test SnapshotContent.
type contentOption func(*storagev1alpha1.SnapshotContent)

// newSnapshotContent builds the SnapshotContent a restore is meant to succeed from: Ready, with a
// checkpoint and a data binding, and with the back-reference pointing at the test VolumeSnapshot.
func newSnapshotContent(opts ...contentOption) *storagev1alpha1.SnapshotContent {
	content := &storagev1alpha1.SnapshotContent{
		ObjectMeta: metav1.ObjectMeta{Name: testContent},
		Spec: storagev1alpha1.SnapshotContentSpec{
			SnapshotRef: &storagev1alpha1.SnapshotSubjectRef{
				APIVersion: sfsnapv1.SchemeGroupVersion.String(),
				Kind:       KindVolumeSnapshot,
				Namespace:  testNamespace,
				Name:       testVSName,
				UID:        "vs-uid-1",
			},
		},
		Status: storagev1alpha1.SnapshotContentStatus{
			ManifestCheckpointName: "mcp-1",
			Data: &storagev1alpha1.SnapshotDataBinding{
				SourceRef: storagev1alpha1.SnapshotSubjectRef{
					APIVersion: "v1",
					Kind:       "PersistentVolumeClaim",
					Namespace:  testNamespace,
					Name:       testPVCName,
					UID:        testPVCUID,
				},
				ArtifactRef: storagev1alpha1.SnapshotDataArtifactRef{
					APIVersion: "snapshot.storage.k8s.io/v1",
					Kind:       "VolumeSnapshotContent",
					Name:       testVSCName,
				},
			},
			Conditions: []metav1.Condition{{
				Type:   storagev1alpha1.ConditionReady,
				Status: metav1.ConditionTrue,
				Reason: "Ready",
			}},
		},
	}
	for _, opt := range opts {
		opt(content)
	}
	return content
}

func withContentNotReady() contentOption {
	return func(c *storagev1alpha1.SnapshotContent) {
		c.Status.Conditions = []metav1.Condition{{
			Type:   storagev1alpha1.ConditionReady,
			Status: metav1.ConditionFalse,
			Reason: "Capturing",
		}}
	}
}

func withSnapshotRef(ref *storagev1alpha1.SnapshotSubjectRef) contentOption {
	return func(c *storagev1alpha1.SnapshotContent) { c.Spec.SnapshotRef = ref }
}

func withoutDataBinding() contentOption {
	return func(c *storagev1alpha1.SnapshotContent) { c.Status.Data = nil }
}

func withoutCheckpoint() contentOption {
	return func(c *storagev1alpha1.SnapshotContent) { c.Status.ManifestCheckpointName = "" }
}

// capturedPVCJSON is one captured PVC as the content layer hands it over: raw, with status and the source
// namespace and uid intact.
func capturedPVCJSON() []byte {
	return []byte(`[{
      "apiVersion": "v1",
      "kind": "PersistentVolumeClaim",
      "metadata": {
        "name": "` + testPVCName + `",
        "namespace": "` + testNamespace + `",
        "uid": "` + testPVCUID + `",
        "resourceVersion": "4711"
      },
      "spec": {"accessModes": ["ReadWriteOnce"], "resources": {"requests": {"storage": "1Gi"}}},
      "status": {"phase": "Bound"}
    }]`)
}

// newTestHandler wires a handler over the given reader and core.
func newTestHandler(reader client.Reader, core CoreContentManifests) *Handler {
	log := logr.Discard()
	return NewHandler(NewRestoreService(reader, core, log), NewUploadFacade(reader, core, log), log)
}

// serve routes one request through the handler's own mux, so the tests exercise the registered routes
// and not just the methods behind them.
func serve(h *Handler, req *http.Request) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	h.SetupRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// subresourcePath builds /apis/<group>/v1/namespaces/<ns>/volumesnapshots/<name>/<subresource>.
func subresourcePath(group, namespace, name, subresource string) string {
	return fmt.Sprintf("/apis/%s/%s/namespaces/%s/%s/%s/%s",
		group, GroupVersion, namespace, ResourceVolumeSnapshots, name, subresource)
}

// decodeStatus decodes a Kubernetes Status error response.
func decodeStatus(t *testing.T, body []byte) metav1.Status {
	t.Helper()
	var status metav1.Status
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatalf("response body is not a Status: %v (body %s)", err, body)
	}
	return status
}

// objectKey is the identity of one object in a compiled manifest array.
func objectKey(obj map[string]interface{}) string {
	u := unstructured.Unstructured{Object: obj}
	return u.GetAPIVersion() + "/" + u.GetKind() + "/" + u.GetNamespace() + "/" + u.GetName()
}
