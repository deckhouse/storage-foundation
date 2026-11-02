---
title: "Модуль storage-foundation"
description: "Включает поддержку снимков, клонирования томов и передачи данных томов по HTTP для совместимых CSI-драйверов в кластере Kubernetes."
---

Модуль предоставляет общие возможности работы с хранилищем для совместимых CSI-драйверов в кластере Kubernetes: создание и
восстановление снимков томов, клонирование PersistentVolumeClaim (PVC), а также экспорт и импорт содержимого постоянных
томов по HTTP.

Модуль связан с тремя другими модулями Deckhouse Platform:

- `snapshot-controller` устарел. Модуль `storage-foundation` полностью заменяет его.
- `storage-volume-data-manager` выполняет такую же передачу данных томов. Модуль `storage-foundation` берёт эту функцию
  на себя, при этом оба модуля могут оставаться включёнными. Один и тот же том экспортируется только через один из
  них.
- `state-snapshotter` зависит от модуля `storage-foundation`. При создании снимка неймспейса он захватывает и
  восстанавливает тома через этот модуль.

Модули Deckhouse Platform, которые поддерживают снимки:

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

## Основные возможности

Модуль предоставляет следующие операции с хранилищем:

- создание точечных снимков постоянных томов с помощью совместимых CSI-драйверов;
- восстановление постоянных томов из снимков;
- клонирование существующих PVC с помощью механизма CSI Volume Cloning;
- экспорт данных PVC, VolumeSnapshot, VirtualDisk и VirtualDiskSnapshot по HTTP;
- импорт данных по HTTP в новый PVC или сохраняемый артефакт снимка;
- возобновление частичной передачи данных с помощью HTTP-механизмов диапазонов и смещений.

## Как работает модуль

Компоненты модуля работают в неймспейсе `d8-storage-foundation`. Снимок создаётся следующим образом:

1. Пользователь создаёт VolumeSnapshot, который ссылается на PVC.
1. Валидирующий вебхук модуля проверяет запрос и заполняет `volumeSnapshotClassName` из аннотации StorageClass,
   если поле не задано.
1. Контроллер снимков обращается к CSI-драйверу, который создаёт снимок в системе хранения, и связывает
   VolumeSnapshot с VolumeSnapshotContent.
1. VolumeSnapshot получает `readyToUse: true`, и пользователь может восстановить из него PVC. Экспортировать данные
   снимка можно после того, как модуль `state-snapshotter` дополнительно привяжет снимок к объекту SnapshotContent
   и заполнит данные этого SnapshotContent.

Данные тома передаются следующим образом:

1. Пользователь создаёт DataExport или DataImport API-группы `storage-foundation.deckhouse.io` в неймспейсе PVC
   с помощью команд `d8 data` или манифеста.
1. Контроллер передачи данных проверяет объект и запускает для него сервер передачи данных в неймспейсе модуля.
   Если задан `spec.publish`, контроллер также публикует сервер за пределами кластера.
1. Объект получает `Ready=True`, а поля `status.url` и `status.publicURL` содержат адреса сервера. Клиент скачивает
   или загружает данные по HTTP.
1. По истечении периода бездействия из `spec.ttl` контроллер останавливает сервер, а позже удаляет объект. При
   импорте в новый PVC этот PVC остаётся в неймспейсе.

Модуль расширяет VolumeSnapshot полем `spec.mode` и секциями `status.data`, `status.captureState` и
`status.conditions`. Модуль `state-snapshotter` использует их, чтобы включить снимок тома в снимок неймспейса.
Для этого модуля контроллер также обслуживает агрегированный API субресурсов снимков и обрабатывает объекты
VolumeCaptureRequest и VolumeRestoreRequest. Это служебные объекты, они не предназначены для прямого использования.

Модуль `storage-volume-data-manager` может оставаться включённым одновременно с модулем `storage-foundation`: каждый из них
обслуживает свои объекты DataExport и DataImport в своей API-группе и своём неймспейсе. Команды `d8 data`
(версии 0.33.14 и новее) работают с модулем `storage-foundation` через группу `storage-foundation.deckhouse.io`, если
кластер её обслуживает и у пользователя есть права на чтение её объектов в целевом неймспейсе. Иначе они работают с
модулем `storage-volume-data-manager` через группу `storage.deckhouse.io`. Команды `d8 snapshot` работают только с
модулем `storage-foundation`.

## Ограничения

При использовании модуля учитывайте следующие ограничения:

- Возможность создания снимков и клонирования зависит от поддержки в CSI-драйвере и системе хранения.
- PVC можно экспортировать только тогда, когда он не смонтирован ни одним подом.
- Каждый DataImport или DataExport работает с одним томом или снимком.
- Один и тот же PVC или VirtualDisk можно экспортировать одновременно только через один модуль: `storage-foundation`
  или `storage-volume-data-manager`. Одновременный экспорт приводит к конфликту владения PersistentVolume.
