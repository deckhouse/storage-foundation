# Patches

Applied to `kubernetes-csi/external-provisioner` v6.3.0 (both the k8s
1.20 and 1.34 tracks) during build of the `csi-provisioner` binary.

## Delivery model (current)

Both `csi-external-provisioner` entries in `oss.yaml` point at the
upstream tag `v6.3.0`, and these patches are applied on top:

```
id: csi-external-provisioner
versions:
  - condition: { k8s: ["1.20"] }
    version: v6.3.0
  - condition: { k8s: ["1.34"] }
    version: v6.3.0
```

`werf.inc.yaml` clones the tag from `SOURCE_REPO`
(`.../kubernetes-csi/external-provisioner.git`) and, because a patch dir
whose path contains the `version` string exists (`patches/v6.3.0/`),
applies every `*.patch` there, in name order: `002-vrr-executor.patch`,
`003-fix-cve.patch`, `004-snapshot-finalizer-merge-patch.patch`. The `002-` prefix is kept on purpose: `werf.inc.yaml`
detects the executor patch by the `002-vrr-executor` substring to mount
`api/`, and `images/data-manager-controller` tests read the patch by path.

Why patches and not a direct fork-branch clone: werf caches the
`src-artifact` stage by the digest of its shell instructions. A
`git clone --branch <branch>` of a moving branch is **not**
content-addressed, so once the stage is cached werf never re-clones (a
bumped branch HEAD is invisible to the digest). The patch dir is mounted
via `git: add: patches/<version>` with `stageDependencies: install:
'**/*'`, so editing any patch file reliably busts the cache. This is the
same pattern `csi-external-snapshotter` uses.

`002-vrr-executor.patch` is the source of truth for the executor; it is a
plain diff against `v6.3.0` (excluding `vendor/`) applied directly to the
tag at build time. There is no long-lived fork branch in the build path —
the patch was developed on the throwaway branch `d8-63742164-vrr` and
ported by hand to v6.3.0 (see below). To re-derive the diff from such a
branch:

```
git -C <external-provisioner-checkout> diff v6.3.0 <branch> \
    -- . ':(exclude)vendor' > 002-vrr-executor.patch
```

API dependency: the executor imports
`github.com/deckhouse/storage-foundation/api`. The executor reads the
**honest-refs** VRR API — `spec.sourceRef` + `spec.pvcTemplate`
(`metadata.name` + `spec.storageClassName`/`volumeMode`/`accessModes`)
with `fsType` at the spec root — and never writes status. The dependency
is resolved with `replace github.com/deckhouse/storage-foundation/api =>
../api`, i.e. against the `api/` directory of this very repository, so
the executor always compiles against the API schema of the same commit.
The `go` directive stays at upstream's `1.26.0`: the pruned module graph
of `api/` needs nothing newer.

`werf.inc.yaml` mounts `api/` into the build stage next to `/src` (`git:
add: /api` with `stageDependencies: install: '**/*'`), which is why the
relative path resolves both in the image (`/src/../api` = `/api`) and in
a local checkout where `api/` sits beside the provisioner tree.

Verified: applying `002` + `003` to a clean `v6.3.0` checkout with `api/`
placed alongside it reproduces the build pipeline —
`go mod download && go mod vendor && make build` under the default
read-only module mode, plus `go test ./pkg/controller/` (VRR tests) —
all green, and neither `go.mod` nor `go.sum` is modified during the
build.

Why not a pseudo-version: the previous pin
`v0.0.0-20260706134706-2c525506f13c` pointed at a commit of a PR branch
that squash-merge left unreachable. Once the werf stage cache was
invalidated (a base-images bump), `go mod download` could no longer
resolve it — neither over the Go module proxy nor by direct fetch — and
the image stopped building. A directory `replace` cannot rot that way.

## 002-vrr-executor.patch

Adds the low-level **VolumeRestoreRequest (VRR) executor** to the
`csi-provisioner` binary. This is the executor half of the VRR feature;
the VRR lifecycle controller (CR lifecycle, ObjectKeeper, TTL,
finalizers, status/conditions) lives in `images/controller` of this
module. The executor only:

- watches `storage-foundation.deckhouse.io/v1alpha1 VolumeRestoreRequest` and
  enqueues work on a rate-limited workqueue;
- filters by CSI driver (via the target `StorageClass.provisioner`);
- validates the restore source (`VolumeSnapshotContent` ready-to-use, or
  an existing `PersistentVolume`);
