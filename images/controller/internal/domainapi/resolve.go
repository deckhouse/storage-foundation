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
	"errors"
	"fmt"
	"net/http"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	storagev1alpha1 "github.com/deckhouse/state-snapshotter/api/storage/v1alpha1"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/restoreapi"
	"github.com/deckhouse/state-snapshotter/pkg/snapshotsdk/transform"
	sfsnapv1 "github.com/deckhouse/storage-foundation/api/snapshot/v1"
)

// volumeSnapshotIdentity is everything the three subresources need to know about the addressed
// VolumeSnapshot: who it is (for the anti-spoofing handshake), whether it may be written to, and what it
// is bound to.
type volumeSnapshotIdentity struct {
	namespace string
	name      string
	uid       types.UID

	// importMode is spec.mode == Import. The upload facade refuses anything else, so an upload can never
	// overwrite the manifests of a live capture.
	importMode bool
	// deleting reports a set metadata.deletionTimestamp.
	deleting bool
	// readyToUse is the CSI status.readyToUse of the snapshot. Restore gates on it; the raw download does
	// not, matching the per-CR download of the snapshot core.
	readyToUse bool
	// boundContentName is status.boundSnapshotContentName: the cluster-scoped SnapshotContent that holds
	// this node's manifests and data binding. Empty until the binder has bound it.
	boundContentName string
	// boundVSCName is the CSI status.boundVolumeSnapshotContentName: the durable VolumeSnapshotContent
	// holding the volume data. It is what makes this snapshot the visibility leaf a restored PVC can be
	// pointed at.
	boundVSCName string
}

// apiVersion is the CSI group/version of the addressed object, as the back-reference on a SnapshotContent
// spells it.
func volumeSnapshotAPIVersion() string {
	return sfsnapv1.SchemeGroupVersion.String()
}

// loadVolumeSnapshotIdentity reads the addressed VolumeSnapshot fresh (uncached) and projects the fields
// the subresources act on.
//
// It reads the object unstructured on purpose. This module also carries a typed, Deckhouse-owned view of
// the same GVK, but that view models only the domain-protocol fields its reconciler writes; the fields
// read here are CSI-native status (readyToUse, boundVolumeSnapshotContentName) and the extension
// boundSnapshotContentName, which that type deliberately leaves out so its merge patches can never
// serialize them. Reading unstructured keeps this path independent of that choice, and keeps the two
// registrations of one GVK from colliding in a scheme.
func loadVolumeSnapshotIdentity(ctx context.Context, reader client.Reader, namespace, name string) (volumeSnapshotIdentity, error) {
	vs := &unstructured.Unstructured{}
	vs.SetGroupVersionKind(sfsnapv1.GroupVersionKind)
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, vs); err != nil {
		return volumeSnapshotIdentity{}, err
	}

	mode, _, err := unstructured.NestedString(vs.Object, "spec", "mode")
	if err != nil {
		return volumeSnapshotIdentity{}, fmt.Errorf("%w: VolumeSnapshot %s/%s has an unreadable spec.mode: %v",
			transform.ErrContractViolation, namespace, name, err)
	}
	readyToUse, _, err := unstructured.NestedBool(vs.Object, "status", "readyToUse")
	if err != nil {
		return volumeSnapshotIdentity{}, fmt.Errorf("%w: VolumeSnapshot %s/%s has an unreadable status.readyToUse: %v",
			transform.ErrContractViolation, namespace, name, err)
	}
	boundContentName, _, err := unstructured.NestedString(vs.Object, "status", "boundSnapshotContentName")
	if err != nil {
		return volumeSnapshotIdentity{}, fmt.Errorf("%w: VolumeSnapshot %s/%s has an unreadable status.boundSnapshotContentName: %v",
			transform.ErrContractViolation, namespace, name, err)
	}
	boundVSCName, _, err := unstructured.NestedString(vs.Object, "status", "boundVolumeSnapshotContentName")
	if err != nil {
		return volumeSnapshotIdentity{}, fmt.Errorf("%w: VolumeSnapshot %s/%s has an unreadable status.boundVolumeSnapshotContentName: %v",
			transform.ErrContractViolation, namespace, name, err)
	}

	return volumeSnapshotIdentity{
		namespace:        namespace,
		name:             name,
		uid:              vs.GetUID(),
		importMode:       mode == sfsnapv1.VolumeSnapshotModeImport,
		deleting:         vs.GetDeletionTimestamp() != nil,
		readyToUse:       readyToUse,
		boundContentName: boundContentName,
		boundVSCName:     boundVSCName,
	}, nil
}

