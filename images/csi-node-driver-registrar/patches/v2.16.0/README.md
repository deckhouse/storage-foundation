# Patches

Applied to `kubernetes-csi/node-driver-registrar` v2.16.0 during build of the `csi-node-driver-registrar` image.

## 001-fix-cve.patch

Bumps the following dependencies in `go.mod`/`go.sum` to versions that
fix CVEs reported by Trivy:

- `google.golang.org/grpc` -> `v1.80.0` (fixes CVE-2026-33186)
- `go.opentelemetry.io/otel` -> `v1.43.0` (fixes CVE-2026-29181, CVE-2026-24051, CVE-2026-39883)
- `go.opentelemetry.io/otel/{sdk,metric,trace,exporters/otlp/...}` -> `v1.43.0`
- `golang.org/x/crypto` -> `v0.49.0` (fixes CVE-2025-47914)

The patch only touches `go.mod` and `go.sum`; `go mod download` during
the build picks up the upgraded versions.
