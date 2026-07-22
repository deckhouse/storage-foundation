---
title: "VolumeCaptureRequest и VolumeRestoreRequest"
description: "Текущий служебный контракт запросов захвата и восстановления тома для storage-контроллеров."
---

# Служебные request-ресурсы

`VolumeCaptureRequest` (VCR) и `VolumeRestoreRequest` (VRR) — one-shot служебные ресурсы,
предназначенные для контроллеров наподобие state-snapshotter, а не стабильный пользовательский
backup UX. Фактический create/read-доступ определяется RBAC кластера; admission пока не enforce'ит
controller-only policy. Оба ресурса namespaced и находятся в
`storage-foundation.deckhouse.io/v1alpha1`; namespace запроса совпадает с namespace
исходного/целевого PVC.

## VolumeCaptureRequest

VCR создаёт durable data artifact для одного PVC. Domain snapshot SDK использует только
`spec.mode: Snapshot`; `Detach` — отдельный flow storage-foundation.

```yaml
apiVersion: storage-foundation.deckhouse.io/v1alpha1
kind: VolumeCaptureRequest
metadata:
  name: nss-vcr-...
  namespace: my-app
spec:
  mode: Snapshot
  target:
    uid: "<pvc-uid>"
    apiVersion: v1
    kind: PersistentVolumeClaim
    name: data
status:
  data:
    artifactRef:
      apiVersion: snapshot.storage.k8s.io/v1
      kind: VolumeSnapshotContent
      name: snapcontent-...
      uid: "<artifact-uid>"
  completionTimestamp: "..."
  conditions:
    - type: Ready
      status: "True"
      reason: Completed
```

Контракт:

- `spec.target` — единичная identity PVC; namespace неявно равен namespace VCR.
- Request point-in-time: менять target после создания нельзя. SDK fail-closed отклоняет существующий
  request с другим target; CRD пока не enforce'ит transition-иммутабельность target через CEL.
- `status.data.artifactRef` указывает на durable `VolumeSnapshotContent`/`PersistentVolume`.
  Identity исходного PVC остаётся в immutable `spec.target` и в status не дублируется.
- `Ready=True/Completed` — успех.
- `Ready=False/TargetsPending` — **нетерминальное** ожидание: CSI capture продолжает retry.
  Сам по себе `VolumeSnapshotContent.status.error` request не терминалит.
- Любой другой `Ready=False` терминален. `SnapshotCreationFailed` сохранён для совместимости API,
  но контроллер больше его не выставляет.
- Core state-snapshotter сворачивает терминальный VCR в `VolumeCaptureFailed`; доменный контроллер
  должен читать `snapshotsdk.CoreCaptureOutcome`, а не интерпретировать VCR conditions напрямую.

## VolumeRestoreRequest

VRR просит patched external-provisioner создать и забайндить один PVC из
`VolumeSnapshotContent` или `PersistentVolume`.

```yaml
apiVersion: storage-foundation.deckhouse.io/v1alpha1
kind: VolumeRestoreRequest
metadata:
  name: restore-data
  namespace: restore-ns
spec:
  sourceRef:
    apiVersion: snapshot.storage.k8s.io/v1
    kind: VolumeSnapshotContent
    name: snapcontent-...
  pvcTemplate:
    metadata:
      name: data
    spec:
      accessModes: [ReadWriteOnce]
      storageClassName: local
      resources:
        requests:
          storage: 10Gi
  fsType: ext4
status:
  pvcRef:
    apiVersion: v1
    kind: PersistentVolumeClaim
    namespace: restore-ns
    name: data
    uid: "<pvc-uid>"
  completionTimestamp: "..."
  conditions:
    - type: Ready
      status: "True"
      reason: Completed
```

Контракт:

- `spec.pvcTemplate.metadata.name` обязателен; cross-namespace restore не поддерживается.
- Источник — `VolumeSnapshotContent` или `PersistentVolume`.
- Patched external-provisioner watch'ит VRR, вызывает CSI `CreateVolume` и создаёт PV/PVC, но
  **не пишет** VRR status.
- `VolumeRestoreRequestController` — единственный status writer: валидирует source, наблюдает PVC и
  публикует `pvcRef`, `completionTimestamp`, `Ready`.
- Терминальный VRR одноразовый; для новой попытки создаётся новый request.

## Retention и удаление

Терминальные VCR/VRR удаляет cron-driven generic GC по `status.completionTimestamp`.

| Переменная | Default |
|---|---|
| `GC_VCR_TTL` / `GC_VRR_TTL` | `24h` |
| `GC_VCR_SCHEDULE` / `GC_VRR_SCHEDULE` | `0 * * * *` |

Per-object TTL-аннотаций и module settings для этих значений нет. Подробности ownership артефактов
и adoption DataImport — в `images/controller/docs/TTL_MECHANISM.md`. На успешном пути
state-snapshotter может удалить VCR раньше, после durable handoff; generic GC чистит терминальные
остатки.

## Валидация и RBAC

Валидация выполняется CRD schema/CEL и контроллерами; admission webhook с
`SelfSubjectAccessReview` для VCR/VRR отсутствует.

Инициирующим SA нужны только verb'ы конкретного flow. Snapshot-домен обычно создаёт/читает/патчит
VCR и создаёт/удаляет VRR. Status запросов принадлежит controller storage-foundation.
Хук `040-vrr-provisioner-rbac` выдаёт patched CSI provisioner'ам cluster-wide read/watch VRR и
создание целевого PVC; sidecar'ы не должны обновлять VRR status. Текущий production registry
namespace'ов покрывает `d8-sds-local-volume`; generic cross-driver VRR требует расширения общего
CSI deployment/RBAC-контракта.
