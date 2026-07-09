# Patches

## 001-fix-cve.patch

Fix CVE

## 002-fix-cve-otel-sdk.patch

Fix CVE-2026-39883 in `go.opentelemetry.io/otel/sdk` by upgrading
OpenTelemetry-Go modules from v1.40.0 to v1.43.0. The vulnerability is
a PATH hijacking flaw on BSD/Solaris caused by the `kenv` command not
using an absolute path.

## 003-volumesnapshot-dataimport-fork.patch

Deckhouse fork of the CSI `VolumeSnapshot` API for the state-snapshotter
import flow. Must keep applying to the build branch `d8-63742164-vsc-only`.

- Adds `spec.source.import` (an empty marker object, third mutually-exclusive
  source) and extends the CEL one-of to allow an empty source (restore intent)
  or exactly one of `persistentVolumeClaimName` / `volumeSnapshotContentName` /
  `import`; once present, `import` cannot be removed. The marker carries no
  DataImport name — the owning `DataImport` is resolved by reverse-lookup
  (`DataImport.spec.targetRef`), mirroring the unified `spec.source.import: {}`
  marker used by every state-snapshotter snapshot kind.
- Adds `status.boundSnapshotContentName` (points at the cluster-scoped
  state-snapshotter `SnapshotContent`, alongside legacy
  `boundVolumeSnapshotContentName`) plus `status.storageClassName`,
  `status.size` and `status.volumeMode` — mirrored volume metadata for d8
  export/consumption. Forking the Go types + deepcopy is enough:
  `updateSnapshotStatus` does read -> `DeepCopy()` -> `UpdateStatus`, so the
  fields are preserved without controller logic changes.
- Behavioral skip: `syncSnapshot` and `syncSnapshotByKey` (before snapshot-class
  resolution) skip any `VolumeSnapshot` whose `spec.source.import` is set
  — those objects are owned/bound by the state-snapshotter common controller.

The patch edits both `./client/...` (the authoritative copy via `go.mod`
`replace => ./client`) and `vendor/...`. The werf build does `rm -rf vendor` and
compiles `./client`, so the `vendor/` hunks are NOT consumed by the image build;
they are kept only so local `-mod=vendor` builds of external-snapshotter stay
consistent. The deployed CRD is hand-maintained in `crds/` (this build does not
run controller-gen), so the CEL markers here and the CRD must be kept in sync by
hand.

## 004-fix-cve-golang-x.patch

Bumps `golang.org/x/net` -> `v0.55.0`, `golang.org/x/crypto` -> `v0.52.0`
and `golang.org/x/sys` -> `v0.45.0` in `go.mod` / `go.sum` to fix the
`golang.org/x/net/html`, `golang.org/x/crypto/ssh` and
`golang.org/x/sys/windows` CVEs reported by Trivy on the
snapshot-controller / csi-snapshotter binaries. The build re-runs
`go mod vendor`, so the patch only touches `go.mod` / `go.sum`.
