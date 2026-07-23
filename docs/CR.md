---
title: "VolumeCaptureRequest and VolumeRestoreRequest"
description: "The current service contract for volume capture and restore requests used by storage controllers."
---

`VolumeCaptureRequest` (VCR) and `VolumeRestoreRequest` (VRR) are one-shot service resources intended
for controllers such as state-snapshotter, not a stable end-user backup UX. Actual create/read access
is determined by cluster RBAC; admission currently does not enforce a controller-only policy. Both
are namespaced in `storage-foundation.deckhouse.io/v1alpha1`; their namespace is also the namespace
of the source or target PVC.

This page summarizes the current generated CRDs and controller/sidecar behavior. The
state-snapshotter domain SDK ADR defines how domain controllers consume these resources, while the
unified-snapshots overview owns the core `Ready` reason mapping. The original
`2025-11-30-volume-capture-and-restore-request.md` ADR is historical rationale, not the current
implementable schema.

## VolumeCaptureRequest

VCR creates a durable data artifact for one PVC. The domain snapshot SDK uses only
`spec.mode: Snapshot`; `Detach` is a separate storage-foundation flow.

```yaml
apiVersion: storage-foundation.deckhouse.io/v1alpha1
kind: VolumeCaptureRequest
metadata:
  name: nss-vcr-4f2c8a91d0e3b7c2
  namespace: my-app
spec:
  mode: Snapshot
  target:
    uid: "2b4f6c7e-7e1d-4d85-95d0-8b52d61534d8"
    apiVersion: v1
    kind: PersistentVolumeClaim
    name: data
status:
  data:
    artifactRef:
      apiVersion: snapshot.storage.k8s.io/v1
      kind: VolumeSnapshotContent
      name: snapshot-73a4cda0-4f2c8a91d0e3
      uid: "1148b6bd-9de2-4c86-a814-7688300932eb"
  completionTimestamp: "2026-07-23T12:34:56Z"
  conditions:
    - type: Ready
      status: "True"
      lastTransitionTime: "2026-07-23T12:34:56Z"
      reason: Completed
      message: "target ready"
```

Contract:

- `spec.target` is one PVC identity; namespace is implicit from the VCR. This is a single-target
  request, not a bulk capture API.
- A request is point-in-time, but the enforcement is incomplete. The CRD requires non-empty
  `uid`, `apiVersion`, `kind`, and `name` for Snapshot mode, but has no transition-CEL making
  `spec` immutable. The SDK detects a changed existing target only when `EnsureVCR` reconciles it
  again. The storage-foundation controller currently resolves the live PVC by request namespace and
  target name, does not require `apiVersion: v1` or `kind: PersistentVolumeClaim`, and does not compare
  the live PVC UID with `spec.target.uid`; the UID is used in deterministic artifact naming. Server-side
  immutability/typed drift is backlog item #26 and the remaining UID/GVK/admission hardening is #28.
  The CRD only requires `target` for Snapshot mode, although the Detach controller path requires it too.
- `status.data.artifactRef` points to the durable `VolumeSnapshotContent` or `PersistentVolume`.
  The source PVC identity remains in `spec.target` and is not duplicated in status.
- `Ready=True/Completed` is success.
- `Ready=False/TargetsPending` is **non-terminal**: CSI capture is still retrying. In particular,
  `VolumeSnapshotContent.status.error` does not by itself make the request terminal.
- Any other VCR `Ready=False` reason is terminal.
- In the current SDK flow, a domain with a post-capture wait/action calls `MarkPlanned`, then observes
  `snapshotsdk.CoreCaptureOutcome`; state-snapshotter maps a terminal VCR failure to
  `VolumeCaptureFailed`. The active `namespace-root-mcr-before-planned` plan targets a guarded direct
  `Finished` path for a childless domain with a complete plan. That target behavior is not implemented
  in this revision and, once implemented, such a leaf will not be required to interpret VCR conditions
  itself.

## VolumeRestoreRequest

VRR asks the patched external-provisioner to create and bind one PVC from a
`VolumeSnapshotContent` or `PersistentVolume`.

```yaml
apiVersion: storage-foundation.deckhouse.io/v1alpha1
kind: VolumeRestoreRequest
metadata:
  name: restore-data
  namespace: restore-ns
spec:
  sourceRef:
    apiVersion: snapshot.storage.k8s.io/v1
    kind: VolumeSnapshotContent
    name: snapshot-73a4cda0-4f2c8a91d0e3
  pvcTemplate:
    metadata:
      name: data
    spec:
      accessModes: [ReadWriteOnce]
      storageClassName: local
      volumeMode: Filesystem
  fsType: ext4
status:
  pvcRef:
    kind: PersistentVolumeClaim
    namespace: restore-ns
    name: data
    uid: "8c37aa04-af6e-4671-9446-87062a18c189"
  completionTimestamp: "2026-07-23T12:36:04Z"
  conditions:
    - type: Ready
      status: "True"
      lastTransitionTime: "2026-07-23T12:36:04Z"
      reason: Completed
      message: "PVC restore-ns/data restored successfully"
```

Contract:

- The CRD schema requires `sourceRef.name`, `pvcTemplate`, and
  `pvcTemplate.metadata.name`. Restore is never cross-namespace.