- restores a source only after the storage-foundation controller has set
  the `SourceVerified=True` condition for the current generation of the
  request, whatever namespace the source names in
  `spec.volumeSnapshotRef` or `spec.claimRef`: the controller alone checks
  that the source belongs to the namespace of the request (or, in the
  namespace of the module, to the namespace the request was made for);
- re-reads the request from the API server right before CSI
  `CreateVolume` and stops if it has meanwhile become terminal, lost
  `SourceVerified`, changed generation or been recreated;
- calls CSI `CreateVolume` (from `volumeSnapshotHandle` or `volumeHandle`);
- creates the target `PersistentVolume` + `PersistentVolumeClaim`
  (explicit binding, no `dataSourceRef`, no temporary `VolumeSnapshot`);
- emits Kubernetes events; is idempotent / retry-safe.

The PV carries the same StorageClass secret references as one upstream
`prepareProvision` creates: `controllerPublish`, `nodeStage`,
`nodePublish`, `controllerExpand` and `nodeExpand` in `spec.csi`, the
provisioner secret in the deletion annotations and the controller-modify
secret in the modify annotations. Templates resolve against the PV name and
the target PVC name and namespace; the PVC is created without annotations,
so a `${pvc.annotations[...]}` template does not resolve. A template that
does not resolve is reported as `VRRInvalidStorageClass` and not retried,
and no volume is created. `spec.csi` is immutable, so a PV created without
these references stays broken: ceph-csi refuses `NodeStageVolume` with
`stage secrets cannot be nil or empty`, and the claim is Bound but cannot
be mounted. When the executor reuses such a PV from an earlier attempt, it
reports `VRRPVMissingSecretRefs` on the VolumeRestoreRequest with every
missing field.

The target PVC is created by the ServiceAccount of the CSI driver, which the
grant webhooks of multitenancy-manager do not exempt, so in a project limited
by a ClusterResourceGrantPolicy the claim goes through the same checks as a
user's one. When the API server refuses it (Forbidden or Invalid, e.g. the class
is no longer granted to the project), the executor reports
`VRRPVCCreateFailed` on the VolumeRestoreRequest with the refusal and retries.
When the admission chain admits the claim with a different class (the
`storageclasses-pvc` reference uses `defaulting: Coerce` and replaces a class
the project lost with the project default), the PV keeps the requested class
and binds anyway, because it is pre-bound by `claimRef`; the executor reports
`VRRStorageClassReplaced` with both names.

The executor **never writes VRR status** — that is owned by the
storage-foundation VRR controller, which observes the bound target PVC
and sets `Ready`. No `volumerestorerequests/status` RBAC is required for
the provisioner ServiceAccount.

Touches: `cmd/csi-provisioner/csi-provisioner.go` (wiring; executor
workers are started only after the main informer factory cache — which
backs the StorageClass lister the executor reads — has synced, so a
freshly-started worker never sees an existing StorageClass as NotFound),
`pkg/controller/vrr_handler.go` (+`_test.go`, executor), `go.mod` /
`go.sum` (api `replace`), and
`deploy/kubernetes/rbac.yaml` (reference RBAC). It imports
`github.com/deckhouse/storage-foundation/api/v1alpha1` (VRR/VCR API
types); the module is not fetched from a proxy at all — it is resolved
from the `api/` directory mounted next to the source tree (see below).

This patch is the source of truth for the executor code (developed on the
throwaway branch `d8-63742164-vrr`, then diffed against upstream v6.2.0
excluding `vendor/`).

Port to v6.3.0: `pkg/controller/vrr_handler.go` (+`_test.go`) and the
`deploy/kubernetes/rbac.yaml` hunk carry over unchanged. In
`cmd/csi-provisioner/csi-provisioner.go` upstream v6.3.0 added the
snapshot-orphan-sweep controller right where the executor is constructed and
moved `topologyInformer.RunWorker` next to the factory start, so the two
conflicting hunks keep both sides: the upstream block first, then the executor
construction / `vrrInformerFactory.Start`. The upstream module path is now
`github.com/kubernetes-csi/external-provisioner/v6`; the executor only uses
package-local identifiers, so no import changes. `go.mod` / `go.sum` were
regenerated with `go mod edit` (require + `replace => ../api`) and
`go mod tidy`. Verified on a clean `v6.3.0` checkout with `002` + `003`
applied and `api/` placed beside it: `rm -rf vendor && go mod download &&
go mod vendor && make build`-equivalent build, `go vet`, and
`go test ./pkg/controller/` (all 19 VRR `TestReconcile_*` tests) are green.
`TestProvisionFromPVC` is flaky on the pristine v6.3.0 tag as well.

### API dependency — resolved via a directory replace

