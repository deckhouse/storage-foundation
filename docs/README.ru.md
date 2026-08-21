---
title: "Модуль storage-foundation"
description: "Включает поддержку снапшотов и клонирование томов для совместимых CSI-драйверов в кластере Kubernetes."
---

Этот модуль включает поддержку снапшотов для совместимых CSI-драйверов в кластере Kubernetes.

CSI-драйверы в Deckhouse Kubernetes Platform, которые поддерживают снапшоты:

- [cloud-provider-openstack](/modules/cloud-provider-openstack/)
- [cloud-provider-vsphere](/modules/cloud-provider-vsphere/)
- [cloud-provider-aws](/modules/cloud-provider-aws/)
- [cloud-provider-azure](/modules/cloud-provider-azure/)
- [cloud-provider-gcp](/modules/cloud-provider-gcp/)
- [sds-local-volume](/modules/sds-local-volume/stable/)
- [sds-replicated-volume](/modules/sds-replicated-volume/stable/)
- [csi-ceph](/modules/csi-ceph/stable/)
- [csi-nfs](/modules/csi-nfs/stable/)
- [csi-hpe](/modules/csi-hpe/stable/)
- [csi-huawei](/modules/csi-huawei/stable/)
- [csi-yadro-tatlin-unified](/modules/csi-yadro-tatlin-unified/stable/)

## Экспорт и импорт содержимого томов по HTTP

Модуль также обеспечивает безопасные экспорт и импорт содержимого постоянных томов по протоколу HTTP. Он создаёт namespaced-ресурс `DataExport` или `DataImport` в целевом неймспейсе, который ссылается на том для экспорта через поле `targetRef`. Поддерживаемые типы целевых ресурсов — `PersistentVolumeClaim` и `VolumeSnapshot`.

Сервер данных построен на основе стандартного файлового сервера Go и поддерживает как файловый, так и блочный режимы работы с томами. Аутентификация пользователей осуществляется через Kubernetes RBAC с поддержкой частичного скачивания/загрузки контента с использованием HTTP-заголовков `Range`.

### Ключевые параметры

- `ttl` — время жизни после последней передачи данных. Окно отсчитывается от простоя и учитывает обе стороны: его сдвигают и скачивание (чтение файла или просмотр директории), и заливка, пока её байты продолжают поступать, поэтому передача, которая длится дольше TTL, не обрывается — истекает только простаивающий сервер. По истечении TTL под сервера автоматически удаляется вместе с остальной серверной инфраструктурой, а условие `Ready` ресурса устанавливается в `false` с причиной `Expired`. Для `DataExport` PVC при этом возвращается к исходному PV; `DataImport`, истёкший до завершения заливки, не производит ничего — чтобы повторить попытку, нужно создать новый `DataImport`.

- `publish` — при установке в `true` обеспечивает внешний доступ к экспортер-поду извне кластера. В поле `status.publicURL` ресурса генерируется публичный URL в формате: `https://api.<public-domain>/<namespace>/<kindShort>/<name>/`.

Примеры использования (утилита `d8`, манифесты и описание HTTP API) приведены в [документации по использованию](usage.html).
