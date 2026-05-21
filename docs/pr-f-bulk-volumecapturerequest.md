# PR-F: Bulk VolumeCaptureRequest (`targets[]`, `status.dataRefs[]`)

**Status:** **PR-F-1 (API only) ✅ done** — bulk controller loop is **PR-F-2** (in progress / not started).  
**Blocks:** state-snapshotter N5 **PR-4** (publish `SnapshotContent.status.dataRefs[]` from VCR).  
**Aligned with:** state-snapshotter [`volume-node-dual-capture.md`](../../state-snapshotter/docs/state-snapshotter-rework/design/volume-node-dual-capture.md), spec §3.9, implementation-plan §2.4.5 **PR-F**.

## PR-F-1 (API only) — done

| Deliverable | State |
|-------------|--------|
| `VolumeCaptureTarget`, `VolumeDataArtifactRef`, `VolumeDataBinding` | ✅ `api/v1alpha1/volumecapturerequest_types.go` |
| `spec.targets[]` (`listMapKey=uid`), `status.dataRefs[]` (`listMapKey=targetUID`) | ✅ |
| Removed `spec.persistentVolumeClaimRef`, `status.dataRef` | ✅ Go types + CRD |
| CEL: Snapshot mode requires `size(spec.targets) > 0` | ✅ `+kubebuilder:validation:XValidation` → CRD `x-kubernetes-validations` |
| CRD drift: `spec.volumeSnapshotClassName` | ✅ removed from generated CRD (not in Go types; controller uses SC annotation) |
| Codegen | ✅ `zz_generated.deepcopy.go`, `crds/storage.deckhouse.io_volumecapturerequests.yaml` |
| API tests | ✅ `volumecapturerequest_prf1_test.go` (JSON round-trip, map-list CRD schema, no singular fields) |
| Controller behavior | **unchanged semantics** — `singleVolumeCaptureTarget` shim (exactly one target); compile fixes + `dataRefs[]` status shape only |
| Duplicate `targets[].uid` at apiserver | **TODO PR-F-2** — map-list keys; no envtest in `api/` module |

**PR-F-2 next:** per-target controller loop, aggregate `Ready`, incremental `dataRefs[]`, `TargetsPending` reason, envtest for 2 PVC / duplicate uid.

## Goal

Extend **VolumeCaptureRequest (VCR)** so one request captures **0..N PVC targets** for a **single logical snapshot node** (one bound `SnapshotContent` in state-snapshotter). Symmetric to bulk **ManifestCaptureRequest** (`spec.targets[]` → MCP).

**MUST NOT:** fan out to **N VCRs per PVC** on the state-snapshotter side. **One VCR per logical `SnapshotContent`.**

## Current state — after PR-F-1 (API)

| Area | Today |
|------|--------|
| **Spec** | `spec.mode`, `spec.targets[]` (`VolumeCaptureTarget`: apiVersion/kind/namespace/name/uid) |
| **Status** | `status.dataRefs[]` (`VolumeDataBinding`: targetUID, target, artifact), `status.conditions[]`, `status.completionTimestamp` |
| **Conditions** | Single type `Ready` (`api/v1alpha1/conditions.go`); reasons include `Completed`, `NotFound`, `SnapshotCreationFailed`, … — **no** aggregate-pending reason yet |
| **Snapshot flow** | `processSnapshotMode`: **single-target shim** — validate one PVC from `spec.targets[0]` → … → set `dataRefs[]` + `Ready=True` (bulk loop: PR-F-2) |
| **ObjectKeeper** | Name `retainer-{vscName}` (derived from VSC, not VCR); `FollowObject` → this VCR; **controller owner** of VSC |
| **Detach flow** | Same single `persistentVolumeClaimRef`; `dataRef` → `PersistentVolume` |
| **TTL / cleanup** | `cleanupArtifactsForVCR` iterates `status.dataRefs[].artifact` |
| **Tests** | Ginkgo + cleanup unit tests assert `status.dataRefs[]` |
| **CRD drift** | ✅ resolved in PR-F-1 — no `volumeSnapshotClassName` / singular refs in CRD |

