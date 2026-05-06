# Patches

Applied to `kubernetes-sigs/vsphere-csi-driver` v3.0.3 during build of
the `vsphere-syncer` binary.

## 001-go-mod.patch

Bumps `go` directive to 1.23 (with toolchain 1.24.2) and refreshes
direct/indirect dependency versions in `go.mod` / `go.sum` to pull in
security fixes and to make the module buildable with the toolchain
declared in our base images.
