# Patches

Applied to `kubernetes-csi/external-snapshotter` v8.5.0 in glob order —
`000-vsc-only-mode.patch` must stay first: `003-volumesnapshot-dataimport-fork.patch`
was generated on top of it and overlaps in `pkg/common-controller/snapshot_controller.go`
(the historical build order was fork-branch content first, then the numbered patches).

## 000-vsc-only-mode.patch

Deckhouse fork: VSC-only snapshot content mode (`pkg/vscmode` + sidecar/common
controller wiring). Lets the csi-snapshotter sidecar create and delete physical
snapshots for `VolumeSnapshotContent` objects with a completely EMPTY
`spec.volumeSnapshotRef` (no bound `VolumeSnapshot`) — the VolumeCaptureRequest
flow relies on this. vsc-only is the default mode (`SNAPSHOT_CONTROLLER_VSC_MODE`
unset); legacy per-content behavior is preserved for bound (wired-ref) contents.
Touches `pkg/` only (no `client/`, `vendor/`, `go.mod`).

Provenance: byte-identical to `git diff v8.5.0..d8-63742164-vsc-only` (commit
`d648e7e94` "Add VSC-only snapshot content mode") in the fox mirror
`deckhouse/3p/kubernetes-csi/external-snapshotter`; the build used to clone that
branch directly. Keep this file in sync with the identically named patch in
`images/csi-external-snapshotter/patches/v8.5.0/` — same content, both builds.

## 001-fix-cve.patch

Fix CVE

## 002-fix-cve-otel-sdk.patch

Fix CVE-2026-39883 in `go.opentelemetry.io/otel/sdk` by upgrading
OpenTelemetry-Go modules from v1.40.0 to v1.43.0. The vulnerability is
a PATH hijacking flaw on BSD/Solaris caused by the `kenv` command not
using an absolute path.

## 003-volumesnapshot-dataimport-fork.patch

Deckhouse fork of the CSI `VolumeSnapshot` API for the state-snapshotter
import flow. Generated against v8.5.0 + `000-vsc-only-mode.patch` — must keep
applying on top of that pair (see the ordering note above).

- Adds `spec.source.import` (an empty marker object, third mutually-exclusive
  source) and extends the CEL one-of to allow an empty source (restore intent)
  or exactly one of `persistentVolumeClaimName` / `volumeSnapshotContentName` /
  `import`; once present, `import` cannot be removed. The marker carries no
  DataImport name — the owning `DataImport` is resolved by reverse-lookup
  (`DataImport.spec.targetRef`), mirroring the unified `spec.source.import: {}`
  marker used by every state-snapshotter snapshot kind.
- Adds `status.boundSnapshotContentName` (points at the cluster-scoped
  state-snapshotter `SnapshotContent`, alongside legacy
  `boundVolumeSnapshotContentName`) plus `status.data` — a self-contained data
  binding (`source` + `artifact` + volume metadata: `volumeMode` / `fsType` /
  `accessModes` / `storageClassName` / `size`) whose JSON wire shape is
  byte-identical to the state-snapshotter `SnapshotContent.status.data` and to
  the domain data leaves, so d8 resolves the captured-volume descriptor from the
  namespaced `VolumeSnapshot` alone (no cluster-scoped `SnapshotContent` read).
  Forking the Go types + deepcopy is enough: `updateSnapshotStatus` does
  read -> `DeepCopy()` -> `UpdateStatus`, so the field is preserved without
  controller logic changes.
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
