---
title: "Модуль storage-foundation: использование"
weight: 40
description: "Использование снимков, клонирование томов и передача данных томов по HTTP с помощью модуля storage-foundation."
---

## Использование снимков

Объекты VolumeSnapshotClass создают CSI-модули или администратор кластера. Выберите класс, соответствующий
CSI-драйверу исходного PVC. Посмотреть доступные классы можно командой:

```shell
d8 k get volumesnapshotclasses.snapshot.storage.k8s.io
```

При создании VolumeSnapshot из PVC модуль выбирает VolumeSnapshotClass следующим образом:

- Если StorageClass этого PVC имеет лейбл `storage.deckhouse.io/managed-by`, модуль берёт класс из аннотации
  `storage.deckhouse.io/volumesnapshotclass` этого StorageClass. Поле `volumeSnapshotClassName` можно не указывать.
  Если оно указано, значение должно совпадать с аннотацией, иначе создание VolumeSnapshot отклоняется.
- Для остальных StorageClass модуль заполняет `volumeSnapshotClassName` из той же аннотации, только если поле не указано
  и аннотация есть. В остальных случаях укажите класс, соответствующий CSI-драйверу PVC.

У PVC должен быть задан `storageClassName`.

Создайте снимок существующего PVC:

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

После создания снимка проверьте его состояние:

```shell
d8 k describe volumesnapshots.snapshot.storage.k8s.io my-first-snapshot
```

Пример вывода:

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

Чтобы восстановить содержимое снимка, создайте новый PersistentVolumeClaim (PVC) и укажите снимок в качестве источника:

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

### Поля VolumeSnapshot, добавленные модулем

Модуль обслуживает CRD VolumeSnapshot с полями, которых нет в исходном CRD. Модуль `state-snapshotter` использует
их, чтобы включить снимок тома в снимок неймспейса. Пользователь задаёт только `spec.mode`, поля статуса заполняют
контроллеры:

- `spec.mode` определяет, как снимок получает содержимое. `Capture` (модуль устанавливает это значение, если поле не
  указано) создаёт CSI-снимок тома из `spec.source`. `Import` помечает снимок, содержимое которого загружается из
  архива снимка командой `d8 snapshot upload`: `spec.source` не указывается, и модуль ничего не захватывает. Изменить
  поле после создания нельзя.
- `status.data` описывает захваченный том: исходный PVC в `sourceRef`, его StorageClass, режим тома, тип файловой
  системы, выделенный размер и сохраняемый артефакт в `artifactRef` (VolumeSnapshotContent). Модуль `state-snapshotter`
  заполняет эту секцию при обработке снимка. Команды `d8 snapshot` определяют том снимка по этой секции. DataExport
  читает те же данные из объекта SnapshotContent модуля `state-snapshotter`, имя которого указано в
  `status.boundSnapshotContentName`. Поэтому экспортировать снимок можно только после того, как модуль
  `state-snapshotter` заполнил `status.boundSnapshotContentName` и данные этого SnapshotContent, а одного
  `status.readyToUse: true` для этого недостаточно. Ожидание помогает, только пока модуль ещё обрабатывает снимок.
  Модуль пропускает VolumeSnapshot, созданный из существующего VolumeSnapshotContent (pre-provisioned), VolumeSnapshot,
  созданный до включения модуля `storage-foundation`, и VolumeSnapshot, у исходного PVC которого есть метка
  `state-snapshotter.deckhouse.io/exclude`. Для такого снимка SnapshotContent с данными не появится, поэтому
  экспортировать его данные через DataExport нельзя.
- `status.conditions` содержит условие `Ready`, которое сообщает о готовности снимка для модуля `state-snapshotter`.
  Оно не совпадает с `status.readyToUse`: это поле сообщает, что CSI-снимок привязан к своему VolumeSnapshotContent.
- `status.captureState`, `status.childrenSnapshotRefs` и `status.sourceRef` — служебные поля обработки дерева снимков.
  VolumeSnapshot является листом дерева, поэтому `status.childrenSnapshotRefs` обычно пуст.

## Клонирование CSI-томов

Чтобы создать копию существующего PVC, укажите его в `dataSource` нового PVC. Клонирование — отдельная возможность
CSI-драйвера, для которой не требуется предварительно создавать VolumeSnapshot.

При клонировании действуют следующие ограничения:

- Исходный PVC должен находиться в состоянии `Bound`.
- Исходный и новый PVC должны находиться в одном неймспейсе и иметь одинаковый `volumeMode`.
- Запрашиваемый размер нового PVC должен быть не меньше размера исходного.
- Тома должны предоставляться динамически через CSI-драйвер, поддерживающий клонирование.
- Другой StorageClass допустим, если оба StorageClass обслуживаются одним CSI-драйвером.
- Если `storageClassName` не указан, используется StorageClass по умолчанию. Его должен обслуживать тот же CSI-драйвер,
  что и исходный PVC.

Чтобы клонировать `my-origin-pvc`, создайте новый PVC и укажите исходный PVC в поле `dataSource`:

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

## Экспорт и импорт данных томов

