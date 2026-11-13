---
title: "Module storage-foundation: usage"
weight: 40
description: "Using snapshots, cloning volumes, and transferring volume data over HTTP with the storage-foundation module."
---

## Using snapshots

CSI modules or cluster administrators create VolumeSnapshotClass objects. Choose a class that matches the CSI driver of
the source PVC. List the available classes:

```shell
d8 k get volumesnapshotclasses.snapshot.storage.k8s.io
```

When a VolumeSnapshot is created from a PVC, the module selects the VolumeSnapshotClass as follows:

- If the StorageClass of the PVC carries the `storage.deckhouse.io/managed-by` label, the module takes the
  class from the `storage.deckhouse.io/volumesnapshotclass` annotation of that StorageClass. The
  `volumeSnapshotClassName` field can be omitted. If it is set, it must match the annotation; otherwise the
  VolumeSnapshot is rejected.
- For other StorageClasses, the module fills `volumeSnapshotClassName` from the same annotation only when the field is
  omitted and the annotation exists. Otherwise, specify a class that matches the CSI driver of the PVC.

The PVC must have `storageClassName` set.

Create a snapshot of an existing PVC:

```yaml
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: my-first-snapshot
spec:
  volumeSnapshotClassName: sds-replicated-volume
  source:
    persistentVolumeClaimName: my-first-volume
```

After creating the snapshot, check its status:

```shell
d8 k describe volumesnapshots.snapshot.storage.k8s.io my-first-snapshot
```

Example output:

```console {.nowrap-default}
...
Spec:
  Source:
    Persistent Volume Claim Name:  my-first-volume
  Volume Snapshot Class Name:      sds-replicated-volume
Status:
  Bound Volume Snapshot Content Name:  snapcontent-b6072ab7-6ddf-482b-a4e3-693088136d2c
  Ready To Use:                        true
  Restore Size:                        500Mi
```

To restore the content of this snapshot, create a new PVC and specify the snapshot as its source:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-first-volume-from-snapshot
spec:
  storageClassName: sds-replicated-volume-data-r2
  dataSource:
    name: my-first-snapshot
    kind: VolumeSnapshot
    apiGroup: snapshot.storage.k8s.io
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 500Mi
```

### VolumeSnapshot fields added by the module

The module serves the VolumeSnapshot CRD with fields that the upstream CRD does not have. The `state-snapshotter`
module uses them to include the snapshot in a namespace snapshot. Only `spec.mode` is set by the user; the status
fields are written by the controllers:

- `spec.mode` defines how the snapshot obtains its content. `Capture`, set by the module when the field is omitted,
  creates a CSI snapshot of `spec.source`. `Import` marks a snapshot whose content is uploaded from a snapshot archive
  by `d8 snapshot upload`: `spec.source` is omitted and the module does not capture anything. The field cannot be
  changed after creation.
- `status.data` describes the captured volume: the source PVC in `sourceRef`, its StorageClass, volume mode, filesystem
  type, allocated size, and the durable artifact in `artifactRef`, which is a VolumeSnapshotContent. The
  `state-snapshotter` module fills this section when it processes the snapshot. The `d8 snapshot` commands read the
  volume of a snapshot from this section. DataExport reads the same data from the SnapshotContent object of the
  `state-snapshotter` module named in `status.boundSnapshotContentName`. A snapshot can therefore be exported only
  after the `state-snapshotter` module has set `status.boundSnapshotContentName` and filled the data of that
  SnapshotContent, and `status.readyToUse: true` alone is not enough. Waiting helps only while the module is still
  processing the snapshot. The module skips a pre-provisioned VolumeSnapshot created from an existing
  VolumeSnapshotContent, a VolumeSnapshot created before the `storage-foundation` module was enabled, and a
  VolumeSnapshot whose source PVC carries the `state-snapshotter.deckhouse.io/exclude` label. Such a snapshot never
  gets a SnapshotContent with data, so its data cannot be exported with DataExport.
- `status.conditions` carries the `Ready` condition, which reports the readiness of the snapshot for the
  `state-snapshotter` module. It is distinct from `status.readyToUse`, which reports that the CSI snapshot is bound to
  its VolumeSnapshotContent.
- `status.captureState`, `status.childrenSnapshotRefs`, and `status.sourceRef` are service fields of snapshot tree
  processing. A VolumeSnapshot is a leaf of the tree, so `status.childrenSnapshotRefs` is normally empty.

## CSI volume cloning

To create a copy of an existing PVC, specify it in the `dataSource` field of a new PVC. CSI volume cloning is a
separate driver capability and does not require creating a VolumeSnapshot.

The following conditions apply:

- The source PVC must be in the `Bound` phase.
- The source and destination PVCs must belong to the same namespace and use the same `volumeMode`.
- The requested size of the destination PVC must be at least the size of the source PVC.
- Volumes must be dynamically provisioned by a CSI driver that supports cloning.
- A different StorageClass is allowed if both StorageClasses use the same CSI driver.
- If `storageClassName` is omitted, the default StorageClass is used. It must use the same CSI driver as the source PVC.

To clone `my-origin-pvc`, create a new PVC and specify the source PVC in its `dataSource` field:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-cloned-pvc
spec:
  storageClassName: sds-replicated-volume-data-r2
  dataSource:
    name: my-origin-pvc
    kind: PersistentVolumeClaim
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 500Mi
```

