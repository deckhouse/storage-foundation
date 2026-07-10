# Patches

Applied to `kubernetes-csi/external-snapshotter` v8.5.0 during build of
the `csi-snapshotter` sidecar. Applied in glob order — `000-vsc-only-mode.patch`
must stay first (the historical build order was fork-branch content first, then
CVE patches).

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
`images/snapshot-controller/patches/` — same content, both builds.

## 001-fix-cve.patch

Fix CVE

## 002-fix-cve-otel-sdk.patch

Fix CVE-2026-39883 in `go.opentelemetry.io/otel/sdk` by upgrading
OpenTelemetry-Go modules from v1.40.0 to v1.43.0. The vulnerability is
a PATH hijacking flaw on BSD/Solaris caused by the `kenv` command not
using an absolute path.

## 003-fix-cve-golang-x.patch

Bumps `golang.org/x/net` -> `v0.55.0`, `golang.org/x/crypto` -> `v0.52.0`
and `golang.org/x/sys` -> `v0.45.0` in `go.mod` / `go.sum` to fix the
`golang.org/x/net/html`, `golang.org/x/crypto/ssh` and
`golang.org/x/sys/windows` CVEs reported by Trivy on the
snapshot-controller / csi-snapshotter binaries. The build re-runs
`go mod vendor`, so the patch only touches `go.mod` / `go.sum`.
