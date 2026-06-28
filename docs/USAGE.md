---
title: "The storage-foundation module: configuration examples"
description: "Examples for creating volume snapshots, restoring from snapshots, and cloning PVCs using the storage-foundation module."
---

## Using snapshots

To use snapshots, specify a `VolumeSnapshotClass`.
To get a list of available VolumeSnapshotClasses in your cluster, run:

```shell
d8 k get volumesnapshotclasses.snapshot.storage.k8s.io
```

You can then use VolumeSnapshotClass to create a snapshot from an existing PVC:

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

After a short wait, the snapshot will be ready:

```console
$ d8 k describe volumesnapshots.snapshot.storage.k8s.io my-first-snapshot
...
Spec:
  Source:
    Persistent Volume Claim Name:  my-first-snapshot
  Volume Snapshot Class Name:      sds-replicated-volume
Status:
  Bound Volume Snapshot Content Name:  snapcontent-b6072ab7-6ddf-482b-a4e3-693088136d2c
  Creation Time:                       2020-06-04T13:02:28Z
  Ready To Use:                        true
  Restore Size:                        500Mi
```

You can restore the content of this snapshot by creating a new PVC with the snapshot as source:

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

## CSI Volume Cloning

Based on the concept of snapshots, you can also perform cloning of persistent volumes — or, more precisely, existing persistent volume claims (PVC).
However, the CSI specification mentions some restrictions regarding cloning PVCs in different namespaces or storage classes than the original PVC.
For details, see [Kubernetes documentation](https://kubernetes.io/docs/concepts/storage/volume-pvc-datasource/).

To clone a volume create a new PVC and define the origin PVC in the dataSource:

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

## Exporting and importing volume data over HTTP

### Creating DataExport and DataImport resources with the d8 utility

Use the `d8` utility to create and manage `DataExport` and `DataImport` resources.

Create a `DataExport` — the target volume is passed as a positional `<type>/<name>` argument:

```bash
d8 data export create -n <namespace> <DataExport name> <type>/<name> --ttl 5m --publish=(true/false)
```

Supported target types: `pvc`/`persistentvolumeclaim`, `vs`/`volumesnapshot`, `vd`/`virtualdisk`, `vds`/`virtualdisksnapshot`.

Create a `DataImport` — the destination PVC is provided as a manifest via `-f` (there is no positional volume argument):

```bash
d8 data import create -n <namespace> <DataImport name> -f <pvc-manifest> --ttl 2m --publish --wffc
```

{{< alert level="warning" >}}
PVC resources can only be exported when they are not currently mounted by any pods.
{{< /alert >}}

Example: creating a `DataExport` resource for a PVC named "data" in the "project" namespace with a 5-minute TTL:

```bash
d8 data export create -n project my-export pvc/data --ttl 5m
```

To view details about the created `DataExport` resource:

```bash
d8 k -n project get de my-export
```

To download files from exported volumes:

```bash
d8 data export download -n <namespace> <resource type (pvc/vs/dataexport)>/<resource name>/<file path> -o <file name> --publish=true
```

Example:

```bash
d8 data export download -n project dataexport/my-export -o test_file.txt --publish=true
```

### Alternative ways to create and use DataExport and DataImport resources without the d8 utility

Besides using the `d8` utility, you can create `DataExport` and `DataImport` resources directly through a YAML manifest. In the example below, environment variables are used for easier configuration, replace their values with the required ones:

```bash
export NAMESPACE="d8-storage-foundation"
export DATA_EXPORT_RESOURCE_NAME="example-dataexport"
export TARGET_TYPE="PersistentVolumeClaim"
export TARGET_NAME="fs-pvc-data-exporter-fs-0"
```

```bash
d8 k apply -f -<<EOF
apiVersion: storage.deckhouse.io/v1alpha1
kind: DataExport
metadata:
  name: ${DATA_EXPORT_RESOURCE_NAME}
  namespace: ${NAMESPACE}
spec:
  ttl: 10h
  targetRef:
    kind: ${TARGET_TYPE}
    name: ${TARGET_NAME}
EOF
```

After creating the resource, extract the CA certificate by executing the following command:

```bash
d8 k -n $NAMESPACE get dataexport $DATA_EXPORT_RESOURCE_NAME  -o jsonpath='{.status.ca}' | base64 -d > ca.pem
```

Check the certificate:

```bash
openssl x509 -in ca.pem -noout -text | head
```

Example output:

```console
Issuer: CN = data-exporter-CA
Signature Algorithm: ecdsa-with-SHA256
```

Extract the URL from the DataExport resource and verify the export:

```bash
export POD_URL=$(d8 k -n $NAMESPACE get dataexport $DATA_EXPORT_RESOURCE_NAME  -o jsonpath='{.status.url}')
echo "POD_URL: $POD_URL"
```

After creating the DataExport resource and extracting the necessary data, you can connect to the exporter using one of the following methods:

#### 1. Authentication using certificate and key from local kubeconfig

This method uses existing credentials from your local `kubeconfig` file. Extract the keys from the configuration by executing the following commands:

```bash
cat ~/.kube/config | grep "client-certificate-data" | awk '{print $2}' | base64 -d > client.crt
cat ~/.kube/config | grep "client-key-data" | awk '{print $2}' | base64 -d > client.key
```

Check the contents of the target PVC using the command:

```bash
curl -v --cacert ca.pem ${POD_URL}api/v1/files/ --key client.key --cert client.crt
```

#### 2. Authentication using token and roles

This method involves creating a separate ServiceAccount with appropriate access permissions. Create a ServiceAccount using the command:

```bash
d8 k -n $NAMESPACE create serviceaccount data-exporter-test
```

Create a ClusterRole by executing the following command:

```bash
d8 k create -f - <<EOF
kind: ClusterRole
apiVersion: rbac.authorization.k8s.io/v1
metadata:
  name: data-exporter-test-role
rules:
- apiGroups: ["storage.deckhouse.io"]
  resources: ["dataexports/download"]
  verbs: ["create"]
EOF
```

Create a token by running the commands:

```bash
export TOKEN=$(d8 k -n $NAMESPACE create token data-exporter-test --duration=24h)
echo $TOKEN
```

Create a RoleBinding by applying the manifest:

```bash
d8 k create -f - <<EOF
kind: RoleBinding
apiVersion: rbac.authorization.k8s.io/v1
metadata:
  name: data-exporter-test-role-binding
  namespace: ${NAMESPACE}
subjects:
- kind: ServiceAccount
  name: data-exporter-test
  namespace: ${NAMESPACE}
roleRef:
  kind: ClusterRole
  name: data-exporter-test-role
  apiGroup: rbac.authorization.k8s.io
EOF
```

Check the contents of the target PVC by executing the request:

```bash
curl -H "Authorization: Bearer $TOKEN" \
-v --cacert ca.pem ${POD_URL}api/v1/files/
```

### HTTP API: data export

All endpoints are available via the short path `/api/v1/files|block` and the full path `/{namespace}/{kindShort}/{name}/api/v1/files|block`.

Definitions:

- kindShort — short name of the exported resource (persistentVolumeClaim => pvc)
- name — the exported resource name

#### Filesystem mode (export)

- Get file

```http
    GET /api/v1/files/{path} HTTP/1.1

    Query (optional, for extra attributes):
      attribute=stat      — return static attributes (uid, gid, permissions, modtime)
      attribute=hash.md5  — return MD5 for regular files
```

- Directory listing

```http
    GET /api/v1/files/{path/} HTTP/1.1  # directory path must end with '/'

    Query (optional): attribute=stat | attribute=hash.md5
```

- Metadata without body

```http
    HEAD /api/v1/files/{path|path/} HTTP/1.1
```

Filesystem errors (main):

```http
    400 Bad Request  — type mismatch (file/directory), forbidden links, etc.
    404 Not Found    — path not found
    405 Method Not Allowed — methods other than GET/HEAD
    500 Internal Server Error — internal error
```

#### Block mode (export)

- Download block device (raw image)

```http
    GET /api/v1/block HTTP/1.1
```

- Metadata without body

```http
    HEAD /api/v1/block HTTP/1.1
```

### HTTP API: data import

All endpoints are available via the short path `/api/v1/files|block` and the full path `/{namespace}/{kindShort}/{name}/api/v1/files|block`.

#### Filesystem mode (import)

- Upload file

```http
    PUT /api/v1/files/{path} HTTP/1.1
    Content-Length: <bytes> — required header
    X-Attribute-Permissions: 0775 — required header (octal 0000–0777)
    X-Attribute-Uid: <uid> — required header
    X-Attribute-Gid: <gid> — required header
    X-Offset: 0 — offset from the file start (non-negative integer)
    X-Content-Length: 10 — required header, expected total file size (non-negative integer)
    X-Attribute-ModTime: RFC3339 timestamp (e.g., 2024-12-27T10:16:53.4537715+03:00)
```

- Query current upload progress

```http
    HEAD /api/v1/files/{path} HTTP/1.1
    Content-Length: 0
```

- Finish import (signal to stop the file server)

```http
    POST /api/v1/finished HTTP/1.1
    Content-Length: 0
```

#### Block mode (import)

- Upload raw block images

```http
    PUT /api/v1/block HTTP/1.1
    Content-Length: <bytes> — required header
    X-Attribute-Permissions: 0775 — required header (octal 0000–0777)
    X-Attribute-Uid: <uid> — required header
    X-Attribute-Gid: <gid> — required header
    X-Offset: 0 — optional; offset from the device start (non-negative integer)
    X-Content-Length: 10 — optional; expected total size (non-negative integer)
```

- Query current upload progress

```http
    HEAD /api/v1/block HTTP/1.1
    Content-Length: 0
```

- Finish import (signal to stop the file server)

```http
    POST /api/v1/finished HTTP/1.1
    Content-Length: 0
```

### Important notes on working with the API

When working with exported data through the HTTP API, consider the following:

- File downloads: Files are downloaded using standard GET requests containing the file path in the URL, e.g. `GET /api/v1/files/largeimage.iso`. The file path must not end with `/`. This download method is supported by standard tools (browsers, curl, etc.). Resuming a download is supported, but compression is not.
- Directory browsing: Accessing a directory uses a similar GET request where the directory path must end with `/`, e.g. `GET /api/v1/files/` for the root or `GET /api/v1/files/directory/` for a subdirectory.
- File listing: When accessing a directory, a JSON listing is returned in the response body, including the name, type, and size of each entry. File sizes are not cached and are recalculated on each directory request.