The executor needs the **honest-refs** VRR API: `spec.sourceRef` +
`spec.pvcTemplate` (with `metadata.name` and
`spec.storageClassName`/`volumeMode`/`accessModes`), `fsType` at the spec
root, and `status.pvcRef`. `pvcTemplate` uses the storage-foundation
string-alias mirror types (`PersistentVolumeMode`,
`PersistentVolumeAccessMode`), so the executor converts them to core `v1`
before building CSI capabilities / the PV+PVC (see `convertAccessModes`,
`vrrVolumeMode`, `vrrStorageClassName`, `vrrTargetPVCName/Namespace`).

The dependency is declared as:

```go.mod
require github.com/deckhouse/storage-foundation/api v0.0.0

replace github.com/deckhouse/storage-foundation/api => ../api
```

so the API always matches the commit being built and no version has to be
maintained by hand. `go.sum` carries no entry for the module itself (a
directory replace needs none), only the hashes its dependency graph pulls
in. Because `api/` requires `golang.org/x/net v0.56.0` and
`golang.org/x/text v0.39.0`, minimal version selection raises the
provisioner's own indirect `golang.org/x/*` requirements (`x/crypto`
`v0.53.0`, `x/mod` `v0.37.0`, `x/net` `v0.56.0`, `x/text` `v0.39.0`, ...)
already in `002`; `003-fix-cve.patch` then moves the remaining flagged
modules.

The `go` directive is left at upstream's `1.26.0`.

### RBAC for the provisioner ServiceAccount

In Deckhouse the patched provisioner runs as a sidecar inside each CSI
driver controller Pod (deployed by the driver module, not by this
module), so the executor RBAC must be granted to that driver's
ServiceAccount. The executor needs (NO `volumerestorerequests/status`):

- `volumerestorerequests`: `get`, `list`, `watch`;
- `persistentvolumes`: `get`, `list`, `watch`, `create`, `update`, `patch`;
- `persistentvolumeclaims`: `get`, `list`, `watch`, `create`, `update`, `patch`;
- `volumesnapshotcontents`: `get`, `list`, `watch`;
- `storageclasses`: `get`, `list`, `watch`;
- `secrets`: `get`, `list` (only if the driver uses provisioner secrets);
- `events`: `create`, `patch`, `update`.

A ready-to-apply additive manifest and helper live at
`hack/vrr-rbac-manual.yaml` and `hack/apply-vrr-rbac.sh`. Most of these
verbs already exist in a driver's provisioner ClusterRole for normal
provisioning; the only strictly net-new permission is
`volumerestorerequests` `get/list/watch`. FOLLOW-UP: fold this into the
shared CSI-driver helm lib so every driver gets it automatically.

## 003-fix-cve.patch

Bumps the modules Trivy still flags after `002` (upstream v6.3.0 plus the
`api/` graph) to their minimal fixed releases:

- `github.com/google/cel-go` `v0.27.0 -> v0.29.0` (GHSA-gcjh-h69q-9w9g)
- `golang.org/x/crypto` `v0.53.0 -> v0.56.0` (CVE-2026-56854,
  CVE-2026-56855, CVE-2026-78662)
- `golang.org/x/mod` `v0.37.0 -> v0.40.0` (CVE-2026-56864, CVE-2026-56865)
- `google.golang.org/grpc` `v1.81.1 -> v1.83.2` (GHSA-hrxh-6v49-42gf,
  CVE-2026-84303, CVE-2026-84304, CVE-2026-84445)
- `go.opentelemetry.io/otel/sdk`, `.../exporters/otlp/otlptrace`,
  `.../exporters/otlp/otlptrace/otlptracegrpc` `v1.44.0 -> v1.45.0`
  (CVE-2026-81870)
- `golang.org/x/net` `-> v0.60.0` (CVE-2026-78669, CVE-2026-78659,
  CVE-2026-78660, CVE-2026-78663, CVE-2026-97032), the version `api/` requires

Minimal version selection moves the rest of the set along (`golang.org/x/text`
-> `v0.42.0`, `golang.org/x/sys` / `sync` /
`term` / `tools`, the remaining `go.opentelemetry.io/otel*` modules,
`go.opentelemetry.io/proto/otlp`, `google.golang.org/genproto/googleapis/*`,
`github.com/go-logr/logr`). The remaining `golang.org/x/crypto` advisory
GO-2026-5932 has no fixed release and is covered by
`../../known_vulnerabilities.vex`. Generated by cloning `v6.3.0` with `api/`
beside it, applying `002`, then `go get` and `go mod tidy`; touches only
`go.mod` / `go.sum` (the build re-runs `go mod download` / `go mod vendor`).

