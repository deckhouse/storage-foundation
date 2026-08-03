---
title: "The storage-foundation module"
description: "Enables snapshot support and volume cloning for compatible CSI drivers in a Kubernetes cluster."
---

This module enables snapshot support for compatible CSI-drivers in the Kubernetes cluster.

Deckhouse Kubernetes Platform CSI-drivers that support snapshots:

- [cloud-provider-openstack](/modules/cloud-provider-openstack/stable/)
- [cloud-provider-vsphere](/modules/cloud-provider-vsphere/stable/)
- [cloud-provider-aws](/modules/cloud-provider-aws/stable/)
- [cloud-provider-azure](/modules/cloud-provider-azure/stable/)
- [cloud-provider-gcp](/modules/cloud-provider-gcp/stable/)
- [sds-local-volume](/modules/sds-local-volume/stable/)
- [sds-replicated-volume](/modules/sds-replicated-volume/stable/)
- [csi-ceph](/modules/csi-ceph/stable/)
- [csi-nfs](/modules/csi-nfs/stable/)
- [csi-hpe](/modules/csi-hpe/stable/)
- [csi-huawei](/modules/csi-huawei/stable/)
- [csi-yadro-tatlin-unified](/modules/csi-yadro-tatlin-unified/stable/)

## HTTP-based volume data export and import

The module also enables secure HTTP-based export and import of persistent volume contents. It creates a namespaced `DataExport` or `DataImport` resource in the target namespace, which references the volume to be exported via the `targetRef` field. The supported target types include `PersistentVolumeClaim` and `VolumeSnapshot`.

The data server is built on the standard Go file server and supports both filesystem and block-level volume work modes. User authentication is handled through Kubernetes RBAC, with support for partial content transfer using HTTP `Range` headers.

### Key parameters

- `ttl`: Time-to-live duration after the last server access (file download or directory listing). When the TTL expires, the exporter pod is automatically deleted and the PVC is released back to the original PV. The `DataExport` resource's `Ready` condition is set to `false` with reason `Expired`.

- `publish`: When set to `true`, enables external cluster access to the exporter pod. A public URL is generated in the resource's `status.publicURL` field with the format: `https://api.<public-domain>/<namespace>/<kindShort>/<name>/`.

### If the resources of a running export are deleted

While a `PersistentVolumeClaim` is being exported, the module moves its volume to an export claim of its
own in the module namespace, and returns the volume to your claim when the export ends. Deleting that
export claim by hand therefore has two different outcomes, depending on whether the volume has already
been moved:

- before the move, the claim is simply recreated and the export continues; `status.cleanupReason` stays
  empty;
- after the move, the export cannot be continued — the volume it was serving is gone. The module returns
  the volume to your original claim and ends the export with `Ready=False` and reason
  `ManagedResourceLost`, or `ManagedResourceIdentityMismatch` if a different object took the name.

Your data is not touched in either case: the export only ever moves the binding of the volume, never its
contents, and returning it restores the original reclaim policy.

`status.cleanupReason` is set while that return is in progress and cleared when it completes. A resource
with a non-empty `cleanupReason` is not finished yet even if it already looks failed — do not treat it as
a final state, and do not remove the finalizer by hand: that is what keeps the volume from being reclaimed
mid-return.

Reason `CleanupBlocked` means the return is waiting for something outside the module: a pod still using
the export claim, a volume attachment that has not been detached yet, or an object that occupies the
export claim's name but does not belong to this export. Such a foreign object is never modified or
deleted — remove it yourself, and the export finishes on its own.

When upgrading the module, CRDs must be applied before the new controller image. A new controller writing
`status.cleanupReason` or `status.recovery` against an old schema has the field silently dropped by the API
server: with no recorded identity the export cannot prove which claim the volume belongs to, so it stops on
`CleanupBlocked` and the return of the volume never resumes.

For usage examples (the `d8` utility, raw manifests, and the HTTP API reference), see the [usage documentation](usage.html).
