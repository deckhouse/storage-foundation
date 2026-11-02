---
title: "Module storage-foundation"
description: "Enables snapshots, volume cloning, and HTTP-based volume data transfer for compatible CSI drivers in a Kubernetes cluster."
---

The module provides common storage capabilities for compatible CSI drivers in a Kubernetes cluster: volume snapshots and
restoration, PVC cloning, and HTTP-based export and import of persistent volume data.

The module is related to three other Deckhouse Platform modules:

- `snapshot-controller` is deprecated. The `storage-foundation` module replaces it completely.
- `storage-volume-data-manager` performs the same volume data transfer. The `storage-foundation` module takes over this
  function, and both modules can stay enabled at the same time. The same volume is exported through only one of
  them.
- `state-snapshotter` depends on the `storage-foundation` module. When it creates a namespace snapshot, it captures and
  restores volumes through this module.

Deckhouse Platform modules that support snapshots:

- [cloud-provider-openstack](/modules/cloud-provider-openstack/)
- [cloud-provider-vsphere](/modules/cloud-provider-vsphere/)
- [cloud-provider-aws](/modules/cloud-provider-aws/)
- [cloud-provider-azure](/modules/cloud-provider-azure/)
- [cloud-provider-gcp](/modules/cloud-provider-gcp/)
- [sds-local-volume](/modules/sds-local-volume/)
- [sds-replicated-volume](/modules/sds-replicated-volume/)
- [csi-ceph](/modules/csi-ceph/)
- [csi-nfs](/modules/csi-nfs/)
- [csi-hpe](/modules/csi-hpe/)
- [csi-huawei](/modules/csi-huawei/)
- [csi-yadro-tatlin-unified](/modules/csi-yadro-tatlin-unified/)

## Main features

The module provides the following storage operations:

- Creating point-in-time snapshots of persistent volumes with compatible CSI drivers
- Restoring persistent volumes from snapshots
- Cloning existing PersistentVolumeClaims using CSI volume cloning
- Exporting PersistentVolumeClaim, VolumeSnapshot, VirtualDisk, and VirtualDiskSnapshot data over HTTP
- Importing data over HTTP into a new PersistentVolumeClaim or into a durable snapshot artifact
- Resuming partial data transfers by using HTTP range and offset mechanisms

## How the module works

The module components run in the `d8-storage-foundation` namespace. A snapshot is created as follows:

1. The user creates a VolumeSnapshot that refers to a PVC.
1. The validation webhook of the module checks the request and fills `volumeSnapshotClassName` from the StorageClass
   annotation when the field is omitted.
1. The snapshot controller calls the CSI driver, which creates the snapshot in the storage backend, and binds the
   VolumeSnapshot to a VolumeSnapshotContent.
1. The VolumeSnapshot reports `readyToUse: true`, and the user can restore a PVC from it. The data of the snapshot
   can be exported once the `state-snapshotter` module has also bound the snapshot to its SnapshotContent object and
   filled the data of that SnapshotContent.

Volume data is transferred as follows:

1. The user creates a DataExport or DataImport of the `storage-foundation.deckhouse.io` API group in the namespace of
   the PVC, with the `d8 data` commands or a manifest.
1. The data manager controller validates the object and starts a data transfer server for it in the module namespace.
   If `spec.publish` is set, the controller also publishes the server outside the cluster.
1. The object reports `Ready=True`, and its `status.url` and `status.publicURL` fields carry the addresses of the
   server. The client downloads or uploads data over HTTP.
1. When the idle period set in `spec.ttl` elapses, the controller stops the server and later removes the object. An
   import into a new PVC leaves the PVC in the namespace.

The module extends VolumeSnapshot with the `spec.mode` field and the `status.data`, `status.captureState`, and
`status.conditions` sections. The `state-snapshotter` module uses them to include the snapshot in a namespace snapshot.
For that module, the controller also serves the aggregated API of snapshot subresources and processes
VolumeCaptureRequest and VolumeRestoreRequest objects. These are service objects and are not intended for direct use.

The `storage-volume-data-manager` module can stay enabled alongside with the module `storage-foundation`: each of the two serves its own
DataExport and DataImport objects in its own API group and namespace. The `d8 data` commands (version 0.33.14 or
later) work with the `storage-foundation` module through the `storage-foundation.deckhouse.io` group whenever the
cluster serves it and the user is authorized to read its objects in the target namespace. Otherwise, they work with
the `storage-volume-data-manager` module through the `storage.deckhouse.io` group. The `d8 snapshot` commands work
only with the `storage-foundation` module.

## Limitations

Consider the following limitations when using the module:

- Snapshot and cloning capabilities depend on support in the CSI driver and storage backend.
- A PersistentVolumeClaim can be exported only while it is not mounted by a Pod.
- HTTP import and export operate on a single volume or snapshot per DataImport or DataExport.
- The same PersistentVolumeClaim or VirtualDisk can be exported through only one module at a time, either
  `storage-foundation` or `storage-volume-data-manager`. Simultaneous exports cause a PersistentVolume ownership
  conflict.
