# Patches

## 001-fix-cve.patch

Fix CVE

## 002-fix-cve-otel-sdk.patch

Fix CVE-2026-39883 in `go.opentelemetry.io/otel/sdk` by upgrading
OpenTelemetry-Go modules from v1.40.0 to v1.43.0. The vulnerability is
a PATH hijacking flaw on BSD/Solaris caused by the `kenv` command not
using an absolute path.

## 003-vsc-only-model.patch

Enable a VSC-only snapshot model in snapshot-controller and the
`csi-snapshotter` sidecar.

`VolumeSnapshotContent` (VSC) objects may now be created and reconciled
without a corresponding `VolumeSnapshot` (VS). Internal Deckhouse
controllers (e.g. storage-foundation VCR/VRR) can drive CSI snapshot
and restore operations using service-level APIs without creating
`VolumeSnapshot` resources in user namespaces.

The change is backward-compatible: VSCs that still carry
`spec.volumeSnapshotRef` continue to follow the legacy
`VolumeSnapshot ↔ VolumeSnapshotContent` workflow.

Highlights:

- `common-controller` accepts VSCs without `volumeSnapshotRef`, adds
  the protection finalizer and skips the legacy
  `VolumeSnapshot` ↔ `VolumeSnapshotContent` synchronization for them.
- `sidecar-controller`:
  - derives the CSI snapshot identity from `VolumeSnapshotContent.UID`
    when `volumeSnapshotRef.UID` is empty;
  - performs `CreateSnapshot` / `DeleteSnapshot` for VSC-only objects;
  - relies on the VSC `DeletionTimestamp` + `Status.SnapshotHandle` to
    drive cleanup when no `VolumeSnapshot` is present.
- The CRD validation in `client/config/crd/...` is relaxed accordingly;
  the Deckhouse-shipped CRD in `crds/` is updated to match.

The patch also ships unit tests in `pkg/sidecar-controller/vsc_only_test.go`
covering VSC-only and legacy paths.
