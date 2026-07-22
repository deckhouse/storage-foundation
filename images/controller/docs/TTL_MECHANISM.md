# Garbage collection of VCR/VRR request resources

## Overview

`VolumeCaptureRequest` (VCR) and `VolumeRestoreRequest` (VRR) are one-time request resources. Once they
reach a terminal state they are no longer useful and are deleted automatically after a retention window by
a cron-triggered garbage-collection controller (one per kind), built on the generic collector in
`common/gc`.

**Scope**: the collector directly deletes only request resources (VCR/VRR), but deleting a VCR also
removes its follow-ObjectKeeper and can therefore Kubernetes-GC an artifact that has no other owner.
Artifacts retained by another bridge/retainer follow that owner's independent retention policy
(`RETENTION_SNAPSHOT_TTL`, `RETENTION_DETACH_TTL`). In other words, request GC never directly selects
artifacts, but it can remove the last ownership edge; see “Deletion effects” below.

> Historical note: earlier versions used an informational `storage-foundation.deckhouse.io/ttl` annotation
> plus a per-controller background TTL scanner driven by `REQUEST_TTL`. That mechanism has been removed —
> there is **no TTL annotation** and **no `REQUEST_TTL`** anymore. Retention is configured exclusively by
> the GC environment variables below.

## What gets deleted

An object is deleted when **all** of the following hold:

- it is **terminal** — its `Ready` condition is `True` (completed) or `False` (failed). A VCR that is
  `Ready=False` with reason `TargetsPending` (a bulk capture still in progress) is NOT terminal and is
  never collected;
- it is not already being deleted (`metadata.deletionTimestamp` is unset);
- its retention age has elapsed: `now - status.completionTimestamp > TTL`.

The age is measured from `status.completionTimestamp`, which the controller stamps once when the object
first becomes terminal — never from `creationTimestamp`. A terminal object without a
`completionTimestamp` is a fail-safe no-op (never collected).

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
- **VRR**: the request is deleted; the export PVC it provisioned through the external-provisioner has its
  own lifecycle.

### Interaction with DataImport artifact adoption

A DataImport captures its scratch volume by creating a VCR; the produced artifact is bridged to the
DataImport by an additional (non-controller) ObjectKeeper so it survives past the VCR. That artifact is
therefore retained until BOTH bridges fall away: the VCR keeper (dropped when the VCR is GC'd,
`GC_VCR_TTL` after the VCR completes) and the DataImport keeper (dropped when the DataImport is GC'd,
`GC_DATA_IMPORT_TTL` after the import completes). The effective artifact-adoption window before an
unadopted artifact can be Kubernetes-GC'd is thus `min(GC_VCR_TTL from VCR completion, GC_DATA_IMPORT_TTL
from import completion)`. Both default to 24h; an operator shortening only `GC_VCR_TTL` also shortens that
window.

## Implementation

- Generic collector: `common/gc` — `CronSource`, `Reconciler`, `DefaultFilter`, the `ReconcileGCManager`
  contract, and the optional `PreDeleter` hook. The controller reacts only to cron-source ticks (informer
  events are dropped), and runs leader-only via the manager.
- Per-kind managers and wiring: `internal/controllers/gc.go` (`vcrGCManager`, `vrrGCManager`,
  `SetupVCRGC`, `SetupVRRGC`), registered from `add_reconcilers.go`.
