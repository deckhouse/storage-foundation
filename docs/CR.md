---
title: "VolumeCaptureRequest and VolumeRestoreRequest"
description: "The current service contract for volume capture and restore requests used by storage controllers."
---

# Service request resources

`VolumeCaptureRequest` (VCR) and `VolumeRestoreRequest` (VRR) are one-shot service resources intended
for controllers such as state-snapshotter, not a stable end-user backup UX. Actual create/read access
is determined by cluster RBAC; admission currently does not enforce a controller-only policy. Both
are namespaced in `storage-foundation.deckhouse.io/v1alpha1`; their namespace is also the namespace
of the source or target PVC.

## VolumeCaptureRequest

VCR creates a durable data artifact for one PVC. The domain snapshot SDK uses only
`spec.mode: Snapshot`; `Detach` is a separate storage-foundation flow.

```yaml
apiVersion: storage-foundation.deckhouse.io/v1alpha1
kind: VolumeCaptureRequest
metadata:
  name: nss-vcr-...
  namespace: my-app
spec:
  mode: Snapshot
  target:
    uid: "<pvc-uid>"
    apiVersion: v1
    kind: PersistentVolumeClaim
    name: data
status:
  data:
    artifactRef:
      apiVersion: snapshot.storage.k8s.io/v1
      kind: VolumeSnapshotContent
      name: snapcontent-...
      uid: "<artifact-uid>"
  completionTimestamp: "..."
  conditions:
    - type: Ready
      status: "True"
      reason: Completed
```

Contract:

- `spec.target` is a single PVC identity; namespace is implicit from the VCR.
- A request is point-in-time. Consumers must not change its target after creation. The SDK rejects
  a mismatching existing target fail-closed; the CRD currently does not enforce target transition
  immutability with CEL.
- `status.data.artifactRef` points to the durable `VolumeSnapshotContent` or `PersistentVolume`.
  The source PVC identity remains in immutable `spec.target` and is not duplicated in status.
- `Ready=True/Completed` is success.
- `Ready=False/TargetsPending` is **non-terminal**: CSI capture is still retrying. In particular,
  `VolumeSnapshotContent.status.error` does not by itself make the request terminal.
- Any other `Ready=False` reason is terminal. Current reason constants include `InvalidMode`,
  `Incompatible`, `InternalError`, `NotFound`, `RBACDenied`, `InvalidSource`, `PVBound`, and
  `RestoreFailed`. `SnapshotCreationFailed` is retained for API compatibility but is no longer
  emitted.
- The state-snapshotter core maps terminal VCR failure to its own `VolumeCaptureFailed`; domain
  controllers should observe `snapshotsdk.CoreCaptureOutcome`, not interpret VCR conditions
  directly.

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
    name: snapcontent-...
  pvcTemplate:
    metadata:
      name: data
    spec:
      accessModes: [ReadWriteOnce]
      storageClassName: local
      resources:
        requests:
          storage: 10Gi
  fsType: ext4
status:
  pvcRef:
    apiVersion: v1
    kind: PersistentVolumeClaim
    namespace: restore-ns
    name: data
    uid: "<pvc-uid>"
  completionTimestamp: "..."
  conditions:
    - type: Ready
      status: "True"
      reason: Completed
```

Contract:

- `spec.pvcTemplate.metadata.name` is required. Restore is never cross-namespace.
- Supported source kinds are `VolumeSnapshotContent` and `PersistentVolume`.
- The patched external-provisioner watches VRRs, calls CSI `CreateVolume`, and creates the PV/PVC.
  It must not write VRR status.
- `VolumeRestoreRequestController` is the only status writer. It validates the source, observes the
  provisioned PVC, and publishes `pvcRef`, `completionTimestamp`, and `Ready`.
- A terminal VRR is one-shot; create a new request for another attempt.

## Retention and deletion

Terminal VCR/VRR objects are deleted by cron-driven generic GC using
`status.completionTimestamp`. Defaults:

| Variable | Default |
|---|---|
| `GC_VCR_TTL` / `GC_VRR_TTL` | `24h` |
| `GC_VCR_SCHEDULE` / `GC_VRR_SCHEDULE` | `0 * * * *` |

There are no per-object TTL annotations and no module settings for these values. See
`images/controller/docs/TTL_MECHANISM.md` for artifact ownership and DataImport adoption details.
On the successful state-snapshotter path, core may delete a VCR earlier after it has persisted the
data handoff; generic GC is the cleanup path for terminal leftovers.

## Validation and RBAC

Validation is performed by CRD schema/CEL plus the controllers; there is no VCR/VRR admission
webhook performing `SelfSubjectAccessReview`.

Initiating service accounts need only the verbs required by their flow. A snapshot domain normally
creates/reads/patches VCRs and creates/deletes VRRs. The storage-foundation controller owns request
status. The `040-vrr-provisioner-rbac` hook grants patched CSI provisioner service accounts
cluster-wide VRR read/watch and target-PVC creation; those sidecars must not update VRR status.
The current production namespace registry covers `d8-sds-local-volume`; generic cross-driver VRR
support requires extending the shared CSI deployment/RBAC contract.
