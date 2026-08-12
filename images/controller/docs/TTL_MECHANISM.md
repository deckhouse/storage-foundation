# Garbage collection of VCR/VRR request resources

## Overview

`VolumeCaptureRequest` (VCR) and `VolumeRestoreRequest` (VRR) are one-time request resources. Once they
reach a terminal state they are no longer useful and are deleted automatically after a retention window by
a cron-triggered garbage-collection controller (one per kind), built on the generic collector in
`common/gc`.

**Scope**: the collector directly deletes only request resources (VCR/VRR). Secondary deletion depends
on the ownership edges that are actually wired:

- a VCR has a `FollowObject` ObjectKeeper, and that keeper is the controller owner of the produced
  `VolumeSnapshotContent` or detached `PersistentVolume`; deleting the VCR removes this owner chain;
- a VRR controller also creates a `FollowObject` ObjectKeeper, but it currently does not put the
  `storage-foundation.deckhouse.io/object-keeper-ref` annotation on the VRR. The patched executor only
  copies a keeper ownerReference to the restore target PVC when that annotation is present. On the normal
  current path the keeper is therefore not connected to the PVC, and request GC does not delete the PVC.

The loaded legacy `RETENTION_SNAPSHOT_TTL` / `RETENTION_DETACH_TTL` config fields are not used by these
GC managers and do not create or extend an ownership edge. See “Deletion effects” below.

> Historical note: earlier versions used an informational `storage-foundation.deckhouse.io/ttl` annotation
> plus a per-controller background TTL scanner driven by `REQUEST_TTL`. That mechanism has been removed —
> there is **no TTL annotation** and **no `REQUEST_TTL`** anymore. Retention is configured exclusively by
> the GC environment variables below.

## What gets deleted

An object is deleted when **all** of the following hold:

- it is **terminal** — its `Ready` condition is `True` (completed) or `False` (failed). A VCR that is
  `Ready=False` with reason `TargetsPending` (its single-target CSI capture is still in progress) is NOT
  terminal and is never collected;
- it is not already being deleted (`metadata.deletionTimestamp` is unset);
- its retention age has elapsed: `now - status.completionTimestamp > TTL`.

The age is measured from `status.completionTimestamp`, which the controller stamps once when the object
first becomes terminal — never from `creationTimestamp`. A terminal object without a
`completionTimestamp` is a fail-safe no-op (never collected). A malformed VRR that the schema accepts but
the executor ignores (for example one without execution-required `storageClassName` or `volumeMode`) can
remain with empty status forever and is likewise not collected.

## Configuration (env-only)

| Variable          | Applies to | Default     |
|-------------------|------------|-------------|
| `GC_VCR_TTL`      | VCR        | `24h`       |
| `GC_VCR_SCHEDULE` | VCR        | `0 * * * *` |
| `GC_VRR_TTL`      | VRR        | `24h`       |
| `GC_VRR_SCHEDULE` | VRR        | `0 * * * *` |

`*_TTL` accepts a Go duration (e.g. `24h`, `30m`) and must be positive. `*_SCHEDULE` is a standard cron
expression. With the hourly default schedule, worst-case retention is `TTL` plus one sweep interval
(~25h). There are no module settings and no per-object TTL annotations.

## Deletion effects

Deletion is a plain `DELETE`:

- **VCR**: its `ObjectKeeper` (created in `FollowObject` mode on the VCR) is deleted with it, and the owned
  artifact (`VolumeSnapshotContent`/`PersistentVolume`) is reclaimed by Kubernetes GC through the ownerRef
  cascade. As a safety net, the VCR GC first reaps an artifact that has **no ownerReferences at all** (a
  true orphan the cascade cannot reach), via the `common/gc` `PreDeleter` hook — an artifact that still has
  any owner (its VCR keeper, or a bridging keeper such as a live DataImport's) is left untouched, so GC of
  the VCR never deletes an artifact another object is still retaining. The reap is best-effort.
- **VRR**: the request and its otherwise unconnected follow-ObjectKeeper are deleted. The restore target
  PVC provisioned by the external-provisioner remains because the current VRR controller does not publish
  the keeper-ref annotation consumed by the executor. If that annotation is supplied by another writer,
  the executor attaches the keeper as the PVC's controller owner and deleting the VRR can then cascade to
  that PVC; this conditional path is not the normal controller wiring.

### Interaction with DataImport artifact adoption

A DataImport captures its scratch volume by creating a VCR; the produced artifact is bridged to the
DataImport by an additional (non-controller) ObjectKeeper so it survives past the VCR. That artifact is
therefore retained until BOTH bridges fall away: the VCR keeper (dropped when the VCR is GC'd,
`GC_VCR_TTL` after the VCR completes) and the DataImport keeper (dropped when the DataImport is GC'd,
`GC_DATA_IMPORT_TTL` after the import completes). If those absolute owner-removal deadlines are `D_vcr`
and `D_import`, the earliest the artifact object can become ownerless is therefore
`max(D_vcr, D_import)`, not the minimum. Both TTLs default to 24h, but their clocks start at different
completion timestamps and each collector adds up to its own sweep delay. Shortening only one TTL shortens
the adoption window only when that owner would otherwise have the later removal deadline.

## Implementation

- Generic collector: `common/gc` — `CronSource`, `Reconciler`, `DefaultFilter`, the `ReconcileGCManager`
  contract, and the optional `PreDeleter` hook. The controller reacts only to cron-source ticks (informer
  events are dropped), and runs leader-only via the manager.
- Per-kind managers and wiring: `internal/controllers/gc.go` (`vcrGCManager`, `vrrGCManager`,
  `SetupVCRGC`, `SetupVRRGC`), registered from `add_reconcilers.go`.
- Ownership wiring: VCR and VRR keepers are created in
  `internal/controllers/volumecapturerequest_controller.go` and
  `volumerestorerequest_controller.go`; the DataImport bridge is in
  `images/data-manager-controller/internal/controllers/data-import/object_keeper.go`; the conditional
  VRR keeper annotation is consumed only by
  `images/csi-external-provisioner/patches/v6.2.0/002-vrr-executor.patch`.

`docs/CR.md` is the current consumer summary. The historical
`2025-11-30-volume-capture-and-restore-request.md` ADR is rationale only; current behavior is determined
by the generated CRDs and the implementation files above.
