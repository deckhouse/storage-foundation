# Patches

Applied to `kubernetes-csi/external-snapshotter` v8.5.0 during build of
the `csi-snapshotter` sidecar.

## 001-fix-cve.patch

Fix CVE

## 002-fix-cve-otel-sdk.patch

Fix CVE-2026-39883 in `go.opentelemetry.io/otel/sdk` by upgrading
OpenTelemetry-Go modules from v1.40.0 to v1.43.0. The vulnerability is
a PATH hijacking flaw on BSD/Solaris caused by the `kenv` command not
using an absolute path.

## 003-vsc-only-model.patch

Enable a VSC-only snapshot model in the `csi-snapshotter` sidecar.

`VolumeSnapshotContent` (VSC) objects may now be created and reconciled
without a corresponding `VolumeSnapshot` (VS). Internal Deckhouse
controllers (e.g. storage-foundation VCR/VRR) can drive CSI snapshot
and restore operations using service-level APIs without creating
`VolumeSnapshot` resources in user namespaces.

The change is backward-compatible: VSCs that still carry
`spec.volumeSnapshotRef` continue to follow the legacy
`VolumeSnapshot ↔ VolumeSnapshotContent` workflow.

Highlights of the sidecar-side changes:

- snapshot identity is derived from `VolumeSnapshotContent.UID` when
  `volumeSnapshotRef.UID` is empty;
- `CreateSnapshot` / `DeleteSnapshot` work for VSC-only objects;
- the VSC `DeletionTimestamp` + `Status.SnapshotHandle` drive cleanup
  when no `VolumeSnapshot` is present.

The patch is shared with `images/snapshot-controller/patches/` so both
binaries (`snapshot-controller` and `csi-snapshotter`) are built from
identical source.