- The executor additionally requires non-empty `pvcTemplate.spec.storageClassName` and an exact
  `pvcTemplate.spec.volumeMode` of `Block` or `Filesystem`. These fields are optional in the current
  schema. A schema-accepted request that omits either field is ignored by the executor with an event;
  the controller keeps waiting for a PVC without writing `Ready` or `completionTimestamp`. Such a VRR
  is non-terminal and therefore immortal to generic GC. This malformed empty-status gap needs a code
  fix; documentation does not make it an accepted execution contract.
- `pvcTemplate` is only partially materialized, and source kinds differ. The executor validates
  template storage class and volume mode for every request. For a `VolumeSnapshotContent` source it
  uses the template's storage class, volume mode and access modes (default `ReadWriteOnce`), plus root
  `fsType` for Filesystem volumes. For a `PersistentVolume` source, the source PV is authoritative
  for volume mode, access modes, filesystem type and capacity; corresponding template values are not
  the effective restore shape. Neither path copies template labels/annotations or treats
  `pvcTemplate.spec.resources.requests.storage` as authoritative.
- Supported source kinds are `VolumeSnapshotContent` and `PersistentVolume`. The current controller
  and executor select a source by `kind` and `name`; `sourceRef.apiVersion`, `namespace`, and `uid` are
  not enforced against the live source. Backlog item #28 tracks source identity and admission
  hardening.
- The patched external-provisioner watches VRRs, calls CSI `CreateVolume`, and creates the PV/PVC.
  It must not write VRR status.
- `VolumeRestoreRequestController` is the only status writer. It validates the source, observes the
  provisioned PVC, and publishes `pvcRef`, `completionTimestamp`, and `Ready`.
- The current `pvcRef` writer publishes `kind`, `namespace`, `name`, and `uid`, but leaves the optional
  `apiVersion` empty; the example intentionally matches the writer. Setting it to `v1` and adding a
  writer test is active work in the `namespace-root-mcr-before-planned` plan.
- A terminal VRR is one-shot; create a new request for another attempt.

## Current conditions and reasons

| Resource | Reasons written by the current controller |
| --- | --- |
| VCR | `Completed`, `TargetsPending`, `InternalError`, `NotFound`, `RBACDenied`; `InvalidMode` remains a defensive writer path although the CRD enum rejects unknown modes |
| VRR | `Completed`, `InvalidSource`, `InternalError`, `NotFound` |

`SnapshotCreationFailed` is compatibility-only and is no longer emitted: a VCR-side
`VolumeSnapshotContent.status.error` stays `TargetsPending`. The shared constants `Incompatible`,
`UnsupportedTargetKind`, `PVBound`, and `RestoreFailed` are currently unused by both request
controllers. A VRR source VSC with `status.error` is a different path and is finalized as
`Ready=False/InternalError`.

## Retention and deletion

Terminal VCR/VRR objects are deleted by cron-driven generic GC using
`status.completionTimestamp`. Defaults:

| Variable | Default |
| --- | --- |
| `GC_VCR_TTL` / `GC_VRR_TTL` | `24h` |
| `GC_VCR_SCHEDULE` / `GC_VRR_SCHEDULE` | `0 * * * *` |

There are no per-object TTL annotations and no module settings for these values. See
`images/controller/docs/TTL_MECHANISM.md` for artifact ownership and DataImport adoption details.
On the successful state-snapshotter path, core may delete a VCR earlier after it has persisted the
data handoff; generic GC is the cleanup path for terminal leftovers. A request without a terminal
`Ready` condition and `completionTimestamp`, including the malformed VRR case above, is not collected.
The current VRR keeper is not connected to the executor-created restore target PVC, so collecting the
VRR does not delete that PVC; see the implementation-specific ownership details in the linked TTL page.

## Validation and RBAC

Validation is performed by CRD schema/CEL plus the controllers; there is no VCR/VRR admission
webhook performing `SelfSubjectAccessReview`.

| Actor | Effective/request-specific access |
| --- | --- |
| Deckhouse `User` / RBAC v2 viewer | Cluster-wide `get`, `list`, `watch` on VCR and VRR |
| Deckhouse `ClusterEditor` / RBAC v2 manager | Effective read access plus `create`, `update`, `patch`, `delete`, `deletecollection`; no request `/status` grant |
| Capture domain using snapshotsdk | Needs VCR `get`/`create`/`patch` (and list/watch if its deployment watches them); it does not write VCR status |
| state-snapshotter core | Its current template grants VCR CRUD/delete and VCR `/status` update/patch, although the service contract assigns VCR status to storage-foundation; core reaps a VCR after durable handoff |
| Restore consumer (current DataExport path) | The data-manager role grants VCR/VRR `create`, `delete`, `list`, `get`, `watch`, `update`; it does not receive request `/status` |
| Patched CSI provisioner executor | Cluster-wide VRR `get`, `list`, `watch` and target-PVC `get`, `list`, `watch`, `create`, `update`, `patch`; no VRR `/status` |
| storage-foundation controller | VCR/VRR CRUD plus both `/status` subresources; it is the request status writer and manages the request-following ObjectKeepers |

The user-facing grants mean these resources are not effectively controller-only today, despite their
intended service-resource role. The `040-vrr-provisioner-rbac` hook currently binds the executor grant
only to the `csi` ServiceAccount in `d8-sds-local-volume`. Generic cross-driver VRR support requires
extending the shared CSI deployment/RBAC contract.
