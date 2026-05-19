# 63742164 Sidecar Migration

## external-snapshotter

Local 3p mirror branch prepared:

- repository: `fox.flant.com/deckhouse/3p/kubernetes-csi/external-snapshotter`
- branch: `d8-63742164-vsc-only`
- base: upstream `v8.5.0`
- local commit: `d648e7e94 Add VSC-only snapshot content mode`

The branch contains the VSC-only snapshot-controller changes from
`kneumoin-initial`:

- `pkg/vscmode/*`
- `SNAPSHOT_CONTROLLER_VSC_MODE`
- default mode `vsc-only` when the env var is empty
- common/sidecar controller handling for `VolumeSnapshotContent` without
  `spec.volumeSnapshotRef`
- VSC-only and legacy compatibility tests

Nothing was pushed. A maintainer still needs to push the local branch manually:

```bash
cd /Users/kneumoin/GolandProjects/external-snapshotter
git push origin d8-63742164-vsc-only
```

## storage-foundation

`oss.yaml` now points both external-snapshotter based images at the downstream
branch:

- `csi-external-snapshotter`: `d8-63742164-vsc-only`
- `snapshot-controller`: `d8-63742164-vsc-only`

The active build model remains `oss.yaml -> external-snapshotter ref`.
`images/snapshot-controller/patches/003-vsc-only-mode.patch` is not part of the
production build source.

The existing small CVE patch baseline for `csi-external-snapshotter` was copied
from `patches/v8.5.0` to `patches/d8-63742164-vsc-only` so changing the source
ref does not silently disable those patches. Its `werf.inc.yaml` applies patches
sequentially because the CVE patches depend on each other.

## Out of scope for this task

- VRR external-provisioner downstream branch/patch migration.
- VolumeRestoreRequest sidecar integration.
- Restore flow validation.

## Manual Follow-Up

- Push `d8-63742164-vsc-only` to the 3p mirror after review.
- Confirm the build environment can clone the pushed branch from `SOURCE_REPO`.
- Keep `VolumeSnapshotContent.spec.volumeSnapshotRef` optional in the installed
  CRD; VSC-only objects depend on this.