// The two resolve outcomes whose reported STATUS differs between the three subresources. They are
// sentinels rather than finished errors because the resolve itself is shared while the answer is not:
// resolveStatusFor holds the whole per-subresource table in one place.
var (
	// errContentNotBound is "the binder has not caught up yet": the addressed snapshot names no content.
	errContentNotBound = errors.New("no SnapshotContent bound yet")
	// errContentAbsent is a name that points at nothing: the snapshot names a content that does not exist.
	errContentAbsent = errors.New("the bound SnapshotContent does not exist")
)

// resolveStatusFor turns any shared resolve failure into the status ONE subresource answers with. Two of
// those failures are answered differently per subresource; everything else is classified once, below.
//
// The three columns are deliberately different, and the differences are the contract clients already
// depend on, not an oversight:
//
//   - a read of a volume node whose durable content is not bound yet is reported as a client-side
//     addressing error (400) by the raw download, and as not-ready (409) by the restore compiler, which
//     treats an unpublished node handle as a node that is not usable yet;
//   - an upload reports the same condition with the retryable reason clients wait on, because an import
//     legitimately races the binder;
//   - a name pointing at a content that does not exist is a plain 404 on the paths that address that
//     content directly, while restore reports it as a broken model state (409) — it got there by
//     following a reference it had already accepted.
//
// Guarded cell by cell by TestResolveStatuses_MatchThePerSubresourceContract; changing a cell here
// changes what an existing client does with the answer.
func resolveStatusFor(subresource string, err error) error {
	switch {
	case errors.Is(err, errContentNotBound):
		switch subresource {
		case SubresourceManifestsDownload:
			return newFacadeStatusError(http.StatusBadRequest, "BadRequest", err.Error())
		case SubresourceManifestsUpload:
			return newFacadeStatusError(http.StatusConflict, ReasonImportContentNotBound, err.Error())
		default:
			return newFacadeStatusError(http.StatusConflict, "Conflict", err.Error())
		}
	case errors.Is(err, errContentAbsent):
		switch subresource {
		case SubresourceManifestsRestore:
			return newFacadeStatusError(http.StatusConflict, "Conflict", err.Error())
		default:
			return newFacadeStatusError(http.StatusNotFound, "NotFound", err.Error())
		}
	}

	// Everything else a resolve can fail with means the same on every subresource — the snapshot is not
	// there, the content is not this snapshot's, a field is unreadable — so it is classified once, by the
	// shared restore mapping. Attaching the status here rather than leaving the bare error is what makes
	// this function total: a caller can answer from its result without a mapping of its own, which is how
	// the read paths and the write path stay in step.
	status, reason := restoreapi.HTTPStatusForError(err)
	return newFacadeStatusError(status, reason, err.Error())
}

// loadAddressedVolumeSnapshot reads the addressed snapshot and states a failure in the SHARED vocabulary,
// so that every subresource classifies it through resolveStatusFor rather than each one deciding for
// itself. Without this, a snapshot that cannot be read answers one way on a read and another on a write —
// a permission failure reported as 403 to a reader and as an internal error to an importer.
func loadAddressedVolumeSnapshot(ctx context.Context, reader client.Reader, namespace, name string) (volumeSnapshotIdentity, error) {
	id, err := loadVolumeSnapshotIdentity(ctx, reader, namespace, name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return volumeSnapshotIdentity{}, fmt.Errorf("%w: VolumeSnapshot %s/%s", transform.ErrNotFound, namespace, name)
		}
		return volumeSnapshotIdentity{}, fmt.Errorf("get VolumeSnapshot %s/%s: %w", namespace, name, err)
	}
	return id, nil
}

