# Patches

Applied to the `d8-63742164-vsc-only` source ref, based on
`kubernetes-csi/external-snapshotter` v8.5.0, during build of the
`csi-snapshotter` sidecar.

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
