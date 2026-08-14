# Patches

Applied to `kubernetes-csi/external-snapshotter` v8.5.0 in glob order —
`000-vsc-only-mode.patch` must stay first: `003-volumesnapshot-dataimport-fork.patch`
was generated on top of it and overlaps in `pkg/common-controller/snapshot_controller.go`
(the historical build order was fork-branch content first, then the numbered patches).

## Verifying an edit locally

Editing a patch is verifiable without a build: the whole chain applies to a throwaway
copy of the upstream tag in about a minute. Take the version from `oss.yaml` (entry
`id: snapshot-controller`); the upstream repository is public
(<https://github.com/kubernetes-csi/external-snapshotter>), so a fresh shallow clone of the
tag is the simplest source. If you reuse an existing local checkout instead, export the tag
into a temp dir with `git archive` (then `git init` there) — never `git worktree`, and never
switch branches in that checkout, because work in progress may be sitting in it. Then replay
the chain the way `werf.inc.yaml` does (cwd = upstream root, plain `git apply`, glob order),
checking each patch against the state the previous ones produced:

```bash
tmp=$(mktemp -d)
git clone -q --depth 1 --branch v8.5.0 https://github.com/kubernetes-csi/external-snapshotter.git "$tmp"
cd "$tmp"
for p in <this-dir>/*.patch; do
  git apply --check "$p" && git apply "$p" && echo "ok $p" || { echo "FAILED $p"; break; }
done
```

After editing the body of a patch, two kinds of metadata go stale and must be recomputed:
the hunk headers (`@@ -a,b +c,d @@` counts, plus the new-side start of every later hunk in
the same file) and the `index <pre>..<post>` blob hashes (`git hash-object` on the produced
file; the pre-image hash is a free check that the computation is right). Recounting headers
by hand catches most mistakes but not all — for them `git apply --check` is the ground
truth.

The recomputed post-image hash has no automatic gate. Nothing in the apply path reads it: a
falsified one passes both `git apply --check` and `git apply --3way --check` with exit 0 and
identical output. It still matters — it is the half of the line an edit changes — so confirm
it the only way that works, by comparing `git hash-object` on the produced file against what
the line records. Run `git apply --3way --check` anyway, but for what it does validate: it
needs the recorded *pre*-image blob to exist, so it confirms the pre-image hash and that the
tree is the one the patch was generated against. Apply the preceding patches and `git add -A`
first so those blobs are in the temp repo. Expect `repository lacks the necessary blob` and a
fallback to direct application for the `pkg/common-controller/` files, whose recorded
pre-images predate this tree — that is expected and the command still succeeds.

To type-check the result, run `go build -mod=vendor ./...` on the tag plus
`000-vsc-only-mode.patch` and `003-volumesnapshot-dataimport-fork.patch` only. The CVE
patches bump `go.mod` without re-vendoring (the build re-runs `go mod vendor` afterwards),
so with them applied `-mod=vendor` refuses to run.

## 000-vsc-only-mode.patch

Deckhouse fork: VSC-only snapshot content mode (`pkg/vscmode` + sidecar/common
controller wiring). Lets the csi-snapshotter sidecar create and delete physical
snapshots for `VolumeSnapshotContent` objects with a completely EMPTY
`spec.volumeSnapshotRef` (no bound `VolumeSnapshot`) — the VolumeCaptureRequest
flow relies on this. vsc-only is the default mode (`SNAPSHOT_CONTROLLER_VSC_MODE`
unset); legacy per-content behavior is preserved for bound (wired-ref) contents.
Touches `pkg/` only (no `client/`, `vendor/`, `go.mod`).

This patch is the sole source of the VSC-only changes: they are maintained here as a
patch in this repo, not on any external-snapshotter fork/branch (the `3p`
external-snapshotter repo only mirrors upstream). Keep this file byte-for-byte in sync
with the identically named patch in `images/csi-external-snapshotter/patches/v8.5.0/` —
same content, both builds.

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
  binding (`sourceRef` + `artifactRef` + volume metadata: `volumeMode` / `fsType` /
  `storageClassName` / `size`) whose JSON wire shape is
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

Bumps `golang.org/x/net` -> `v0.56.0` (CVE-2026-46600) and
`golang.org/x/text` -> `v0.39.0` (CVE-2026-56852) in `go.mod` / `go.sum` to
clear the findings reported by Trivy; this also pulls the transitive
`golang.org/x/crypto` -> `v0.53.0`, `golang.org/x/sys` -> `v0.46.0`,
`golang.org/x/sync` -> `v0.21.0` and `golang.org/x/term` -> `v0.44.0` bumps on the
snapshot-controller / csi-snapshotter binaries. The build re-runs
`go mod vendor`, so the patch only touches `go.mod` / `go.sum`.

## 005-fix-cve-grpc-cel-go.patch

Bumps `google.golang.org/grpc` -> `v1.82.1` (GHSA-hrxh-6v49-42gf, xDS RBAC
and HTTP/2 vulnerabilities) and `github.com/google/cel-go` -> `v0.29.0`
(GHSA-gcjh-h69q-9w9g, JSON private fields exposed via NativeTypes) to clear
the findings reported by Trivy on the built binary. Generated by cloning the
pinned upstream version, applying the earlier patches, then `go get` and
`go mod tidy`; touches only `go.mod` / `go.sum` (the build re-runs
`go mod download`/`go mod vendor`).
