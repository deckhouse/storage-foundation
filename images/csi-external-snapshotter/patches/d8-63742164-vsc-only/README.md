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
