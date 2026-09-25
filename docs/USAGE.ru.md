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

Создайте токен:

```bash
export TOKEN=$(d8 k -n $NAMESPACE create token data-exporter-test --duration=24h)
echo $TOKEN
```

Запросы к эндпоинтам данных запущенного экспорта или импорта — `/api/v1/files…` и `/api/v1/block…`, как по короткому пути, так и по полному — авторизуются через `SubjectAccessReview` на глагол `create` для **субресурса `download`** в API-группе `storage-foundation.deckhouse.io`: `dataexports/download` — при чтении данных из тома, `dataimports/download` — при записи данных в том. Обратите внимание: импорт авторизуется тем же субресурсом `download`, а не `upload` — субресурса `upload` не существует, поэтому роль, выдающая права на него, не даёт доступа вовсе.

Выдайте права сервис-аккаунту одним из двух способов ниже.

##### Привязка встроенной роли (предпочтительный способ)

В составе модуля поставляется ClusterRole `d8:use:capability:module:storage-foundation:transfer_volume_data`. Она выдаёт `create` на оба субресурса `download`, а также позволяет создавать, читать и удалять сами ресурсы `DataExport` и `DataImport` — это то, что нужно клиенту, чтобы выполнить перенос самостоятельно. Роль не сложена ни в один из общекластерных уровней доступа, поэтому вместе с таким уровнем права переноса не приходят.

При этом роль входит в набор, который собирает ClusterRole `d8:use:capability:module:state-snapshotter:backup_agent` — одна роль, выдаваемая внешней системе резервного копирования вместо нескольких отдельных. Этот агрегат тоже выдаётся только явной привязкой, поэтому сами по себе права никому не достаются; но действуют они в каждом namespace, где привязана либо эта роль, либо агрегат. Учитывайте это при аудите того, кто может вычитать том из кластера или записать в него данные: привязки, у которых в `roleRef` стоит эта роль, — ещё не полный ответ, привязки агрегата дают то же самое. В кластере без модуля, в котором определён агрегат, агрегата не существует, и значимы только привязки этой роли.

Привяжите роль к сервис-аккаунту:

```bash
d8 k create -f - <<EOF
kind: RoleBinding
apiVersion: rbac.authorization.k8s.io/v1
metadata:
  name: data-exporter-test-transfer-binding
  namespace: ${NAMESPACE}
subjects:
- kind: ServiceAccount
  name: data-exporter-test
  namespace: ${NAMESPACE}
roleRef:
  kind: ClusterRole
  name: d8:use:capability:module:storage-foundation:transfer_volume_data
  apiGroup: rbac.authorization.k8s.io
EOF
```

##### Создание роли вручную

Если встроенная роль разрешает больше, чем вы хотите выдать, создайте свою ClusterRole только с нужными субресурсами. Роль ниже разрешает скачивание экспортированных данных и ничего больше; добавьте `dataimports/download` в `resources`, чтобы разрешить ещё и загрузку данных в том:

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

Такая роль покрывает только HTTP-запросы: сами ресурсы `DataExport` и `DataImport` должен создать тот, у кого есть отдельные права на них.

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

##### Проверка доступа

Какую бы из двух ролей вы ни привязали, проверьте содержимое целевого PVC:

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

Ответ (обычный файл):

```http
    HTTP/1.1 200 OK
    Content-Type: application/octet-stream
    Content-Length: <размер>
    Content-Disposition: attachment; filename=<basename>
    X-Attribute-Uid: <uid>
    X-Attribute-Gid: <gid>
    X-Attribute-Permissions: <права>
    X-Attribute-Modtime: <RFC3339>
    X-Attribute-Hash-Md5: <md5>  # только если запрошен attribute=hash.md5
    X-Type: file

    <содержимое файла>
```

Ответ (символическая ссылка) — ссылка сообщается, но никогда не разыменовывается, поэтому клиент, копирующий дерево, воссоздаёт ссылку, а не дублирует её цель:

```http
    HTTP/1.1 200 OK
    Content-Type: application/x-symlink
    Content-Length: 0        # константа для символической ссылки
    X-Attribute-Uid: <uid>
    X-Attribute-Gid: <gid>
    X-Attribute-Permissions: <права>
    X-Attribute-Modtime: <RFC3339>
    X-Link-Target: /dir/file
    X-Type: link
```

- Просмотр содержимого директории

```http
    GET /api/v1/files/{path/} HTTP/1.1  # путь к директории должен заканчиваться на '/'

    Query (опционально): attribute=stat | attribute=hash.md5
```