**state-snapshotter today:** expects **one** `volumeCaptureRequestName` per snapshot node; **no** VCR publish code yet (**PR-4** blocked). Binding shape in `SnapshotContent` uses `targetUID` + full `target` + `artifact` (`apiVersion`/`kind`/`name`) — **align PR-F `dataRefs[]` to that**, not to foundation `ObjectReference`.

## Target API

### `spec.targets[]`

Replace (or deprecate without bridge) singular `persistentVolumeClaimRef` with a **list of volume targets** for this capture job.

```yaml
spec:
  mode: Snapshot  # unchanged: Snapshot | Detach
  targets:
    - apiVersion: v1
      kind: PersistentVolumeClaim
      namespace: demo
      name: data-a
      uid: "<pvc-uid-a>"   # required for map key / dedup
    - apiVersion: v1
      kind: PersistentVolumeClaim
      namespace: demo
      name: data-b
      uid: "<pvc-uid-b>"
```

**Rules:**

- `targets[]` is the **only** input for which PVCs to capture (no implicit fanout).
- **0 targets** — valid only if product allows empty data capture; otherwise validation rejects at admission or controller sets terminal failure (align with MCR empty-target policy).
- Each entry **MUST** identify a PVC (`apiVersion`, `kind`, `name`, `namespace`; `uid` **MUST** be set for running captures).
- **MUST NOT** duplicate the same `uid` in one VCR.

### `status.dataRefs[]`

Publish **per-target durable artifacts**, not execution intermediates.

```yaml
status:
  dataRefs:
    - targetUID: "<pvc-uid-a>"
      target:
        apiVersion: v1
        kind: PersistentVolumeClaim
        namespace: demo
        name: data-a
        uid: "<pvc-uid-a>"
      artifact:
        apiVersion: snapshot.storage.k8s.io/v1
        kind: VolumeSnapshotContent
        name: snapcontent-aaa
    - targetUID: "<pvc-uid-b>"
      target: { ... }
      artifact:
        apiVersion: snapshot.storage.k8s.io/v1
        kind: VolumeSnapshotContent
        name: snapcontent-bbb
  conditions:
    - type: Ready
      status: "True"   # only when ALL targets have ready artifacts
```

**OpenAPI (kubebuilder):**

- `listType=map`, `listMapKey=targetUID`
- `targetUID` **Required**, **MinLength=1**
- `target` and `artifact` **Required** on each entry
- `artifact`: `apiVersion`, `kind`, `name` **Required**, **MinLength=1**; cluster-scoped durable kinds only in controller validation

**Remove** singular `status.dataRef` and `spec.persistentVolumeClaimRef` in the same PR (**no** dual-write, **no** legacy bridge, **no** CEL oneOf bridge).

**Do not reuse** `ObjectReference` for `targets[]` / `dataRefs[]` entries — it lacks `apiVersion` and `uid`. Add dedicated types (names TBD, mirror state-snapshotter `SnapshotSubjectRef` / `SnapshotDataArtifactRef`):

- `VolumeCaptureTarget` — `apiVersion`, `kind`, `name`, `namespace`, `uid` (required for map key)
- `VolumeDataArtifactRef` — `apiVersion`, `kind`, `name` (cluster-scoped durable artifact)
- `VolumeDataBinding` — `targetUID`, `target`, `artifact`

### Ready semantics

- **`Ready=True`** only when **every** `spec.targets[]` entry has a corresponding `status.dataRefs[]` binding whose **artifact exists and is ready** (for VSC: `status.readyToUse=true`).
- While any target is pending: `Ready=False`, reason/message **MUST** mention pending count and/or target identifiers (mirror state-snapshotter PR-2 style).
- Partial progress **MAY** appear in `status.dataRefs[]` as artifacts complete (incremental map entries).
- Terminal failure on one target **MUST** fail the whole VCR (no silent subset Ready).

## Controller behavior

