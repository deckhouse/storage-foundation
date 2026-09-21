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

- `ttl`: Time-to-live duration after the last data transfer. The window is an idle one and it counts both directions: it is moved forward by a download (a file being read or a directory being listed) and by an upload while its bytes keep arriving, so a transfer that takes longer than the TTL is not interrupted — only an idle server is. When the TTL expires, the server pod is automatically deleted along with the rest of the server-side infrastructure, and the resource's `Ready` condition is set to `false` with reason `Expired`. For a `DataExport` the PVC is then released back to the original PV. A `DataImport` that expires before its upload has finished behaves differently by mode: with `mode: PopulateData` it produces nothing — no durable artifact exists — and a new `DataImport` has to be created to try again; with `mode: CreatePVC` the target PersistentVolumeClaim is created up front, in the user's namespace, before the upload starts, and it is not deleted when the import expires — it is simply left unprovisioned.

- `waitForFirstConsumer` (`mode: CreatePVC` only, default `true`): when the target PersistentVolumeClaim's StorageClass delays volume binding until a consumer appears, the import waits until a pod that mounts the target PersistentVolumeClaim is scheduled, then places the imported data on the node that pod was scheduled to. That pod stays bound to the node it first selected for the entire duration of the upload — it cannot move to another node until the import completes and hands the volume over. Setting this to `false` skips the wait: the import provisions the data right away instead of waiting for a consumer to appear.

- `publish`: When set to `true`, enables external cluster access to the exporter pod. A public URL is generated in the resource's `status.publicURL` field with the format: `https://api.<public-domain>/<namespace>/<kindShort>/<name>/`.

- A `DataImport` (`mode: CreatePVC`) that fails terminally is not torn down right away: it keeps its internal staging volume and importer workload running — consuming backend storage capacity — until the `DataImport` is deleted or garbage-collected.

For usage examples (the `d8` utility, raw manifests, and the HTTP API reference), see the [usage documentation](usage.html).
