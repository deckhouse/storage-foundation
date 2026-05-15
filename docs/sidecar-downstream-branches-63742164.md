# Sidecar Downstream Branches for 63742164

This note records the manual migration path for the sidecar changes that came
from `kneumoin-initial`. Cursor must not create or push branches to the 3p
mirrors; the mirror branches are created manually by a maintainer with the right
access and workflow context.

## Current build model

The `main` build model must stay in place:

- `oss.yaml` selects a source ref/version.
- `werf.inc.yaml` clones the source from the configured 3p mirror through
  `SOURCE_REPO`.
- Only small, stable downstream fixes should remain in `images/*/patches`.

Do not return to building sidecars from local fork source directories under
`images/*`.

The old local fork sources are intentionally not kept in the active
`storage-foundation` tree. They are still recoverable from `kneumoin-initial`
and from the merge history and should be moved into 3p mirror branches instead.

## snapshot-controller

Upstream: `https://github.com/kubernetes-csi/external-snapshotter`

Internal 3p mirror:
`https://fox.flant.com/deckhouse/3p/kubernetes-csi/external-snapshotter`

Create a downstream branch manually in that mirror. Example branch name:
`d8-63742164-vsc-only`.

The branch should be based on the source currently used by `oss.yaml`
(`snapshot-controller` `v8.5.0`) plus the existing stable CVE patch baseline if
the mirror workflow expects CVE changes in the source branch.

Move the VSC-only changes from the old local fork / reference diff into that
branch:

- `pkg/vscmode/config.go`
- `pkg/vscmode/vsc_only.go`
- the rest of `pkg/vscmode`
- `SNAPSHOT_CONTROLLER_VSC_MODE`
- default mode `vsc-only` when the env var is empty
- sidecar/common controller logic that allows `VolumeSnapshotContent` without
  `spec.volumeSnapshotRef`
- tests that prove legacy mode still works and VSC-only creates CSI snapshots

After the mirror branch exists and has been validated, replace the
`snapshot-controller` ref in `oss.yaml`, for example:

```yaml
id: snapshot-controller
version: d8-63742164-vsc-only
```

The local file `images/snapshot-controller/patches/003-vsc-only-mode.patch` is
not part of the target build model and was removed from the active
`storage-foundation` diff. Use the old fork / branch history only as reference
material when preparing the 3p mirror branch.

Local preparation example:

```bash
git clone https://fox.flant.com/deckhouse/3p/kubernetes-csi/external-snapshotter /tmp/external-snapshotter-63742164
cd /tmp/external-snapshotter-63742164
git checkout -b d8-63742164-vsc-only v8.5.0

# Copy or cherry-pick the downstream files from storage-foundation's old branch.
# Example source:
#   git --git-dir=/path/to/storage-foundation/.git show origin/kneumoin-initial:images/snapshot-controller/pkg/vscmode/config.go
```

To inspect the downstream delta without creating a remote branch:

```bash
git -C /path/to/storage-foundation diff --no-index \
  /tmp/external-snapshotter-63742164 \
  /path/to/storage-foundation/images/snapshot-controller
```

## external-provisioner

Upstream: `https://github.com/kubernetes-csi/external-provisioner`

Internal 3p mirror must be confirmed before changing active refs. Expected
mirror shape, if it follows the same convention:
`https://fox.flant.com/deckhouse/3p/kubernetes-csi/external-provisioner`

`oss.yaml` currently contains two source lines:

- `v5.3.0` for Kubernetes `1.20`
- `v6.2.0` for Kubernetes `1.34`

Before moving VRR into the sidecar source, confirm whether both lines are used by
the module. If both are used, prepare two downstream branches, for example:

- `d8-63742164-vrr-v5.3.0`
- `d8-63742164-vrr-v6.2.0`

Move the VRR changes from the old local fork / reference diff into the mirror
branch or branches:

- support for `VolumeRestoreRequest`
- restore operation controller
- restore parameters and secret handling
- RBAC needed by the sidecar
- registration and informer startup inside `cmd/csi-provisioner`
- `go.mod` integration with `github.com/deckhouse/storage-foundation/api`

If the sidecar imports `github.com/deckhouse/storage-foundation/api`, the
`werf.inc.yaml` build integration also needs a deliberate solution for making
that module available during `go mod download`. Do not hide this as an
unreviewed large patch.

The local files below are not part of the target build model and were removed
from the active `storage-foundation` diff:

- `images/csi-external-provisioner/patches/v5.3.0/002-volume-restore-request.patch`
- `images/csi-external-provisioner/patches/v5.3.0/003-use-local-storage-foundation-api.patch`
- `images/csi-external-provisioner/patches/v6.2.0/002-volume-restore-request.patch`
- `images/csi-external-provisioner/patches/v6.2.0/003-use-local-storage-foundation-api.patch`

After the mirror branches exist and are validated, update `oss.yaml` to point to
the chosen downstream refs.

Local preparation example:

```bash
git clone https://fox.flant.com/deckhouse/3p/kubernetes-csi/external-provisioner /tmp/external-provisioner-63742164
cd /tmp/external-provisioner-63742164
git checkout -b d8-63742164-vrr-v5.3.0 v5.3.0

# Copy or cherry-pick the downstream VRR files from storage-foundation's old branch.
# Example source:
#   git --git-dir=/path/to/storage-foundation/.git show origin/kneumoin-initial:images/csi-external-provisioner/external-provisioner/pkg/controller/vrr_handler.go
```

Repeat the same process for `v6.2.0` only if the Kubernetes `1.34` line is
actually built and supported by this module.

## Required compatibility checks

The `VolumeSnapshotContent` CRD must accept VSC-only objects. In
`crds/snapshot.storage.k8s.io_volumesnapshotcontents.yaml`,
`spec.required` must not include `volumeSnapshotRef`.

Manual check:

```bash
grep -n "volumeSnapshotRef" crds/snapshot.storage.k8s.io_volumesnapshotcontents.yaml
grep -n "required:" -A10 crds/snapshot.storage.k8s.io_volumesnapshotcontents.yaml
```

If `volumeSnapshotRef` is required, `VolumeCaptureRequest` will create a
`VolumeSnapshotContent` that the Kubernetes API server rejects.

## Functionality that must not be lost

`VolumeCaptureRequest`:

- API type
- controller registration
- ObjectKeeper creation
- direct `VolumeSnapshotContent` creation
- `status.dataRef` pointing to the ready `VolumeSnapshotContent`

VSC-only snapshot-controller:

- VSC without `spec.volumeSnapshotRef`
- CSI snapshot creation from such VSC
- `SNAPSHOT_CONTROLLER_VSC_MODE`
- default `vsc-only` behavior if that remains the downstream decision

`VolumeRestoreRequest`:

- API type
- restore flow
- restore controller logic
- restore/secrets handling
- RBAC for the component that owns the restore operation

## Final patch model

After the downstream mirror branches are stable and validated, create normal
small patches from the mirror branch diff against the upstream tag/ref. Do not
review or maintain a 3000+ line functional patch in `storage-foundation` as the
primary development workflow.