DataExport позволяет экспортировать данные PersistentVolumeClaim (PVC), VolumeSnapshot, VirtualDisk и
VirtualDiskSnapshot. VirtualDisk и VirtualDiskSnapshot всегда используют блочный режим. DataImport загружает данные в
новый PVC или в снимок, см. [раздел «Импорт данных»](#импорт-данных).

### Права доступа

В Deckhouse Platform 1.78 и новее переносить данные могут субъекты RBAC (пользователи, группы и ServiceAccount) с
уровнем доступа `admin` в соответствующем неймспейсе. Остальным субъектам необходима явная привязка встроенной
ClusterRole `d8:namespace-capability:storage-foundation:transfer_volume_data`. Она позволяет создавать, читать и удалять
DataExport и DataImport, а также передавать данные через HTTP API.

Например, привяжите роль к пользователю, работающему с данными в неймспейсе `project`:

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

Замените `<USER_NAME>` именем пользователя. Для ServiceAccount или группы настройте `subjects` в соответствии с типом
субъекта. В версиях Deckhouse Platform ниже 1.78 используйте прежние имена ролей:

| Назначение | 1.78 и новее | Ниже 1.78 |
|---|---|---|
| Перенос данных | `d8:namespace-capability:storage-foundation:transfer_volume_data` | `d8:use:capability:module:storage-foundation:transfer_volume_data` |
| Агент резервного копирования | `d8:state-snapshotter:backup-agent` | `d8:use:capability:module:state-snapshotter:backup_agent` |

При аудите учитывайте, что доступ к переносу данных также предоставляют уровень `admin` неймспейса и агрегированная роль
`backup-agent` при наличии соответствующих привязок. Подробности приведены
[на странице «Интеграция с системами резервного копирования»](/modules/state-snapshotter/backup_integration.html).
Ручная настройка прав для HTTP-клиента описана [на странице «Расширенное использование»](advanced_usage.html).

Объекты DataExport и DataImport могут обслуживать два модуля: `storage-foundation` в API-группе
`storage-foundation.deckhouse.io` и `storage-volume-data-manager` в API-группе `storage.deckhouse.io`. Команды
`d8 data` (версии 0.33.14 и новее) при каждом запуске определяют, с каким из этих модулей работать. Пользователь, у
которого есть права только в группе `storage.deckhouse.io`, работает с модулем `storage-volume-data-manager` без
предупреждения. Проверить права в группе `storage-foundation.deckhouse.io` можно следующей командой:

```bash
d8 k auth can-i get dataexports.storage-foundation.deckhouse.io -n project
```

### Основные параметры

Следующие параметры DataExport и DataImport управляют временем жизни, размещением и внешней доступностью ресурсов
передачи данных. Каждый пункт заканчивается описанием того, как параметр задают команды `d8 data`.

- `ttl` задаёт время простоя после последней передачи данных. Отсчёт `ttl` обновляется при передаче данных в теле
  HTTP-запроса или ответа. Если ни запрос, ни ответ не содержат тела, как при `HEAD`, срок не продлевается. После
  истечения срока DataImport или DataExport получает `Ready=False` с причиной `Expired`, и сервер передачи данных
  останавливается: удаляются его под и сервис, а также Ingress (при `publish: true`). Сам объект DataImport или
  DataExport удаляется автоматически позже. При экспорте исходный PVC снова привязывается к своему PV, данные в нём не
  изменяются. При импорте также удаляется внутренний временный том, поэтому данные незавершённой загрузки не
  сохраняются и передачу нужно начать заново с новым DataImport. В режиме `CreatePVC` целевой PVC после истечения срока
  не удаляется. В `d8 data`: команды `create` и `download` без флага `--ttl` задают `2m`.
- `waitForFirstConsumer` применяется только к импорту в новый PVC (`CreatePVC`). При `true` импорт в StorageClass с отложенной привязкой
  ожидает появления пода, который использует PVC. Том создаётся на узле, выбранном для этого пода. Под остаётся в
  состоянии `Pending` до завершения импорта. При `false` том создаётся без ожидания такого пода. Выбирайте значение с
  учётом StorageClass и способа использования PVC. В YAML-манифесте значение по умолчанию равно `true`. В `d8 data`:
  `d8 data import create` без флага `--wffc` передаёт `false`.
- При `publish: true` сервер передачи доступен извне кластера по адресу из `status.publicURL`. Без этого параметра
  сервер доступен только из сети кластера по адресу из `status.url`. В `d8 data`: без флага `--publish` утилита `d8`
  проверяет, доступен ли сервис `kubernetes` неймспейса `default` по его ClusterIP, и при доступности выбирает прямое
  подключение, иначе опубликованный адрес. Явный `--publish=true` или `--publish=false` переопределяет выбор. Если
  проверка не даёт однозначного результата, команда завершается с ошибкой, см.
  [раздел «d8 не может определить режим публикации»](#d8-не-может-определить-режим-публикации).

DataExport и DataImport создаются в неймспейсе исходного PVC или PVC, который нужно создать. В `d8 data`: без `-n`
команды используют неймспейс текущего контекста kubeconfig, а если в контексте он не задан, то `default`, как kubectl;
`d8 data import create` берёт `metadata.namespace` из манифеста PVC раньше, чем неймспейс контекста. Так работают
версии `d8` начиная с v0.34.8. До v0.34.8 `d8` по умолчанию использует неймспейс `d8-storage-volume-data-manager`,
поэтому с этими версиями всегда указывайте `-n` с неймспейсом PVC.

{{< alert level="warning" >}}
Экспортировать PVC можно только тогда, когда он не смонтирован ни одним подом.
{{< /alert >}}

### Экспорт данных

Для передачи данных используйте `d8`. Справку по аргументам можно получить командой `d8 data export download --help`.
Если аргумент источника начинается с одного из следующих префиксов, `d8` автоматически создаёт DataExport:

| Ресурс-источник | Префиксы CLI | Режим тома |
|---|---|---|
| PersistentVolumeClaim | `pvc/`, `persistentvolumeclaim/` | Filesystem или Block |
| VolumeSnapshot | `vs/`, `volumesnapshot/` | Filesystem или Block, в зависимости от исходного тома |
| VirtualDisk | `vd/`, `virtualdisk/` | Только Block |
| VirtualDiskSnapshot | `vds/`, `virtualdisksnapshot/` | Только Block |

Регистр символов в префиксах не учитывается. Без префикса передаётся имя уже созданного DataExport.

{{< tabs name="export_method" >}}
{{< tab name="Через d8" >}}

Скачайте файл из PVC `data` в неймспейсе `project` одной командой. При `--cleanup=true` команда `d8` создаёт временный
DataExport, скачивает файл и удаляет DataExport после завершения передачи:

```bash
d8 data export download -n project pvc/data mydir/file.txt -o file.txt --cleanup=true
```

Для уже созданного DataExport передайте его имя:

```bash
d8 data export create -n project my-export pvc/data --ttl 5m
d8 data export download -n project my-export mydir/file.txt -o file.txt
```

Если скачивается директория, сначала создайте директорию назначения. Путь внутри тома должен заканчиваться на `/`:

```bash
mkdir -p out
d8 data export download -n project pvc/data mydir/ -o out --cleanup=true
```

В блочном режиме путь внутри тома не указывается:

```bash
d8 data export download -n project vd/my-virtualdisk -o disk.img --cleanup=true
```

Без `--cleanup` утилита запрашивает подтверждение удаления временного DataExport и ждёт до 30 секунд. В неинтерактивном
режиме без этого флага DataExport не удаляется автоматически.

{{< /tab >}}
{{< tab name="Через YAML-манифест" >}}

Создайте DataExport для PVC `data` в пользовательском неймспейсе `project`:

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

Сохраните манифест в файл `dataexport.yaml`, примените его и проверьте состояние DataExport:

```bash
d8 k apply -f dataexport.yaml
d8 k -n project get dataexports.storage-foundation.deckhouse.io my-export
```

После применения манифеста создаётся DataExport, но данные на локальный диск не сохраняются. Скачивание по HTTP описано
[в разделе «HTTP API: экспорт данных»](advanced_usage.html#http-api-экспорт-данных), сертификаты и аутентификация —
[в разделе «Аутентификация HTTP-клиента»](advanced_usage.html#аутентификация-http-клиента).

{{< /tab >}}
{{< /tabs >}}

### Импорт данных

DataImport работает в одном из двух режимов, который задаёт поле `mode`. Режим определяет, куда попадают загруженные
данные и кто обычно создаёт объект:

| Режим | Куда попадают данные | Где создаётся том | Кто создаёт DataImport |
|---|---|---|---|
| `CreatePVC` (по умолчанию) | Новый PVC, который остаётся после импорта | Целевой PVC в неймспейсе пользователя и временный PVC в неймспейсе `d8-storage-foundation` | Пользователь, командой `d8 data import` или манифестом |
| `PopulateData` | VolumeSnapshotContent, привязанный к существующему снимку | Временный PVC в неймспейсе `d8-storage-foundation` | Команда `d8 snapshot upload` |

Используйте `CreatePVC`, чтобы загрузить файлы или образ блочного устройства в собственный том. `PopulateData` — этап
передачи данных при переносе снимка между неймспейсами или кластерами.

#### Импорт в новый PVC

При ручном импорте в режиме `CreatePVC` создаётся новый PVC. Подготовьте шаблон PVC для `d8` либо манифест DataImport. В
примерах используется неймспейс `project`. Укажите StorageClass, доступный в кластере.

{{< tabs name="import_method" >}}
{{< tab name="Через d8" >}}

Сохраните шаблон PVC в файл `pvc-template.yaml`, заменив `<STORAGE_CLASS>` на имя класса хранения:

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

Создайте DataImport и загрузите файл в том. При `--wffc=false` импорт начинается без ожидания пода, который будет
использовать PVC:

```bash
d8 data import create -n project my-import -f ./pvc-template.yaml --ttl 10m --wffc=false
d8 data import upload -n project my-import -f ./file.txt -d /file.txt
```

Флаг `-d` задаёт путь **внутри тома**, а `-f` — путь к локальному файлу. `upload` самостоятельно завершает импорт, поэтому
отдельный запрос `POST /api/v1/finished` не нужен. Если предыдущий запуск команды прервался, повторите загрузку с
`--resume`, чтобы продолжить с места остановки:

```bash
d8 data import upload -n project my-import -f ./file.txt -d /file.txt --resume
```

Если связь обрывается во время выполнения команды, `d8` автоматически пытается продолжить передачу.

{{< /tab >}}
{{< tab name="Через YAML-манифест" >}}

Создайте DataImport для нового PVC `restored-data` в неймспейсе `project`. Замените `<STORAGE_CLASS>` на существующий
StorageClass:

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

Сохраните манифест в файл `dataimport.yaml`, примените его и проверьте состояние DataImport:

```bash
d8 k apply -f dataimport.yaml
d8 k -n project get dataimports.storage-foundation.deckhouse.io my-import
```

После применения манифеста создаётся DataImport. Для передачи данных и отправки завершающего запроса используйте
[раздел «HTTP API: импорт данных»](advanced_usage.html#http-api-импорт-данных). В отличие от приведённого примера, при
отсутствии `waitForFirstConsumer` API устанавливает значение `true`.

{{< /tab >}}
{{< /tabs >}}

#### Импорт в снимок

В режиме `PopulateData` DataImport записывает загруженные данные не в новый PVC, а в сохраняемый артефакт снимка.
Режим восстанавливает данные томов из архива снимка, созданного командой `d8 snapshot download`. Объекты этого режима
создаёт сама команда `d8 snapshot upload`: VolumeSnapshot с `spec.mode: Import` и DataImport для него. Порядок действий
описан [в разделе «Перенос снимка в другой неймспейс или кластер»](/modules/state-snapshotter/user_guide.html#перенос-снимка-в-другой-неймспейс-или-кластер)
документации модуля `state-snapshotter`. Ниже описано, что делают созданные `d8` объекты и что проверить, если импорт
не завершается.

DataImport в режиме `PopulateData` содержит следующие поля:

- `snapshotRef` ссылается на существующий снимок в том же неймспейсе, который получает данные: VolumeSnapshot с
  `spec.mode: Import` или VirtualDiskSnapshot.
- `storageParams` описывает временный том, в который загружаются данные: `size`, `storageClassName` и `volumeMode`
  (по умолчанию `Filesystem`). StorageClass должен поддерживать снимки: его аннотация
  `storage.deckhouse.io/volumesnapshotclass` должна указывать на существующий VolumeSnapshotClass. Иначе DataImport
  завершается с причиной `TargetFailed` до загрузки данных.
- `ttl` и `publish` работают так же, как в режиме `CreatePVC`.

Импорт проходит следующим образом:

1. Контроллер создаёт временный PVC с префиксом имени `import-pvc-for-` в неймспейсе `d8-storage-foundation` и
   запускает сервер загрузки. На этот PVC действует квота неймспейса модуля, см. [раздел «Ограничения»](#ограничения).
1. Клиент загружает данные и отправляет `POST /api/v1/finished`. Условие `UploadFinished` получает значение `True`.
1. Контроллер останавливает сервер загрузки, создаёт CSI-снимок временного PVC и устанавливает политику удаления
   полученного VolumeSnapshotContent в `Retain`. Ссылка на него появляется в `status.data.artifactRef`, условие
   `Completed` получает значение `True`.
1. Временный PVC удаляется. Модуль `state-snapshotter` привязывает VolumeSnapshotContent к снимку из `snapshotRef`, и
   снимок получает `Ready=True`.
1. DataImport удаляется по истечении `ttl`. VolumeSnapshotContent остаётся у снимка.

Пример DataImport в режиме `PopulateData` для VolumeSnapshot `data` в неймспейсе `project`. Команда
`d8 snapshot upload` создаёт такой объект с именем `<SNAPSHOT_NAME>-<ID>` и значением `ttl` из флага `--ttl`, по
умолчанию `1h`:

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

Проверьте состояние импорта по условиям DataImport и полю `status.data.artifactRef`:

```bash
d8 k -n project get dataimports.storage-foundation.deckhouse.io data-import -o jsonpath='{.status.phase}{"\n"}{.status.data.artifactRef}{"\n"}'
```

### Ограничения

При передаче данных действуют следующие ограничения:

- DataExport и DataImport передают данные одного тома или снимка за операцию.
- Один и тот же PVC или VirtualDisk экспортируйте только через один модуль — `storage-foundation` или
  `storage-volume-data-manager`. При одновременном экспорте возникает конфликт владения PersistentVolume. Адрес берите
  из `status.publicURL` **своего** DataExport. Для одинакового источника модули могут формировать совпадающие публичные
  адреса.
- В обоих режимах загружаемые данные сначала попадают во временный PVC в неймспейсе `d8-storage-foundation`, поэтому
  на любой импорт действует ResourceQuota неймспейса модуля. Администратор должен учитывать суммарный объём
  параллельных импортов, число PVC и ограничения конкретного StorageClass
  (`<STORAGE_CLASS>.storageclass.storage.k8s.io/requests.storage` и
  `<STORAGE_CLASS>.storageclass.storage.k8s.io/persistentvolumeclaims`). Для одной передачи квота на объём хранения
  должна быть не меньше размера временного PVC: в режиме `CreatePVC` он равен размеру целевого PVC, а в режиме
  `PopulateData` значению `spec.storageParams.size`. В режиме `CreatePVC` целевой PVC дополнительно учитывается в
  квоте неймспейса пользователя.
- Если DataImport в режиме `CreatePVC` переходит в фазу `Failed`, временные объекты могут сохраняться до удаления
  DataImport или сборки мусора.

## Ограничение StorageClass и VolumeSnapshotClass для проектов

Если включён модуль `multitenancy-manager`, модуль регистрирует в нём гранты (разрешения на использование) двух cluster-wide-ресурсов (ресурсов уровня кластера): `StorageClass` (определение `storageclasses`) и `VolumeSnapshotClass` (определение `volumesnapshotclasses`). Пока администратор кластера не создал `ClusterResourceGrantPolicy` для проектов, ничего не ограничено: по умолчанию каждому проекту доступны все классы. Если `multitenancy-manager` выключен, модуль ничего не регистрирует.

Определение `storageclasses` и ссылку `storageclasses-pvc` поставляет и multitenancy-manager. Версии multitenancy-manager, которые рендерят их независимо от состояния storage-foundation, используют тот же spec; пока установлена такая версия, владельцем этих объектов может быть любой из двух модулей (релиз-владелец указан в аннотации `meta.helm.sh/release-name`), и при обновлении модулей владение может переходить от одного к другому. Spec при этом не меняется, поэтому не меняется и проверка. Более новые версии multitenancy-manager рендерят их, только пока storage-foundation выключен, поэтому при включённом storage-foundation владелец у них один — storage-foundation. storage-foundation и эти новые версии multitenancy-manager помечают оба объекта аннотацией `helm.sh/resource-policy: keep`, поэтому модуль, который перестаёт их рендерить, оставляет их в кластере, а другой модуль забирает их себе. В копии multitenancy-manager нет правила `excluded`, поэтому, пока объектами владеет multitenancy-manager, описанная ниже метка `storage.deckhouse.io/internal` не действует: пока storage-foundation выключен, а с версией multitenancy-manager, которая рендерит их независимо от состояния storage-foundation, — пока релиз storage-foundation снова не применит объекты. В версиях DKP ниже 1.78 storage-foundation тоже не задаёт это правило, чтобы его объекты совпадали с копией multitenancy-manager, и метка там не действует.

Политика со списком разрешённых классов распространяется на объекты модуля в неймспейсах проектов. Проверка применяется к следующим полям:

| Объект | Поле | Ресурс |
|---|---|---|
| PersistentVolumeClaim | `spec.storageClassName` | `storageclasses` |
| DataImport | `spec.pvcTemplate.spec.storageClassName` (класс целевого PVC) | `storageclasses` |
| DataImport | `spec.storageParams.storageClassName` (класс временного тома) | `storageclasses` |
| VolumeRestoreRequest | `spec.pvcTemplate.spec.storageClassName` | `storageclasses` |
| VolumeSnapshot | `spec.volumeSnapshotClassName` | `volumesnapshotclasses` |

Запрос с классом, недоступным проекту, отклоняется при создании с сообщением `[multitenancy] <KIND> "<NAME>" references "<CLASS>" which is not available to project "<PROJECT>"`. Исключение — PersistentVolumeClaim: его класс заменяется классом проекта по умолчанию, а в ответе приходит предупреждение; если у проекта нет доступного класса по умолчанию, claim (заявка на том) отклоняется. В DKP 1.78 и новее класс PersistentVolumeClaim, который восстанавливает VolumeSnapshot или клонирует другой PersistentVolumeClaim (`spec.dataSource` или `spec.dataSourceRef` с `kind: VolumeSnapshot` или `kind: PersistentVolumeClaim`), только проверяется и никогда не заменяется, потому что такой том может создать только CSI-драйвер источника: если класс недоступен проекту, claim отклоняется, а в сообщении сказано указать доступный проекту класс, который обслуживает этот драйвер. Если в таком claim класс не указан, он получает класс кластера по умолчанию и отклоняется, если этот класс недоступен проекту. В более ранних версиях DKP такие claim обрабатываются как остальные PersistentVolumeClaim. В DKP 1.78 и новее PersistentVolumeClaim с аннотацией `volume.beta.kubernetes.io/storage-class` проверяется по классу из аннотации, который Kubernetes использует вместо `spec.storageClassName`: если этот класс недоступен проекту, claim отклоняется, а класс не заменяется. Поля DataImport и VolumeRestoreRequest только проверяются и никогда не заменяются, поэтому том не создаётся в классе, который пользователь не запрашивал.

Например, чтобы оставить проектам только класс `fast-ssd` и класс снимков `fast-ssd-snapclass`:

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

На StorageClass, которые создают модули хранения Deckhouse, стоит метка `storage.deckhouse.io/managed-by` с именем модуля, например `csi-ceph`, `sds-local-volume` или `sds-replicated-volume`. Политика может выбирать классы модуля через `allowedSelector` или `deniedSelector`, не перечисляя их имена. Например, чтобы оставить проектам только классы sds-replicated-volume:

```yaml
apiVersion: multitenancy.deckhouse.io/v1alpha2
kind: ClusterResourceGrantPolicy
metadata:
  name: replicated-only
spec:
  projectSelector:
    matchLabels:
      environment: production
  resources:
    - resourceName: storageclasses
      allowedSelector:
        matchLabels:
          storage.deckhouse.io/managed-by: sds-replicated-volume
```

В DKP 1.78 и новее StorageClass с меткой `storage.deckhouse.io/internal: "true"` никогда не доступен проектам, что бы ни было указано в ClusterResourceGrantPolicy: его нет в `AvailableClusterResource`, а запрос с этим классом отклоняется, даже если политика перечисляет класс в `allowed`. С PersistentVolumeClaim поступают так, как описано выше: его класс заменяется классом проекта по умолчанию, если он у проекта есть. Модули хранения ставят эту метку на классы, которые создают для своих компонентов, например на `sds-elastic-osd` модуля sds-elastic, к которому привязываются тома OSD Ceph. Администратор может поставить её на любой другой StorageClass, которым проекты не должны пользоваться никогда. На неймспейсы вне проектов, где работают компоненты модулей, метка не влияет.

Учитывайте следующее:

- Если в VolumeSnapshot класс не указан, грант его не подставляет, но класс, который выберет для снимка кластер, тоже проверяется:
  - Если у StorageClass исходного PersistentVolumeClaim есть аннотация `storage.deckhouse.io/volumesnapshotclass`, storage-foundation подставляет этот класс при создании снимка, и запрос отклоняется, если класс недоступен проекту.
  - Иначе класс снимков по умолчанию CSI-драйвера записывает в снимок snapshot-controller уже после создания. snapshot-controller работает в неймспейсе `d8-storage-foundation` и из проверки не исключён, поэтому, если класс по умолчанию недоступен проекту, эта запись отклоняется: снимок остаётся без класса и не становится готовым, а в его `status.error` и в событии `SnapshotContentCreationFailed` указано `failed to take snapshot <NAME> without a snapshot class`.
- Проверяются только поля из таблицы выше. Объекты других модулей, которые указывают класс, проверяются, только если этот модуль сам регистрирует свою ссылку.
- Запросы участников группы `system:masters`, узлов и ServiceAccount из неймспейсов `kube-system`, `d8-system` и `d8-user-authz` не проверяются, поэтому политика никогда не блокирует администратора кластера и контроллеры платформы. Контроллеры этого модуля не исключены: PersistentVolumeClaim, который они создают в неймспейсе проекта, проверяется так же, как созданный пользователем.
- PersistentVolumeClaim, который создаёт DataImport в режиме `CreatePVC` или VolumeRestoreRequest, проверяется ещё раз, когда его создаёт модуль:
  - Если в `pvcTemplate` у DataImport класс не указан, PVC получает класс проекта по умолчанию. Если его нет, PVC получает класс кластера по умолчанию, и тогда он должен быть доступен проекту.
  - Если проект потерял указанный класс после создания DataImport или VolumeRestoreRequest, PVC получает класс проекта по умолчанию, а если доступного класса по умолчанию нет — отклоняется. DataImport сообщает об отказе в условии `Ready` с причиной `TargetFailed` и текстом отказа, VolumeRestoreRequest — событием `VRRPVCCreateFailed`. Оба повторяют попытку, пока класс снова не станет доступен. Если PVC от VolumeRestoreRequest получил другой класс, VolumeRestoreRequest сообщает об этом событием `VRRStorageClassReplaced`: восстановленный том остаётся в запрошенном классе.
  - Временные тома DataImport в режиме `PopulateData` и DataExport создаются в неймспейсе `d8-storage-foundation`, на который политика не действует. Класс временного тома DataImport проверяется в самом DataImport (`spec.storageParams.storageClassName`).
- Если отключить storage-foundation при включённом multitenancy-manager, удаляются гранты, которые регистрирует только этот модуль: грант `volumesnapshotclasses` и ссылки из DataImport и VolumeRestoreRequest. Пока они не зарегистрированы снова, ClusterResourceGrantPolicy не ограничивает в проектах VolumeSnapshotClass, а поля классов в DataImport и VolumeRestoreRequest не проверяются. Чтобы вернуть проверки, снова включите storage-foundation. Грант `storageclasses` и его ссылка из PersistentVolumeClaim `storageclasses-pvc` остаются в кластере: у них аннотация `helm.sh/resource-policy: keep`, и их забирает себе multitenancy-manager, поэтому StorageClass в PersistentVolumeClaim по-прежнему проверяется.
- Пользователь проекта видит доступные проекту классы в объектах `AvailableClusterResource`. Для `storageclasses` показаны `provisioner`, `reclaimPolicy`, `volumeBindingMode` и `allowVolumeExpansion` каждого класса, для `volumesnapshotclasses` — `driver` и `deletionPolicy`. Поле `parameters` класса не показывается, потому что в нём могут быть имена секретов. Доступные классы проверяются так:

  ```bash
  d8 k get available volumesnapshotclasses -n <PROJECT_NAME> -o yaml
  ```

## Диагностика

В разделах ниже описано, как читать состояние объектов модуля и как устранять типичные ошибки.

### Проверка состояния DataExport и DataImport

Каждый объект сообщает этап жизненного цикла в `status.phase`, а подробности — в `status.conditions`:

- DataExport: `Pending` → `Ready` → `Expired` или `Failed`. Причина указана в условии `Ready`.
- DataImport: `Pending` → `Ready` → `Completed`, `Expired` или `Failed`. Условия `Ready`, `UploadFinished` и
  `Completed` описывают этапы импорта.
- `Terminating` у любого объекта означает, что объект удаляется.

Посмотрите условия объекта и события неймспейса:

```bash
d8 k -n project describe dataexports.storage-foundation.deckhouse.io my-export
d8 k -n project get events --sort-by=.lastTimestamp
```

Причины условия `Ready` DataExport со `status: "False"`:

| Причина | Значение | Что делать |
|---|---|---|
| `ValidationFailed` | Спецификация или целевой объект не прошли проверку: неподдерживаемый вид цели, ссылка на cluster-wide-ресурс VolumeSnapshotContent, PVC не в состоянии `Bound` или PVC используется. | Исправьте спецификацию или освободите PVC. Причина указана в сообщении. |
| `TargetNotFound` | Целевой объект не существует или его CRD не установлен. | Проверьте `spec.targetRef` и целевой объект. |
| `TargetNotReady` | Снимок-цель ещё не привязан к SnapshotContent модуля `state-snapshotter` или у этого SnapshotContent ещё нет данных. | Пока снимок обрабатывается, дождитесь, когда модуль `state-snapshotter` заполнит `status.boundSnapshotContentName`. Если модуль пропускает снимок, экспортировать его через DataExport нельзя. См. [«Поля VolumeSnapshot, добавленные модулем»](#поля-volumesnapshot-добавленные-модулем). |
| `PVConflict` | PersistentVolume уже принадлежит другому экспорту или его аннотации противоречивы. | См. [«Экспорт не публикуется из-за конфликта PersistentVolume»](#экспорт-не-публикуется-из-за-конфликта-persistentvolume). |
| `DeploymentFailed` | Не удалось запустить сервер передачи данных. | Проверьте события и логи контроллера. |
| `PublishFailed` | Не удалось создать Service или Ingress для внешнего доступа. | Проверьте события и логи контроллера. |
| `CleanupFailed` | Не удалось удалить временные объекты после передачи. | Проверьте события и логи контроллера. |
| `Expired` | Истёк период бездействия из `spec.ttl`. | Создайте новый DataExport. |

Причины DataImport, которые указывают на сбой: `Failed`, `TargetFailed` (не удалось подготовить или передать
целевой PVC или его том) и `CleanupFailed`. Причина указана в сообщении условия.

{{< alert level="info" >}}
Для чтения логов контроллера нужны права администратора кластера.
{{< /alert >}}

Посмотрите логи контроллера:

```bash
d8 k -n d8-storage-foundation logs -l app=data-manager-controller -c controller --prefix --tail=200
```

### Создание VolumeSnapshot отклоняется

Модуль проверяет VolumeSnapshot, создаваемый из PVC, на этапе допуска. Причина указана в сообщении об ошибке:

| Сообщение | Причина | Что делать |
|---|---|---|
| `PVC StorageClassName is nil` | У исходного PVC не задан `storageClassName`. | Создавайте снимки только PVC с заданным StorageClass. |
| `StorageClass does not have volume snapshot class name annotation` | StorageClass имеет лейбл `storage.deckhouse.io/managed-by`, но не имеет аннотации `storage.deckhouse.io/volumesnapshotclass`. | Обратитесь к администратору: StorageClass должен иметь аннотацию с VolumeSnapshotClass своего драйвера. |
| `if VolumeSnapshotClassName is set, it must match the StorageClass annotation` | `volumeSnapshotClassName` отличается от аннотации StorageClass с лейблом `storage.deckhouse.io/managed-by`. | Не указывайте поле или укажите класс из аннотации. |

### VolumeSnapshot не переходит в состояние готовности

Если `status.readyToUse` остаётся `false`, проверьте снимок, его VolumeSnapshotContent и события:

```bash
d8 k -n project describe volumesnapshots.snapshot.storage.k8s.io my-first-snapshot
d8 k get volumesnapshotcontents.snapshot.storage.k8s.io
d8 k -n project get events --sort-by=.lastTimestamp
```

{{< alert level="info" >}}
Для проверки подов контроллера снимков и чтения их логов нужны права администратора кластера.
{{< /alert >}}

Поле `status.error` VolumeSnapshot содержит сообщение CSI-драйвера. Если сообщения нет, проверьте, что контроллер снимков
запущен, и посмотрите его логи:

```bash
d8 k -n d8-storage-foundation get pod -l app=snapshot-controller
d8 k -n d8-storage-foundation logs -l app=snapshot-controller -c snapshot-controller --tail=200
```

Алерты `D8SnapshotControllerTargetDown`, `D8SnapshotControllerTargetAbsent`, `D8SnapshotControllerPodIsNotReady` и
`D8SnapshotControllerPodIsNotRunning` сообщают о недоступности самого контроллера снимков.

### d8 data не находит модуль или не имеет прав

Перед каждой командой `d8 data` определяет, какой модуль обслуживает DataExport и DataImport. Возможны две ошибки:

- `no module in this cluster serves this resource`: не включён ни `storage-foundation`, ни
  `storage-volume-data-manager`, либо CRD модуля ещё не установлены. Включите модуль или дождитесь завершения
  установки.
- `not authorized to use this resource`: ресурс обслуживается, но пользователь не может выполнять `get` для него в
  неймспейсе ни в одной из групп. В сообщении указаны найденные группы и команда для проверки прав, например
  `d8 k auth can-i get dataexports.storage-foundation.deckhouse.io -n project`. Выдайте права, как описано
  [в разделе «Права доступа»](#права-доступа).

Если объекты, созданные `d8`, не появляются в группе `storage-foundation.deckhouse.io`, значит пользователь может
выполнять `get` только в группе `storage.deckhouse.io`, и `d8` работает с модулем `storage-volume-data-manager`.
Выдайте права в группе `storage-foundation.deckhouse.io`.

### d8 не может определить режим публикации

Команда завершается с сообщением `cannot auto-detect publish mode, specify --publish=true or --publish=false`, если
флаг `--publish` не указан, а проверка сервиса `kubernetes`, описанная [в разделе «Основные параметры»](#основные-параметры),
завершается ошибкой, отличной от недоступности сети или отказа TLS. Укажите флаг явно: `--publish=false` из сети
кластера, `--publish=true` извне.

### Экспорт не начинается, потому что PVC используется

PVC экспортируется только тогда, когда он не смонтирован ни одним подом. Если под использует PVC, условие `Ready`
DataExport получает причину `ValidationFailed` с сообщением
`user's PVC isn't free because it's being occupied by pods <POD_NAMES>` или
`user's PV not free because has volumeAttachments with status True: <ATTACHMENT_NAMES>`, если том ещё подключён к
узлу. Контроллер повторяет проверку каждые 10 секунд. Остановите поды, использующие PVC, или экспортируйте
VolumeSnapshot этого PVC.

### Экспорт не публикуется из-за конфликта PersistentVolume

Если DataExport не переходит в состояние готовности и условие `Ready` содержит причину `PVConflict`, PersistentVolume
уже принадлежит другому экспорту: DataExport этого модуля, указанному в сообщении
`PV <PV_NAME> is already owned by DataExport <NAMESPACE>/<NAME>`, или экспорту через `storage-volume-data-manager`.
Завершите первый экспорт и дождитесь освобождения тома, прежде чем создавать другой. Для обращения к опубликованному
экспорту используйте `status.publicURL` именно созданного вами DataExport.

### Импорт не начинается из-за ограничений ResourceQuota

Если квота превышена, API-сервер может отклонить создание PVC с `403 Forbidden` и сообщением `exceeded quota`. В этом
случае PVC не создаётся. Состояние `Pending` у существующего PVC не доказывает превышение квоты и может быть связано,
например, с нехваткой ёмкости.

Какая квота действует, зависит от режима импорта:

- В режиме `CreatePVC` действуют две квоты. Квота неймспейса пользователя распространяется на целевой PVC, а квота
  неймспейса `d8-storage-foundation` на временный PVC того же размера и StorageClass, в который сначала попадают
  данные, прежде чем том передаётся целевому PVC.
- В режиме `PopulateData` создаётся только временный PVC в неймспейсе `d8-storage-foundation`, поэтому действует
  квота неймспейса модуля.

Проверить и изменить квоту неймспейса `d8-storage-foundation` может только администратор.

Пользователь проверяет условия DataImport, события и квоту в своём неймспейсе:

```bash
d8 k -n project describe dataimports.storage-foundation.deckhouse.io my-import
d8 k -n project get events --sort-by=.lastTimestamp
d8 k -n project get resourcequota
```

{{< alert level="info" >}}
Для следующих проверок в неймспейсе `d8-storage-foundation` нужны права администратора кластера.
{{< /alert >}}

Для режима `PopulateData` администратор проверяет ResourceQuota, временные PVC и события в неймспейсе модуля, а при
отсутствии причины в событиях — логи контроллера импорта:

```bash
d8 k -n d8-storage-foundation get resourcequota
d8 k -n d8-storage-foundation get pvc
d8 k -n d8-storage-foundation get events --sort-by=.lastTimestamp
d8 k -n d8-storage-foundation logs -l app=data-manager-controller -c controller --prefix --tail=200
```

Проверьте общую квоту на хранение, число PVC и квоты по используемому StorageClass. Если ограничение превышено,
администратор корректирует квоту или число параллельных импортов. Отсутствие ошибки в условиях DataImport не исключает
отказ API-сервера.

### DataImport или DataExport остаётся в состоянии Terminating

Удалите DataImport или DataExport штатным способом. Если объект продолжает находиться в `Terminating`, проверьте
события и зависимые объекты и обратитесь к администратору. Удаление финалайзера допускается лишь как крайняя мера после
проверки зависимых объектов: в противном случае временные PVC, PV или поды могут остаться в кластере. Не удаляйте PVC
только на основании его имени или лейбла `app: data-importer`.
