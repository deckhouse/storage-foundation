---
title: "Модуль storage-foundation: расширенное использование"
weight: 50
description: "Аутентификация HTTP-клиента, ручная настройка прав и протоколы экспорта и импорта данных томов."
---

## Аутентификация HTTP-клиента

Для передачи данных по HTTP сначала создайте DataExport или DataImport в пользовательском неймспейсе
[по инструкциям из раздела «Экспорт и импорт данных томов»](usage.html#экспорт-и-импорт-данных-томов). Используйте
`status.url` при доступе из сети кластера и `status.publicURL` для подключения через опубликованный Ingress. Для этих
адресов используются разные цепочки доверия TLS.

Для **внутреннего** `status.url` сертификат CA хранится в `status.ca` в формате Base64. Пример для DataExport
`my-export` в неймспейсе `project`:

```bash
d8 k -n project get dataexports.storage-foundation.deckhouse.io my-export -o jsonpath='{.status.ca}' | base64 -d > internal-ca.pem
INTERNAL_URL=$(d8 k -n project get dataexports.storage-foundation.deckhouse.io my-export -o jsonpath='{.status.url}')
```

Для подключения по **опубликованному** `status.publicURL` используется тот же TLS-сертификат, что и для опубликованного
Kubernetes API (`api.<publicDomain>`), а не CA из `status.ca`. Если сертификат доверенный для системы, `--cacert` не
требуется. Иначе используйте CA опубликованного API, например `certificate-authority-data` из kubeconfig для
`api.<publicDomain>`.

```bash
# Выполните команду, только если CA опубликованного API отсутствует в системном хранилище доверенных CA.
d8 k config view --raw --minify --flatten -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d > public-api-ca.pem
```

Клиент `d8` объединяет системные CA, CA из kubeconfig и внутренний CA из `status.ca`. Для `curl` используйте CA,
соответствующий выбранному адресу. Не передавайте `status.ca` как CA для `status.publicURL`.

### Сертификат клиента из kubeconfig (только для внутреннего адреса)

Если kubeconfig содержит клиентский сертификат и ключ, используйте их **только с `status.url`**. Опубликованный
Ingress завершает TLS и не передаёт сертификат клиента серверу передачи. Защищайте приватный ключ и удаляйте
временные файлы после работы.

```bash
umask 077
d8 k config view --raw --minify --flatten -o jsonpath='{.users[0].user.client-certificate-data}' | base64 -d > client.crt
d8 k config view --raw --minify --flatten -o jsonpath='{.users[0].user.client-key-data}' | base64 -d > client.key
curl --cacert internal-ca.pem --cert client.crt --key client.key "${INTERNAL_URL%/}/api/v1/files/"
```

### Аутентификация токеном и права

Для опубликованного адреса используйте токен Kubernetes. Создайте ServiceAccount, привяжите встроенную роль переноса
данных и получите токен с ограниченным сроком действия:

```bash
d8 k -n project create serviceaccount data-transfer
d8 k -n project create rolebinding data-transfer --clusterrole=d8:namespace-capability:storage-foundation:transfer_volume_data --serviceaccount=project:data-transfer
TOKEN=$(d8 k -n project create token data-transfer --duration=24h)
```

Вместо [встроенной роли переноса данных](usage.html#права-доступа) можно
[настроить более узкие права вручную](#ручная-настройка-rbac). В Deckhouse Platform ниже 1.78 используйте прежнее имя
роли из таблицы раздела «Права доступа».

Создайте DataExport с опубликованным доступом и дождитесь заполнения `status.publicURL`:

```bash
d8 data export create -n project my-export pvc/data --ttl 10m --publish=true

d8 k -n project wait dataexports.storage-foundation.deckhouse.io/my-export \
  --for=jsonpath='{.status.publicURL}' --timeout=5m
```

Для изменения `spec.publish` у существующего DataExport требуется право `patch` на объекты DataExport. Встроенная
роль для переноса данных не предоставляет это право.

Затем выполните запрос с токеном:

```bash
PUBLIC_URL=$(d8 k -n project get dataexports.storage-foundation.deckhouse.io my-export -o jsonpath='{.status.publicURL}')
curl -H "Authorization: Bearer ${TOKEN}" "${PUBLIC_URL%/}/api/v1/files/"
```

Если сертификат опубликованного API подписан CA, которому система не доверяет, добавьте `--cacert public-api-ca.pem`.
При доступе из сети кластера используйте внутренний адрес и `--cacert internal-ca.pem`.

## Ручная настройка RBAC

HTTP-запросы к данным авторизуются через `SubjectAccessReview` с глаголом `create` для субресурсов `dataexports/download`
при экспорте и `dataimports/download` при импорте в API-группе `storage-foundation.deckhouse.io`. Несмотря на
направление передачи, импорт также использует субресурс `download`; субресурса `upload` нет.

Пример минимальной ClusterRole для чтения экспортируемых данных:

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

Чтобы также разрешить импорт, добавьте `dataimports/download` в `resources`. Такая роль разрешает доступ только к
HTTP-передаче; для создания DataExport или DataImport нужны отдельные права на соответствующие объекты. Привяжите
роль к ServiceAccount в нужном неймспейсе:

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

Проверка не указывает имя объекта, поэтому такая привязка разрешает переносить данные любого DataExport или DataImport
в неймспейсе, а не только созданных этим субъектом.

При аудите прав учитывайте также доступ уровней `Editor` и выше текущей ролевой модели, уровней `manager` и `admin`
неймспейса в RBACv2 и привязки роли `d8:state-snapshotter:backup-agent`. Подробности агрегирования приведены
[на странице «Интеграция с системами резервного копирования»](/modules/state-snapshotter/backup_integration.html).

## HTTP API экспорта и импорта

Эндпоинты данных доступны по короткому пути `/api/v1/files` или `/api/v1/block`, а также по полному пути
`/<NAMESPACE>/<KIND_SHORT>/<NAME>/api/v1/files` или `/<NAMESPACE>/<KIND_SHORT>/<NAME>/api/v1/block`. При экспорте
`<KIND_SHORT>` обозначает тип исходного ресурса, например `pvc`. При импорте используйте адрес своего DataImport. Не
смешивайте адреса, предоставленные разными модулями.

### HTTP API: экспорт данных

При экспорте `GET` возвращает данные, а `HEAD` — метаданные. API также поддерживает просмотр содержимого директорий и
возобновление скачивания с помощью заголовка `Range`. Сжатие на уровне этого API не поддерживается. Размеры файлов в
списке содержимого директории вычисляются заново при каждом запросе.

{{< tabs name="export_volume_mode" >}}
{{< tab name="Файловый режим" >}}

Следующие запросы позволяют читать файлы, директории, символические ссылки и их метаданные.

#### Получение файла

Параметр `attribute=stat` запрашивает UID, GID, права и время изменения. Заголовки `stat` возвращаются и без
этого параметра. Для обычного файла параметр `attribute=hash.md5` запрашивает MD5. Вычисление хеша требует полного
чтения файла и может увеличить время ответа для больших файлов.

Пример запроса содержимого файла и MD5:

```http
GET /api/v1/files/<FILE_PATH>?attribute=hash.md5 HTTP/1.1
```

Замените `<FILE_PATH>` путём к файлу относительно корня тома.

Ответ (обычный файл с запрошенным MD5):

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

`<BASE_NAME>` — имя файла без пути, записанное как параметр заголовка по RFC 6266: имя, не являющееся простым токеном,
заключается в кавычки (`filename="a b.txt"`), а имя с управляющими символами или символами вне ASCII кодируется
процентами как `filename*=utf-8''<ENCODED_NAME>`.

Для символической ссылки сервер возвращает сведения о ней, не переходя к её цели. Поэтому при копировании дерева клиент
воссоздаёт символическую ссылку, а не копирует содержимое её цели. `Content-Length` всегда равен `0`:

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

#### Просмотр содержимого директории

Путь к директории должен заканчиваться на `/`. Необязательные параметры запроса те же, что и для файла.
Вычисление MD5 для содержимого директории может быть длительным, поскольку требуется чтение каждого обычного файла.

```http
GET /api/v1/files/<DIR_PATH>/ HTTP/1.1
```

Замените `<DIR_PATH>` путём к директории относительно корня тома. Для корня тома используйте `/api/v1/files/`.

Ответ (JSON отформатирован для удобства чтения):

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

#### Получение метаданных файла или директории без тела ответа

Запросите метаданные файла или директории:

```http
HEAD /api/v1/files/<FILE_PATH> HTTP/1.1
```

Для получения метаданных директории используйте путь `/api/v1/files/<DIR_PATH>/`.

Ответ содержит те же заголовки, что и соответствующий GET (файл, директория или символическая ссылка), но без тела. По
заголовку `X-Type` клиент определяет тип элемента без скачивания его содержимого.

{{< /tab >}}
{{< tab name="Блочный режим" >}}

Блочный эндпоинт позволяет скачать содержимое экспортируемого блочного устройства или получить его метаданные.

#### Скачивание блочного устройства (RAW-образ)

Запросите содержимое блочного устройства:

```http
GET /api/v1/block HTTP/1.1
```

Ответ:

```http
HTTP/1.1 200 OK
Content-Type: application/octet-stream
Content-Length: <DEVICE_SIZE>
Content-Disposition: attachment; filename=data.img

<RAW_BLOCK_CONTENT>
```

#### Получение метаданных блочного устройства без тела ответа

Запросите метаданные блочного устройства:

```http
HEAD /api/v1/block HTTP/1.1
```

Ответ повторяет заголовки GET выше, но без тела — так клиент узнаёт размер устройства до начала скачивания.

{{< /tab >}}
{{< /tabs >}}

### HTTP API: импорт данных

Для передачи данных используйте `PUT` и заголовки размера и смещения, `HEAD` для получения текущего смещения и
`POST /api/v1/finished` для завершения импорта. В отличие от команды `d8 data import upload`, HTTP-клиент должен явно
завершить загрузку.

`POST` на сам эндпоинт данных (`/api/v1/files/<FILE_PATH>` или `/api/v1/block`) тоже завершает импорт и отвечает
`200 OK`. Оба маршрута делают одно и то же; предпочтителен `/api/v1/finished` — он не требует пути.

{{< tabs name="import_volume_mode" >}}
{{< tab name="Файловый режим" >}}

Следующие запросы позволяют загружать файлы и отслеживать прогресс загрузки.

#### Загрузка файла

Передайте файл двумя чанками по пять байт. `Content-Length` задаёт размер тела текущего запроса, `X-Offset` — смещение
в файле, а `X-Content-Length` — ожидаемый полный размер.

Каждый запрос `PUT` должен содержать заголовки `X-Offset`, `X-Content-Length`, `X-Attribute-Permissions`,
`X-Attribute-Uid` и `X-Attribute-Gid`. Если какого-либо из них нет, сервер возвращает `400` с JSON-телом, в котором
поле `missingHeaders` перечисляет отсутствующие заголовки. Заголовок `X-Attribute-ModTime` необязателен.

Заголовки атрибутов задают атрибуты загружаемого файла. `X-Attribute-Permissions` задаёт права восьмеричным значением
`0000`–`0777`, `X-Attribute-Uid` и `X-Attribute-Gid` — владельца и группу, `X-Attribute-ModTime` — время изменения в
формате RFC3339. Передавайте одинаковые значения атрибутов во всех чанках файла.

Первый чанк:

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

Замените `<FILE_PATH>` путём, по которому файл будет создан в томе.

Пока записанный объём меньше `X-Content-Length`, данные хранятся во временном файле `.tempupload`, а сервер отвечает
`204`. Ответ указывает, с какого смещения передавать следующий чанк, в этом примере — с `5`:

```http
HTTP/1.1 204 No Content
X-Next-Offset: 5
```

Второй чанк:

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

Когда объём записанных данных достигает `X-Content-Length`, временный файл `.tempupload` переименовывается в целевой
файл. В ответе на завершение файла нет `X-Next-Offset`:

```http
HTTP/1.1 201 Created
```

После загрузки всех файлов отправьте `POST /api/v1/finished`, даже если последний чанк вернул `201`.

#### Запрос прогресса загрузки файла

Запросите текущее смещение загрузки файла:

```http
HEAD /api/v1/files/<FILE_PATH> HTTP/1.1
Content-Length: 0
```

Ответ во время незавершённой загрузки:

```http
HTTP/1.1 200 OK
X-Next-Offset: 5
```

#### Завершение импорта файлов

Завершите импорт:

```http
POST /api/v1/finished HTTP/1.1
Content-Length: 0
```

Ответ:

```http
HTTP/1.1 204 No Content
```

Если ответ `409` содержит `X-Expected-Offset`, продолжите загрузку со смещения из этого заголовка. Без `X-Expected-Offset`
ответ `409` означает, что запись уже выполняется или файл уже полностью загружен. Изменение смещения не поможет.
Проверьте состояние файла запросом `HEAD`.

{{< /tab >}}
{{< tab name="Блочный режим" >}}

Блочный эндпоинт позволяет загрузить RAW-образ и отслеживать текущее смещение и размер целевого устройства.

#### Загрузка RAW-образа блочного устройства

`Content-Length` — размер тела текущего запроса, `X-Offset` — смещение в устройстве, `X-Content-Length` — ожидаемый
полный размер передаваемых данных.

Каждый запрос `PUT` должен содержать заголовки `X-Offset`, `X-Content-Length`, `X-Attribute-Permissions`,
`X-Attribute-Uid` и `X-Attribute-Gid`, как и в файловом режиме. Для блочного устройства сервер тоже требует заголовки
атрибутов, но не применяет их значения.

Первый чанк:

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

Ответ на частичную загрузку:

```http
HTTP/1.1 204 No Content
X-Next-Offset: 5
```

Второй чанк:

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

В отличие от файлового режима, завершённая передача также возвращает `X-Next-Offset`:

```http
HTTP/1.1 201 Created
X-Next-Offset: 10
```

После передачи данных всегда отправляйте `POST /api/v1/finished`, в том числе после ответа `201`.

#### Запрос прогресса загрузки образа

Запросите текущее смещение и размер целевого устройства:

```http
HEAD /api/v1/block HTTP/1.1
Content-Length: 0
```

Ответ содержит размер целевого устройства в байтах в заголовке `X-Device-Size`. Другие ответы API этот размер не
сообщают:

```http
HTTP/1.1 200 OK
X-Next-Offset: 10
X-Device-Size: 1024
```

#### Завершение импорта образа

Завершите импорт:

```http
POST /api/v1/finished HTTP/1.1
Content-Length: 0
```

Ответ:

```http
HTTP/1.1 204 No Content
```

Ответ `416` не содержит протокольных заголовков с оставшимся или требуемым размером. Перед загрузкой получите
`X-Device-Size` с помощью запроса HEAD и проверьте, не превышает ли размер образа размер целевого устройства.

{{< /tab >}}
{{< /tabs >}}

### Коды ответов

Таблица показывает коды ошибок для каждой операции. Успешные ответы (`200`, `201`, `204`) приведены в примерах выше.
Ошибки аутентификации и авторизации также могут возвращаться до обработки запроса сервером данных.

| Операция | Код | Когда возвращается |
|---|---|---|
| Файловый экспорт (GET, HEAD) | `400` | Неверный путь или тип файла/директории, запрещённый переход по ссылке, некорректный запрос |
| Файловый экспорт (GET, HEAD) | `404` | Путь не найден |
| Файловый экспорт (GET, HEAD) | `405` | Метод отличается от GET и HEAD |
| Файловый экспорт (GET, HEAD) | `500` | Внутренняя ошибка |
| Блочный экспорт (GET, HEAD) | `400` | Неверный путь для блочного устройства |
| Блочный экспорт (GET, HEAD) | `405` | Метод отличается от GET и HEAD |
| Блочный экспорт (GET, HEAD) | `500` | Внутренняя ошибка |
| Файловый импорт (PUT, HEAD, POST) | `400` | Не передан обязательный заголовок PUT (список отсутствующих — в поле `missingHeaders` JSON-тела), некорректное значение заголовка или запрос HEAD к директории |
| Файловый импорт (PUT, HEAD, POST на эндпоинт данных) | `403` | Импорт уже завершён |
| Файловый импорт (HEAD) | `404` | Файл не найден |
| Файловый импорт (PUT, HEAD, POST) | `405` | Метод отличается от PUT, HEAD и POST |
| Файловый импорт (PUT) | `409` | Расхождение смещения (`X-Expected-Offset`), конкурирующая запись или завершённый файл |
| Файловый импорт (PUT, HEAD, POST) | `500` | Внутренняя ошибка файловой системы |
| Блочный импорт (PUT, HEAD, POST) | `400` | Лишние компоненты пути или завершающий `/` после `/api/v1/block`, не передан обязательный заголовок PUT (список отсутствующих — в поле `missingHeaders` JSON-тела) или некорректное значение заголовка |
| Блочный импорт (PUT, HEAD, POST на эндпоинт данных) | `403` | Импорт уже завершён. Для PUT ошибка также может означать отсутствие прав на запись в блочное устройство |
| Блочный импорт (PUT, HEAD, POST) | `405` | Метод отличается от PUT, HEAD и POST |
| Блочный импорт (PUT) | `409` | Расхождение смещения или конкурирующая запись; при расхождении возвращается `X-Expected-Offset` |
| Блочный импорт (PUT) | `416` | Запрос превышает оставшийся размер устройства |
| Блочный импорт (PUT) | `422` | Тело запроса длиннее, чем допускает `X-Content-Length` |
| Блочный импорт (PUT, HEAD, POST) | `500` | Внутренняя ошибка |
