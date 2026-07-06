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