Ответ:

```http
    HTTP/1.1 200 OK
    Content-Type: application/json
    Transfer-Encoding: chunked
    X-Attribute-Uid: <uid>
    X-Attribute-Gid: <gid>
    X-Attribute-Permissions: <права>
    X-Attribute-Modtime: <RFC3339>
    X-Type: dir

    {"apiVersion":"v1","items":[{"name":"1.txt","size":12,"modTime":"2025-04-01 08:02:00.228156524 +0000 UTC","type":"file"}]}
```

- Метаданные без тела

```http
    HEAD /api/v1/files/{path|path/} HTTP/1.1
```

Ответ содержит те же заголовки, что и соответствующий GET (файл, директория или символическая ссылка), но без тела. Различает три случая именно `X-Type` — по нему клиент классифицирует запись, не скачивая её.

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

Ответ:

```http
    HTTP/1.1 200 OK
    Content-Type: application/octet-stream
    Content-Length: <размер устройства>
    Content-Disposition: attachment; filename=data.img

    <сырое содержимое устройства>
```

- Метаданные без тела

```http
    HEAD /api/v1/block HTTP/1.1
```

Ответ повторяет заголовки GET выше, но без тела — так клиент узнаёт размер устройства до начала скачивания.

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

Ответ на частичную загрузку — `X-Next-Offset` указывает, с какого смещения должен начаться следующий кусок:

```http
    HTTP/1.1 204 No Content
    X-Next-Offset: 10
```

Ответ на завершённую загрузку (записанный размер достиг `X-Content-Length`):

```http
    HTTP/1.1 201 Created
```

- Запрос текущего прогресса загрузки

```http
    HEAD /api/v1/files/{path} HTTP/1.1
    Content-Length: 0
```

Ответ:

```http
    HTTP/1.1 200 OK
    X-Next-Offset: 10
```

- Завершение импорта (сигнал на остановку файлового сервера)

```http
    POST /api/v1/finished HTTP/1.1
    Content-Length: 0
```

Ответ:

```http
    HTTP/1.1 204 No Content
```

`POST` на сам эндпоинт данных (`/api/v1/files/{path}` или `/api/v1/block`) тоже завершает импорт и отвечает `200 OK`. Оба маршрута делают одно и то же; предпочтителен `/api/v1/finished` — он не требует пути.

Ошибки импорта в файловом режиме:

```http
    400 Bad Request           — отсутствуют или некорректны обязательные заголовки; некорректные X-Offset / X-Content-Length / X-Attribute-ModTime
    401 Unauthorized          — креды отсутствуют, пусты или некорректны
    403 Forbidden             — нет прав на запись в файловую систему
    404 Not Found             — HEAD для несуществующего файла
    405 Method Not Allowed    — методы, отличные от PUT/HEAD/POST
    409 Conflict              — расхождение смещения (ожидаемое возвращается в X-Expected-Offset);
                                конкурирующая запись (без заголовков); файл уже загружен полностью
    500 Internal Server Error — внутренние ошибки файловой системы (mkdir, open/seek/copy/close, установка атрибутов)
```

Клиент обязан различать два случая 409 по наличию `X-Expected-Offset`: с этим заголовком загрузка восстанавливается перепозиционированием, без него файл держит другой писатель и повтор с любым смещением не поможет.

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

Ответ на частичную загрузку:

```http
    HTTP/1.1 204 No Content
    X-Next-Offset: 10
```

Ответ на завершённую загрузку:

```http
    HTTP/1.1 201 Created
    X-Next-Offset: 10        # равен X-Content-Length
```

- Запрос текущего прогресса загрузки

```http
    HEAD /api/v1/block HTTP/1.1
    Content-Length: 0
```

Ответ — `X-Device-Size` не сообщается больше нигде в этом API, поэтому только так клиент узнаёт, сколько места есть на целевом устройстве:

```http
    HTTP/1.1 200 OK
    X-Next-Offset: 10
    X-Device-Size: 1024      # размер устройства в байтах
```

- Завершение импорта (сигнал на остановку файлового сервера)

```http
    POST /api/v1/finished HTTP/1.1
    Content-Length: 0
```

Ответ:

```http
    HTTP/1.1 204 No Content
```

`POST` на сам эндпоинт данных (`/api/v1/files/{path}` или `/api/v1/block`) тоже завершает импорт и отвечает `200 OK`. Оба маршрута делают одно и то же; предпочтителен `/api/v1/finished` — он не требует пути.