## 004-snapshot-finalizer-merge-patch.patch

Makes every write of the `provisioner.storage.kubernetes.io/volumesnapshot-as-source-protection`
finalizer a JSON merge patch of `metadata.finalizers` only, instead of a
full `Update` of the VolumeSnapshot.

Why: upstream `setSnapshotFinalizer` / `removeSnapshotFinalizer`
(`pkg/controller/controller.go`) and the snapshot-orphan-sweep
(`SnapshotFinalizerController.removeSnapshotFinalizer`,
`pkg/controller/snapshot_finalizer_controller.go`, every 5 minutes by
default) decode the VolumeSnapshot with the typed external-snapshotter
client and `Update` it whole. That type knows nothing about `spec.mode`,
which the VolumeSnapshot CRD of this module adds (`Capture | Import`,
`default: Capture`, `self == oldSelf`). The Update drops `mode`, the API
server defaults it back to `Capture`, and for a VolumeSnapshot in
`mode: Import` the transition rule rejects the write:

```
VolumeSnapshot.snapshot.storage.k8s.io "<name>" is invalid: spec.mode: Invalid value: "string": mode is immutable
```

The provisioner then reports `ProvisioningFailed` and a claim restored
from an Import-mode VolumeSnapshot never provisions. It surfaced once
lib-helm 1.72.25 granted the provisioner `update` on volumesnapshots;
before that the Update was Forbidden, which upstream logs and ignores.
Any other `spec` field the CRD adds would be lost the same way; the
`status` extensions (`captureState`, `childrenSnapshotRefs`, `data`,
`sourceRef`, ...) are behind the status subresource and are not written
by either call.

The patch carries `metadata.resourceVersion`, so it stays conditional
exactly like the Update it replaces: a concurrent change of the object
yields a Conflict instead of a finalizer list that silently drops
another writer's finalizer. The rest of upstream behaviour is unchanged:
the already-present / not-present checks, NotFound handling, and
Forbidden logged and ignored (the log text now names the `patch` verb).

RBAC: the provisioner now needs `patch` (instead of `update`) on
`volumesnapshots` for the protection to work; without it the write is
Forbidden and provisioning continues without snapshot protection, as
upstream does. `deploy/kubernetes/rbac.yaml` is changed accordingly.

Touches `pkg/controller/controller.go` (`patchSnapshotFinalizers`, used by
`setSnapshotFinalizer` and `removeSnapshotFinalizer`),
`pkg/controller/snapshot_finalizer_controller.go` (sweep),
`pkg/controller/controller_test.go` (`TestProvisionFromSnapshot` serves
`patch` and fails on any `update` of volumesnapshots),
`pkg/controller/snapshot_finalizer_patch_test.go` (the patch type and
body, the already-present check, Forbidden, the sweep) and
`deploy/kubernetes/rbac.yaml`. Generated as `git diff` against a
`v6.3.0` checkout with `002` + `003` applied, excluding `vendor/`.

## 900-fix-cve-go-deps.patch

Generated by `fix_go_vulns.py` (deckhouse/storage/common) against `v6.3.0` on top of the other patches in this directory; regenerated on every run, do not edit by hand. When the upstream version changes, delete this patch or rerun the script (`fix_go_vulns.py --only <module>`): the old one will not apply to the new tag. Raises Go dependencies with known vulnerabilities to the lowest fixed versions:

- `go.mod`: `github.com/stretchr/testify` v1.11.1 → v1.12.1
- `go.mod`: `go.opentelemetry.io/otel` v1.45.0 → v1.47.0
- `go.mod`: `go.opentelemetry.io/otel/metric` v1.45.0 → v1.47.0
- `go.mod`: `go.opentelemetry.io/otel/trace` v1.45.0 → v1.47.0 (CVE-2026-128085)
- `go.mod`: `go.yaml.in/yaml/v3` v3.0.4 → v3.0.5
- `go.mod`: `golang.org/x/crypto` v0.57.0 → v0.58.1-0.20261009181718-2f1f834330b2 (GO-2026-5932)
- `go.mod`: `golang.org/x/net` v0.60.0 → v0.62.0 (CVE-2026-78661, CVE-2026-97045)
- `go.mod`: `golang.org/x/sync` v0.23.0 → v0.24.0
- `go.mod`: `golang.org/x/sys` v0.48.0 → v0.49.0
- `go.mod`: `golang.org/x/term` v0.46.0 → v0.47.0
- `go.mod`: `golang.org/x/text` v0.42.0 → v0.43.0
- `go.mod`: `golang.org/x/tools` v0.49.0 → v0.51.0