| Area | Requirement |
|------|-------------|
| Cardinality | **One VCR** per logical snapshot scope; state-snapshotter passes **all** owned PVCs in `spec.targets[]` |
| VSC creation | **One VSC per target** (internal detail); **not** one VSC for multiple PVCs |
| Ownership | Keep existing ObjectKeeper / VSC ownership model; VCR remains job + TTL, not owner of durable artifacts long-term |
| Published refs | `status.dataRefs[].artifact` **MUST** reference **VolumeSnapshotContent** (or equivalent durable object), **MUST NOT** reference VolumeSnapshot, VolumeCaptureRequest, or other requests |
| Fanout | **MUST NOT** implement “create child VCR per PVC” as the primary model |
| Idempotency | Re-reconcile **MUST** not duplicate VSCs for the same `targetUID` |
| ObjectKeeper | **One per VCR** (not per VSC): e.g. `retainer-vcr-{vcrUID}`; all VSCs owned by that keeper |
| VSC naming | Deterministic per target, e.g. `snapshot-{vcrUID}-{targetUID-suffix}` (must not collide within one VCR) |
| Detach mode | **PR-F v1: Snapshot bulk only.** Detach keeps separate follow-up or single-target until product defines multi-PV detach |

## Consumer contract (state-snapshotter PR-4)

After **PR-F** is deployed:

1. Snapshot-domain controller creates **one VCR** per bound `SnapshotContent` with `spec.targets[]` = all PVCs owned by that node.
2. Waits for `VCR.status.conditions[Ready=True]`.
3. Copies `VCR.status.dataRefs[]` → `SnapshotContent.status.dataRefs[]` (after artifact handoff / ownerRef rules).
4. Deletes VCR only after publish chain complete (symmetric to MCR).

**PR-4 MUST NOT** ship until this API and controller behavior exist in storage-foundation.

## Implementation checklist (PR-F)

- [x] **PR-F-1** API types: `VolumeCaptureTarget`, `VolumeDataBinding`; `spec.targets[]`, `status.dataRefs[]`
- [x] **PR-F-1** CRD + codegen; remove `persistentVolumeClaimRef` / `dataRef` / `volumeSnapshotClassName` from active contract
- [x] **PR-F-1** CEL: empty `spec.targets` rejected for Snapshot mode; OpenAPI `minLength=1` on uid / targetUID / artifact fields
- [ ] **PR-F-2** Admission/runtime: duplicate `targets[].uid` rejected at reconcile (apiserver map-list where enforced)
- [ ] **PR-F-2** Controller: iterate `spec.targets[]`; create/track VSC per target; append `status.dataRefs[]`
- [ ] Ready aggregation: all targets → `Ready=True`
- [ ] Unit tests: 0/1/2 targets; one pending; one failed; duplicate uid rejected
- [ ] Integration: 2 PVC → 1 VCR → 2 VSC → 2 `dataRefs[]` → `Ready=True`
- [ ] Document breaking change for any external VCR consumers (singular ref removal)

## Out of scope (PR-F)

- state-snapshotter `SnapshotContent` publish / SCC ( **PR-4** )
- MCR bulk changes (already symmetric in state-snapshotter)
- Restore tree ( **PR-3** in state-snapshotter — done )
- Per-PVC VCR naming fanout in state-snapshotter

## Implementation plan (pre-code)

### 1. API type changes

- Add `VolumeCaptureTarget`, `VolumeDataArtifactRef`, `VolumeDataBinding` in `api/v1alpha1/`.
- `VolumeCaptureRequestSpec`: `targets []VolumeCaptureTarget` with `listType=map`, `listMapKey=uid` (field name `uid` or `targetUID` — pick one and match state-snapshotter **PR-4** copy path).
- `VolumeCaptureRequestStatus`: `dataRefs []VolumeDataBinding` with same map semantics.
- **Delete** `PersistentVolumeClaimRef`, `DataRef` from Go types (not deprecated markers).
- Add `ConditionReasonTargetsPending` (and optionally `ConditionReasonDataArtifactInvalid`) in `conditions.go`.
- Reconcile CRD: drop orphan `volumeSnapshotClassName` from OpenAPI **or** add to Go types — pick one (controller today uses SC annotation only).

### 2. CRD / codegen