Ошибки импорта в блочном режиме:

```http
    400 Bad Request           — некорректный путь (внутри блочного префикса допустим только "/"); некорректные X-Offset / X-Content-Length
    401 Unauthorized          — креды отсутствуют, пусты или некорректны
    403 Forbidden             — нет прав на запись в блочное устройство
    405 Method Not Allowed    — методы, отличные от PUT/HEAD/POST
    409 Conflict              — расхождение смещения (возвращается X-Expected-Offset); конкурирующая запись
    416 Range Not Satisfiable — смещение меньше нуля или за концом устройства; Content-Length превышает остаток
    422 Unprocessable Entity  — записано больше байт, чем объявлено в X-Content-Length
    500 Internal Server Error — внутренние ошибки (открытие устройства, чтение размера, seek/copy)
```

Обе ветки 416 отвечают вообще без заголовков — поэтому клиенту следует прочитать `X-Device-Size` из HEAD выше до начала записи: иначе образ, больший целевого устройства, обнаружится лишь в конце передачи, которая может идти часами.

### Важные замечания по работе с API

При работе с экспортированными данными через HTTP API учитывайте следующее:

- Скачивание файлов: файлы скачиваются обычными GET-запросами с указанием пути к файлу в URL, например `GET /api/v1/files/largeimage.iso`. Путь к файлу не должен заканчиваться на `/`. Этот способ поддерживается стандартными инструментами (браузеры, curl и т. д.). Возобновление скачивания поддерживается, сжатие — нет.
- Просмотр директорий: доступ к директории выполняется аналогичным GET-запросом, где путь к директории должен заканчиваться на `/`, например `GET /api/v1/files/` для корня или `GET /api/v1/files/directory/` для поддиректории.
- Список файлов: при обращении к директории в теле ответа возвращается JSON со списком файлов (имя, тип и размер). Размеры файлов не кэшируются и пересчитываются при каждом запросе.
- Квота на хранилище для `mode: PopulateData`: временный (staging) том теперь выделяется в неймспейсе модуля `d8-storage-foundation`, а не в неймспейсе пользователя — потребление хранилища учитывается в `ResourceQuota` неймспейса `d8-storage-foundation` (если она задана), а не пользовательского. Рассчитывайте квоту как минимум на наибольший ожидаемый размер `spec.storageParams.size` — заниженная квота оставит внутренний PVC в статусе `Pending` навсегда, и импорт никогда не станет Ready. Режим `mode: CreatePVC` не изменился (PVC остаётся в неймспейсе пользователя).
- Замечание об обновлении: импорт в режиме `PopulateData`, находящийся в процессе загрузки во время обновления модуля до этой версии, необходимо перезапустить (повторно выполнить `d8 snapshot upload`); частично загруженные данные будут отброшены. Завершённые импорты и их артефакты не затрагиваются. Временный (staging) PVC, оставшийся от такого прерванного импорта в вашем неймспейсе, автоматически не удаляется — удалите его вручную; если он остаётся в статусе `Terminating`, снимите с него finalizer `storage-foundation.deckhouse.io/storage-manager-controller`.

Модуль `storage-volume-data-manager` можно держать включённым одновременно с `storage-foundation`. Оба модуля продолжают работать: каждый обслуживает свои собственные ресурсы `DataExport` и `DataImport` независимо — своя API-группа, своё пространство имён и свои финализаторы на задействованных томах. При этом один и тот же живой том (`PersistentVolumeClaim` или `VirtualDisk`) следует экспортировать только через один из двух модулей. Причин тому две, и они независимы:

- Публичные адреса совпадают. Оба модуля собирают `status.publicURL` на одном и том же хосте и из одного и того же пути `/<namespace>/<вид-таргета>/<имя-таргета>`, поэтому экспорт одного и того же тома получает один и тот же публичный адрес независимо от того, какой из двух модулей его создал. Если экспорт удалить в одном модуле и затем создать для того же тома в другом, адрес переиспользуется, и клиент, запомнивший адрес, не сможет определить, чей экспорт ему отвечает. Всегда берите адрес из поля `status.publicURL` того ресурса, который создали сами.
- Экспорт живого тома забирает его `PersistentVolume`. Том на время экспорта перепривязывается к экспортеру, поэтому один и тот же том нельзя экспортировать через оба модуля одновременно: экспорт, начатый вторым, до публикации не доходит вовсе и завершается конфликтом за `PersistentVolume`. Прежде чем экспортировать тот же том через другой модуль, удалите первый экспорт или дождитесь истечения его срока.
