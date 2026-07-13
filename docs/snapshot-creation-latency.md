# Snapshot-creation latency

Single, self-contained document for the snapshot-creation latency work. It answers four questions:

- **What to apply** — the validated fixes, file-by-file, as a re-application guide (not a patch / cherry-pick).
- **What was proven** — confirmed root causes, confirmed-but-secondary optimizations, and architecture/correctness
  cleanups.
- **What did not work** — rejected hypotheses (so nobody re-runs them) and the issues still open.
- **How to re-find it all after a rework** — [section 12](#12-carry-over-to-a-reworked-main--portable-bottleneck-playbook-tag-walk)
  is a code-agnostic, grep-able **tag-walk**: the durable *patterns*, anchors, and re-verify recipes to re-apply this
  whole investigation on a substantially rewritten `main` where the `file:line` anchors above no longer resolve.

Repos: `state-snapshotter` (controller + demo domain-controller) and `storage-foundation` (VCR controller).
`storage-e2e` is measurement tooling only — see [Tooling](#10-tooling).

> This is the single, authoritative latency document. Any earlier separate "fixes/re-apply" and
> "findings/classification" notes have been consolidated here and removed.

---

## 1. Purpose / scope

The investigation went from ~10 hypotheses to **3 confirmed bottlenecks**, several correct-but-secondary
optimizations, one architecture/correctness cleanup, several rejected hypotheses, and a child-graph-planning cost
(H3) that is now **closed** by Commit D.3 (H1 was disproven as a distinct problem and had been absorbed into H3).
The remaining SETS=10 tail is a **new, narrower question** — the root manifest leg (`ChildrenSnapshotReady` →
`Ready` ≈10s) — which is a separate investigation, not a child-graph fix.

Sections 4–6 are a **re-application guide**: apply by hand on a fresh `main`; section order is the recommended
application order. Sections 3, 7, 8 are the **classification** (why each change matters, what was disproven, what
remains). **Do not claim snapshot scalability is solved** — see section 8.

## 2. Current validated state

Two distinct benchmarks — **do not conflate them**:

- **Parallel same-shape snapshot burst** (N independent trees of the same shape): SET=1 ~3s, TREES=5 ~6s.
- **Namespace fan-out benchmark** (one root Snapshot over N independent standard sets): after Commit D.3,
  **SETS=10 ROOT Ready ~23–24s client-measured** (3 warm runs: 23.4 / 23.7 / 24.2s), down from ~29–30s. Still
  above target, but the bottleneck has moved (see below).

CPU is no longer the bottleneck (genericbinder mapper fix; controller mostly idle during fan-out), the
reverse-watch mapper `List` is gone, the rate limiter is fixed, polling is largely event-driven, and **child-graph
planning cost is closed** (D.3: per-child `Get`s → one `List`/GVK; worst pass 11.1s → ~0.8–1.1s). The remaining
SETS=10 tail was **latency-bound** and concentrated in the **root manifest leg**: after all children are ready
(`ChildrenSnapshotReady` ~13s) the root Snapshot took ~10s to reach `Ready`. That leg (**H4**) is now **closed** by
Commit H4.1 — the reverse-lookup wakes were dead (unstructured `List`s hit the API server with an unsupported
field selector) and the leg ran on poll backstops; routing those `List`s through the cache indexes restored
event-driven wakes, killed the `field label not supported` errors and the 89s lost-wake tail, and left the leg
stable at ~9–10s. **No open latency investigation remains and active latency work is stopped** (see the STOP
decision under H4): the residual ~24–25s wall is genuine, evenly-spread staged readiness propagation with no
single interval >50% that a safe local fix could remove. Further cuts are diminishing-return micro-optimizations
unless a new production-scale trace surfaces a fresh dominant interval.

**Post-STOP API-load / scalability track (separate from wall-clock).** After the latency STOP, an apiserver-audit
attribution of one SETS=10 tree (~1946 LIST/tree; audit policy logs `list` for the controller SA) found the LIST
load dominated by repeated full-collection lists of a few planning-input GVKs. Two safe, correctness-neutral
load cleanups were applied and validated on-cluster: **CSD planning list served from the manager cache** (removes
~208 apiserver CSD LIST/tree from child-graph planning; section 6) and **H5 pre-MCR sweep single-flight** (SETS=10
sweeps/root 2→1, SETS=20 3→1; section 8). Neither is a wall-clock fix (SETS=10 Ready stays ~17–19s, no regression;
0 restarts). A third cleanup then closed the largest single LIST hotspot: **relay reverse-lookup served from a
`childrenSnapshotRefs` field index** (the `nss-chw` relay's per-child-event full-namespace `SnapshotList` →
cached indexed lookup; `storage/snapshots` LIST/tree **~440 → ~2**, 0 field-label errors, 0 restarts, root Ready
~21s; section 8, "Relay reverse-lookup — CLOSED"). Those five closed load fixes (D.3, H4.1, CSD, H5, relay index)
are one pattern — *replace search-by-full-traversal with direct addressing (index / direct-ref / cache)*. The
remaining child-subtree enumeration load (demo VM/disk/snapshot full-lists ~250–272 LIST/tree each, ~1060/tree)
was then attributed to child-graph planning (~1 List per GVK per pass × ~272 pending-phase passes) and **both**
remaining candidates were **rejected** after read-only invariant proofs: source-skip (membership is not frozen
before `ChildrenSnapshotReady=True/Completed`) and `childSnapshotReadCache` via informer cache (the shared per-GVK
list feeds freshness-bound create/dedup, not just readiness) — see section 7. **The API-load / scalability track
is now closed**: all obvious hot LIST candidates are fixed or rejected, and the remaining child-snapshot LIST path
is freshness-bound and intentionally left on the APIReader.

## 3. Confirmed bottlenecks (real root causes, measurable effect)

| # | Root cause | Evidence / effect |
|---|---|---|
| B1 | **client-go rate limiter QPS=5 / Burst=10** on the shared manager client serialized uncached reads + status patches under a multi-tree burst, inflating one reconcile to 4–15s **regardless of `MaxConcurrentReconciles`**. | Raising to 50/100: TREES=5 **57s→6s**, TREES=1 **15s→3s**; reconcile durMs mean 4491→125ms, max 14812→847ms. The dominant tail. Fix = [FIX 1](#fix-1-do-first--raise-manager-client-qpsburst-to-50100). |
| B2 | **500ms self-requeue "poll handshakes" instead of watches** between controllers (MCR↔ManifestCheckpoint, bottom-up `ManifestsArchived` latch, root-MCR planning, VCR↔VolumeSnapshotContent). | Converting to event-driven wakes removed structural per-hop latency (e.g. archive-latch last-child→root ~12s→2.4s). Correct independent of wall-clock. Fixes = [FIX 2](#fix-2--mcr-controller-watch-manifestcheckpoint), [FIX 3](#fix-3--snapshotcontent-two-reverse-lookup-watches), [FIX 5](#fix-5--storage-foundation-vcr-watch-volumesnapshotcontent). |
| B3 | **genericbinder reverse-watch `List`+decode CPU/alloc** — three map functions each did a full unstructured `List` + JSON decode per event (O(#snapshots/#contents)). | pprof: ~69% CPU / ~84% alloc, grows with tree size. Direct-ref O(1) routing → **33.5s→29.2s**. A real scaling liability (load fix), though **not** the dominant wall-clock term at SETS=10. Fix = [FIX 8](#fix-8--genericbinder-reverse-watch-mappers-direct-ref-o1-routing). |

`MaxConcurrentReconciles=1` (implicit in several controllers) was a ceiling once B1 was fixed; raising it needs
one real correctness fix (shared `Config` mutation under concurrent reconciles) — see [FIX 6](#fix-6--concurrency-ceilings--the-one-required-correctness-fix).

**Portable conclusion:** the three durable root-cause *classes* are (B1) a **client-go rate-limit default** that
serializes work regardless of concurrency, (B2) **poll handshakes standing in for watches** between controllers,
and (B3) **`List`+decode reverse lookups on the event path**. The specific seconds (57s→6s, 33.5s→29.2s, etc.) are
workload-dependent and not portable; what is portable is the *class* of defect and its fix — `[CO-QPS]`,
`[CO-EVENT-WAKE]`, `[CO-DIRECT-REF]` in section 12.2. Grep the new tree for the class, not the number.

---

## 4. Validated fixes to re-apply

> **Status: Active carry-over guidance (mandatory / recommended).** These are validated fixes to re-apply on a
> reworked `main`; portable form is [`[CO-*]` in section 12.2](#122-tag-walk-apply-in-order).

### FIX 1 (do first) — raise manager client QPS/Burst to 50/100

Root cause B1. In each `main.go`, right after the `rest.Config` is created and **before** `manager.New`, set:

```go
kConfig.QPS = 50
kConfig.Burst = 100
```

| repo | file | anchor |
|---|---|---|
| state-snapshotter | `images/state-snapshotter-controller/cmd/main.go` | after config built, before scheme/manager (`log.Info("[main] kubernetes config ... created")`) |
| state-snapshotter | `images/domain-controller/cmd/domain-controller/main.go` | same anchor (`[domain-main] kubernetes config ... created`) |
| storage-foundation | `images/controller/cmd/main.go` | same anchor (before `apiruntime.NewScheme()`) |

Why: the decisive fix. Precedent in-repo: the capture path already used QPS 100 / Burst 200 on its own clients.
The domain-controller is the demo **planning** layer; note in a comment that a production domain controller must
set its own QPS (not a contract).

Validated: TREES=5 57s→6s; TREES=1 15s→3s; reconcile durMs mean 4491→125ms, max 14812→847ms.

Caveat: **QPS=50/100 is capacity tuning, not proof of low work.** It does not reduce work; it stops the default
limiter from serializing it. Validated by flat reconciles-per-tree and no post-Ready storm — not by showing the
work is small. Pick a production value deliberately. **UPDATE:** the deliberate value is now chosen — a QPS→Ready
saturation sweep sets the **production default at 200/400** (knee; 500/1000 no gain) and the value is
env-configurable; see section 8, "QPS/Burst saturation sweep — production default 200/400 (CLOSED)".

### FIX 2 — MCR controller: watch ManifestCheckpoint

Root cause B2. File `images/state-snapshotter-controller/internal/controllers/manifestcapture/checkpoint_controller.go`.

1. Add a mapper `mapManifestCheckpointToMCR(ctx, obj) []reconcile.Request` that reads
   `checkpoint.spec.manifestCaptureRequestRef` (name+namespace) and enqueues that MCR. (`Owns()` cannot route it
   because the checkpoint is owned by the execution ObjectKeeper, not the MCR.)
2. In `SetupWithManager`, add to the builder:
   ```go
   .Watches(&storagev1alpha1.ManifestCheckpoint{}, handler.EnqueueRequestsFromMapFunc(mapManifestCheckpointToMCR))
   ```

Validated: clean tree 10–17s → 5–7s. Apply [FIX 6](#fix-6--concurrency-ceilings--the-one-required-correctness-fix) (concurrency + `configMu`) to this same file at the same time.

### FIX 3 — SnapshotContent: two reverse-lookup watches (pre-adoption MCP wake + event-driven archive latch)

Root cause B2. File `images/state-snapshotter-controller/internal/controllers/snapshotcontent/controller.go`.
Drives both the manifest-leg wake before ownerRef adoption (L9a) and the bottom-up archive latch (C-2).

Register cache field indexes (per content GVK, in the setup that registers each GVK):

```go
const indexKeyManifestCheckpointName = ".status.manifestCheckpointName"
const indexKeyChildContentName       = ".status.childrenSnapshotContentRefs.name"
```

- `extractManifestCheckpointNameIndex` → projects `status.manifestCheckpointName`.
- `extractChildContentNamesIndex` → projects every `status.childrenSnapshotContentRefs[].name`.
- Register both with `mgr.GetFieldIndexer().IndexField(...)` for each content GVK.

Add two mappers + watches (keep the existing ownerRef-based watches as dual-path backstop):

1. **L9a pre-adoption MCP wake** — `mapManifestCheckpointToContent` uses `lookupContentsByManifestCheckpointName`
   (List by `indexKeyManifestCheckpointName`) so a ManifestCheckpoint event wakes the content whose
   `status.manifestCheckpointName` matches, even before ownerRef adoption. Wire into `addArtifactWakeUpWatches`
   alongside the ownerRef mapper (`ownerRefToContentRequests`).
2. **C-2 event-driven archive latch** — `mapChildContentToParentContentsByEdge` lists parents by
   `indexKeyChildContentName` = this content's name and enqueues them:
   ```go
   .Watches(obj, handler.EnqueueRequestsFromMapFunc(mapSnapshotContentToParentContent))        // existing ownerRef path
   .Watches(obj, handler.EnqueueRequestsFromMapFunc(r.mapChildContentToParentContentsByEdge))  // new forward-edge path
   ```

Keep `MaxConcurrentReconciles: 8` (controller options) and the 500ms self-requeue backstop. No status contract
changes. Validated (C-2): archive-latch gap last-child→root ~12s → ~2.4s.

> This is also the file changed by [ARCH 2 (H2)](#arch-2--single-snapshotcontent-controller-with-dynamic-snapshot-status-watches-h2): `SetupWithManager` uses `builder.Build(r)` (not
> `Complete(r)`) to retain the primary controller handle, so snapshot-status watches attach to it dynamically
> instead of building a second per-GVK controller.

### FIX 5 — storage-foundation VCR: watch VolumeSnapshotContent (event-driven data leg)

Root cause B2. Files under `images/controller/internal/controllers/`.

1. `constants.go`: add label key `LabelKeyVCRNamespaceFull = "storage.deckhouse.io/vcr-namespace"` (a `vcr-name`
   label already exists).
2. At VSC creation (`volumecapturerequest_snapshot_bulk.go`): stamp both `LabelKeyVCRNameFull` and
   `LabelKeyVCRNamespaceFull` on the CSI VolumeSnapshotContent so it carries its owning VCR coordinates.
3. `volumecapturerequest_controller.go`: add mapper `mapVolumeSnapshotContentToVCR(ctx, obj)` that reads those
   two labels and enqueues that VCR; wire into `SetupWithManager`:
   ```go
   .Watches(&snapshotv1.VolumeSnapshotContent{}, handler.EnqueueRequestsFromMapFunc(mapVolumeSnapshotContentToVCR))
   ```
   Also apply [FIX 6](#fix-6--concurrency-ceilings--the-one-required-correctness-fix) concurrency here.

Keep the 5s requeue as a safety net (covers VSCs created before the label existed).

### FIX 6 — concurrency ceilings + the one required correctness fix

Raise `MaxConcurrentReconciles` (conservative 4) on the controllers that were implicitly 1, and add the `Config`
race guard.

| repo | file | change |
|---|---|---|
| state-snapshotter | `genericbinder/controller.go` | `MaxConcurrentReconciles: 4` **+ RateLimiter** `NewTypedItemExponentialFailureRateLimiter[ctrl.Request](200ms, 10s)` (via `genericBinderControllerOptions()`) |
| state-snapshotter | `manifestcapture/checkpoint_controller.go` | `MaxConcurrentReconciles: 4` **plus** the `configMu` guard below (no RateLimiter here) |
| state-snapshotter | `domain-controller/.../demo/virtualmachinesnapshot_controller.go`, `.../virtualdisksnapshot_controller.go` | `MaxConcurrentReconciles: 4` (snapshot demo controllers only — **not** the VM/Disk lifecycle controllers) |
| storage-foundation | `volumecapturerequest_controller.go` | `MaxConcurrentReconciles: 4` + `RateLimiter: NewTypedItemExponentialFailureRateLimiter[ctrl.Request](200ms, 10s)` |

**Required correctness fix (`checkpoint_controller.go`):** `loadConfigFromConfigMap` rewrites shared `Config`
fields (`MaxChunkSizeBytes`, `DefaultTTL`, `DefaultTTLStr`) on every reconcile — a data race once concurrency
> 1. Guard them with a mutex `configMu` behind accessors (`cfgMaxChunkSizeBytes`, `cfgDefaultTTL`,
`cfgDefaultTTLStr`) and snapshot the config once per reconcile before use. Do not raise concurrency here without
this guard.

Full concurrency picture: genericbinder 4, checkpoint 4, foundation VCR 4, demo VMS/VDS 4, SnapshotContent 8.
These did not move the wall on their own (the gate was downstream each time) but remove the ceiling and are
prerequisites for FIX 2–5 to run correctly under load. Start at 4, not 8.

### FIX 8 — genericbinder reverse-watch mappers: direct-ref O(1) routing

Root cause B3 (**load/throughput fix, not the wall-clock fix**). Files under
`images/state-snapshotter-controller/internal/controllers/genericbinder/`.

The three reverse-watch map functions each did a full `unstructuredClient.List` of a GVK + JSON decode of every
object, then filtered to the one match — O(#snapshots/#contents) work + allocations **per event**. Replace with
the references already on the event object (no `List`, no decode):

| file | mapper | change |
|---|---|---|
| `content_watch.go` | `mapBoundContentToSnapshots` | read `content.spec.snapshotRef`, enqueue it directly (O(1)) |
| `content_watch.go` | `mapParentContentToChildSnapshots` | `Get` the owning child Snapshot (from `content.spec.snapshotRef`), enqueue the parents it lists in `status.childrenSnapshotRefs` |
| `mcr_watch.go` | `mapMCRToOwningSnapshots` | walk `obj.GetOwnerReferences()` for the matching Kind/APIVersion, enqueue the owner |

Update `controller.go` watch registrations to the standalone (no-`r.`) mapper signatures where applicable. No
reconcile-contract or status changes; no field index needed (the direct references are the index). Optional
diagnostics shipped alongside (off by default): per-mapper atomic counters (`watch_map_stats.go`, env
`STATE_SNAPSHOTTER_WATCH_MAP_STATS`) and controller-runtime metrics on `:8080` in `cmd/main.go`.

Validated (SETS=10, post-deploy): the two `List`-based mappers **disappear** from the CPU profile; watch-path
`unstructuredClient.List` drops to ~1%; controller ~73% idle during fan-out; reconciles bounded (~3–5 / object),
0 errors, no post-Ready storm. ROOT Ready ~33.5s → **~29.2s** (−13%): a real scaling liability but **not** the
dominant wall-clock term.

---

## 5. Architectural / correctness improvements

These are correct event-driven / single-owner changes worth keeping. They are **not** headline latency
root-cause fixes; apply them as architecture, not as wall-clock levers.

### ARCH 1 — Snapshot controller: wake the gated parent on child-content archive

> **Status: Historical architecture/correctness change. Keep as architecture, not a latency lever.** The wake path
> fires but does not close the tail; do not credit it with a wall-clock win.

File `images/state-snapshotter-controller/internal/controllers/snapshot/content_watch.go`, handler
`snapshotContentToSnapshotEnqueueHandler`. Today it wakes only the **bound owner** Snapshot. Also wake the
**gated parent(s)** — the Snapshot whose root-MCR gate (`usecase.requireContentManifestsArchived`) reads this
child content's archive latch — on the `ManifestsArchived` False→True transition:

- `UpdateFunc`: `archivedTransition := !archived(old) && archived(new)`; when true, additionally enqueue gated
  parents.
- `CreateFunc`: enqueue gated parents when the created content is already `ManifestsArchived=True`
  (resync/restart).
- Gated-parent resolution: `content.spec.snapshotRef` (owning child Snapshot S) →
  `findParentsReferencingChildSnapshot(S)` (Snapshots listing S in `status.childrenSnapshotRefs`,
  namespace-local). Helper: `gatedParentRequestsFromContent`.
- **Dedup** bound-owner + gated-parent requests by `NamespacedName` within one event
  (`enqueueContentDrivenSnapshots`).

Do not weaken the gate (`requireContentManifestsArchived` / `BuildRootNamespaceManifestCaptureTargets`); keep the
500ms backstop. A root's own content maps to no parent (no self-wake cycle). Reclassified: earlier looked like a
root-MCR latency fix, but the root MCR was not gated on this wake (Commit B/C decomposition). Validated (wake
path only): root MCR created ~30–31s → ~24.4s; ROOT Ready ~37s → ~33.5s — confirms the path fires; does **not**
close the SETS=10 tail.

### ARCH 2 — single SnapshotContent controller with dynamic snapshot-status watches (H2)

> **Status: Final — implemented and validated. Keep.** Architecture/correctness cleanup, latency-neutral (not a
> wall-clock lever).

**Status: implemented and validated. Architecture/correctness cleanup, latency-neutral.**

Problem (was open hypothesis H2): a single `SnapshotContent` UID was reconciled by **two distinct
controller-runtime Controllers** — the primary `For(SnapshotContent)` controller
(`snapshotcontent-storage.deckhouse.io-SnapshotContent`) and a per-snapshot-GVK snapshot-status watch controller
(`snapshotcontent-snapshot-<gvk>`), each registered `.Complete(r)` with the **same** reconciler. Both ran the
full reconcile and both patched status → concurrent same-key processing and 409 conflict churn.

Fix, file `images/state-snapshotter-controller/internal/controllers/snapshotcontent/controller.go`:

- `SetupWithManager` / `AddWatchForContent` call `built, err := builder.Build(r)` (instead of `Complete(r)`) and,
  for the common-SnapshotContent GVK, retain the handle in `r.primaryContentController`. `Complete` is exactly
  `Build` with the handle discarded, so nothing else changes.
- `addSnapshotStatusWatchLocked` attaches each runtime-discovered snapshot GVK as an **additional event source on
  the single primary controller** via `r.primaryContentController.Watch(source.Kind(...,
  mapSnapshotStatusToBoundCommonContent))`, instead of building a second Controller. `Controller.Watch` accepts
  sources before or after `Start`, so this preserves the registry-driven / dynamic-CSD activation model.
- The snapshot-status wake is **necessary** and kept: the reconciler reads the owning Snapshot's
  `status.childrenSnapshotRefs` (authoritative declared child set) via `APIReader` to evaluate the
  `ManifestsArchived` latch, so a Snapshot `status.boundSnapshotContentName` change must still wake the bound
  content. `activeSnapshotWatchSet` dedups repeat registrations.

Validated (SETS=10 ×3 + SETS=1, post-deploy):

- **Topology:** exactly one `snapshotcontent-*` controller in startup logs and in the
  `controller_runtime_reconcile_total` registry; no `snapshotcontent-snapshot-<gvk>` controller. (The
  `snapshot-demo…` controllers are domain *Snapshot* controllers — a different concern.)
- **Dynamic watches:** 3 discovered snapshot GVKs (`storage Snapshot`, `DemoVirtualDiskSnapshot`,
  `DemoVirtualMachineSnapshot`) attached to the single controller (8 EventSources total = 5 builder + 3
  snapshot-status); gauges `resolved=3, active=3, stale=0` (dedup works). Virtualization GVK skipped ("not in
  API").
- **Reconcile ownership:** every content reconcile (and every touch of the root content UID) comes from the
  single primary controller; zero from any per-GVK controller.
- **Conflicts:** PUT-409s ~20–27 per SETS=10 run, down from the duplicate-controller era's 34–167/run; the
  dual-writer same-object race is structurally eliminated. Residual 409s are transparent optimistic-lock retries
  on `Update`/finalizer writes (`reconcile_errors_total = 0`); no functional impact.
- **Latency:** SETS=10 ~25s server-side — unchanged within run-to-run noise (this is a cleanup, not a latency
  lever). No reconcile inflation (~477/run, at the pre-Commit-C baseline).
- **Regressions:** none observed in Ready propagation, delete/finalizer handling, retry, or the snapshot→content
  wake. Failure propagation is covered by the integration suite (not exercised by the benchmark).

---

## 6. Confirmed optimizations (correct and helpful, but NOT the bottleneck)

> **Status: Active carry-over guidance (keep).** Correct and helpful; **do not** credit any of them with solving
> scalability — none is a wall-clock lever.

Keep these — they reduce work or are prerequisites — but do not credit them with solving scalability.

- **Commit B — skip child-graph replan after readiness:** 34s → ~28s (−18%) on SETS=10. An optimization, not a
  root-cause fix; the namespace fan-out tail remained.
- **T-cost — defer the expensive declared-child walk** to the only pass that can latch `ManifestsArchived=True`
  (`snapshotcontent/controller.go`, `aggregateChildrenManifestsArchived` takes `ownManifestReady bool`; defer the
  uncached `declaredNonLeafChildContentNames` walk until `ownManifestReady && no pending linked child`):
  observe-lag ~4.3s → ~3.1s.
- **APIReader audit** — cache only the three watched-object mirror reads; keep every correctness-critical uncached
  read (see [Appendix](#11-appendix--apireader-audit)). Hygiene, no regression.
- **CSD planning list served from the manager cache (hot-LIST attribution outcome):** child-graph planning
  (`reconcileParentOwnedChildGraph`, `parent_graph.go`) resolved `csdregistry.EligibleResourceSnapshotMappings` via
  the uncached `APIReader` on **every** planning pass. Audit attribution (SETS=10) measured **~208 apiserver
  `CustomSnapshotDefinition` LISTs per tree** from this callsite — part of ~1946 audited LISTs/tree, ~71% of which
  are repeated full-collection lists of six planning-input GVKs (snapshots, CSDs, demo VM/disk sources and their
  snapshots) driven by the relay waking the root ~200×/tree. The CSD informer is already running (the CSD controller
  watches `CustomSnapshotDefinition`), so the list now goes through the cached `r.Client`; resolution is unchanged
  (same `EligibleResourceSnapshotMappings`, same RESTMapper, same fail-closed). Removes the per-pass apiserver CSD
  list (~208 → ~0 from this callsite). Correctness-neutral load/scalability cleanup, **not** expected to move
  wall-clock. CSD spec/eligibility changes still reach planning via the informer cache.
- **Relay reverse-lookup served from a childrenSnapshotRefs field index (primary relay LIST eliminated):** the
  `nss-chw` child relay resolved parent Snapshot(s) by a full-namespace `SnapshotList` (APIReader) on every child
  event — the #1 audited LIST hotspot at **~440 `storage/snapshots` LIST/tree**. Replaced with a cached
  `Client.List(MatchingFields{status.childrenSnapshotRefs.identity})` + defensive re-match; the child object's own
  read-after-write `Get` stays on the APIReader. Validated: ~440 → **~2** LIST/tree, 0 `field label not supported`,
  0 restarts, root Ready ~21s (no regression). Correctness-neutral load/scalability cleanup (see section 8,
  "Relay reverse-lookup — CLOSED").
- **Concurrency ceilings + `configMu` race fix** ([FIX 6](#fix-6--concurrency-ceilings--the-one-required-correctness-fix)) — mandatory correctness/prerequisite; no wall-clock move
  on its own (the gate was downstream each time).
- **MCP→MCR and VSC→VCR watches** ([FIX 2](#fix-2--mcr-controller-watch-manifestcheckpoint), [FIX 5](#fix-5--storage-foundation-vcr-watch-volumesnapshotcontent)) — correct event-driven architecture regardless of
  the measured win.
- Keep any per-reconcile SnapshotContent trace at **debug** level (diagnostics, not a fix).

---

## 7. Rejected hypotheses (checked — do not revisit)

> **Status: Rejected — do not re-implement.** These were checked and disproven; the "Why" column is the invariant
> that makes each a dead end. Preserve those invariants in any rework.

| Hypothesis | Result | Why |
|---|---|---|
| **Commit C — VSC wake loss dominates leaf latency** | **False** | Dual-path VSC→content wake raised content reconciles (490 → 554/579/613, SETS=10 ×3) with **no** wall-clock improvement (staircase 20→18-19s, observe-lag 6/11→~5/9). Leaves are gated on `ManifestCapturePending`, not on the VSC wake; the cost-cut guard's precondition (manifest latched while volume pending) never holds for a leaf, so it cannot fire. Extra wakes just added load. |
| **genericbinder reverse `List` is the remaining wall-clock bottleneck** | **False as a wall-clock cause** | pprof confirmed it as a major CPU/alloc hotspot (kept as B3 / FIX 8), but removing it moved wall only 33.5s→29.2s. The residual tail is latency-bound, not CPU-bound. |
| **Archive latch is the remaining dominant tail** | **No (partially true earlier)** | The event-driven archive latch (C-2) correctly cut last-child→root ~12s→2.4s and is **kept**, but it is not the remaining dominant bottleneck at SETS=10. |
| **Repeated child-graph planning is the dominant root latency** | **Partially true** | Removing repeated planning (Commit B) gave ~18% wall, but did not eliminate the fan-out tail → not the dominant term. (Superseded by H3: the *pending-phase* re-plan, not the single MCR pass, is the remaining cost.) |
| **H1 leaf staircase is a distinct leaf-side bottleneck (`vscReady → leaf content Ready`)** | **False** | Per-leaf server-side + log trace: once created, a leaf latches fast/constant (MCP created→Ready ~0–1s, content Ready ~1–4s). The staircase is purely *delayed creation*, gated by the repeated root re-plan (H3). No distinct leaf problem. |
| **"Leaf-skip": skip child-graph planning for leaf snapshot GVKs** | **False (no-op)** | Premised on leaves running planning. `reconcileParentOwnedChildGraph` is registered `For(&storagev1alpha1.Snapshot{})` and runs **only** on the root; demo VM/disk snapshots are reconciled by the separate domain-controller. The "leaf `DemoVirtualDiskSnapshot` spent 10s planning" log lines were the **root** reconcile triggered via the `nss-chw` relay, mislabeled with the relay's inherited logger context (`"snapshot":{"name":"bench-root"}` on every one). Nothing to skip. |
| **`childSnapshotReadCache` via informer cache** (move the ~542 VMSnapshot/DiskSnapshot readiness LIST/tree — the last large child-snapshot LIST candidate — off the uncached `r.Client`) | **False (freshness-bound as implemented)** | `priorityReady`/status reads alone are mostly stale-tolerant (generation-guarded readiness `conditionSliceHasCurrentTrue`; child-watch `nss-chw` wakeups; 30s poll backstop `snapshotChildGraphPollInterval`). But `childSnapshotReadCache` is **one shared per-GVK LIST** feeding **three** planning consumers: (1) coverage/dedup walk, (2) existence-before-create in `ensureParentOwnedChildSnapshot`, (3) `priorityReady` readiness classification. The ~542 LISTs cannot be removed for readiness alone — it is the same list. Moving the shared LIST to informer-cache reads also makes create/dedup decisions stale, which is **not** correctness-neutral: a stale NotFound after a child was created but before the informer observes it (exactly the pending phase, where children are created in bursts) makes `ensureParentOwnedChildSnapshot` call `Create` again; `Create` (`parent_graph.go:326`) does **not** ignore `AlreadyExists`, so it transiently surfaces as `ChildrenSnapshotReady=False/GraphPlanningFailed` and propagates into root `Ready` **flapping** (separately guarded by `ready_flap_test.go`), and skips the found-branch ownerRef re-ensure; the same stale-uncovered view can double-plan an already-created child. **Case B.** A future Case A path is separate correctness work, not a load-only change: make `ensure` idempotent under stale-NotFound/`AlreadyExists` (+ preserve/re-ensure ownerRef on that path); prove coverage-dedup tolerates transient stale-uncovered; prove `ChildrenSnapshotReady` monotonicity within one generation **or** split readiness reads from create/dedup reads. Until those invariants are proven and tested, leave this LIST on the APIReader/uncached `r.Client`. |
| **"Source-skip": skip source re-discovery once `status.childrenSnapshotRefs` is first published** (to cut the ~1060 demo source/snapshot LIST/tree — VM/Disk `r.Dynamic` source lists + VMSnapshot/DiskSnapshot readiness lists, ~1 per GVK per planning pass × ~272 pending-phase passes) | **False (changes semantics)** | Premise "membership is frozen after first publish" is **false**. Proven read-only from code: (1) publication is a **full recompute**, not accumulation — `mergeSnapshotManagedChildRefs` (`parent_graph.go`) drops every `nss-child-*` from the current status and writes the freshly-discovered `desired` each pass; (2) membership is built **incrementally across priority layers** — a pending layer early-returns with `ChildrenSnapshotReady=False/PriorityLayerPending` after publishing only the layers planned so far (`parent_graph.go:168-178`), so the **first publish can be partial** and later passes legally **grow** (next layer's children created/discovered) or **shrink** (source removed, INV-REF-M2) the set; (3) the tests enforce this — `child_graph_replan_skip_test.go` Test 3 requires a **full re-plan** for every non-`True/Completed` state (`False`, `Unknown`, `True`-but-not-`Completed`, missing). The **only** valid freeze point is `ChildrenSnapshotReady=True` + `Reason=Completed` + `ObservedGeneration==Generation`, and that skip **already exists** as `childGraphReplanSkippable` (`controller.go`). Skipping source re-discovery earlier (at first publish, during the pending window) would freeze at layer 0 and never create/discover lower priority-layer children — breaking multi-layer trees. Do not implement. The remaining pending-phase LIST load is a separate, freshness-gated question (readiness reads through the informer cache — prove tolerance first). |

---

## 8. Remaining open issues (open investigations)

> **Status: Mostly historical — this section is preserved to document the investigation path.** Despite the title,
> every sub-investigation below is now **CLOSED, rejected, or deferred**, and **active latency work is stopped**
> (see "Final residual diagnosis — STOP" and "Concurrency campaign — CLOSED"). Read each subsection's own banner
> for its final status. The portable outcome is in [section 12](#12-carry-over-to-a-reworked-main--portable-bottleneck-playbook-tag-walk).

State at SETS=10 (pre-D.3): **ROOT Ready ~25s server-side (~29–30s client-measured)**. CPU, mapper `List`, the
rate limiter, and most polling are **no longer** bottlenecks. The remaining tail is **latency-bound** and, per the
Commit-D audit below, sat in **repeated root child-graph planning** (H3). H1 is not a distinct hypothesis.
**After D.3 (see below): wall ~23–24s client-measured** and the child-graph-planning cost is effectively closed;
the residual tail then moved to the **root manifest leg** (ChildrenSnapshotReady ~13s → Ready ~24s), which is
itself **now closed by H4.1** (see H4 below). **No open latency investigation remains**; the residual ~24–25s wall
is genuine bottom-up archive propagation (child `ManifestsArchived` staircase to ~17–22s), a lower-priority
follow-up only if further cuts are wanted.

- **H3 — repeated root child-graph planning (CLOSED by Commit D.3; kept for history).** Was the primary open
  bottleneck; the per-pass cost was attributed by the Commit-D instrumentation and then removed by D.3 (per-child
  `Get`s → one `List`/GVK): worst pass 11.1s → ~0.8–1.1s, the three `Get`-heavy sections dropped ~7×, wall
  ~29–30s → ~23–24s. The mechanism below remains true — the root is still re-reconciled many times and still
  re-plans each pending pass — but each pass is now cheap, so neither the per-pass cost nor the duplicate passes
  are worth optimizing further (see "no longer recommended" below). Original diagnosis, for the record:
  `reconcileParentOwnedChildGraph` runs **only** on the root unified
  `Snapshot` (it is registered `For(&storagev1alpha1.Snapshot{})`; demo VM/disk snapshots are reconciled by the
  separate domain-controller, not by this path). During the pending fan-out phase the root is re-reconciled
  **~36×/run** (SETS=10) — ~2/3 of those wakes come from the `nss-chw-*` child-watch relays firing on every child
  snapshot status change — and each pending pass re-runs the **full** O(N) plan (`childGraphReplanSkippable` only
  skips *after* `ChildrenSnapshotReady=Completed`). Measured passes reached **child-graph-planning ~10s** and
  root reconcile **~16–17.5s** end-to-end, growing with the child/coverage set. This repeated full re-plan — not
  any per-leaf step — is what delays child creation and produces the observed leaf-Ready staircase. The Commit-D
  instrumentation (below) has now **attributed the per-pass cost**: it is the **per-child `Get`-heavy sections**
  (`coverageWalk` + `ensureChildren` + `priorityReady`), **not** the source `List`s; and the wall is inflated by
  **both** cost-per-pass growth **and** concurrent duplicate re-plans (the relay calls `Reconcile` directly).

> **H1 (leaf staircase) is absorbed by H3 — not a separate problem.** Earlier framing (`vscReady → leaf content
> Ready` grows ~4→9–11s, suspected leaf-side MCP/worker contention) was disproven by a per-leaf server-side +
> log trace: once a leaf is *created*, its legs latch fast and roughly constant (MCP created→Ready ~0–1s,
> content Ready ~1–4s after). The staircase is entirely in **when each leaf is created**, and creation is gated
> by the repeated root re-plan (H3). There is no leaf-level child-graph planning to fix — see the rejected
> "leaf-skip" hypothesis in [section 7](#7-rejected-hypotheses-checked--do-not-revisit).

> **H2 is closed** — see [ARCH 2](#arch-2--single-snapshotcontent-controller-with-dynamic-snapshot-status-watches-h2). It was implemented and validated as an architecture/correctness cleanup
> (duplicate controller removed, single dynamic `Watch` on the primary controller, correctness OK, latency
> unchanged within noise). It is **not** a remaining latency lever.

### Commit D — instrument `reconcileParentOwnedChildGraph` (diagnosis before optimizing)

> **Status: Historical diagnosis. Final status: CLOSED by D.3** (per-child `Get`s → one `List`/GVK). D.2 and D.1′
> below are **historical proposals — do not implement** (premise gone). Retained to document how the per-pass cost
> was attributed. Portable rule: `[CO-DIRECT-REF]` (section 12.2).

**Status: instrumentation applied and measured (SETS=10, warm).** `reconcileParentOwnedChildGraph` accumulates
per-section wall time (`childGraphPlanningTimings`) and logs one breakdown per pass (covering the hot "priority
layer pending" early return that dominates fan-out), at the same 150ms threshold as the caller's total:
`resolveMappings` · `listSources` (with `listCalls`/`sourceObjects`) · `coverageWalk`
(`IsCovered`/`ObservePlannedSnapshot` recursive `childrenSnapshotRefs` `Get`s) · `ensureChildren` (per-child
`Get`+`Create`/`Patch`) · `priorityReady` (per-child readiness `Get`) · `publish`. File
`images/state-snapshotter-controller/internal/controllers/snapshot/parent_graph.go` (diagnosis-only; no
behaviour, data-model, or status-contract change).

**Measured attribution (worst pass, totalMs=11144):** coverageWalk **4501** + ensureChildren **3396** +
priorityReady **3132** = ~11.0s (99%); listSources **41** (2 List calls / 30 source objects), resolveMappings
**72**, publish **0**. Across the 6 logged passes: coverageWalk **17.5s**, priorityReady **12.8s**,
ensureChildren **11.4s**, listSources **0.27s**, resolveMappings **0.18s**, publish **0.05s**.

Conclusions:

- **The cost is per-child `Get`s, not source `List`s.** The earlier "uncached dynamic `List`s / ~40k GET" framing
  was wrong on attribution: listing sources is ~40ms; the ~seconds are spent in the three sections that do a
  `Get` (and recursion) **per child** — the recursive coverage walk over `childrenSnapshotRefs`, the per-child
  ensure `Get`, and the per-child readiness `Get`. A single `List` of children would be far cheaper than N `Get`s.
- **Both axes are bad.** Cost-per-pass grows with N (coverageWalk 289→4685ms across passes) **and** passes are
  duplicated concurrently — pairs of passes with near-identical duration fire in the same second (e.g. 4261/4234
  at 12:10:45; 11134/11144 at 12:10:56) because the `nss-chw` relay calls `r.main.Reconcile` **directly** (not
  via the workqueue), so concurrent child events spawn concurrent full re-plans of the same root (~42s of planning
  work packed into ~26s wall).

Fixes, one variable at a time, guided by the numbers:

- **D.3 — collapse per-child `Get`s into one `List` per child GVK (implemented and validated, SETS=10 warm).**
  `parent_graph.go` only. A per-pass `childSnapshotReadCache` lazily lists each child snapshot GVK once (via
  `r.Client` — the **same** client the former per-item `Get`s used, so the source of truth is identical) and
  serves the coverage walk, the ensure existence check, and the per-child readiness check from that map. No
  status-contract, membership-skip, debounce, or coverage-invariant change. Guardrail: all three reads were
  already `r.Client` (cached), **not** intentionally-uncached `APIReader`, so this is purely N `Get`s → 1 `List`.
  **Measured (SETS=10 warm, 3 runs after redeploy):** the three `Get`-heavy sections collapsed exactly as
  predicted — worst child-graph pass **11144ms → 788–1089ms** (target <3–4s met); section sums per run dropped
  coverageWalk **17.5s → ~2.5s**, ensureChildren **11.4s → ~2.3–2.6s**, priorityReady **12.8s → ~0.01–0.02s**
  (near-eliminated); per-pass child reads went from N per-child `Get`s to **≤2 `List`/pass** (`childListCalls`).
  Wall (client) **~29–30s → 23.4/23.7/24.2s**; ROOT Ready correctness unchanged (20 children, 20 leaves Ready
  every run). Risk-3 (stale per-pass list) did **not** materialize: wall dropped rather than grew, so newly-created
  children were not pushed into an extra pass in a way that cost latency. Residual wall is now dominated by the
  root manifest leg (ChildrenSnapshotReady ~13s → Ready ~24s) and leaf-creation cadence, **not** child-graph
  planning — H3's planning cost is effectively closed; the remaining tail moves to the manifest leg.
  Pre-deploy risk review (checked in code, not just claimed):
  - **List-GVK convention** — the cache builds an `UnstructuredList` with `Kind: gvk.Kind+"List"` and lists via
    `r.Client`. Precedent: `snapshotcontent/controller.go` already does exactly this against the module's snapshot
    GVKs in production, and the former coverage/ensure/readiness `r.Client.Get`s on the same GVKs worked, so the
    cache has informers and a RESTMapping. Not a new risk.
  - **Namespace scope** — the cache lists a single namespace (the root Snapshot's). A `Get` with a differently
    namespaced key now returns a **hard error**, not a misleading `NotFound` (guard + `TestChildSnapshotReadCache`).
    Correct today because the whole run tree is namespace-local to the root; the guard prevents a future footgun.
  - **Stale list within one pass** — the list is taken once per pass, so a child created earlier in the *same*
    pass by `ensureChildren` is not visible to the later `priorityReady` in that pass. This is **latency-safe**: a
    freshly-created child has not run its own reconcile, so it is **not** `ChildrenSnapshotReady` regardless of
    whether the read sees it (`NotFound`) or sees it (present-but-not-ready) — either way the layer stays *pending*
    and requeues; the next pass (woken by the child event) re-lists fresh. No extra pass is added versus the former
    post-`Create` `Get`. If, contrary to this reasoning, the wall does **not** drop or grows after deploy, this is
    the first thing to re-check.
- **D.2 — NO LONGER RECOMMENDED (premise gone).** Was: avoid the walk when child *membership* is unchanged. It
  only mattered while a pass was expensive; after D.3 a full pass is ~0.8–1.1s and the walk is ~2.5s summed across
  all passes, so the invariant-proof risk of a membership-skip is not justified by the remaining cost.
- **D.1′ — NO LONGER RECOMMENDED (premise gone).** Was: coalesce/dedupe the concurrent relay-driven root re-plans.
  Duplicate passes only hurt because each pass was expensive; now that passes are cheap, changing event-delivery
  semantics (a debounce can hide a lost event) is not worth the risk for the remaining latency.

Adjacent future work: choose the production manager client QPS/Burst deliberately (50/100 was capacity tuning, not
proof of low work — see FIX 1 caveat). **RESOLVED:** the QPS→Ready sweep (section 8, "QPS/Burst saturation sweep")
sets the production default at **200/400** (the saturation knee; 500/1000 gives no gain). Now env-configurable.

**Portable conclusion:** a reverse lookup or existence/readiness check done as **N per-child `Get`s** must become
**one `List` per child GVK per pass** served from the same source of truth. The exact `Get` count and the 11.1s→~1s
per-pass numbers are workload-dependent; the invariant is that repeated per-item `Get`/full-traversal on the
reconcile path is the hotspot, and the fix is direct addressing (index / one list / cache), never a second search.
Duplicate concurrent re-plans stop mattering once each pass is cheap — do **not** add a debounce that can drop an
event.

### H4 — root manifest leg (`ChildrenSnapshotReady` → `Ready` ≈10s) — CLOSED by H4.1

> **Status: Historical investigation. Final status: CLOSED by H4.1** (dead reverse-index `List`s routed through the
> cache; field-selector errors and lost-wake tails gone; leg stable event-driven ~9–10s). Portable rules:
> `[CO-EVENT-WAKE]` + `[CO-DIRECT-REF]` (section 12.2).

With H3 closed, this was the **only** remaining SETS=10 latency issue and it is well-localised. After every child
is ready (`ChildrenSnapshotReady=True` at ~13s), the root Snapshot still takes ~10s to reach `Ready`
(`ManifestsArchived`/`Ready` at ~24s). New problem statement — **not** a child-graph fix:

> Why, once `ChildrenSnapshotReady=True`, does the root Snapshot take ~10s more to become `Ready`?

**Diagnosis (server-side trace of 3 warm SETS=10 runs + controller logs; no code change).** The 6 requested
sub-intervals (offsets from root create, `lastTransitionTime` second-granularity):

| boundary | r1 | r2 | r3 |
|---|---|---|---|
| ChildrenSnapshotReady (snap) | 13 | 10 | 12 |
| root MCP created | 22 | 19 | 87 |
| root MCP Ready | 23 | 19 | 87 |
| content ManifestsReady | 24 | 23 | 89 |
| ManifestsArchived (snap) | 25 | 24 | 89 |
| Ready (snap) | 26 | 26 | 89 |

- **Dominant interval = `ChildrenSnapshotReady → ManifestsArchived`** (r1 **12s**, r2 **14s**, r3 **77s**), inside
  which `ChildrenSnapshotReady → root MCP created` is the largest sub-part (~9s warm, ~75s in r3). **Interval 6
  (`ManifestsArchived → Ready`) ≈ 0–2s** on all runs — the root Ready mirror is not the problem.
- **Classification: lost wake → self-requeue/poll backstop** (not real capture work, not an expensive reconcile).
  Controller logs show the manifest-leg reverse watches erroring at runtime and dropping the wake:
  `field label not supported: .status.childrenSnapshotContentRefs.name` (**669×** in one window),
  `.status.manifestCheckpointName` (**10×**), `.status.dataRef.artifact.name` (**6×**), each followed by
  `self-requeue backstops` and `ManifestCheckpoint event resolved to no SnapshotContent … dropping`. The bottom-up
  archive latch therefore advances on the slow self-requeue cadence, not on events. The child-content
  `ManifestsArchived=True` staircase confirms it (r1 tail `…19, 24`; r3 stragglers `…76, 78, 89`), and the root
  subtree latch waits for the slowest child. r3's 77s is the same interval with the lost-wake tail fully exposed.
- **Root cause (code): the reverse-lookup `List`s read through the manager client, which does not cache
  unstructured objects, so `client.MatchingFields` is sent to the API server as a field selector it rejects.**
  The three field indexes (`indexKeyManifestCheckpointName` / `indexKeyChildContentName` /
  `indexKeyDataRefArtifactName`) **are** registered (`SetupWithManager` runs at startup with
  `SnapshotContentGVKs = [CommonSnapshotContentGVK]` and an empty `activeContentWatchSet`, so the guard does not
  skip them — the earlier "guard skips `IndexField`" hypothesis was **disproven**). The real defect is that
  `manager.Options` sets no `Client.Cache.Unstructured=true`, so controller-runtime's default applies:
  **unstructured `Get`/`List` bypass the cache and go to the API server.** `Get`-by-name still works (name
  selectors are supported), but the reverse-lookup `List`s (`snapshotcontent/controller.go` `reverseLookupReader`
  sites: `lookupContentsByManifestCheckpointName`, `mapVolumeSnapshotContentToContent`,
  `mapChildContentToParentContentsByEdge`) pass a **custom status field selector** the API server refuses
  (`field label not supported`). The registered cache indexes are therefore never consulted, and FIX 2 / FIX 3 /
  FIX 5's event wakes are effectively dead — only their poll/requeue backstops carry the archive wave. Not caused
  by D.3.

**Fix (H4.1, implemented and validated).** Route the three enqueue-only reverse-lookup `List`s
through the **manager cache** (`mgr.GetCache()`, exposed as `r.cacheReader` via `reverseLookupReader()`), which
uses the registered `indexKey*` indexes, instead of `r.Client` (which hits the API server for unstructured). This
is deliberately **not** a global `Client.Cache.Unstructured=true` flip — that would also change D.3's child-List
read semantics (cached/eventually-consistent) and couple two unrelated changes. The reverse lookups only enqueue
`reconcile.Request`s and are fully backstopped by the 500ms self-requeue, so an eventually-consistent cache read is
safe by design. Unit tests are unaffected (they wire an indexed fake client as `Client`; `cacheReader` is nil in
tests and `reverseLookupReader()` falls back to `Client`). No status-contract change; poll/requeue backstops stay.
Acceptance (SETS=10 warm r1/r2): the three `field label not supported` errors disappear, the reverse-lookup
uncached API `List`s collapse, the apiserver-timeout count drops, `ChildrenSnapshotReady → ManifestsArchived`
falls well below the current ~12–14s, and Ready correctness is unchanged. r3-style lost-wake tails should also
disappear. Validate with the same server-side trace across 3 warm runs; **r3 (89s) is excluded from latency stats
as a control-plane-stall / leader-election-lost incident during a load spike** (both controllers lost their lease
to the same apiserver at 13:27; leader-election hardening is tracked separately, intentionally **not** in H4.1 so
it cannot mask the load problem).

**Result (SETS=10 warm, fresh pod `controller-7d5cb5fb85`, 3 measured runs after 1 warm-up).** All acceptance
signals met:

| signal | before (r1/r2 pre-fix) | after (r1/r2/r3) |
|---|---|---|
| `field label not supported` (whole pod window) | 669× / 10× / 6× per run | **0** |
| controller restarts during runs | 3–5 (leader-election lost) | **0** |
| `leader election lost` / apiserver-timeout | present (r3 = 89s outlier) | **0** |
| `error`-level log lines | present | **0** |
| root Ready wall | ~23–30s, with r3 89s outlier | **23.8 / 24.5 / 24.9s** (tight, no outlier) |
| `ChildrenSnapshotReady → Ready` leg | ~12–14s | **8.5 / 10.4 / 10.6s** (avg ~9.8s) |

The reverse-lookup `List`s now resolve through the cache indexes: the API-server field-selector rejections are
gone, event wakes for the manifest leg are live again (FIX 2 / FIX 3 / FIX 5 no longer degraded to poll-only), and
the lost-wake tail that produced r3's 89s incident did not recur — the three runs are within ~1s of each other.
The `ChildrenSnapshotReady → Ready` leg improved by ~2–4s and, more importantly, became **stable and event-driven**
rather than poll-backstopped. Ready correctness unchanged (all subtrees reached Ready on every run). The residual
~9–10s leg is now genuine bottom-up archive propagation (child-content `ManifestsArchived` staircase up to ~17–22s
from root create), not lost wakes — a separate, lower-priority investigation if further cuts are wanted.

**Portable conclusion:** a cross-controller wake that relies on a custom status **field selector** silently dies if
the backing client does not cache/index that object (unstructured reads bypass the cache and the apiserver rejects
the selector). Route reverse-lookup `List`s that only *enqueue* through the **manager cache** with a registered
field index; keep the poll purely as a backstop. Symptom to grep for: `field label not supported` + a leg that
advances on the self-requeue cadence. The seconds saved are workload-dependent; the invariant is "event wakes must
actually fire, not degrade to poll-only".

### Final residual diagnosis (post-H4.1) — decision: STOP active latency work

> **Status: Final recommendation (STOP).** After H4.1 no single interval exceeds 50% of the tail; the residual wall
> is genuine staged readiness propagation. No relay debounce / membership-skip / new cache change is justified.
> This is current guidance, not historical context.

A read-only server-side trace of 3 warm SETS=10 runs after H4.1 was used to decide whether one more safe,
high-leverage fix exists. Timeline (offsets in s from root create, 1s-granularity `lastTransitionTime`):

| boundary | r1 | r2 | r3 |
|---|---|---|---|
| first child content `ManifestsArchived` | 2 | 3 | 2 |
| `ChildrenSnapshotReady` | 12 | 13 | 13 |
| last direct child `ManifestsArchived` | 16 | 16 | 17 |
| root MCP `Ready` | 20 | 20 | 20 |
| root content `ManifestsReady` / `ManifestsArchived` | 22 | 23 | 22 |
| root Snapshot `Ready` (client wall) | 23 (24.5) | 23 (23.5) | 24 (25.0) |

Derived: direct-child archived span 13–15s (n=30); last-child-archived → root-MCP-Ready 3–4s; MCP-Ready →
content-archived ~2s; content-archived → root-Ready 1–2s; **total `ChildrenSnapshotReady → Ready` 10–11s**. No root
MCR is created (root captures via MCP directly). Counters (3 runs + warm-up, ~13 min): 0 `field label not
supported`, 0 `leader election lost`, 0 error lines, ~3 conflicts/run; SnapshotContent ~766 reconciles/run, root
`bench-root` ~176 reconciles/run, ~37 relay triggers/run; ~456 dropped `MCP→content` / `VSC→content` wakes/run that
fall back to the 500ms self-requeue. Top-5 slowest contents all show `archived == ready` with **no stuck gate**.

**Classification of the ~24–25s wall.** No single interval exceeds 50% of the tail. Two blocks: (a) root create →
last direct child archived ≈ 16–17s (~70% of wall) = child-snapshot creation + **demo volume/manifest readiness**
(categories A/B, with C as its bottom-up latch) — a smooth staircase at ~one content per 0.5s; (b) root manifest
leg ≈ 7s spread evenly across four real controller handoffs of 2–4s each. The only smell is category E (~456
dropped wakes/run on poll cadence), but each pass is cheap post-D.3, the wall is stable, and the trace does **not**
prove event-count is the dominant cost.

**Decision: stop active latency work here.** Further gains are likely diminishing-return micro-optimizations unless
a new production-scale trace shows a fresh dominant interval. The biggest block is simulated demo readiness pacing
(not a controller defect; out of latency-fix scope), and the manifest leg is evenly spread across genuine pipeline
stages. Per the guardrails, **no** relay debounce (D.1′), membership-skip (D.2), or new cache/APIReader change is
proposed — none is justified by this trace. Remaining items are **future / low-priority**, not active work:
the dropped-wake poll fallback and the child-archive staircase can be revisited only if a real-scale trace shows
them dominating.

### H5 — concurrent pre-MCR namespace-sweep race — CLOSED (single-flight implemented + validated)

> **Status: Historical investigation. Final status: CLOSED (implemented + validated).** Correctness-neutral
> concurrency dedup; not a scalability fix (~8–9% of per-tree load). Portable rule: `[CO-DIRECT-REF]` (avoid
> duplicated full sweeps); the exact GET counts are workload-dependent.

**Status: IMPLEMENTED and validated by measurement. A per-`Snapshot`-UID in-process single-flight now gates the
pre-MCR sweep so only one concurrent reconcile plans; the others requeue and take the frozen `mcr-present` branch.
Correctness-neutral (concurrency dedup only; the MCR-gate still owns temporal dedup and the plan result is not
cached across time). Not the dominant API-load term (~8–9% of per-tree GET at fan-out) but a clean, safe cleanup
that removes the concurrent duplicate sweeps and the `AlreadyExists` Create race.**

**Root cause.** The root namespace-manifest capture plan is built by
`BuildRootNamespaceManifestCaptureTargets` → `BuildManifestCaptureTargets` (`pkg/namespacemanifest/targets.go`):
one full **discovery enumeration of ~130 namespaced types + a parallel per-type `List` sweep** of the namespace,
all via the uncached capture *dynamic* client (`snapshot/controller.go` `captureRESTConfig`, QPS 100/200) — so
every sweep is real apiserver traffic, never cache-served. `capture.go` has an **MCR-gate** (`capture.go:185-201`):
once the root `ManifestCaptureRequest` exists, subsequent reconciles take the frozen-plan branch
(`branch=mcr-present`) and do **not** re-list. That gate dedups sweeps **across time (post-creation)** but **not
across concurrent reconciles in the pre-creation window**: the child-watch relay calls `Reconcile` directly and
`MaxConcurrentReconciles=8`, so several reconciles of the same root pass the `APIReader.Get(MCR)=NotFound` gate
before any of them creates the MCR, and each runs the full sweep for identical namespace state (the extra Creates
land on `AlreadyExists`).

**Evidence (measured, no code change).** Method: count the controller's own DEBUG branch tags
(`mcr-created` / `subtree-pending` / `mcr-present`) and the `namespace-list-manifest-planning` durMs lines for one
root, cross-checked against a `rest_client_requests_total` GET delta over the same window (port-forward metrics).

| run | sets | sweeps/root | max concurrent | window span | GET / root (total) | sweep share |
|---|---|---|---|---|---|---|
| SETS=1 | 1 | 3 | 3 (08:04:35–36) | ~2.5s | ~1550–1850 | ~40–50% |
| SETS=10 r1/r2/r3 | 10 | 2 / 2 / 2 | 2 | 2.0–3.1s | 5797–6060 | ~8–9% |
| SETS=20 | 20 | 3 | 3 | 7.6s | 12760 | ~4% |

The race is **structural and amplifies with fan-out** (longer pre-MCR window → more relay-driven self-reconciles →
more concurrent sweeps: 2 at SETS=10, 3 at SETS=20), not a single-tree artifact. Redundant sweeps/root = sweeps−1
(1 at SETS=10, 2 at SETS=20). Each sweep ≈ 250–500 uncached GET and ~1.5–5.5s of planning.

**Leverage (why it is a cleanup, not a scalability fix).** The sweep is ~250–500 GET/root, but total is ~5800
(SETS=10) → ~12760 (SETS=20) GET/root — i.e. **~90% of per-tree API load is NOT the sweep**; it is child-subtree
processing (per-disk child snapshots + their contents), which grows linearly with fan-out while the sweep does not.
The single-flight removes the 1–2 redundant full sweeps/root (~250–500 GET each + ~1.5–5.5s duplicated planning +
the concurrent-apiserver spike in the pre-MCR window) but does **not** flatten the scalability curve on its own.

**Fix (implemented).** A non-blocking per-`Snapshot`-UID in-process single-flight
(`snapshot/capture_sweep_singleflight.go`) gates the pre-MCR planning span in `reconcileCaptureN2a`, placed right
after the MCR-gate `NotFound`. The first reconcile to `TryAcquire(UID)` plans (sweep + MCR create) and releases via
`defer`; a concurrent reconcile for the same UID does **not** sweep — it logs `branch=sweep-inflight` and requeues
200ms, then takes the `mcr-present` frozen branch once the leader has created the MCR. Distinct Snapshots key on
distinct UIDs and still plan in parallel. A leader that returns without creating the MCR (transient) releases the
flight, so a later reconcile re-plans — the plan result is never cached across time (temporal dedup stays with the
MCR-gate; the gap closed here is concurrency only). Key is the UID (not name) for generation safety.

**Validation (measured, after deploy).** Fan-out `SETS=10 ×3` + `SETS=20`, counting the
`namespace-list-manifest-planning` sweep lines, the new `sweep-inflight` deferrals, and the
`rest_client_requests_total` GET delta.

| run | sets | sweeps/root (before → after) | max concurrent (before → after) | sweep-inflight (deferred) | GETΔ/root | root Ready |
|---|---|---|---|---|---|---|
| SETS=10 r1/r2/r3 | 10 | 2 → **1** | 2 → **1** | 1 | 5628 / 5804 / 5724 | 18 / 19 / 17s |
| SETS=20 | 20 | 3 → **1** | 3 → **1** | 2 | 11709 | 38s |

Redundant sweeps/root → **0** (the `sweep-inflight` count equals the eliminated redundant sweeps: 1 at SETS=10, 2
at SETS=20). `max concurrent sweeps = 1` ⇒ no concurrent MCR `Create` ⇒ the `AlreadyExists` planning race is gone.
GETΔ dropped ~by the removed sweep cost (SETS=20 ~12760 → ~11709). Root Ready unchanged within noise; controller
restarts = 0; no `ListFailed`/incomplete.

**Next investigation (higher leverage).** Attribute the remaining ~90% of per-tree GET (child subtree). Build a
Top-5 API-cost contributor table and check whether the child-subtree reads contain their **own** redundancy (e.g.
repeated traversals of the same subtree). A redundancy there is a 50–80% lever vs H5's <10%.

**Portable conclusion:** when the same object can be reconciled concurrently in a pre-creation window, a
non-blocking per-UID in-process single-flight around the plan/create span removes duplicate full sweeps and the
`AlreadyExists` create race — correctness-neutral because the temporal dedup (the create-gate) still owns "already
done", and the plan result is never cached across time. Key on **UID, not name** (generation safety). The percent
figures are workload-dependent; the invariant is "concurrency dedup ≠ temporal dedup — add the missing one without
caching the decision".

### Relay reverse-lookup — CLOSED (childrenSnapshotRefs field index)

> **Status: Historical investigation. Final status: CLOSED (implemented + validated).** Textbook `[CO-DIRECT-REF]`:
> full-namespace `SnapshotList` per child event → cached field-index lookup. Portable invariant: a relationship
> enumeration authored elsewhere can be served from an index; the `~440 → ~2` numbers are workload-dependent.

**Status: IMPLEMENTED and validated on-cluster. Primary relay reverse-lookup LIST eliminated.** Replaced the
namespace-wide `SnapshotList` in `findParentsReferencingChildSnapshot` with a cached field-index lookup
(`status.childrenSnapshotRefs.identity`). Validation: `storage.deckhouse.io/snapshots` LIST/tree dropped from
**~440 to ~2** with no correctness regression. Correctness-neutral load/scalability cleanup, same class as CSD,
D.3, FIX 8.

**Root cause.** The largest single audited LIST hotspot (~440 `storage/snapshots` LIST/tree at SETS=10) came from
the `nss-chw` child-snapshot relay: on **every** child-snapshot status event it looked up the parent Snapshot(s)
that reference the child in `status.childrenSnapshotRefs` by doing a full `SnapshotList` in the child's namespace
via the **APIReader** and scanning every Snapshot's `childrenSnapshotRefs` in Go. That is not a freshness read —
it is a relationship enumeration; the parent set is authored by the Snapshot controller and is unaffected by the
triggering child write, so it can be served from a cache index. (The relay's own `Get` of the *child object*
stays on the APIReader for read-after-write; only the parent enumeration moved.)

**Fix (implemented).** A new field index `SnapshotChildrenRefFieldIndex = "status.childrenSnapshotRefs.identity"`
(`content_watch.go`) indexes each parent by child identity (normalized GVK + `\x00` + name, namespace-less by
design; registered on the manager indexer in `controller.go`). `findParentsReferencingChildSnapshot`
(`child_snapshot_watches.go`) now does `Client.List(InNamespace(childNS), MatchingFields{...})` on the cached
`m.main.Client` with a defensive exact re-match, replacing the APIReader full-namespace `SnapshotList`. Namespace
isolation comes from `InNamespace` on the namespace-less key (two parents in different namespaces that reference
the same child name+GVK index under the same key) — covered by a bidirectional unit test. Same reverse-index
pattern as `SnapshotBoundContentFieldIndex` / `MapSnapshotContentToBoundSnapshots`. On an index-List error the
lookup returns no parents and logs, matching the previous fail-quiet behaviour; a stale cache can at most delay a
parent wake to the next cache event / poll backstop, never drop it silently.

**Validation (measured, after deploy).** SETS=10 warm, one tree, apiserver-audit attribution for the controller SA.

| resource (LIST/tree) | before | after |
|---|---|---|
| `storage.deckhouse.io/snapshots` | ~440 (#1 hotspot) | **~2** (out of top-15) |
| total audited LIST/tree | ~1802 | **1352** (−~450, the snapshots delta) |
| root Ready (server-side) | ~24–25s wall | **21s** (no regression) |
| `field label not supported` | — | **0** |
| controller `level=error` / restarts / leader loss | — | **0 / 0 / 0** |

nss relay still wakes the root event-driven (relay reconciles observed, tree reached `Ready`). The residual ~2
`snapshots` LISTs are occasional cache-miss/resync, not the per-child-event storm. Remaining LIST load is now the
demo source/snapshot enumerations (VM / VMSnapshot / DiskSnapshot / Disk ~250–272 each, ~1060/tree) — the next
layer (see below).

**Portable conclusion:** a child→parent relay must find its parents by a **direct reference on the event object**
or a **cached field index**, never a full-namespace `List`+decode on every child event. The parent set is authored
by the parent controller and is unaffected by the triggering child write, so it is index-serviceable (this is a
relationship lookup, not a freshness read). The `~440 → ~2 LIST/tree` figures are workload-dependent; the
invariant is "reverse lookups use indexes/direct references, not full traversal".

### Next open item — remaining repeated full-collection operations

> **Status: Historical framing. Final status: API-load / scalability track CLOSED.** The two remaining candidates
> were rejected (see section 7); the remaining child-snapshot LIST path is intentionally freshness-bound. Portable
> rule: `[CO-DIRECT-REF]` with the correctness caveat in `[CO-APIREADER]`.

**Reframed (was "find another APIReader").** The five closed load fixes (D.3 `N Get→1 List`; H4.1 dead reverse
indexes; CSD `APIReader List`→cache; H5 duplicate pre-MCR sweep; relay `SnapshotList`→field index) are one
pattern, not five accidents: **wherever a "search by full traversal" existed, it was replaced by direct
addressing (index / direct-ref / cache).** So the next chapter is **not** "hunt for another APIReader" — the
client kind (APIReader, dynamic client, `Client.List`, unstructured `List`) is irrelevant. The question is:
**is a full-collection traversal being done where an index, a direct reference, or a local cache would serve the
same source of truth?** Concretely, the open target is the child-subtree enumeration load: the demo source and
snapshot full-lists (VM/VMSnapshot/DiskSnapshot/Disk ~250–272 LIST/tree each, ~1060/tree combined) plus the
audit-hidden individual GETs. Attribution (read-only) already located all four at **one callsite family**:
child-graph planning, ~1 List per GVK per pass × ~272 planning passes/tree, all in the **pending window** before
`ChildrenSnapshotReady=True/Completed` (each `nss-chw` relay wake on a child-snapshot status event re-plans).

Two candidate levers were considered; the first is **rejected**:

- **Skip source re-discovery after first publish — REJECTED** (section 7, "Source-skip"): `childrenSnapshotRefs`
  before `Completed` is not a frozen membership set (full recompute per pass, grows/shrinks across priority
  layers, first publish can be partial). The only valid freeze is `True/Completed/generation`, already exploited
  by `childGraphReplanSkippable`. Do not skip earlier.
- **Serve child-snapshot readiness reads (`childSnapshotReadCache`, VMSnapshot/DiskSnapshot ~542 LIST/tree) from
  the informer cache — REJECTED** (section 7, "`childSnapshotReadCache` via informer cache"). Proven read-only:
  the ~542 LISTs are one shared per-GVK list feeding coverage/dedup + existence-before-create + readiness, so they
  cannot be removed for readiness alone; moving the shared list to cache makes create/dedup stale (stale-NotFound →
  `Create` → `AlreadyExists`, not ignored → `GraphPlanningFailed`/root-`Ready` flap). Case B: freshness-bound as
  implemented. The source lists (VM/Disk via `r.Dynamic`) have no informer and are not a candidate.

**API-load chapter conclusion.** All obvious hot LIST candidates are either fixed (QPS/Burst, FIX 8, D.3, H4.1,
CSD planning cache, H5 single-flight, relay reverse-lookup index) or rejected (source-skip; `childSnapshotReadCache`
via informer cache). The remaining child-snapshot LIST path is **freshness-bound and intentionally left on the
APIReader/uncached `r.Client`**. This closes the API-load / scalability track: further reduction would require the
multi-part correctness work listed in the two rejected entries, not a load-only cache swap.

### Deferred — registry-derived planning view (architectural cleanup, NOT required)

> **Status: Historical optimization proposal. Do not implement** unless a "Return criteria" below holds. The
> performance problem it targeted is already solved by the cached-CSD fix; this is an architectural nice-to-have.

**Status: Deferred — deliberately not implemented after the cached-CSD planning fix (section 6) already removed the
hot apiserver LIST.**

**Context.** The original intent for the per-pass CSD-LIST hot spot was to remove it by **extending the registry**:
compute `EligibleResourceSnapshotMappings` during `Provider` refresh and expose the resolved mappings through
`LiveReader`, so planning reads a pre-built view. On inspection the maintained registry (`snapshot.GVKRegistry`,
built in `snapshotgraphregistry/build.go`) carries only `Snapshot GVK ↔ SnapshotContent GVK` + `DataBacked` — it
does **not** hold the `SourceGVR/SourceGVK/SnapshotGVR/Priority` that `EligibleResourceSnapshotMapping` needs, so
"just read the registry" was not literally possible without extending its data model. Instead the minimal fix was
applied: planning lists CSD via the cached `mgr.Client` instead of the uncached `APIReader` (section 6).

**Result.** The hot apiserver LIST is gone without any registry change. Planning still calls
`EligibleResourceSnapshotMappings(...)` each pass, but now over controller-runtime cache data rather than a direct
apiserver call.

**Why the registry-based variant is deferred.** After the switch to the cached client the original performance
problem is solved. The remaining registry-derived variant is an **architectural improvement, not a performance
fix**. Potential benefits: a single source of truth for the planning view; no cached CSD List during reconcile at
all; O(1) eligible-mappings read; one shared lifecycle for dynamic watches and the planning snapshot. Cost:
extend `LiveReader`; change the `Provider` refresh pipeline; maintain an atomically-updated derived mappings
projection; update `Static` (test reader) and the integration refresh hook (`ReplaceCurrent` does not currently
recompute mappings); extra unit/integration tests. It also introduces a new desync risk (watches refreshed, CSD
registry refreshed, but derived mappings forgotten / non-atomically swapped / Static behaves differently).

**Return criteria (implement only if at least one holds):**
- planning becomes CPU-bound from recomputing mappings per pass;
- a single immutable registry snapshot is required for planning;
- strong consistency between registry refresh and the planning view becomes necessary;
- new derived planning structures appear that are naturally computed once per refresh.

Until one of these applies, the cached-CSD planning fix is considered sufficient.

### QPS/Burst saturation sweep — production default 200/400 (CLOSED)

> **Status: Final recommendation.** Production default = **200/400** (saturation knee; 500/1000 gives no gain).
> Env-configurable. Portable rule: `[CO-QPS]` (section 12.2). The exact table numbers are workload-dependent; the
> invariant is the knee shape and that QPS removes *artificial throttling*, not structural work.

**Status: measured and closed.** The client-go rate limit is now **env-configurable** (defaults preserved, fail-fast
on invalid values, startup logging unchanged) and a QPS→Ready sweep resolved the open "pick a production QPS/Burst
deliberately" item (FIX 1 caveat / "Adjacent future work"). **Verdict: production default = `200/400`.** `500/1000`
gives **no** additional gain over `200/400` — it only adds apiserver pressure for free.

**Env knobs (override at runtime; defaults are the documented production values).**

| component | client | env QPS | env Burst |
|---|---|---|---|
| state-snapshotter controller | manager | `STATE_SNAPSHOTTER_KUBE_QPS` | `STATE_SNAPSHOTTER_KUBE_BURST` |
| state-snapshotter controller | capture dynamic | `STATE_SNAPSHOTTER_CAPTURE_QPS` | `STATE_SNAPSHOTTER_CAPTURE_BURST` |
| domain-controller | manager | `DOMAIN_CONTROLLER_KUBE_QPS` | `DOMAIN_CONTROLLER_KUBE_BURST` |

Invalid (non-numeric / non-positive) values fail fast at startup rather than silently falling back to the client-go
default (5/10). `storage-foundation` was **not** part of this experiment and is intentionally untouched.

**Experiment 1 — lift 50/100 → 500/1000, N=10/20/50 (does the limiter bind at all?).** Server-side per-tree phase
means (`lastTransitionTime`; single run per N; 0 errors / 0 restarts / 0×429):

| N | phase | 50/100 | 500/1000 | Δ |
|---|---|---|---|---|
| 10 | A cre→CSR | 8.2 | 7.0 | −15% |
| 10 | B CSR→Arch | 9.6 | 4.6 | **−52%** |
| 10 | C Arch→Rdy | 0.3 | 1.2 | noise |
| 10 | **total** | 18.1 | 12.8 | −29% |
| 20 | A cre→CSR | 17.9 | 18.3 | ~0 |
| 20 | B CSR→Arch | 16.5 | 5.0 | **−70%** |
| 20 | C Arch→Rdy | 3.5 | 0.1 | −97% |
| 20 | **total** | 37.9 | 23.4 | −38% |
| 50 | A cre→CSR | 47.2 | 35.0 | −26% |
| 50 | B CSR→Arch | 41.4 | 34.1 | −18% |
| 50 | C Arch→Rdy | 13.5 | 0.8 | −94% |
| 50 | **total** | 102.1 | 69.9 | −32% |

![Phase decomposition, default 50/100 vs 500/1000, N=10/20/50](./qps-phase-overlay.png)

The per-tree total slope drops from **2.11 → 1.46 s/tree (~31% flatter)** but the line does **not** go flat: raising
QPS is a downward shift + slope reduction, not a switch to a flat regime.

**Experiment 2 — QPS sweep at N=20 (pick the minimal production value).** Same harness, N=20 only, one run per
point:

| QPS/Burst | A | B | C | **total** | wall |
|---|---|---|---|---|---|
| 50/100 | 17.9 | 16.5 | 3.5 | **37.9** | 62s |
| 100/200 | 18.1 | 8.0 | 0.1 | **26.2** | 45s |
| 200/400 | 17.4 | 5.8 | 0.1 | **23.4** | 36s |
| 500/1000 | 18.3 | 5.0 | 0.1 | **23.4** | 40s |

![QPS→Ready saturation curve at N=20](./qps-saturation-curve.png)

Classic saturation knee at **200**: 50→100 = **−31%**, 100→200 = **−11%**, **200→500 = 0%**. `200/400` captures the
entire win (total 23.4 = same as 500/1000).

**Per-phase verdicts (this is the more valuable result than the QPS number itself).**

- **Phase C (`ManifestsArchived → Ready`) — CLOSED, was API-limiter-bound.** Collapsed 13.5 → 0.8s (N=50) and
  effectively vanishes by QPS 100 (3.5 → 0.1 at N=20). There is almost no real work in C (tree already archived,
  children ready, only the final Ready mirror) — the time was purely waiting for a limiter token before a
  GET/PATCH. Further optimization of C is pointless.
- **Phase B (`ChildrenSnapshotReady → ManifestsArchived`) — partly QPS-bound.** Saturates by ~200 (16.5 → 8.0 →
  5.8 → 5.0 at N=20); the residual ~5–6s is the content-controller's **own** throughput, not the limiter. **But at
  N=50 B re-inflates (5.0 at N=20 → 34.1 at N=50)** — a second ceiling that QPS 500 does not lift. That ceiling is
  reconcile/worker-saturation + requeue churn (content controller N=50: ~5465 reconciles, ~4599 `requeue_after`,
  0.145 s/rec), not REST QPS.
- **Phase A (`creation → ChildrenSnapshotReady`) — QPS-independent.** Flat across the entire sweep at N=20
  (17.9 / 18.1 / 17.4 / 18.3) and still ~linear in N (7 → 18 → 35). This is **not** on the API limiter at all — it
  is the genuine reconcile-throughput limit of child-graph planning + child materialization + child readiness,
  exactly the residual the earlier API-load track (D.3/H4.1/CSD/H5/relay-index) cleaned the noise away from. **Phase
  A is now the dominant remaining term and the next candidate is `MaxConcurrentReconciles`, not QPS.**

**Why this validates the whole prior sequence.** With API-load noise removed (five closed load fixes) and the
artificial limiter lifted, the pipeline splits cleanly into a QPS-bound tail (B/C, now handled) and a
reconcile-throughput term (A, and B at high N). The architecture was sound; it was throttled by a stack of overhead,
not by a fundamental design flaw.

**Priority reset.** Phase C is **closed**. Focus is A (primary) and B-at-high-N (secondary), both pointing at
`MaxConcurrentReconciles` / worker saturation / informer propagation — a local, easily-measured experiment to run
**after** fixing the QPS default (so only one variable changes at a time). `requeue_after` semantics are touched
last (they carry correctness), per the agreed order QPS → default → concurrency → requeue.

**Portable conclusion:** raising client QPS removes *artificial throttling*, not structural work — it is a
downward shift + slope reduction, never a switch to a flat regime. There is a saturation knee (here ~200/400)
beyond which more QPS only adds apiserver pressure. Phase C (final Ready mirror) is purely limiter-bound and
vanishes once QPS is adequate. The exact knee and per-phase seconds are workload-dependent; the invariants are
(a) a knee exists, (b) QPS is a capacity fix not a work fix, (c) after the knee the remainder is reconcile
throughput, addressed by concurrency/architecture, not more QPS.

### Concurrency / data-leg probes — throughput is exhausted as a latency lever (N=20, CLOSED)

> **Status: Historical investigation. Final status: CLOSED.** Portable conclusion: once ss/domain QPS is at the
> knee, **no throughput knob (foundation VCR workers, foundation QPS) moves the N=20 wall** — they are env
> infrastructure, not latency levers. Also establishes the methodology caveat: single-run A/B splits are noise.

**Status: measured and closed.** With `ss/domain` QPS fixed at the 200/400 knee, three further throughput levers
were probed to test whether the residual N=20 wall (~26s, Phase-A-dominated) is throughput-bound. Two env knobs were
added and kept as **infrastructure** (defaults unchanged): foundation VCR `MaxConcurrentReconciles`
(`STORAGE_FOUNDATION_VCR_MAX_CONCURRENT_RECONCILES`, default 4) and foundation client QPS/Burst
(`STORAGE_FOUNDATION_KUBE_QPS`/`_BURST`, default 50/100).

| lever | range tested | N=20 wall (Ready-offset) | verdict |
|---|---|---|---|
| ss/domain client QPS | 50 → 200 → 500 | 38 → **26** → 26 | knee at 200 (real, already applied) |
| foundation VCR workers | 4 → 8 → 16 | ~26 (flat) | not a lever |
| foundation client QPS | 50 → 200 | ~26 (flat) | not a lever |

- **No throughput lever moves the N=20 wall below ~26s** once ss/domain QPS is at 200/400. VCR 4→8→16 left Phase A
  at a 17.4 → 16.1 → 15.9 plateau (≤ noise); foundation QPS 50→200 left the wall unchanged.
- **Methodology caveat (important, applies to the whole doc): single-run per-phase A/B is noise-dominated at
  N=20.** Two runs of the *identical* foundation-QPS=200 config gave `A=12.5 / B=13.8` and `A=17.9 / B=4.6` — A and B
  **anti-correlate** and swing ±5–8s with batch staggering (a tree that starts late has a long A and short B, and
  vice-versa). Only **total** and **Ready-offset (wall)** are trustworthy from a single run; any A-vs-B split must be
  averaged over ≥3 runs before it is used for a conclusion. (The QPS-sweep *total* trend 38→26→23 remains valid — it
  is large and monotonic, well above the swing.)
- The concurrency/QPS knobs are retained as **env-configurable infrastructure** for production tuning, **not** as a
  validated latency win; live defaults are unchanged (VCR 4, foundation QPS 50/100).

**Conclusion: throughput is exhausted as a latency lever.** Client QPS (all three clients) and reconcile worker
counts do not reduce the residual ~26s N=20 wall. The residual is **structural** — staged readiness propagation plus
the demo-chain 500ms `RequeueAfter` poll cadence (see next). Further QPS/concurrency tuning is diminishing returns.
The investigation therefore moves from *throughput* to the *architecture of readiness propagation*.

### Next — readiness-propagation poll audit (read-only): Phase A is already event-driven — OPEN

> **Status: Historical diagnosis retained for context only** (the "OPEN" in the title is superseded). Final
> finding: **Phase A is already event-driven** (30s poll is a missed-event backstop, not the pacing mechanism);
> the 500ms cadence lives in Phase B, not Phase A. Portable rule: `[CO-EVENT-WAKE]`.

**Hypothesis tested.** The residual Phase-A / wall is paced by a `500ms` self-requeue cadence in the demo/domain
controllers waiting for children to become Ready/Archived.

**Read-only finding — the premise does not hold for Phase A.** A full audit of every `RequeueAfter` on the create
path shows the demo/domain chain and the root child-graph barrier (= Phase A, `creation → ChildrenSnapshotReady`)
are **already event-driven**, with polls used only as missed-event backstops:

| callsite | interval | phase | role |
|---|---|---|---|
| `snapshot/controller.go` `snapshotChildGraphPollInterval` | **30s** | **A** | missed-event **backstop** only; primary wake is the per-GVK child watch relay |
| `snapshot/dynamic_watch.go` (`nss-chw-*`) + `child_snapshot_watches.go` | event | **A** | per-child-snapshot-GVK watch (allow-all predicate, status-only updates fire) → reverse field-index (`SnapshotChildrenRefFieldIndex`) → enqueue parent |
| `demo/virtualmachinesnapshot_controller.go` | event | **A** | watches child `DemoVirtualDiskSnapshot`; `MarkPlanningReady` in one pass (does **not** wait for child readiness) |
| `demo/virtualdisksnapshot_controller.go:135` `defaultDemoSnapshotRequeueAfter` | 500ms | **A** | **not hot path** — fires only when the source PVC is absent (never on a Ready stand); happy path is one-shot plan → `MarkPlanningReady` |
| `demo/virtualmachine_controller.go`, `virtualdisk_controller.go` `defaultDemoResourceRequeueAfter` | 500ms | stand-setup | source VM/Disk lifecycle (stand readiness, **before** any snapshot) — not on snapshot Phase A |
| `snapshotcontent/controller.go` `defaultSnapshotContentRequeueAfter` | 500ms | **B** | **the real poll cadence** — bottom-up `ManifestsArchived` staircase; every not-ready node self-requeues 500ms (already has partial content/MCP/artifact watches, 500ms is the backstop) |
| `snapshot/volume_capture.go` `requeueVolumeCaptureIf` | 500ms | **B** | volume data leg (VolumeSnapshot/VCR pending, safe-delete-after-handoff) |
| `snapshot/ready_patch.go:87` | 500ms | C | terminal child-failed Ready bridge (rare) |

**Conclusion.** There is **no 500ms poll on the Phase-A critical path** — Phase A is event-driven with a 30s
missed-event backstop, and the demo controllers are event-driven single-pass planners. Converting a "demo 500ms
poll" would therefore **not** move the dominant Phase-A term. The only genuine 500ms staircase is in the
`SnapshotContent` controller (Phase B, bottom-up archive), which is the *smaller* term (~5.8s vs A's ~17.4s at N=20)
and was already found partly QPS-bound.

**Open question re-framed.** Since Phase A is already event-driven yet still ~17s at N=20, the residual is either
(a) the 30s child-graph backstop landing on the critical path when a relay wake is missed/lagged (would also explain
the large single-run A/B variance), or (b) genuine reconcile-throughput on a shared workqueue at N × (VMs+disks). The
cheap next diagnostic is to instrument, per root, whether `ChildrenSnapshotReady=True` is driven by a relay wake or
by the 30s poll (and measure inter-priority-layer wake latency) — this decides between *fix a laggy relay* and
*genuine reconcile work → domain `MaxConcurrentReconciles` with proper ≥3-run statistics*. **Do not** blanket-shrink
500ms → 50ms anywhere (hides the problem, multiplies reconcile churn).

**Instrumentation added (diagnosis-only, off by default).** Env `STATE_SNAPSHOTTER_PHASE_A_TRACE=1` turns on
`phaseA-trace` Info logs in the Snapshot controller (`internal/controllers/snapshot/phase_a_trace.go`); no behaviour
change when unset. Events and key fields:
- `root-reconcile-entry`: `wakeSource` (`relay` | `relay-delete` | `queue`), `gapSincePrevMs`, `viaBackstop`
  (true when a `queue` wake lands ~30s after the previous reconcile = the child-graph poll fired, not a child
  event), and `relayLatencyMs` (child `ChildrenSnapshotReady=True` lastTransitionTime → parent reconcile) on relay
  wakes;
- `layer-ready`: per priority layer, `priority`, `layerSize`, `observeLagMs` (now − newest child ready in the layer);
- `root-children-ready`: `phaseATotalMs` (creationTimestamp → `ChildrenSnapshotReady=True`, the robust per-root
  number), `finalObserveLagMs`, `layers`.

Decision rule for the N=20 harvest: if roots/layers wake via `viaBackstop=true` or `relayLatencyMs`/`observeLagMs`
are large (seconds) → fix the relay/index/watch; if relay latency is small but Phase A is still large → the shared
reconcile queue is the ceiling → domain VMS/VDS `MaxConcurrentReconciles` sweep with 3–5 runs per point.

### Result (N=20, trace ON): Phase A is serialized on reconcile-worker concurrency, not poll-paced — CLOSED (diagnosis)

> **Status: Historical diagnosis. Final status: CLOSED (superseded by the critical-path trace below, which
> localizes the serializer to the disk layer).** Portable finding: the 30s backstop is **never** on the critical
> path; Phase A serializes on reconcile-worker throughput, not polling.

Single N=20 run, 20 root Snapshots fired simultaneously (stand pre-Ready), 794 `phaseA-trace` lines harvested:

| signal | value | reading |
|---|---|---|
| `viaBackstop=true` entries | **0 / 545** | the 30s child-graph poll is **never** on the critical path — the relay wakes parents |
| `finalObserveLagMs` (last child ready → root publishes) | p50 **1.68s**, max 2.27s (flat) | the completing hop is healthy and does **not** grow with load — relay/index/watch are fine |
| `phaseATotalMs` (creation → `ChildrenSnapshotReady`) per root | **1.3s → 28.6s** linear staircase, p50 23.3s | roots created at the same instant complete in strict rank order = **serialization**, not per-root cost (rank-1 root finishes its Phase A in 1.3s) |
| `relayLatencyMs` (triggering child ready → parent reconcile) | p50 5.4s, **p90 33.5s** | deep **queue backlog**: child events wait a long time before their parent reconcile runs |

![Phase A serialization staircase, N=20](phaseA-serialization-N20.png)

**Verdict — decision-rule branch (b): throughput-bound on reconcile-worker concurrency.** Phase A is *not*
poll/backstop-bound (0 backstop wakes) and the relay mechanism is *correct* (flat ~1.5s final-hop lag); the residual
is pure **serialization** — 20 simultaneously-created roots drain through concurrency-limited reconcile workers in a
1.3→28.6s staircase, and the relay path shows a p90 33s queue backlog. Relevant ceilings: demo `DemoVirtualMachineSnapshot`
/ `DemoVirtualDiskSnapshot` reconcilers run at `MaxConcurrentReconciles: 4`; the root `Snapshot` controller at 8; and
each per-child-GVK relay controller (`nss-chw-*`, `dynamic_watch.go`) runs at the controller-runtime **default of 1**
(no `WithOptions`), funnelling every namespace's child-snapshot events through a single goroutine per GVK.

**Next (per branch b), cheapest-first:**
1. **Relay `nss-chw-*` concurrency** (the concrete suspect at default 1) — now env-configurable via
   `STATE_SNAPSHOTTER_RELAY_MAX_CONCURRENT_RECONCILES` (default 1 = no behaviour change). Sweep 1 → 4 → 8 and
   re-measure. This is the cheapest test; if wall/`phaseATotalMs` drops sharply the relay funnel was the ceiling,
   if not the hypothesis is dropped.
2. Domain VMS/VDS `MaxConcurrentReconciles` (env-configurable, 4 → 8 → 16) if the relay bump did little.
3. Root `Snapshot` controller concurrency if neither moved it.

Judge on `phaseATotalMs` distribution and wall (≥3 runs per point), not single-run A/B. The staircase proves a
serializing bottleneck **exists**; *where* it sits is the hypothesis these sweeps isolate.

### Relay `nss-chw` concurrency sweep 1 → 4 → 8 (N=20, 3 runs each) — hypothesis DROPPED

> **Status: Historical investigation. Final status: hypothesis DROPPED.** Portable conclusion: **relay latency was
> not the bottleneck** — raising relay concurrency cut the relay's own queue latency but left the wall flat, so the
> serializer is downstream of the relay. Numbers are workload-dependent; the invariant is "sub-metric moved, wall
> did not".

The relay is a real controller-runtime Controller (`ctrl.NewControllerManagedBy(...).Complete(relay)`, its own
workqueue + reconcile loop; one per child GVK), so `MaxConcurrentReconciles` applies directly. Verified with the
env knob above; means over 3 runs per point:

| relay conc. | wall (mean) | phaseATotalMs p50 (mean) | phaseATotalMs max (mean) | relayLatMs p50 | relayLatMs p90 | backstop |
|---|---|---|---|---|---|---|
| 1 | 36.4s | 19.5s | 26.7s | 9.1s | **19.8s** | 0 |
| 4 | 37.2s | 19.8s | 26.5s | 5.4s | 14.2s | 0 |
| 8 | 39.0s | 15.9s | 24.3s | 6.4s | 14.0s | 0 |

**Verdict: the relay funnel is NOT the binding constraint.** Raising relay concurrency measurably cut the relay's
*own* queue latency (`relayLatP90` ~20s → ~14s, `relayLatP50` ~9s → ~5-6s) — proving the extra workers do drain the
relay queue — **but the wall did not drop** (36→39s, flat/within noise) and `phaseATotalMs` barely moved (p50
19.5→15.9s, max flat, all inside the run-to-run spread already seen at relay=1). So reducing the child→parent wake
latency does **not** advance Phase-A completion: the serializing bottleneck sits **downstream** of the relay. The
env knob is kept (default 1; the relay-latency win is real but harmless and does not justify a new default).

**Next (unchanged ladder):** the remaining suspects are the demo `DemoVirtualMachineSnapshot` /
`DemoVirtualDiskSnapshot` reconcilers (`MaxConcurrentReconciles: 4`, hardcoded) and the root `Snapshot` controller
(8). Make the domain VMS/VDS concurrency env-configurable and sweep 4 → 8 → 16 (≥3 runs), judged on `phaseATotalMs`
and wall.

### Critical-path trace of a single N=20 run — the last serializer is the DISK layer (H4)

> **Status: Historical diagnosis — the final localization of the Phase-A remainder.** Portable conclusion: Phase A
> is a **planning barrier** serialized on the **disk-snapshot layer** (H4 ≈ half the median path); the VM layer is
> flat and the relay is non-binding. Carried forward as `[CO-PHASEA-STRUCTURAL]` (section 12.2). Exact seconds are
> workload-dependent; the invariant is *which hop staircases*.

Instead of another broad sweep, one N=20 run was reconstructed hop-by-hop from **server-side timestamps**
(`creationTimestamp` + condition `lastTransitionTime`, 1 s) cross-checked against the ms `phaseA-trace`. Phase A is
a **planning barrier**: root `ChildrenSnapshotReady=True` waits only for the domain snapshots to be *created* and
publish CSReady (layer by layer, VM priority 100 → disk priority 10); it does **not** wait for MCR/VCR/VSC `Ready`.
Every object in namespace `pa…-i` belongs to tree `i`, so per-tree correlation is by namespace. The path is five
hops (`phaseA-critpath-N20.png`):

| hop | p50 | share | gated by |
|---|---|---|---|
| H1 rootPlanVM | 1.0s | 6% | root(8) |
| H2 vmReconcile | 2.0s | 12% | domain VMS(4) |
| H3 rootPlanDisk (observe L100 → create L10) | 4.0s | 25% | relay(1)+root(8) |
| **H4 diskReconcile (disk created → CSReady)** | **7.5s** | **47%** | **domain VDS(4)** |
| H5 rootPublish | 1.5s | 9% | relay(1)+root(8) |

phaseATotal p50 = 20.5s (server-side) / 21.2s (ms trace) — matches the baseline, so the single run is
representative and the decomposition is structural, not A/B noise.

**Staircase test (decisive):** the VM layer (`VMsnap.created`/`CSReady`) stays low and flat across all 20 trees —
the VM layer does **not** serialize. The tree-to-tree staircase is entirely on the **disk layer**: the diverging
gap `disk.created` → `disk.CSReady` (= H4) grows from ~0 to ~13 s. H4 owns 47% of the median path and is where the
cross-tree delay accumulates.

**Relay-sweep paradox resolved:** `relayLatencyMs` p50≈10s / `observeLagMs` p50≈8s are genuinely large, yet
raising relay concurrency did not move the wall (previous section). The relay latency **overlaps behind** the VDS
throughput floor: speeding the relay only makes roots wait on VDS workers instead. The **binding** floor is
**H4 = the `DemoVirtualDiskSnapshot` reconcile throughput (`MaxConcurrentReconciles=4`)**. The harvest found ~0
persisted MCR/VCR at Phase-A end, so H4 is dominated by **queue-wait for a free VDS worker**, not heavy per-reconcile
work.

**Verdict:** the last Phase-A serializer is domain **disk-snapshot** reconcile throughput (VDS pool = 4), with the
relay/root layer transition (H3) second but proven non-binding. This maps to the
`DOMAIN_CONTROLLER_SNAPSHOT_MAX_CONCURRENT_RECONCILES` lever (already committed). A 4 → 8 → 16 sweep would only
distinguish worker-bound (H4/wall shrink) from intrinsic per-reconcile cost (H4 flat) — it would not explain the
cause, so it is **optional and not required** (see "Concurrency campaign — CLOSED" below).

![Phase-A critical-path trace, N=20](phaseA-critpath-N20.png)

**Portable conclusion:** Phase A is a **planning barrier over sequential priority layers**; its residual latency is
**structural serialization on the layer with the lowest reconcile throughput** (here the disk layer), found by the
staircase test — the hop whose per-tree gap diverges while upstream layers stay flat. A large relay/observe latency
can be a **red herring** if it overlaps behind that throughput floor (speeding it up just re-queues the wait). The
exact hop shares (H4≈47% etc.) are workload-dependent; the invariants are (a) locate the dominant hop by the
staircase, not by which latency looks biggest, and (b) the fix lever is that layer's concurrency/structure, not
QPS/relay.

### Concurrency campaign — CLOSED (STOP)

> **Status: Final recommendation.** The throughput/concurrency campaign is closed: QPS was the only lever that
> moved the wall; the remainder is structural disk-layer serialization; the domain sweep is optional. This is
> current guidance. The deep ms-level trace is a **separate future task**, opened only under section 12.4.

Decision: **close the throughput/concurrency optimization campaign.** The evidence is complete enough for a firm
conclusion:

- **QPS/Burst was the real win** — the client-go rate limiter was the one throughput lever that moved the wall
  (−32…−38%); production default settled at 200/400 (knee).
- **Every other throughput knob does NOT move the wall:** foundation VCR workers, foundation QPS, the 500 ms
  poll/backstop, and relay `nss-chw` concurrency each moved a local sub-metric but left the wall flat. The **relay
  hypothesis is dropped**.
- **The remainder is structural**, not a throughput knob: Phase A serializes on the **disk-snapshot layer** (H4 ≈
  47% of the median path); the VM layer is flat and the relay is non-binding.
- **The domain VMS/VDS concurrency sweep is NOT required.** Only the CONC=4 baseline was run (3× — reconfirmed H4
  dominance, `phaseA_total` p50 ≈ 13–20 s, H4 p50 ≈ 7–13 s). A 4 → 8 → 16 sweep would answer only
  "helped / didn't help"; it would **not explain the cause**, so its expected value is low. The
  `DOMAIN_CONTROLLER_SNAPSHOT_MAX_CONCURRENT_RECONCILES` env lever stays as committed infrastructure.

**If the goal shifts from "improve the metric" to "understand the nature of the remainder", that is a separate
task — "Phase A critical-path trace" (deep / ms-level).** The trace above localized the serializer from server-side
1 s timestamps; a causal explanation needs per-reconcile instrumentation on a single N=20 tree timeline:
root created → VMS reconcile start/end → VDS reconcile start/end → child Snapshot created → child Ready → relay
received → parent reconcile start/end → `ChildrenSnapshotReady=True` → MCR/MCP created/ready → `ManifestsArchived=True`.
Goal: see exactly where the staircase forms — domain controller vs root controller vs layer dependency vs API
update/read-after-write vs inline reconcile. Not started; open only on explicit request.

---

## 9. Application checklist

> **Reading key (machine-oriented).** Each item is tagged: **[Mandatory carry-over]** = must port on any rework;
> **[Recommended carry-over]** = port unless the rework makes it moot; **[Historical]** = already absorbed
> elsewhere, no separate action; **[Do not re-implement]** = deliberately rejected/deferred, port only under its
> stated return criteria. The order is the recommended application order.

1. **[Mandatory carry-over]** [FIX 1](#fix-1-do-first--raise-manager-client-qpsburst-to-50100) (QPS/Burst, 3 files) — biggest win, independent.
2. **[Mandatory carry-over]** [FIX 6](#fix-6--concurrency-ceilings--the-one-required-correctness-fix) (concurrency + `configMu` guard) — before/with the event watches.
3. **[Mandatory carry-over]** [FIX 2](#fix-2--mcr-controller-watch-manifestcheckpoint), [FIX 3](#fix-3--snapshotcontent-two-reverse-lookup-watches), [FIX 5](#fix-5--storage-foundation-vcr-watch-volumesnapshotcontent) (event-driven wake-ups) — any order; each keeps its poll/requeue backstop.
4. **[Recommended carry-over]** [FIX 8](#fix-8--genericbinder-reverse-watch-mappers-direct-ref-o1-routing) (genericbinder O(1) routing) — load fix.
5. **[Recommended carry-over]** [ARCH 1](#arch-1--snapshot-controller-wake-the-gated-parent-on-child-content-archive) (gated-parent wake) and [ARCH 2](#arch-2--single-snapshotcontent-controller-with-dynamic-snapshot-status-watches-h2) (single content controller) — apply as architecture/correctness; not
   headline latency fixes.
6. **[Recommended carry-over]** Confirmed optimizations (section 6) — keep; hygiene.
7. **[Mixed — Mandatory carry-over (D.3, H4.1); Do not re-implement (D.2, D.1′, rejected hypotheses)]** H3 (section 8) — **closed** by Commit D.3 (per-child `Get`s → one `List`/GVK; validated SETS=10 warm). D.2 (membership-skip) and D.1′ (relay debounce) are **no longer recommended** — the premise is gone. **H4 — root manifest leg** (`ChildrenSnapshotReady` → `Ready`) — **closed** by Commit H4.1 (reverse-lookup `List`s routed through cache indexes; `field label not supported` errors and lost-wake tails eliminated; leg now stable event-driven ~9–10s, no restarts). Residual archive-propagation cadence is a lower-priority follow-up. **Active latency work is stopped** (STOP decision, section 8 under H4): no D.1′/D.2/debounce/cache/membership change is planned; reopen only if a production-scale trace shows a fresh dominant interval. Do not re-apply rejected hypotheses (section 7, incl. leaf-skip / distinct-H1).
8. **[Recommended carry-over]** **H5 — concurrent pre-MCR sweep race** (section 8) — **CLOSED (single-flight implemented + validated).**
   Per-`Snapshot`-UID in-process single-flight around the pre-MCR sweep: SETS=10 sweeps/root 2→1, SETS=20 3→1,
   redundant sweeps→0, concurrent MCR `AlreadyExists` race gone, Ready unchanged. Correctness-neutral concurrency
   dedup only (MCR-gate keeps temporal dedup; plan result not cached). ~8–9% of per-tree API load — not the
   dominant lever; the child-subtree GET (~90%) remains the next, higher-leverage investigation.
9. **[Mandatory carry-over (cached CSD list); Do not re-implement (registry-derived view)]** **CSD planning list cached** (section 6) — **implemented.** Removes ~208 apiserver CSD LIST/tree from
   child-graph planning by listing through `mgr.Client` instead of `APIReader`; semantics unchanged. The
   **registry-derived planning view** (section 8, "Deferred") is deliberately **not** implemented — see its return
   criteria before revisiting.
10. **[Mandatory carry-over]** **Relay reverse-lookup field index** (section 6 / section 8 "Relay reverse-lookup — CLOSED") — **implemented +
    validated.** `findParentsReferencingChildSnapshot` uses a cached `status.childrenSnapshotRefs.identity` index
    instead of a per-child-event full-namespace `SnapshotList`: primary relay LIST eliminated (~440 → ~2
    `storage/snapshots` LIST/tree), 0 field-label errors, 0 restarts, root Ready ~21s. Child freshness `Get` stays
    on the APIReader. Next open item reframed to **remaining repeated full-collection operations** (child-subtree
    enumerations), not "another APIReader".

## 10. Tooling (storage-e2e, measurement only)

- Namespace fan-out benchmark `tests/snapshot-latency/namespace_scale_test.go` (N independent standard sets, one
  root Snapshot) and per-object trace `trace_scale_test.go` (per-content manifest-leg / content-side-lag).
- SSH client fix presenting the adjacent OpenSSH certificate: `internal/infrastructure/ssh/client.go`
  (`loadCertSigner`) so cert-only clusters connect.
- The harness builds its own SSH tunnel to a cluster node; if that node's key/user changes (e.g. after a node
  redeploy) the harness cannot connect. The same benchmark can be driven directly via `kubectl` against a working
  kubeconfig, taking timings from server-side condition `lastTransitionTime` relative to the Snapshot
  `creationTimestamp` (immune to client-side polling lag). Note a **cold-start artifact**: the first namespace
  snapshot after an idle/fresh controller can show a one-off inflated root-manifest leg (~80s at SETS=1) that
  disappears on warm runs (~4s) — warm up before measuring.

These produced the numbers above; re-apply only if `storage-e2e` is also reset.

---

## 11. Appendix — APIReader audit (switch to cache vs keep uncached)

Part of the L4 load-shaving (section 6). The win is replacing `r.APIReader.Get` with the cached `r.Client.Get`
**only** on event-driven mirror reads of a **watched** object, where a stale cache costs at most one extra
reconcile and the object is watched so the mirror re-fires. Everything else uses APIReader for a **correctness**
reason and must stay uncached.

**Switch to cached `r.Client.Get` — the ONLY reads the audit found safe to cache (all applied):**

| file | function | read that was switched |
|---|---|---|
| `genericbinder/controller.go` | `checkConsistencyAndSetReady` | bound SnapshotContent GET by `contentKey` (Ready-mirror): `r.APIReader.Get` → `r.Client.Get` |
| `snapshot/content_reader.go` | `getSnapshotContentCached` (new; used by the mirrors below) | bound SnapshotContent for the Ready/ManifestsArchived mirror |
| `snapshot/ready_patch.go` | `mirrorSnapshotReadyFromBoundContent`, `mirrorSnapshotManifestsArchivedFromBoundContent` | `getSnapshotContentFresh` → `getSnapshotContentCached` |

Rationale: SnapshotContent is watched (its status change re-enqueues the bound Snapshot), so these mirrors are
event-driven and a stale cache costs at most one extra reconcile before convergence; `INV-RECONCILE-TRUTH` is the
backstop. This is the whole L4 win — do not extend it beyond these three.

**KEEP as `r.APIReader` — the audit concluded these NEED the uncached read (do NOT "optimize"):**

- **UID-barrier reads of ObjectKeeper after creation** — `genericbinder/controller.go` (~545/599),
  `manifestcapture/checkpoint_controller.go` (~369): must read-after-write the just-created ObjectKeeper UID; a
  cache miss breaks the barrier.
- **Read-after-write existence check of the MCR** — `snapshot/capture.go` (~182): the split-client cache can lag
  a just-created MCR; the gate relies on the uncached read (Create tolerates AlreadyExists).
- **Read-after-write child GET in the binder** — `genericbinder/controller.go` (`childObj` GET, ~826): kept
  uncached deliberately; NOT the mirror read above.
- **`declaredNonLeafChildContentNames` owner reads** (`snapshotcontent`): correctness-critical for the one-way
  `ManifestsArchived` latch — deferred by T-cost, never cached. This is also the read that keeps ARCH 2's
  snapshot-status wake necessary.
- **Edge-set preserve reads** — `snapshotcontent/status_publish.go`, `volume_child_content.go`: current published
  edge set read uncached so it reflects edges just written by the other writer.
- **Internal-only manifest/chunk reads** — `usecase/archive_service.go`, `usecase/import_manifest_reconstruct.go`:
  ManifestCheckpoint + chunks have no informer; they must bypass the cache like the `/manifests` API server.
- **Other binder content/child reads** (`genericbinder` `import.go`/`domain_content.go`,
  `PublishSnapshotContentChildrenFromSnapshotRefs`, safe-to-delete checks): reviewed and left on APIReader — they
  gate planning/binding/deletion where cache lag would risk a wrong decision.

Conclusion: the only unnecessary uncached reads were the three watched-object mirror reads above (now cached).
Every other `APIReader` use is a deliberate correctness choice (UID barrier, read-after-write, one-way latch,
edge-preserve, or informer-less internal objects) and must stay.

---

## 12. Carry-over to a reworked `main` — portable bottleneck playbook (tag-walk)

> **Status: Active carry-over guidance. This is the primary abstraction layer of the document.**
> If the implementation is completely rewritten and every file/function reference elsewhere in this document
> becomes invalid, **this section alone should be sufficient to rediscover and correctly re-apply all validated
> optimizations.** Everything above is evidence and history for these principles; this is what to port.

**Why this section exists.** The rest of the document is a re-application guide keyed to `file:line` / function
names on the *current* code. When `main` is substantially reworked those anchors rot: files move, functions are
renamed, controllers are merged/split. This section re-expresses every durable finding as a **code-agnostic
pattern** you can *walk the new tree by* — each item is a grep-able **tag** with (a) the invariant/bottleneck, (b)
**durable anchors** that survive a rework (CRD kinds, condition types, env knobs, controller-runtime API surface,
behavioural signatures — *not* line numbers), (c) how to **detect** it on the new code, (d) the **fix pattern**,
(e) how to **re-verify**. Work top-to-bottom; the order is the recommended application order.

### 12.0 Durable anchors glossary (these survive a rework — search for these, not line numbers)

- **CRD kinds:** `Snapshot`, `SnapshotContent` (`storage.deckhouse.io`); `DemoVirtualMachineSnapshot`,
  `DemoVirtualDiskSnapshot` (`demo.state-snapshotter.deckhouse.io`); `ManifestCaptureRequest`, `ManifestCheckpoint`
  (`state-snapshotter.deckhouse.io`); `VolumeCaptureRequest` (`storage.deckhouse.io`, reconciled by
  storage-foundation); `VolumeSnapshotContent` (`snapshot.storage.k8s.io`); `ObjectKeeper` (`deckhouse.io`).
- **Condition types:** `ChildrenSnapshotReady` (Phase A gate), `ManifestsArchived` (Phase B latch), `Ready`
  (Phase C mirror). **Reasons that are contract:** `Completed` (terminal-success), `PriorityLayerPending`
  (partial-plan). Readiness is **generation-guarded** (a `True` with `observedGeneration == generation`).
- **Phases (measure by server-side `lastTransitionTime` vs Snapshot `creationTimestamp`):** A = `creation →
  ChildrenSnapshotReady`; B = `ChildrenSnapshotReady → ManifestsArchived`; C = `ManifestsArchived → Ready`.
- **Env knobs (kept as infrastructure):** `STATE_SNAPSHOTTER_KUBE_QPS/_BURST`, `STATE_SNAPSHOTTER_CAPTURE_QPS/_BURST`,
  `DOMAIN_CONTROLLER_KUBE_QPS/_BURST`, `STORAGE_FOUNDATION_KUBE_QPS/_BURST`,
  `STORAGE_FOUNDATION_VCR_MAX_CONCURRENT_RECONCILES`, `DOMAIN_CONTROLLER_SNAPSHOT_MAX_CONCURRENT_RECONCILES`,
  `STATE_SNAPSHOTTER_RELAY_MAX_CONCURRENT_RECONCILES`; diagnostics `STATE_SNAPSHOTTER_PHASE_A_TRACE`,
  `STATE_SNAPSHOTTER_WATCH_MAP_STATS`.
- **Metrics:** `controller_runtime_reconcile_total`, `controller_runtime_reconcile_time_seconds_{count,sum}`,
  `workqueue_*`, `rest_client_requests_total`; apiserver audit `list`/`get` by the controller SA per tree.

### 12.1 Portable engineering principles (implementation-independent)

These are the general rules distilled from the whole investigation. They hold regardless of how `main` is
structured; the tag-walk below is their concrete application.

1. **Never replace one search with another search.** Replace a full-collection traversal with an *index*, a
   *direct reference*, or a *cached lookup* — not with a different `List`. (The client kind — APIReader, dynamic,
   `Client.List`, unstructured — is irrelevant; the traversal is the cost.)
2. **Polling is a safety net, never the primary synchronization mechanism.** Progress must be event-driven
   (watch/mapper); a `RequeueAfter` may only backstop a missed event.
3. **Cache only mirror reads of watched objects.** Planning, create, and dedup decisions require *fresh* reads
   unless their correctness proof is changed first (stale create/dedup reads flap `Ready`).
4. **Optimize only after identifying the dominant interval.** Decompose the timeline first; do not tune a knob
   whose interval is not on the critical path (every "obvious" throughput knob here failed this test).
5. **Preserve correctness first; throughput optimizations come second.** A load/latency win that weakens a UID
   barrier, a read-after-write gate, a one-way latch, or membership recompute is not a win.
6. **Trust only robust aggregates.** Single-run per-phase splits are noise; use `total`/wall and ≥3-run
   distributions. Absolute numbers are workload-dependent — carry the *invariant*, not the benchmark value.

### 12.2 Tag-walk (apply in order)

| tag | invariant / bottleneck (durable) | detect on the new code | fix pattern | re-verify |
|---|---|---|---|---|
| **`[CO-QPS]`** — the one throughput lever that moved the wall | client-go default (QPS 5 / Burst 10) serializes uncached reads + status patches under a burst, inflating a single reconcile to seconds **regardless of `MaxConcurrentReconciles`**. Production knee = **200/400** (500/1000 = no extra gain). | For each binary: locate the `rest.Config` → `ctrl.NewManager`/`manager.New` handoff; check `Config.QPS`/`Burst` are set **before** it. Grep the env names in 12.0. | Set QPS/Burst from env with a deliberate default (ss + domain **200/400**), fail-fast on invalid, log effective at startup. Capture/dynamic clients too. | reconcile `time_seconds` mean/max collapse; QPS→Ready sweep at N=20 shows the knee; Phase C ≈ 0. |
| **`[CO-EVENT-WAKE]`** — watches, not poll handshakes | cross-controller handoffs must be **event-driven**; a `RequeueAfter` may only be a *missed-event backstop*, never the primary progress mechanism. | Grep `RequeueAfter(` in every reconciler. For each, ask "is there a `Watch`/`Owns`/mapper that fires on the awaited object's transition?" Known legs: MCR↔`ManifestCheckpoint`, `SnapshotContent` bottom-up `ManifestsArchived` staircase, `VolumeCaptureRequest`↔`VolumeSnapshotContent`. | Add reverse-lookup `Watches(EnqueueRequestsFromMapFunc(...))` (via ownerRef, spec ref, label, or a **field index**); keep the poll only as backstop. | with `STATE_SNAPSHOTTER_PHASE_A_TRACE=1`, `viaBackstop` must be **0** on the critical path; observe-lag small (not ~poll-interval). |
| **`[CO-DIRECT-REF]`** — never search-by-full-traversal | a reverse lookup must use a **direct reference on the event object** or a **cached field index** — never a full `List`+decode per event, and planning inputs must come from the **manager cache**, not `APIReader`. This one pattern closed 5 load hotspots (relay index, CSD list, genericbinder mappers, D.3, H5). | Grep `.List(` **inside** any `MapFunc`/mapper/relay; grep full-namespace `List` in the child→parent relay; grep `APIReader.*List` in planning. Watch-map load: `STATE_SNAPSHOTTER_WATCH_MAP_STATS`. | replace with `obj.GetOwnerReferences()` / `spec.*Ref` (O(1)) or `Client.List(MatchingFields{...})` on a registered `IndexField`; serve planning lists from the cached client. | apiserver `list`/tree audit drops (e.g. relay `storage/snapshots` ~440→~2); CPU pprof: `List` off the watch path; **0** `field label not supported`. |
| **`[CO-CONCURRENCY]`** — raise the ceiling, guard shared state | implicit `MaxConcurrentReconciles=1` is a ceiling **after** `[CO-QPS]`; raising it is safe **only** if the reconciler holds no unguarded shared mutable state. | Grep controllers missing `WithOptions(controller.Options{MaxConcurrentReconciles:...})`; grep reconcile-time writes to shared `Config`/maps. | set 4 (start conservative); guard shared config with a mutex + per-reconcile snapshot. | `-race`/integration clean; reconcile throughput up. **Note:** not a wall lever on its own — the gate is usually downstream. |
| **`[CO-APIREADER]`** — cache only watched-object mirror reads | only an event-driven **mirror read of a watched object** may use the cached client (stale = one extra reconcile). UID-barrier / read-after-write / one-way-latch / edge-preserve / informer-less reads **must stay uncached**. | Grep `APIReader.Get`/`Client.Get`; classify each against the Appendix-11 table. | cache the 3 mirror reads only; leave the rest. | no `Ready` flapping (guard test); no broken UID barrier. |
| **`[CO-PHASEA-STRUCTURAL]`** — the open remainder | Phase A is a **planning barrier**, and its residual is **structural serialization on the disk-snapshot layer** (domain VDS reconcile throughput), **not** QPS, relay, foundation VCR/QPS, or the 500 ms poll (all disproven). Priority layers are **sequential** (VM 100 → disk 10). | run the critical-path trace (12.3). If the `disk.created → disk.CSReady` gap (H4) dominates and staircases while the VM layer stays flat, the disk layer is the serializer. | **no proven fix** — candidates are domain-snapshot `MaxConcurrentReconciles` (env lever exists) or restructuring the layer dependency; open only under 12.4. | H4 share of the median path; `phaseATotalMs` distribution across N=20. |

### 12.3 Portable measurement toolkit (methodology, not a script path)

- **Phase decomposition (no code):** for a fan-out of N root Snapshots, read each object's `creationTimestamp` +
  condition `lastTransitionTime` (1 s resolution) and difference them into A/B/C. **Trust only `total` and wall
  from a single run** — the A-vs-B split is noise-dominated at N=20 (they anti-correlate with batch stagger);
  average ≥3 runs before splitting.
- **Critical-path trace (server-side, no code):** every object in namespace `<pfx>-i` belongs to tree `i`, so
  correlate by namespace; reconstruct the five Phase-A hops (root→VMsnap create, VMsnap reconcile, root→disk
  create, disk reconcile, root publish) from timestamps. Cross-check with `STATE_SNAPSHOTTER_PHASE_A_TRACE=1`
  (ms `relayLatencyMs` / `observeLagMs` / `phaseATotalMs`). *(The `phaseA_trace_run.sh` + `phaseA_reconstruct.py`
  helpers used here are ad-hoc; regenerate from this description — do not depend on their `/tmp` paths.)*
- **API-load attribution:** apiserver audit `list`/`get` by the controller SA, grouped by resource, per one tree;
  a hotspot = repeated full-collection lists of a planning-input GVK → `[CO-DIRECT-REF]`.
- **QPS→Ready sweep:** N=20, sweep QPS/Burst (50/100 → 500/1000), plot total vs QPS; expect a knee at ~200.
- **Watch-map / CPU:** `STATE_SNAPSHOTTER_WATCH_MAP_STATS` + pprof to catch `List`-in-mapper regressions.

### 12.4 Do-not-revisit + reopen criteria (carry the negatives too)

- **Disproven, do not re-run** (details in section 7): leaf-skip / distinct-H1 leaf staircase; VSC-wake-loss as a
  leaf cause; genericbinder `List` as a *wall* cause; source-discovery-skip (membership is **not** frozen before
  `ChildrenSnapshotReady=True/Completed`); `childSnapshotReadCache` via informer cache (the shared per-GVK list
  feeds freshness-bound create/dedup, and `Create` does **not** tolerate `AlreadyExists` → stale reads flap
  `Ready`). Preserve these invariants in any rework: **membership recomputes each pass and grows/shrinks across
  priority layers**; **create/dedup reads must be fresh**; **readiness is generation-guarded**.
- **Throughput is exhausted as a latency lever** once ss/domain QPS = 200/400: foundation VCR workers, foundation
  QPS, relay concurrency each move only a sub-metric, never the N=20 wall. Keep them as env infrastructure.
- **Reopen** the deep Phase-A causal trace (`[CO-PHASEA-STRUCTURAL]`) **only** if (1) a production-scale trace at
  *realistic* concurrency breaches an SLO, or (2) a decision is taken to actually optimize the disk layer — then
  the trace pays for itself by pinpointing worker-wait vs API vs layer-dependency **before** a fix is written.
