---
title: "Модуль storage-foundation: примеры конфигурации"
description: "Примеры создания снапшотов томов, восстановления из снапшотов и клонирования PVC с помощью модуля storage-foundation."
---

## Использование снапшотов

Чтобы использовать снапшоты, укажите конкретный `VolumeSnapshotClass`.
Чтобы получить список доступных VolumeSnapshotClass в вашем кластере, выполните:

```shell
d8 k get volumesnapshotclasses.snapshot.storage.k8s.io
```

Затем вы сможете использовать VolumeSnapshotClass для создания снапшота из существующего тома:

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

Спустя небольшой промежуток времени снапшот будет готов:

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

Можно восстановить содержимое этого снапшота, создав новый PVC. Для этого необходимо указать снапшот в качестве источника:

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

## Клонирование CSI-томов

На основе концепции снапшотов также можно выполнить клонирование Persistent Volume, а именно, существующих PersistentVolumeClaim (PVC).
Однако спецификация CSI допускает ряд ограничений при клонировании томов в неймспейсах или StorageClass-ах, отличных от оригинального PVC.
Подробнее об ограничениях см. [в документации Kubernetes](https://kubernetes.io/docs/concepts/storage/volume-pvc-datasource/).

Чтобы клонировать том, создайте новый PVC и укажите исходный PVC в `dataSource`:

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

## Экспорт и импорт содержимого томов по HTTP

### Создание ресурсов DataExport и DataImport с помощью утилиты d8

Для создания и управления ресурсами `DataExport` и `DataImport` используется утилита `d8`.

Создание `DataExport` — целевой том передаётся позиционным аргументом `<тип>/<имя>`:

```bash
d8 data export create -n <namespace> <имя DataExport> <тип>/<имя> --ttl 5m --publish=(true/false)
```

Поддерживаемые типы целевых ресурсов: `pvc`/`persistentvolumeclaim`, `vs`/`volumesnapshot`, `vd`/`virtualdisk`, `vds`/`virtualdisksnapshot`.

Создание `DataImport` — целевой PVC задаётся манифестом через `-f` (позиционного аргумента тома нет):

```bash
d8 data import create -n <namespace> <имя DataImport> -f <манифест-pvc> --ttl 2m --publish --wffc
```

{{< alert level="warning" >}}
Ресурсы PVC могут быть экспортированы только в том случае, если они не смонтированы какими-либо подами в данный момент.
{{< /alert >}}

Пример: создание ресурса `DataExport` для PVC с именем `data` в неймспейсе `project` с TTL 5 минут:

```bash
d8 data export create -n project my-export pvc/data --ttl 5m
```

Для просмотра деталей созданного ресурса `DataExport`:

```bash
d8 k -n project get de my-export
```

Для скачивания файлов из экспортированных томов:

```bash
d8 data export download -n <namespace> <тип ресурса (pvc/vs/dataexport)>/<имя ресурса>/<путь к файлу> -o <имя файла> --publish=true
```

Пример:

```bash
d8 data export download -n project dataexport/my-export -o test_file.txt --publish=true
```

### Альтернативные способы создания и использования DataExport и DataImport без утилиты d8

Помимо утилиты `d8`, ресурсы `DataExport` и `DataImport` можно создавать напрямую через YAML-манифест. В примере ниже для удобства используются переменные окружения, замените их значения на нужные:

```bash
export NAMESPACE="d8-storage-foundation"
export DATA_EXPORT_RESOURCE_NAME="example-dataexport"
export TARGET_TYPE="PersistentVolumeClaim"
export TARGET_NAME="fs-pvc-data-exporter-fs-0"
```

```bash
d8 k apply -f -<<EOF
apiVersion: storage-foundation.deckhouse.io/v1alpha1
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

После создания ресурса извлеките CA-сертификат:

```bash
d8 k -n $NAMESPACE get dataexport $DATA_EXPORT_RESOURCE_NAME  -o jsonpath='{.status.ca}' | base64 -d > ca.pem
```

Проверьте сертификат:

```bash
openssl x509 -in ca.pem -noout -text | head
```

Пример вывода:

```console
Issuer: CN = data-exporter-CA
Signature Algorithm: ecdsa-with-SHA256
```

Извлеките URL из ресурса DataExport и проверьте экспорт:

```bash
export POD_URL=$(d8 k -n $NAMESPACE get dataexport $DATA_EXPORT_RESOURCE_NAME  -o jsonpath='{.status.url}')
echo "POD_URL: $POD_URL"
```

После создания ресурса DataExport и извлечения необходимых данных подключиться к экспортеру можно одним из следующих способов.

#### 1. Аутентификация с помощью сертификата и ключа из локального kubeconfig

Этот способ использует существующие учётные данные из локального файла `kubeconfig`:

```bash
cat ~/.kube/config | grep "client-certificate-data" | awk '{print $2}' | base64 -d > client.crt
cat ~/.kube/config | grep "client-key-data" | awk '{print $2}' | base64 -d > client.key
```

Проверьте содержимое целевого PVC:

```bash
curl -v --cacert ca.pem ${POD_URL}api/v1/files/ --key client.key --cert client.crt
```

#### 2. Аутентификация с помощью токена и ролей

Этот способ предполагает создание отдельного ServiceAccount с нужными правами доступа:

```bash
d8 k -n $NAMESPACE create serviceaccount data-exporter-test
```

Создайте ClusterRole:

```bash
d8 k create -f - <<EOF
kind: ClusterRole
apiVersion: rbac.authorization.k8s.io/v1
metadata:
  name: data-exporter-test-role
rules:
- apiGroups: ["storage-foundation.deckhouse.io"]
  resources: ["dataexports/download"]
  verbs: ["create"]
EOF
```

Создайте токен:

```bash
export TOKEN=$(d8 k -n $NAMESPACE create token data-exporter-test --duration=24h)
echo $TOKEN
```

Создайте RoleBinding:

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

Проверьте содержимое целевого PVC:

```bash
curl -H "Authorization: Bearer $TOKEN" \
-v --cacert ca.pem ${POD_URL}api/v1/files/
```

### HTTP API: экспорт данных

Все эндпоинты доступны по короткому пути `/api/v1/files|block` и по полному пути `/{namespace}/{kindShort}/{name}/api/v1/files|block`.

Обозначения:

- kindShort — короткое имя экспортируемого ресурса (persistentVolumeClaim => pvc);
- name — имя экспортируемого ресурса.

#### Файловый режим (экспорт)

- Получить файл

```http
    GET /api/v1/files/{path} HTTP/1.1

    Query (опционально, для дополнительных атрибутов):
      attribute=stat      — вернуть статические атрибуты (uid, gid, права, modtime)
      attribute=hash.md5  — вернуть MD5 для обычных файлов
```

- Просмотр содержимого директории

```http
    GET /api/v1/files/{path/} HTTP/1.1  # путь к директории должен заканчиваться на '/'
```

- Метаданные без тела

```http
    HEAD /api/v1/files/{path|path/} HTTP/1.1
```

Основные ошибки файлового режима:

```http
    400 Bad Request  — несоответствие типа (файл/директория), запрещённые ссылки и т. п.
    404 Not Found    — путь не найден
    405 Method Not Allowed — методы, отличные от GET/HEAD
    500 Internal Server Error — внутренняя ошибка
```

#### Блочный режим (экспорт)

- Скачать блочное устройство (raw-образ)

```http
    GET /api/v1/block HTTP/1.1
```

- Метаданные без тела

```http
    HEAD /api/v1/block HTTP/1.1
```

### HTTP API: импорт данных

Все эндпоинты доступны по короткому пути `/api/v1/files|block` и по полному пути `/{namespace}/{kindShort}/{name}/api/v1/files|block`.

#### Файловый режим (импорт)

- Загрузить файл

```http
    PUT /api/v1/files/{path} HTTP/1.1
    Content-Length: <bytes> — обязательный заголовок
    X-Attribute-Permissions: 0775 — обязательный заголовок (восьмеричное 0000–0777)
    X-Attribute-Uid: <uid> — обязательный заголовок
    X-Attribute-Gid: <gid> — обязательный заголовок
    X-Offset: 0 — смещение от начала файла (неотрицательное целое)
    X-Content-Length: 10 — обязательный заголовок, ожидаемый полный размер файла
    X-Attribute-ModTime: метка времени RFC3339
```

- Запрос текущего прогресса загрузки

```http
    HEAD /api/v1/files/{path} HTTP/1.1
    Content-Length: 0
```

- Завершение импорта (сигнал на остановку файлового сервера)

```http
    POST /api/v1/finished HTTP/1.1
    Content-Length: 0
```

#### Блочный режим (импорт)

- Загрузить raw-образ блочного устройства

```http
    PUT /api/v1/block HTTP/1.1
    Content-Length: <bytes> — обязательный заголовок
    X-Attribute-Permissions: 0775 — обязательный заголовок (восьмеричное 0000–0777)
    X-Attribute-Uid: <uid> — обязательный заголовок
    X-Attribute-Gid: <gid> — обязательный заголовок
    X-Offset: 0 — опционально; смещение от начала устройства
    X-Content-Length: 10 — опционально; ожидаемый полный размер
```

- Запрос текущего прогресса загрузки

```http
    HEAD /api/v1/block HTTP/1.1
    Content-Length: 0
```

- Завершение импорта (сигнал на остановку файлового сервера)

```http
    POST /api/v1/finished HTTP/1.1
    Content-Length: 0
```

### Важные замечания по работе с API

При работе с экспортированными данными через HTTP API учитывайте следующее:

- Скачивание файлов: файлы скачиваются обычными GET-запросами с указанием пути к файлу в URL, например `GET /api/v1/files/largeimage.iso`. Путь к файлу не должен заканчиваться на `/`. Этот способ поддерживается стандартными инструментами (браузеры, curl и т. д.). Возобновление скачивания поддерживается, сжатие — нет.
- Просмотр директорий: доступ к директории выполняется аналогичным GET-запросом, где путь к директории должен заканчиваться на `/`, например `GET /api/v1/files/` для корня или `GET /api/v1/files/directory/` для поддиректории.
- Список файлов: при обращении к директории в теле ответа возвращается JSON со списком файлов (имя, тип и размер). Размеры файлов не кэшируются и пересчитываются при каждом запросе.
