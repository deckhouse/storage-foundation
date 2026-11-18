---
title: "Module storage-foundation: advanced usage"
weight: 50
description: "HTTP client authentication, manual permissions, and volume export and import API protocols."
---

## HTTP client authentication

To transfer data over HTTP, first create a DataExport or DataImport in the user's namespace as described in
[Exporting and importing volume data](usage.html#exporting-and-importing-volume-data). Use `status.url` from the cluster
network or `status.publicURL` for access through the published Ingress. The URLs use different TLS trust chains.

For the **internal** `status.url`, the server CA is in `status.ca` (Base64). Example for DataExport `my-export` in
namespace `project`:

```bash
d8 k -n project get dataexports.storage-foundation.deckhouse.io my-export -o jsonpath='{.status.ca}' | base64 -d > internal-ca.pem
INTERNAL_URL=$(d8 k -n project get dataexports.storage-foundation.deckhouse.io my-export -o jsonpath='{.status.url}')
```

The **published** `status.publicURL` uses the same TLS certificate as the published Kubernetes API
(`api.<publicDomain>`), not the certificate from `status.ca`. No `--cacert` is needed if the certificate is trusted by
the system. Otherwise use the CA for the published API, for example the `certificate-authority-data` from a kubeconfig
that connects to `api.<publicDomain>`.

```bash
# Run this only when the system does not trust the CA of the published API.
d8 k config view --raw --minify --flatten -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d > public-api-ca.pem
```

The `d8` client combines system CAs, kubeconfig CAs, and the internal `status.ca`. With `curl`, select a CA that matches
the endpoint. Do not use `status.ca` as the CA for `status.publicURL`.

### Client certificate from kubeconfig (internal URL only)

If your kubeconfig contains an authenticated client certificate and key, you can use them **only with `status.url`**.
The published Ingress terminates TLS, so the client certificate is not forwarded to the transfer server. Protect the
extracted private key and delete the temporary files after use.

```bash
umask 077
d8 k config view --raw --minify --flatten -o jsonpath='{.users[0].user.client-certificate-data}' | base64 -d > client.crt
d8 k config view --raw --minify --flatten -o jsonpath='{.users[0].user.client-key-data}' | base64 -d > client.key
curl --cacert internal-ca.pem --cert client.crt --key client.key "${INTERNAL_URL%/}/api/v1/files/"
```

### Token authentication and permissions

For published access, use a Kubernetes bearer token. Create a ServiceAccount, bind the built-in data transfer role,
and issue a time-limited token:

```bash
d8 k -n project create serviceaccount data-transfer
d8 k -n project create rolebinding data-transfer --clusterrole=d8:namespace-capability:storage-foundation:transfer_volume_data --serviceaccount=project:data-transfer
TOKEN=$(d8 k -n project create token data-transfer --duration=24h)
```

You can [configure narrower permissions manually](#manual-rbac-configuration) instead of using the
[built-in data transfer role](usage.html#access-permissions). For Deckhouse Platform versions earlier than 1.78,
use the legacy role name from the access permissions table.

Create a DataExport with published access enabled and wait until `status.publicURL` is populated:

```bash
d8 data export create -n project my-export pvc/data --ttl 10m --publish=true

d8 k -n project wait dataexports.storage-foundation.deckhouse.io/my-export \
  --for=jsonpath='{.status.publicURL}' --timeout=5m
```

Changing `spec.publish` on an existing DataExport requires the `patch` permission for DataExport objects. The
built-in data transfer role does not grant this permission.

Then request data with the bearer token:

```bash
PUBLIC_URL=$(d8 k -n project get dataexports.storage-foundation.deckhouse.io my-export -o jsonpath='{.status.publicURL}')
curl -H "Authorization: Bearer ${TOKEN}" "${PUBLIC_URL%/}/api/v1/files/"
```

If the published API uses a certificate signed by a CA that the system does not trust, add `--cacert public-api-ca.pem`
to this command. Use the internal URL and `--cacert internal-ca.pem` when connected to the cluster network.

## Manual RBAC configuration

Data endpoint requests use `SubjectAccessReview` with the `create` verb on the `dataexports/download` subresource for
exporting or `dataimports/download` for importing in the `storage-foundation.deckhouse.io` API group. Import also uses a
`download` subresource despite the upload direction. No `upload` subresource exists.

An example minimal ClusterRole that permits reading exported data:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: volume-export-http-client
rules:
  - apiGroups: ["storage-foundation.deckhouse.io"]
    resources: ["dataexports/download"]
    verbs: ["create"]
```

Add `dataimports/download` to `resources` to permit imports as well. This role authorizes only HTTP data transfer;
creating the DataExport or DataImport object requires separate permissions. Bind the role to a ServiceAccount in the
target namespace:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: volume-export-http-client
  namespace: project
subjects:
  - kind: ServiceAccount
    name: data-transfer
    namespace: project
roleRef:
  kind: ClusterRole
  name: volume-export-http-client
  apiGroup: rbac.authorization.k8s.io
```

The check names no object, so such a binding permits transferring data of every DataExport or DataImport in the
namespace, not only of the ones the subject created.

When auditing permissions, account for the `Editor` and higher access levels of the current role model, the namespace
`manager` and `admin` levels of RBACv2, and bindings of `d8:state-snapshotter:backup-agent` as well. For details, see
[Backup integration](/modules/state-snapshotter/backup_integration.html).

## Export and import HTTP API

Data endpoints support the short paths `/api/v1/files` or `/api/v1/block` and the full paths
`/<NAMESPACE>/<KIND_SHORT>/<NAME>/api/v1/files` or `/<NAMESPACE>/<KIND_SHORT>/<NAME>/api/v1/block`. For exports,
`<KIND_SHORT>` identifies the source kind, such as `pvc`. For imports, use the URL belonging to your DataImport. Do not
mix URLs issued by different modules.

### HTTP API: data export

HTTP export supports `GET` for reading, `HEAD` for metadata, directory listings, and resume through HTTP `Range`. This API
does not apply compression. Directory listing file sizes are computed per request rather than cached.

{{< tabs name="export_volume_mode" >}}
{{< tab name="Filesystem mode" >}}

Use the following requests to read files, directories, links, and their metadata.

#### Get a file

The `attribute=stat` query parameter requests UID, GID, permissions, and modification time. The stat headers are
returned even if the parameter is omitted. Use `attribute=hash.md5` to request an MD5 hash of a regular file;
calculating the hash requires reading the entire file and can delay responses for large files.

Example request for file contents and MD5:

```http
GET /api/v1/files/<FILE_PATH>?attribute=hash.md5 HTTP/1.1
```

Replace `<FILE_PATH>` with the path to the file relative to the volume root.

Response (regular file with requested MD5):

```http
HTTP/1.1 200 OK
Content-Type: application/octet-stream
Content-Length: <SIZE>
Content-Disposition: attachment; filename=<BASE_NAME>
X-Attribute-Uid: <UID>
X-Attribute-Gid: <GID>
X-Attribute-Permissions: <PERMISSIONS>
X-Attribute-Modtime: <RFC3339_TIMESTAMP>
X-Attribute-Hash-Md5: <MD5>
X-Type: file

<FILE_CONTENT>
```

`<BASE_NAME>` is the file's base name, written as a header parameter per RFC 6266: a name that is not a plain token is
quoted (`filename="a b.txt"`), and a name with non-ASCII or control characters is percent-encoded as
`filename*=utf-8''<ENCODED_NAME>`.

For a symbolic link, the server reports the link and never follows it, so a client copying a tree recreates the link
instead of duplicating its target. `Content-Length` is always `0`:

```http
HTTP/1.1 200 OK
Content-Type: application/x-symlink
Content-Length: 0
X-Attribute-Uid: <UID>
X-Attribute-Gid: <GID>
X-Attribute-Permissions: <PERMISSIONS>
X-Attribute-Modtime: <RFC3339_TIMESTAMP>
X-LinkTarget: /dir/file
X-Type: link
```

#### List a directory

The directory path must end with `/`. The optional query parameters are the same as for a file request.
Requesting MD5 hashes for directory entries can be expensive because each regular file must be read.

```http
GET /api/v1/files/<DIR_PATH>/ HTTP/1.1
```

Replace `<DIR_PATH>` with the path to the directory relative to the volume root. For the volume root, use
`/api/v1/files/`.

Response (JSON formatted for readability):

```http
HTTP/1.1 200 OK
Content-Type: application/json
Transfer-Encoding: chunked
X-Attribute-Uid: <UID>
X-Attribute-Gid: <GID>
X-Attribute-Permissions: <PERMISSIONS>
X-Attribute-Modtime: <RFC3339_TIMESTAMP>
X-Type: dir

{
  "apiVersion": "v1",
  "items": [
    {
      "name": "1.txt",
      "type": "file",
      "uri": "1.txt",
      "attributes": {
        "gid": 1000,
        "modtime": "2025-04-01T08:02:00Z",
        "permissions": "0644",
        "size": 12,
        "uid": 1000
      }
    }
  ]
}
```

#### Get file or directory metadata without a body

Request the metadata of a file or directory:

```http
HEAD /api/v1/files/<FILE_PATH> HTTP/1.1
```

To get directory metadata, use the path `/api/v1/files/<DIR_PATH>/`.

The response carries the same headers as the corresponding GET (file, directory, or symbolic link) with no body.
`X-Type` is what distinguishes the three cases, so a client can classify an entry without fetching it.

{{< /tab >}}
{{< tab name="Block mode" >}}

Use the block endpoint to download the raw contents of the exported block device or inspect its metadata.

#### Download a block device (RAW image)

Request the contents of the block device:

```http
GET /api/v1/block HTTP/1.1
```

Response:

```http
HTTP/1.1 200 OK
Content-Type: application/octet-stream
Content-Length: <DEVICE_SIZE>
Content-Disposition: attachment; filename=data.img

<RAW_BLOCK_CONTENT>
```

#### Get block device metadata without a body

Request the block device metadata:

```http
HEAD /api/v1/block HTTP/1.1
```

The response repeats the headers of the GET above with no body, which is how a client learns the device size before
starting a download.

{{< /tab >}}
{{< /tabs >}}

### HTTP API: data import

Use `PUT` with length and offset headers to transfer content, `HEAD` to query the offset, and `POST /api/v1/finished` to
complete the import. Unlike `d8 data import upload`, a direct HTTP client must send the completion request explicitly.

A `POST` to the data endpoint itself (`/api/v1/files/<FILE_PATH>` or `/api/v1/block`) completes the import as well and
answers `200 OK`. Both routes do the same thing; `/api/v1/finished` is the one to prefer, because it does not require a
path.

{{< tabs name="import_volume_mode" >}}
{{< tab name="Filesystem mode" >}}

Use the following requests to upload files and track upload progress.

#### Upload a file

Send the file in two five-byte chunks. `Content-Length` is the size of the current request body, `X-Offset` is the
current file offset, and `X-Content-Length` is the total expected file size.

Every `PUT` request must include `X-Offset`, `X-Content-Length`, `X-Attribute-Permissions`, `X-Attribute-Uid`, and
`X-Attribute-Gid`. If any of them is missing, the server returns `400` with a JSON body that lists the missing headers
in `missingHeaders`. `X-Attribute-ModTime` is optional.

The attribute headers set the attributes of the uploaded file. `X-Attribute-Permissions` sets the permissions in octal
(`0000`–`0777`), `X-Attribute-Uid` and `X-Attribute-Gid` set the owner and the group, and `X-Attribute-ModTime` sets
the modification time in RFC3339. Send the same attribute values in every chunk of a file.

First chunk:

```http
PUT /api/v1/files/<FILE_PATH> HTTP/1.1
Content-Length: 5
X-Offset: 0
X-Content-Length: 10
X-Attribute-Permissions: 0644
X-Attribute-Uid: 1000
X-Attribute-Gid: 1000

hello
```

Replace `<FILE_PATH>` with the path where the file is created in the volume.

While the written size is less than `X-Content-Length`, the data is stored in a temporary `.tempupload` file and the
server returns `204`. The response tells where the next chunk must start, in this case at offset `5`:

```http
HTTP/1.1 204 No Content
X-Next-Offset: 5
```

Second chunk:

```http
PUT /api/v1/files/<FILE_PATH> HTTP/1.1
Content-Length: 5
X-Offset: 5
X-Content-Length: 10
X-Attribute-Permissions: 0644
X-Attribute-Uid: 1000
X-Attribute-Gid: 1000

world
```

The file is renamed from `.tempupload` to its final name when the written size reaches `X-Content-Length`. The response
for a completed file has no `X-Next-Offset`:

```http
HTTP/1.1 201 Created
```

After all files are uploaded, send `POST /api/v1/finished` even if the final chunk returned `201`.

#### Query file upload progress

Request the current upload offset of the file:

```http
HEAD /api/v1/files/<FILE_PATH> HTTP/1.1
Content-Length: 0
```

Response while the upload is incomplete:

```http
HTTP/1.1 200 OK
X-Next-Offset: 5
```

#### Finish the file import

Complete the import:

```http
POST /api/v1/finished HTTP/1.1
Content-Length: 0
```

Response:

```http
HTTP/1.1 204 No Content
```

If a `409` response includes `X-Expected-Offset`, resume the upload from the offset in this header. Without
`X-Expected-Offset`, a `409` response means that another write is in progress or the file is already fully uploaded.
Changing the offset will not help. Check the state of the file with a `HEAD` request.

{{< /tab >}}
{{< tab name="Block mode" >}}

Use the block endpoint to upload a RAW image and track the current offset and target-device size.

#### Upload a RAW block image

`Content-Length` is the size of the current request body, `X-Offset` is the device offset, and `X-Content-Length` is
the total expected upload size.

Every `PUT` request must include `X-Offset`, `X-Content-Length`, `X-Attribute-Permissions`, `X-Attribute-Uid`, and
`X-Attribute-Gid`, the same as in filesystem mode. The server requires the attribute headers for a block device as
well but does not apply their values.

First chunk:

```http
PUT /api/v1/block HTTP/1.1
Content-Length: 5
X-Offset: 0
X-Content-Length: 10
X-Attribute-Permissions: 0644
X-Attribute-Uid: 1000
X-Attribute-Gid: 1000

hello
```

Partial upload response:

```http
HTTP/1.1 204 No Content
X-Next-Offset: 5
```

Second chunk:

```http
PUT /api/v1/block HTTP/1.1
Content-Length: 5
X-Offset: 5
X-Content-Length: 10
X-Attribute-Permissions: 0644
X-Attribute-Uid: 1000
X-Attribute-Gid: 1000

world
```

On completion, the response retains `X-Next-Offset`, unlike filesystem mode:

```http
HTTP/1.1 201 Created
X-Next-Offset: 10
```

Always send `POST /api/v1/finished` after transferring the data, including when the final chunk returned `201`.

#### Query block upload progress

Request the current offset and the size of the target device:

```http
HEAD /api/v1/block HTTP/1.1
Content-Length: 0
```

The response contains the target device size in bytes in `X-Device-Size`. The API reports the device size only in this
response:

```http
HTTP/1.1 200 OK
X-Next-Offset: 10
X-Device-Size: 1024
```

#### Finish the block import

Complete the import:

```http
POST /api/v1/finished HTTP/1.1
Content-Length: 0
```

Response:

```http
HTTP/1.1 204 No Content
```

A `416` response does not include protocol headers with the remaining or required size. Read `X-Device-Size` from the HEAD
response before starting the upload so that an image larger than the target device can be rejected before a long-running
transfer begins.

{{< /tab >}}
{{< /tabs >}}

### HTTP response codes

The table lists HTTP error codes by operation. Successful responses (`200`, `201`, `204`) are shown in the examples
above. Authentication and authorization errors can also be returned before a request reaches the data handler.

| Operation | Code | When it is returned |
|---|---|---|
| Filesystem export (GET, HEAD) | `400` | Incorrect file or directory path/type, forbidden symlink traversal, or invalid request |
| Filesystem export (GET, HEAD) | `404` | Path not found |
| Filesystem export (GET, HEAD) | `405` | Method other than GET or HEAD |
| Filesystem export (GET, HEAD) | `500` | Internal error |
| Block export (GET, HEAD) | `400` | Invalid block device path |
| Block export (GET, HEAD) | `405` | Method other than GET or HEAD |
| Block export (GET, HEAD) | `500` | Internal error |
| Filesystem import (PUT, HEAD, POST) | `400` | Missing required PUT header (listed in `missingHeaders` of the JSON body), invalid header value, or HEAD request to a directory |
| Filesystem import (PUT, HEAD, POST to the data endpoint) | `403` | Import already completed |
| Filesystem import (HEAD) | `404` | File not found |
| Filesystem import (PUT, HEAD, POST) | `405` | Method other than PUT, HEAD, or POST |
| Filesystem import (PUT) | `409` | Offset mismatch, competing writer, or completed file; mismatch includes `X-Expected-Offset` |
| Filesystem import (PUT, HEAD, POST) | `500` | Internal filesystem error |
| Block import (PUT, HEAD, POST) | `400` | Extra path components or a trailing slash after `/api/v1/block`, missing required PUT header (listed in `missingHeaders` of the JSON body), or invalid header value |
| Block import (PUT, HEAD, POST to the data endpoint) | `403` | Import already completed. For PUT, the error may also indicate insufficient permissions to write to the block device |
| Block import (PUT, HEAD, POST) | `405` | Method other than PUT, HEAD, or POST |
| Block import (PUT) | `409` | Offset mismatch or competing writer; mismatch includes `X-Expected-Offset` |
| Block import (PUT) | `416` | Request exceeds the remaining device size |
| Block import (PUT) | `422` | Request body goes past `X-Content-Length` |
| Block import (PUT, HEAD, POST) | `500` | Internal error |