- Regenerate `crds/storage.deckhouse.io_volumecapturerequests.yaml` + `zz_generated.deepcopy.go`.
- OpenAPI: required `targets` for Snapshot when product requires ≥1 PVC; reject duplicate map keys; `artifact` minLength on apiVersion/kind/name.
- **Breaking CRD change** — bump docs / release note.

### 3. Controller changes

- Refactor `processSnapshotMode` into **per-target loop** (or target state map):
  - Validate all `spec.targets[]` up front (RBAC Get PVC, bound, CSI handle, VSC class).
  - `ensureObjectKeeper(ctx, retainer-vcr-{vcrUID}, vcr)` once per VCR.
  - For each target: ensure VSC exists (deterministic name), watch `readyToUse`/error, upsert `status.dataRefs[]` entry when VSC ready.
- **Do not** finalize `Ready=True` until `len(ready targets) == len(spec.targets)`.
- On any target terminal CSI error: `Ready=False`, fail **whole** VCR (optional: record failed target’s VSC in `dataRefs[]` for debug — **must not** set `Ready=True`).
- Status patches: incremental `dataRefs[]` updates while pending (separate from `finalizeVCR`).
- `cleanupArtifactsForVCR`: iterate all `dataRefs[].artifact` (VSC + Detach PV kinds).
- **Detach:** out of bulk loop in PR-F v1 unless explicitly scoped (keep current single-ref path behind feature gate or separate PR).

### 4. Status / conditions

- Keep single `Ready` condition type.
- While targets pending: `Ready=False`, `Reason=TargetsPending`, message `N of M targets ready` + pending targetUIDs/names.
- Success: `Ready=True`, `Reason=Completed` (unchanged).
- Failure: existing reasons (`SnapshotCreationFailed`, `NotFound`, …) with target context in message.
- `CompletionTimestamp` only in `finalizeVCR` (terminal states only) — unchanged.

### 5. Migration / singular field removal

- **No** dual-write to `dataRef` / `persistentVolumeClaimRef`.
- **No** apiserver conversion webhook unless cluster has existing VCR objects that must be migrated (if none in prod, hard cut).
- Update all controller paths: `markFailedSnapshot`, TTL cleanup, tests — **only** `dataRefs[]`.
- Grep repo + downstream for `DataRef`, `PersistentVolumeClaimRef` on VCR.

### 6. Tests

| Layer | Cases |
|-------|--------|
| API | JSON round-trip `targets[]` / `dataRefs[]`; duplicate `targetUID` rejected (if validation in types) |
| Unit/controller | 0 targets → fail; 1 target → 1 VSC + 1 dataRef + Ready; 2 targets one pending → Ready=False TargetsPending; 2 ready → Ready=True; CSI error on one → whole VCR Failed; idempotent re-reconcile |
| Cleanup | `cleanupArtifactsForVCR` with 2 dataRefs (orphan vs managed) |
| Integration | Envtest 2 PVCs, 1 VCR, 2 VSC names, 2 artifacts in status before Ready |

Extend `volumecapturerequest_controller_test.go`; add table tests for binding map updates.

### 7. Break risks (consumers)

| Consumer | Risk |
|----------|------|
| **state-snapshotter PR-4** | Expected consumer; blocked until PR-F ships — design for `dataRefs[]` copy |
| **In-repo tests** | `volumecapturerequest_controller_test.go`, `volumecapturerequest_cleanup_test.go` — rewrite assertions |
| **Manual / scripted VCR YAML** | Any manifest with `persistentVolumeClaimRef` / `dataRef` breaks |
| **Operators reading `status.dataRef`** | Must switch to `dataRefs[]` |
| **ObjectKeeper naming change** | New VCRs use `retainer-vcr-*`; old `retainer-snapshot-*` orphans only if in-flight during upgrade |
| **CRD-only `volumeSnapshotClassName`** | Clients setting it today have no effect in Go controller — clarify in changelog |

## References

- state-snapshotter `SnapshotDataBinding` / `SnapshotContent.status.dataRefs[]` — `api/storage/v1alpha1/snapshotcontent_types.go`
- Current VCR — `api/v1alpha1/volumecapturerequest_types.go`, `images/controller/internal/controllers/volumecapturerequest_controller.go`, `volumecapturerequest_controller_test.go`