// resolveBoundContent turns the addressed VolumeSnapshot into the SnapshotContent that backs it, after the
// fail-closed anti-spoofing handshake. It is the single resolve contract of all three subresources: none
// of them ever trusts a content this snapshot did not actually get bound to.
//
// It enforces no readiness: the raw download is defined to return whatever was captured, and restore adds
// its own gates on top (see RestoreService.ManifestsWithDataRestoration).
func resolveBoundContent(ctx context.Context, reader client.Reader, namespace, name string) (volumeSnapshotIdentity, *storagev1alpha1.SnapshotContent, error) {
	id, err := loadAddressedVolumeSnapshot(ctx, reader, namespace, name)
	if err != nil {
		return volumeSnapshotIdentity{}, nil, err
	}
	content, err := resolveBoundContentFor(ctx, reader, id)
	if err != nil {
		return volumeSnapshotIdentity{}, nil, err
	}
	return id, content, nil
}

// resolveBoundContentFor is resolveBoundContent for a snapshot that has already been read. The upload
// path reads the object for its own gates first, and re-reading it here would answer one request from two
// different observations of the same object.
func resolveBoundContentFor(ctx context.Context, reader client.Reader, id volumeSnapshotIdentity) (*storagev1alpha1.SnapshotContent, error) {
	if id.boundContentName == "" {
		// Not a complaint about the request: the binder creates and binds the content independently, so
		// this is "not yet" and the client is expected to retry. How it is reported is per-subresource
		// (resolveStatusFor).
		return nil, fmt.Errorf("%w for VolumeSnapshot %s/%s (waiting for the binder)", errContentNotBound, id.namespace, id.name)
	}

	content := &storagev1alpha1.SnapshotContent{}
	if err := reader.Get(ctx, types.NamespacedName{Name: id.boundContentName}, content); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: SnapshotContent %q named by VolumeSnapshot %s/%s",
				errContentAbsent, id.boundContentName, id.namespace, id.name)
		}
		return nil, fmt.Errorf("get SnapshotContent %q: %w", id.boundContentName, err)
	}
	if msg := snapshotContentRefMismatch(content.Spec.SnapshotRef, id); msg != "" {
		// status.boundSnapshotContentName is writable by whoever can write the snapshot's status, so the
		// name alone proves nothing; only the reverse reference does. A mismatch can never start working,
		// so it is a permanent refusal (403) rather than something to retry.
		//
		// All three subresources answer 403 here, restore included, and that uniformity is deliberate: a
		// refusal to read a content that does not claim this snapshot is the same refusal whichever
		// endpoint asked, and it is the answer the published contract gives for it. It is not a
		// conflict — nothing about it clears with time — so it is not reported as one.
		return nil, apierrors.NewForbidden(
			storagev1alpha1.SchemeGroupVersion.WithResource("snapshotcontents").GroupResource(),
			id.boundContentName,
			fmt.Errorf("anti-spoofing back-ref check failed for content bound by VolumeSnapshot %s/%s: %s", id.namespace, id.name, msg),
		)
	}
	return content, nil
}

// snapshotContentRefMismatch returns a non-empty reason when a content's spec.snapshotRef does not
// identify the addressed VolumeSnapshot, and "" when the handshake passes.
//
// Full identity match on apiVersion/kind/namespace/name. The uid is compared only when the reference
// carries one: a content may legitimately be re-pointed at a re-created snapshot handle, and a reference
// written before the handle's uid was known is not evidence of spoofing. When both sides have a uid they
// must agree.
func snapshotContentRefMismatch(ref *storagev1alpha1.SnapshotSubjectRef, id volumeSnapshotIdentity) string {
	if ref == nil {
		return "SnapshotContent.spec.snapshotRef is absent (fail closed)"
	}
	if ref.APIVersion != volumeSnapshotAPIVersion() {
		return fmt.Sprintf("snapshotRef apiVersion %q does not match %q (fail closed)", ref.APIVersion, volumeSnapshotAPIVersion())
	}
	if ref.Kind != KindVolumeSnapshot {
		return fmt.Sprintf("snapshotRef kind %q does not match %q (fail closed)", ref.Kind, KindVolumeSnapshot)
	}
	if ref.Namespace != id.namespace {
		return fmt.Sprintf("snapshotRef namespace %q does not match %q (fail closed)", ref.Namespace, id.namespace)
	}
	if ref.Name != id.name {
		return fmt.Sprintf("snapshotRef name %q does not match %q (fail closed)", ref.Name, id.name)
	}
	if ref.UID != "" && id.uid != "" && ref.UID != id.uid {
		return fmt.Sprintf("snapshotRef uid %q does not match %q (fail closed)", ref.UID, id.uid)
	}
	return ""
}