## Exporting and importing volume data

DataExport can export data from a PersistentVolumeClaim (PVC), VolumeSnapshot, VirtualDisk, or VirtualDiskSnapshot.
VirtualDisk and VirtualDiskSnapshot always use Block mode. DataImport uploads data into a new PVC or into a snapshot;
see [Importing data](#importing-data).

### Access permissions

In Deckhouse Platform 1.78 and later, subjects with the namespace `admin` access level can transfer volume data in that
namespace. Other subjects need an explicit binding to the built-in
`d8:namespace-capability:storage-foundation:transfer_volume_data` ClusterRole. It permits creating, reading, and
deleting DataExport and DataImport objects and transferring data through their HTTP endpoints.

For example, bind the role to a user working in the `project` namespace:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: volume-transfer
  namespace: project
subjects:
  - kind: User
    name: <USER_NAME>
    apiGroup: rbac.authorization.k8s.io
roleRef:
  kind: ClusterRole
  name: d8:namespace-capability:storage-foundation:transfer_volume_data
  apiGroup: rbac.authorization.k8s.io
```

Replace `<USER_NAME>` with the user name. For a ServiceAccount or group, adjust `subjects` accordingly. On Deckhouse
Platform versions earlier than 1.78, use the legacy role names:

| Purpose | 1.78 and later | Earlier than 1.78 |
|---|---|---|
| Data transfer | `d8:namespace-capability:storage-foundation:transfer_volume_data` | `d8:use:capability:module:storage-foundation:transfer_volume_data` |
| Backup agent | `d8:state-snapshotter:backup-agent` | `d8:use:capability:module:state-snapshotter:backup_agent` |

When auditing permissions, also account for the namespace `admin` level and the aggregated `backup-agent` role, if their
bindings are present. For details, see [Backup integration](/modules/state-snapshotter/backup_integration.html). Manual
HTTP client permissions are covered on the [Advanced usage](advanced_usage.html) page.

DataExport and DataImport objects can be served by two modules: `storage-foundation` in the
`storage-foundation.deckhouse.io` API group and `storage-volume-data-manager` in the `storage.deckhouse.io` API group.
The `d8 data` commands (version 0.33.14 or later) determine on every run which of these modules to work with. A user
who is granted permissions only in the `storage.deckhouse.io` group works with the `storage-volume-data-manager`
module, without a warning. Check the permissions in the `storage-foundation.deckhouse.io` group with the following
command:

```bash
d8 k auth can-i get dataexports.storage-foundation.deckhouse.io -n project
```

### Key parameters

The following DataExport and DataImport parameters control the lifetime, placement, and external availability of
data transfer resources. Each item ends with the way the `d8 data` commands set the parameter.

- `ttl` defines the idle period since the last transfer. The countdown restarts whenever data is transferred in an HTTP
  request or response body. If neither the request nor the response has a body, as with `HEAD`, the period is not
  extended. When the period expires, the object reports `Ready=False` with reason `Expired` and the data transfer
  server is stopped: its Pod and Service are deleted, as well as the Ingress (with `publish: true`). The DataImport or
  DataExport object itself is removed automatically later. For an export, the source PVC is bound back to its PV and its
  data is left unchanged. For an import, the internal temporary volume is deleted as well, so data from an incomplete
  upload is lost and the transfer must be restarted with a new DataImport. In `CreatePVC` mode, expiry does not delete
  the target PVC. In `d8 data`: the `create` and `download` commands set `2m` when `--ttl` is omitted.
- `waitForFirstConsumer` applies only to import into a new PVC (`CreatePVC`). With `true`, an import into a StorageClass using delayed binding
  waits for a Pod consuming the PVC. The volume is provisioned on the node selected for that Pod. The Pod remains
  `Pending` until the import completes. With `false`, the volume is provisioned without waiting for such a Pod. Select
  the value based on the StorageClass and the planned PVC consumer. The YAML manifest default is `true`. In `d8 data`:
  `d8 data import create` sends `false` unless `--wffc` is passed.
- `publish: true` enables external access and exposes the transfer URL in `status.publicURL`. Without it, the server is
  reachable only from the cluster network at `status.url`. In `d8 data`: without `--publish`, `d8` checks whether the
  `kubernetes` Service of the `default` namespace is reachable at its ClusterIP and chooses direct access when it is, or
  the published endpoint otherwise. Explicit `--publish=true` or `--publish=false` overrides this choice. If the check
  cannot decide, the command fails; see
  [d8 cannot auto-detect the publish mode](#d8-cannot-auto-detect-the-publish-mode).

DataExport and DataImport are created in the namespace of the source PVC or of the PVC to be created. In `d8 data`: the
commands default to the `d8-storage-volume-data-manager` namespace, so always pass `-n` with the namespace of the PVC.

{{< alert level="warning" >}}
PVCs can be exported only when no Pod has them mounted.
{{< /alert >}}

### Exporting data

Use `d8` to transfer data. Run `d8 data export download --help` for the CLI argument reference. When the source argument
starts with one of the following prefixes, `d8` automatically creates a DataExport:

| Source resource | CLI prefixes | Volume mode |
|---|---|---|
| PersistentVolumeClaim | `pvc/`, `persistentvolumeclaim/` | Filesystem or Block |
| VolumeSnapshot | `vs/`, `volumesnapshot/` | Filesystem or Block, depending on the source volume |
| VirtualDisk | `vd/`, `virtualdisk/` | Block only |
| VirtualDiskSnapshot | `vds/`, `virtualdisksnapshot/` | Block only |

Prefixes are case-insensitive. Without a prefix, provide the name of an existing DataExport.

{{< tabs name="export_method" >}}
{{< tab name="Using d8" >}}

Download a file from the `data` PVC in the `project` namespace with one command. `d8` creates a temporary DataExport,
downloads the file, and deletes the DataExport after transfer because `--cleanup=true` is set:

```bash
d8 data export download -n project pvc/data mydir/file.txt -o file.txt --cleanup=true
```

To download from an existing DataExport, pass its name:

```bash
d8 data export create -n project my-export pvc/data --ttl 5m
d8 data export download -n project my-export mydir/file.txt -o file.txt
```

When downloading a directory, create the destination directory first. The source directory path must end in `/`:

```bash
mkdir -p out
d8 data export download -n project pvc/data mydir/ -o out --cleanup=true
```

For Block mode, omit the path inside the volume:

```bash
d8 data export download -n project vd/my-virtualdisk -o disk.img --cleanup=true
```

Without `--cleanup`, the utility prompts for deletion of the temporary DataExport and waits up to 30 seconds. In
non-interactive sessions, the temporary DataExport is not deleted automatically without this flag.

{{< /tab >}}
{{< tab name="Using a YAML manifest" >}}

Create a DataExport for the user PVC `data` in the `project` namespace:

```yaml
apiVersion: storage-foundation.deckhouse.io/v1alpha1
kind: DataExport
metadata:
  name: my-export
  namespace: project
spec:
  ttl: 10m
  targetRef:
    kind: PersistentVolumeClaim
    name: data
```

Save the manifest as `dataexport.yaml`, apply it, and check the DataExport status:

```bash
d8 k apply -f dataexport.yaml
d8 k -n project get dataexports.storage-foundation.deckhouse.io my-export
```

Applying the manifest creates a DataExport but does not save data to the local disk. For HTTP downloads, see
[HTTP API: data export](advanced_usage.html#http-api-data-export). For certificates and authentication, see
[HTTP client authentication](advanced_usage.html#http-client-authentication).

{{< /tab >}}
{{< /tabs >}}

### Importing data

DataImport works in one of two modes, selected by the `mode` field. The mode defines where the uploaded data goes and
who usually creates the object:

| Mode | Where the data goes | Where the volume is created | Who creates the DataImport |
|---|---|---|---|
| `CreatePVC` (default) | A new PVC that remains after the import | The target PVC in the user namespace and a temporary PVC in the `d8-storage-foundation` namespace | The user, with `d8 data import` or a manifest |
| `PopulateData` | A VolumeSnapshotContent bound to an existing snapshot | A temporary PVC in the `d8-storage-foundation` namespace | `d8 snapshot upload` |

Use `CreatePVC` to upload files or a block image into a volume of your own. `PopulateData` is the data leg of moving a
snapshot between namespaces or clusters.

#### Importing into a new PVC

A manual import in `CreatePVC` mode creates a new PVC. Prepare a PVC template for `d8` or a DataImport manifest. The
examples use the `project` namespace. Specify a StorageClass available in your cluster.

{{< tabs name="import_method" >}}
{{< tab name="Using d8" >}}

Save this PVC template as `pvc-template.yaml`, replacing `<STORAGE_CLASS>` with an available StorageClass name:

```yaml
metadata:
  name: restored-data
  namespace: project
spec:
  storageClassName: <STORAGE_CLASS>
  volumeMode: Filesystem
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 1Gi
```

Create a DataImport, then upload the file. This example explicitly sets `--wffc=false` so the import does not wait for a
Pod that consumes the PVC:

```bash
d8 data import create -n project my-import -f ./pvc-template.yaml --ttl 10m --wffc=false
d8 data import upload -n project my-import -f ./file.txt -d /file.txt
```

The `-d` flag specifies the path **inside the volume**, while `-f` is the local file path. `upload` finalizes the import
itself, so a separate `POST /api/v1/finished` request is unnecessary. If the command was interrupted, rerun it with
`--resume` to continue from the previous offset:

```bash
d8 data import upload -n project my-import -f ./file.txt -d /file.txt --resume
```

If the connection drops while the command is running, `d8` automatically attempts to continue the transfer.

{{< /tab >}}
{{< tab name="Using a YAML manifest" >}}

Create a DataImport that provisions a new `restored-data` PVC in the `project` namespace. Replace `<STORAGE_CLASS>` with
an existing StorageClass:

```yaml
apiVersion: storage-foundation.deckhouse.io/v1alpha1
kind: DataImport
metadata:
  name: my-import
  namespace: project
spec:
  mode: CreatePVC
  ttl: 10m
  waitForFirstConsumer: false
  pvcTemplate:
    metadata:
      name: restored-data
    spec:
      storageClassName: <STORAGE_CLASS>
      volumeMode: Filesystem
      accessModes:
        - ReadWriteOnce
      resources:
        requests:
          storage: 1Gi
```

Save the manifest as `dataimport.yaml`, apply it, and check the DataImport status:

```bash
d8 k apply -f dataimport.yaml
d8 k -n project get dataimports.storage-foundation.deckhouse.io my-import
```

Applying the manifest creates a DataImport. To transfer the data and send the completion request, use the
[import HTTP API](advanced_usage.html#http-api-data-import). Unlike this example, omitting `waitForFirstConsumer` lets
the API default it to `true`.

{{< /tab >}}
{{< /tabs >}}

#### Importing into a snapshot

In `PopulateData` mode, DataImport writes the uploaded data into a durable snapshot artifact instead of a new PVC.
The mode restores the volume data of a snapshot archive created by `d8 snapshot download`. The `d8 snapshot upload`
command creates the objects of this mode itself: a VolumeSnapshot with `spec.mode: Import` and a DataImport for it.
The procedure is described on the [Moving a snapshot to another namespace or cluster](/modules/state-snapshotter/user_guide.html#moving-a-snapshot-to-another-namespace-or-cluster)
page of the `state-snapshotter` module. The description below explains what the objects created by `d8` do and what
to check if the import does not complete.

A DataImport in `PopulateData` mode has the following fields:

- `snapshotRef` references the existing snapshot in the same namespace that receives the data: a VolumeSnapshot with
  `spec.mode: Import` or a VirtualDiskSnapshot.
- `storageParams` describes the temporary volume into which the data is uploaded: `size`, `storageClassName`, and
  `volumeMode` (`Filesystem` by default). The StorageClass must support snapshots: its
  `storage.deckhouse.io/volumesnapshotclass` annotation must name an existing VolumeSnapshotClass. Otherwise the
  DataImport fails with the `TargetFailed` reason before any data is uploaded.
- `ttl` and `publish` work as in `CreatePVC` mode.

The import proceeds as follows:

1. The controller creates a temporary PVC with the `import-pvc-for-` name prefix in the `d8-storage-foundation`
   namespace and starts the upload server. The quota of the module namespace applies to this PVC; see [Limitations](#limitations).
1. The client uploads the data and sends `POST /api/v1/finished`. The `UploadFinished` condition becomes `True`.
1. The controller stops the upload server, takes a CSI snapshot of the temporary PVC, and sets the deletion policy of
   the resulting VolumeSnapshotContent to `Retain`. The reference to it appears in `status.data.artifactRef`, and the
   `Completed` condition becomes `True`.
1. The temporary PVC is deleted. The `state-snapshotter` module binds the VolumeSnapshotContent to the snapshot named in
   `snapshotRef`, and the snapshot reports `Ready=True`.
1. The DataImport is removed when its `ttl` expires. The VolumeSnapshotContent stays with the snapshot.

Example of a DataImport in `PopulateData` mode for the `data` VolumeSnapshot in the `project` namespace.
`d8 snapshot upload` creates such an object with the name `<SNAPSHOT_NAME>-<ID>` and the `ttl` from its `--ttl` flag,
`1h` by default:

```yaml
apiVersion: storage-foundation.deckhouse.io/v1alpha1
kind: DataImport
metadata:
  name: data-import
  namespace: project
spec:
  mode: PopulateData
  ttl: 1h
  publish: false
  snapshotRef:
    apiVersion: snapshot.storage.k8s.io/v1
    kind: VolumeSnapshot
    name: data
  storageParams:
    storageClassName: <STORAGE_CLASS>
    size: 1Gi
    volumeMode: Filesystem
```

Check the state of the import with the DataImport conditions and the `status.data.artifactRef` field:

```bash
d8 k -n project get dataimports.storage-foundation.deckhouse.io data-import -o jsonpath='{.status.phase}{"\n"}{.status.data.artifactRef}{"\n"}'
```

### Limitations

The following restrictions apply to volume transfers:

- DataExport and DataImport transfer one volume or snapshot per operation.
- Export the same PersistentVolumeClaim or VirtualDisk using only one module, either `storage-foundation` or
  `storage-volume-data-manager`. Simultaneous exports cause a PersistentVolume ownership conflict. Use
  `status.publicURL` from **your own** DataExport. Both modules can construct the same public path for the same target.
- In both modes, the uploaded data first goes to a temporary PVC in the `d8-storage-foundation` namespace, so the
  ResourceQuota of the module namespace applies to every import. The administrator must account for the total size of
  concurrent imports, PVC count, and StorageClass-specific limits
  (`<STORAGE_CLASS>.storageclass.storage.k8s.io/requests.storage` and
  `<STORAGE_CLASS>.storageclass.storage.k8s.io/persistentvolumeclaims`). For a single transfer, the quota must cover at
  least the size of the temporary PVC, which equals the size of the target PVC in `CreatePVC` mode and
  `spec.storageParams.size` in `PopulateData` mode. In `CreatePVC` mode, the target PVC also counts against the quota
  of the user namespace.
- If a DataImport in `CreatePVC` mode enters the `Failed` phase, temporary objects can remain until the DataImport is
  deleted or the objects are garbage-collected.

## Restricting StorageClass and VolumeSnapshotClass per project

When the `multitenancy-manager` module is enabled, the module registers the grants of two cluster-wide resources with it: `StorageClass` (the `storageclasses` definition) and `VolumeSnapshotClass` (the `volumesnapshotclasses` definition). Nothing is restricted until a cluster administrator creates a `ClusterResourceGrantPolicy` for the projects: by default every class is available to every project. If `multitenancy-manager` is disabled, the module registers nothing.

multitenancy-manager ships the `storageclasses` definition and the `storageclasses-pvc` reference too, with the same spec. While it does, either module may own these objects (the `meta.helm.sh/release-name` annotation names the owning release), and ownership can pass from one module to the other when they are updated. The spec stays the same, so the check does not change.

A policy that lists the allowed classes makes the module's objects in the project namespaces follow it. The check is applied to the following fields:

| Object | Field | Resource |
|---|---|---|
| PersistentVolumeClaim | `spec.storageClassName` | `storageclasses` |
| DataImport | `spec.pvcTemplate.spec.storageClassName` (class of the target PVC) | `storageclasses` |
| DataImport | `spec.storageParams.storageClassName` (class of the temporary volume) | `storageclasses` |
| VolumeRestoreRequest | `spec.pvcTemplate.spec.storageClassName` | `storageclasses` |
| VolumeSnapshot | `spec.volumeSnapshotClassName` | `volumesnapshotclasses` |

A request that names a class not available to the project is rejected on creation with the message `[multitenancy] <KIND> "<NAME>" references "<CLASS>" which is not available to project "<PROJECT>"`. The only exception is a PersistentVolumeClaim: its class is replaced with the default class of the project, and the response carries a warning; if the project has no available default class, the claim is rejected. The DataImport and VolumeRestoreRequest fields are only validated and never replaced, so the volume is not created in a class the user did not ask for.

For example, to leave a project only the class `fast-ssd` and the snapshot class `fast-ssd-snapclass`:

```yaml
apiVersion: multitenancy.deckhouse.io/v1alpha2
kind: ClusterResourceGrantPolicy
metadata:
  name: production-storage
spec:
  projectSelector:
    matchLabels:
      environment: production
  resources:
    - resourceName: storageclasses
      allowed:
        - fast-ssd
    - resourceName: volumesnapshotclasses
      allowed:
        - fast-ssd-snapclass
```

Take the following into account:

- If a VolumeSnapshot does not name a class, the grant does not fill one in, but the class the cluster chooses for it is checked as well:
  - If the StorageClass of the source PersistentVolumeClaim has the `storage.deckhouse.io/volumesnapshotclass` annotation, storage-foundation sets that class when the snapshot is created, and the request is rejected if the class is not available to the project.
  - Otherwise, snapshot-controller writes the default VolumeSnapshotClass of the CSI driver into the snapshot after it is created. snapshot-controller runs in the `d8-storage-foundation` namespace and is not exempt from the check, so if the default class is not available to the project, this write is rejected: the snapshot stays without a class and does not become ready, and its `status.error` and a `SnapshotContentCreationFailed` event report `failed to take snapshot <NAME> without a snapshot class`.
- The checked fields are the ones in the table above. Objects of other modules that name a class are checked only if that module registers its own reference.
- Requests from members of the `system:masters` group, from nodes, and from ServiceAccounts of the `kube-system`, `d8-system`, and `d8-user-authz` namespaces are not checked, so a cluster administrator and the platform controllers are never blocked by a policy. The controllers of this module are not exempt: a PersistentVolumeClaim they create in a project namespace is checked like a user's one.
- The PersistentVolumeClaim that a DataImport in `CreatePVC` mode or a VolumeRestoreRequest creates is checked once more when the module creates it:
  - If `pvcTemplate` of a DataImport names no class, the claim gets the default class of the project. Without a project default it gets the default class of the cluster, which then has to be available to the project.
  - If the project loses the named class after the DataImport or VolumeRestoreRequest is created, the claim gets the default class of the project, or is rejected if the project has no available default. A DataImport reports the rejection in the `Ready` condition with the `TargetFailed` reason and the message of the refusal; a VolumeRestoreRequest reports it with a `VRRPVCCreateFailed` event. Both retry until the class is available again. A VolumeRestoreRequest whose claim got another class reports it with a `VRRStorageClassReplaced` event: the restored volume stays in the requested class.
  - The temporary volumes of a DataImport in `PopulateData` mode and of a DataExport are created in the `d8-storage-foundation` namespace, where a policy does not apply. The class of the temporary volume of a DataImport is checked in the DataImport itself (`spec.storageParams.storageClassName`).
- Disabling storage-foundation while multitenancy-manager is enabled removes the grants the module registers: the `volumesnapshotclasses` grant and the DataImport and VolumeRestoreRequest references, and also the `storageclasses` grant and its PersistentVolumeClaim reference `storageclasses-pvc` if the storage-foundation release owns them at that moment (they carry the `meta.helm.sh/release-name: storage-foundation` annotation). Until a grant is registered again, a ClusterResourceGrantPolicy does not restrict the classes of that grant in projects. To restore the restrictions, enable storage-foundation again.
- A project user sees the classes available to the project in the `AvailableClusterResource` objects. For `storageclasses` it shows `provisioner`, `reclaimPolicy`, `volumeBindingMode`, and `allowVolumeExpansion` of every class; for `volumesnapshotclasses`, `driver` and `deletionPolicy`. The `parameters` field of a class is not shown, because it can name secrets. Check the available classes with the following command:

  ```bash
  d8 k get available volumesnapshotclasses -n <PROJECT_NAME> -o yaml
  ```

## Troubleshooting

The sections below describe how to read the state of module objects and how to fix typical errors.

### Checking the state of DataExport and DataImport

Each object reports its lifecycle in `status.phase` and the details in `status.conditions`:

- DataExport: `Pending` → `Ready` → `Expired` or `Failed`. The `Ready` condition carries the reason.
- DataImport: `Pending` → `Ready` → `Completed`, `Expired`, or `Failed`. The `Ready`, `UploadFinished`, and `Completed`
  conditions describe the stages of the import.
- `Terminating` in either object means that the object is being deleted.

View the conditions and the events of the namespace:

```bash
d8 k -n project describe dataexports.storage-foundation.deckhouse.io my-export
d8 k -n project get events --sort-by=.lastTimestamp
```

Reasons of the DataExport `Ready` condition with `status: "False"`:

| Reason | Meaning | What to do |
|---|---|---|
| `ValidationFailed` | The spec or the target did not pass validation: the target kind is not supported, a cluster-scoped VolumeSnapshotContent is referenced, the PVC is not `Bound`, or the PVC is in use. | Correct the spec or release the PVC. The message names the cause. |
| `TargetNotFound` | The target object does not exist, or its CRD is not installed. | Check `spec.targetRef` and the target object. |
| `TargetNotReady` | The snapshot target is not bound to a SnapshotContent of the `state-snapshotter` module yet, or that SnapshotContent has no data yet. | While the snapshot is being processed, wait until the `state-snapshotter` module sets `status.boundSnapshotContentName`. If the module skips the snapshot, it cannot be exported with DataExport. See [VolumeSnapshot fields added by the module](#volumesnapshot-fields-added-by-the-module). |
| `PVConflict` | The PersistentVolume is already owned by another export, or its annotations are inconsistent. | See [Export is not published because of a PersistentVolume conflict](#export-is-not-published-because-of-a-persistentvolume-conflict). |
| `DeploymentFailed` | The data transfer server could not be started. | Check the events and the controller logs. |
| `PublishFailed` | The Service or Ingress for external access could not be created. | Check the events and the controller logs. |
| `CleanupFailed` | Temporary objects could not be removed after the transfer. | Check the events and the controller logs. |
| `Expired` | The idle period set in `spec.ttl` elapsed. | Create a new DataExport. |

The DataImport reasons that indicate a failure are `Failed`, `TargetFailed` (the target PVC or its volume could not be
prepared or handed over), and `CleanupFailed`. The condition message names the cause.

{{< alert level="info" >}}
Reading the controller logs requires cluster administrator permissions.
{{< /alert >}}

Read the controller logs:

```bash
d8 k -n d8-storage-foundation logs -l app=data-manager-controller -c data-manager-controller --prefix --tail=200
```

### VolumeSnapshot creation is rejected

The module validates a VolumeSnapshot created from a PVC at admission time. The error message names the cause:

| Message | Cause | What to do |
|---|---|---|
| `PVC StorageClassName is nil` | The source PVC has no `storageClassName`. | Create snapshots only of PVCs that have a StorageClass. |
| `StorageClass does not have volume snapshot class name annotation` | The StorageClass carries the `storage.deckhouse.io/managed-by` label but has no `storage.deckhouse.io/volumesnapshotclass` annotation. | Contact the administrator: the StorageClass must be annotated with the VolumeSnapshotClass of its driver. |
| `if VolumeSnapshotClassName is set, it must match the StorageClass annotation` | `volumeSnapshotClassName` differs from the annotation of a StorageClass with the `storage.deckhouse.io/managed-by` label. | Omit the field or set it to the annotated class. |

### VolumeSnapshot does not become ready

If `status.readyToUse` stays `false`, check the snapshot, its VolumeSnapshotContent, and the events:

```bash
d8 k -n project describe volumesnapshots.snapshot.storage.k8s.io my-first-snapshot
d8 k get volumesnapshotcontents.snapshot.storage.k8s.io
d8 k -n project get events --sort-by=.lastTimestamp
```

{{< alert level="info" >}}
Checking the snapshot controller Pods and reading their logs requires cluster administrator permissions.
{{< /alert >}}

The `status.error` field of the VolumeSnapshot carries the message of the CSI driver. If there is no message, check that
the snapshot controller is running and read its logs:

```bash
d8 k -n d8-storage-foundation get pod -l app=snapshot-controller
d8 k -n d8-storage-foundation logs -l app=snapshot-controller -c snapshot-controller --tail=200
```

The `D8SnapshotControllerTargetDown`, `D8SnapshotControllerTargetAbsent`, `D8SnapshotControllerPodIsNotReady`, and
`D8SnapshotControllerPodIsNotRunning` alerts report that the snapshot controller itself is unavailable.

### d8 data cannot find a module or is not authorized

Before every command, `d8 data` resolves which module serves DataExport and DataImport. Two errors are possible:

- `no module in this cluster serves this resource`: neither `storage-foundation` nor `storage-volume-data-manager` is
  enabled, or its CRDs are not installed yet. Enable the module or wait for the installation to finish.
- `not authorized to use this resource`: the resource is served, but the user may not `get` it in the namespace in any
  of the groups. The message names the groups found and the command to check the permissions, for example
  `d8 k auth can-i get dataexports.storage-foundation.deckhouse.io -n project`. Grant the permissions as described in
  [Access permissions](#access-permissions).

If the objects created by `d8` do not appear in the `storage-foundation.deckhouse.io` group, the user may `get` them
only in the `storage.deckhouse.io` group, and `d8` works with the `storage-volume-data-manager` module. Grant the
permissions in the `storage-foundation.deckhouse.io` group.

### d8 cannot auto-detect the publish mode

The command exits with `cannot auto-detect publish mode, specify --publish=true or --publish=false` when the `--publish`
flag is omitted and the probe of the `kubernetes` Service described in [Key parameters](#key-parameters) fails with an
error other than an unreachable network or a TLS rejection. Pass the flag explicitly: `--publish=false` from the cluster
network, `--publish=true` from outside.

### Export does not start because the PVC is in use

A PVC is exported only while no Pod mounts it. If a Pod uses the PVC, the `Ready` condition of DataExport reports
`ValidationFailed` with the message `user's PVC isn't free because it's being occupied by pods <POD_NAMES>`, or
`user's PV not free because has volumeAttachments with status True: <ATTACHMENT_NAMES>` when the volume is still
attached to a node. The controller rechecks the PVC every 10 seconds. Stop the Pods that use the PVC, or export a
VolumeSnapshot of the PVC instead.

### Export is not published because of a PersistentVolume conflict

If DataExport does not become ready and its `Ready` condition reports `PVConflict`, the PersistentVolume is already
owned by another export: a DataExport of this module, named in the message
`PV <PV_NAME> is already owned by DataExport <NAMESPACE>/<NAME>`, or an export through `storage-volume-data-manager`.
Finish the first export and allow the volume to be released before starting another one. Use `status.publicURL` from
your own DataExport for a published export.

### Import does not start because of ResourceQuota limits

If the quota is exceeded, the API server may refuse creation of the PVC with `403 Forbidden` and an `exceeded quota`
message. In this case, the PVC is not created. A PVC in `Pending` is not proof of a quota issue; it may indicate
insufficient capacity, for example.

Which quota applies depends on the import mode:

- In `CreatePVC` mode, two quotas apply. The quota of the user namespace covers the target PVC, and the quota of the
  `d8-storage-foundation` namespace covers a temporary PVC of the same size and StorageClass, which receives the data
  before the volume is handed over to the target PVC.
- In `PopulateData` mode, only the temporary PVC is created in the `d8-storage-foundation` namespace, so the quota of
  the module namespace applies.

Only an administrator can inspect and adjust the quota of the `d8-storage-foundation` namespace.

The user can inspect DataImport conditions, events, and the quota in their own namespace:

```bash
d8 k -n project describe dataimports.storage-foundation.deckhouse.io my-import
d8 k -n project get events --sort-by=.lastTimestamp
d8 k -n project get resourcequota
```

{{< alert level="info" >}}
The following checks in the `d8-storage-foundation` namespace require cluster administrator permissions.
{{< /alert >}}

For `PopulateData` mode, an administrator checks ResourceQuota, temporary PVCs, and events in the module namespace. If
the cause is not visible in events, the administrator checks the import controller logs:

```bash
d8 k -n d8-storage-foundation get resourcequota
d8 k -n d8-storage-foundation get pvc
d8 k -n d8-storage-foundation get events --sort-by=.lastTimestamp
d8 k -n d8-storage-foundation logs -l app=data-manager-controller -c data-manager-controller --prefix --tail=200
```

Check the overall storage quota, the PVC count, and per-StorageClass limits. If exceeded, the administrator adjusts the
quota or reduces concurrent imports. Absence of an error in DataImport conditions does not rule out API admission
rejection.

### DataImport or DataExport remains in Terminating

Delete the DataImport or DataExport normally. If the object remains in `Terminating`, inspect events and dependent
objects and contact the administrator. Removing a finalizer is a last resort only after inspecting dependent objects;
otherwise temporary PVCs, PVs, or Pods may remain in the cluster. Do not delete a PVC merely because of its name or an
`app: data-importer` label.
