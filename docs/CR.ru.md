---
title: "VolumeCaptureRequest и VolumeRestoreRequest"
description: "Текущий служебный контракт запросов захвата и восстановления тома для storage-контроллеров."
---

`VolumeCaptureRequest` (VCR) и `VolumeRestoreRequest` (VRR) — one-shot служебные ресурсы,
предназначенные для контроллеров наподобие state-snapshotter, а не стабильный пользовательский
backup UX. Фактический create/read-доступ определяется RBAC кластера; admission пока не enforce'ит
controller-only policy. Оба ресурса namespaced и находятся в
`storage-foundation.deckhouse.io/v1alpha1`; namespace запроса совпадает с namespace
исходного/целевого PVC.

Эта страница резюмирует текущие generated CRD и поведение controller/sidecar. ADR доменного SDK
state-snapshotter задаёт контракт использования этих ресурсов доменными контроллерами, а
unified-snapshots overview владеет core-маппингом `Ready` reasons. Исходный ADR
`2025-11-30-volume-capture-and-restore-request.md` — историческое обоснование, а не текущая
implementable-схема.

## VolumeCaptureRequest

VCR создаёт durable data artifact для одного PVC. Domain snapshot SDK использует только
`spec.mode: Snapshot`; `Detach` — отдельный flow storage-foundation.

```yaml
apiVersion: storage-foundation.deckhouse.io/v1alpha1
kind: VolumeCaptureRequest
metadata:
  name: nss-vcr-4f2c8a91d0e3b7c2
  namespace: my-app
spec:
  mode: Snapshot
  target:
    uid: "2b4f6c7e-7e1d-4d85-95d0-8b52d61534d8"
    apiVersion: v1
    kind: PersistentVolumeClaim
    name: data
status:
  data:
    artifactRef:
      apiVersion: snapshot.storage.k8s.io/v1
      kind: VolumeSnapshotContent
      name: snapshot-73a4cda0-4f2c8a91d0e3
      uid: "1148b6bd-9de2-4c86-a814-7688300932eb"
  completionTimestamp: "2026-07-23T12:34:56Z"
  conditions:
    - type: Ready
      status: "True"
      lastTransitionTime: "2026-07-23T12:34:56Z"
      reason: Completed
      message: "target ready"
```

Контракт:

- `spec.target` — identity одного PVC; namespace неявно равен namespace VCR. Это single-target
  request, а не bulk capture API.
- Request point-in-time, но enforcement неполон. Для Snapshot-mode CRD требует непустые
  `uid`, `apiVersion`, `kind` и `name`, однако transition-CEL с иммутабельностью `spec` отсутствует.
  SDK обнаруживает изменение существующего target только при следующем reconcile `EnsureVCR`.
  Controller storage-foundation сейчас резолвит live PVC по namespace запроса и имени target, не
  требует точные `apiVersion: v1` и `kind: PersistentVolumeClaim` и не сравнивает UID live PVC с
  `spec.target.uid`; UID используется в детерминированном имени артефакта. Server-side
  immutability/typed drift вынесены в backlog #26, остальные UID/GVK/admission hardening — в #28.
  CRD требует `target` только для Snapshot-mode, хотя controller-path Detach тоже без него не работает.
- `status.data.artifactRef` указывает на durable `VolumeSnapshotContent`/`PersistentVolume`.
  Identity исходного PVC остаётся в `spec.target` и в status не дублируется.
- `Ready=True/Completed` — успех.
- `Ready=False/TargetsPending` — **нетерминальное** ожидание: CSI capture продолжает retry.
  Сам по себе `VolumeSnapshotContent.status.error` request не терминалит.
- Любой другой VCR `Ready=False` терминален.
- В текущем SDK-flow домен с post-capture wait/action вызывает `MarkPlanned`, затем читает
  `snapshotsdk.CoreCaptureOutcome`; state-snapshotter сворачивает терминальный VCR в
  `VolumeCaptureFailed`. Активный план `namespace-root-mcr-before-planned` целится в guarded direct
  `Finished` для childless-домена с полным планом. В этой ревизии target-поведение ещё не
  реализовано; после реализации такой leaf не будет обязан самостоятельно интерпретировать VCR
  conditions.

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
    name: snapshot-73a4cda0-4f2c8a91d0e3
  pvcTemplate:
    metadata:
      name: data
    spec:
      accessModes: [ReadWriteOnce]
      storageClassName: local
      volumeMode: Filesystem
  fsType: ext4
status:
  pvcRef:
    kind: PersistentVolumeClaim
    namespace: restore-ns
    name: data
    uid: "8c37aa04-af6e-4671-9446-87062a18c189"
  completionTimestamp: "2026-07-23T12:36:04Z"
  conditions:
    - type: Ready
      status: "True"
      lastTransitionTime: "2026-07-23T12:36:04Z"
      reason: Completed
      message: "PVC restore-ns/data restored successfully"
```

Контракт:

- CRD schema требует `sourceRef.name`, `pvcTemplate` и `pvcTemplate.metadata.name`.
  Cross-namespace restore не поддерживается.
- Executor дополнительно требует непустой `pvcTemplate.spec.storageClassName` и точный
  `pvcTemplate.spec.volumeMode` со значением `Block` или `Filesystem`. В текущей schema эти поля
  optional. Schema-accepted request без любого из них executor игнорирует с Event, а controller
  продолжает ждать PVC без записи `Ready` и `completionTimestamp`. Такой VRR нетерминален и потому
  бессмертен для generic GC. Этот malformed empty-status gap требует исправления кода; документация
  не объявляет его допустимым execution-контрактом.
- `pvcTemplate` материализуется лишь частично, и source kinds ведут себя по-разному. Executor
  валидирует template storage class и volume mode для каждого request. Для источника
  `VolumeSnapshotContent` он использует template storage class, volume mode и access modes (default
  `ReadWriteOnce`), а также root `fsType` для Filesystem-томов. Для источника `PersistentVolume`
  авторитетен source PV: volume mode, access modes, filesystem type и capacity берутся из него, а
  соответствующие template-поля не определяют фактическую форму restore. Ни один путь не копирует
  template labels/annotations и не считает `pvcTemplate.spec.resources.requests.storage`
  авторитетным.
- Поддерживаемые source kinds — `VolumeSnapshotContent` и `PersistentVolume`. Текущие controller и
  executor выбирают источник по `kind` и `name`; `sourceRef.apiVersion`, `namespace` и `uid` не
  проверяются относительно live source. Hardening source identity и admission отслеживается в
  backlog #28.
- Patched external-provisioner watch'ит VRR, вызывает CSI `CreateVolume` и создаёт PV/PVC, но
  **не пишет** VRR status.
- `VolumeRestoreRequestController` — единственный status writer: валидирует source, наблюдает PVC и
  публикует `pvcRef`, `completionTimestamp`, `Ready`.
- Текущий writer `pvcRef` публикует `kind`, `namespace`, `name` и `uid`, но оставляет optional
  `apiVersion` пустым; пример намеренно совпадает с writer. Запись `v1` и writer-test — активная
  работа плана `namespace-root-mcr-before-planned`.
- Терминальный VRR одноразовый; для новой попытки создаётся новый request.

## Текущие conditions и reasons

| Ресурс | Reasons, которые пишет текущий controller |
| --- | --- |
| VCR | `Completed`, `TargetsPending`, `InternalError`, `NotFound`, `RBACDenied`; `InvalidMode` остаётся defensive writer path, хотя CRD enum отклоняет неизвестный mode |
| VRR | `Completed`, `InvalidSource`, `InternalError`, `NotFound` |

`SnapshotCreationFailed` оставлен только для совместимости и больше не выставляется: на VCR-пути
`VolumeSnapshotContent.status.error` остаётся `TargetsPending`. Общие константы `Incompatible`,
`UnsupportedTargetKind`, `PVBound` и `RestoreFailed` сейчас не используются ни одним из двух request
controllers. Ошибка `status.error` у VSC-источника VRR — другой путь: он терминалится как
`Ready=False/InternalError`.

## Retention и удаление

Терминальные VCR/VRR удаляет cron-driven generic GC по `status.completionTimestamp`.

| Переменная | Default |
| --- | --- |
| `GC_VCR_TTL` / `GC_VRR_TTL` | `24h` |
| `GC_VCR_SCHEDULE` / `GC_VRR_SCHEDULE` | `0 * * * *` |

Per-object TTL-аннотаций и module settings для этих значений нет. Подробности ownership артефактов
и adoption DataImport — в `images/controller/docs/TTL_MECHANISM.md`. На успешном пути
state-snapshotter может удалить VCR раньше, после durable handoff; generic GC чистит терминальные
остатки. Request без терминального `Ready` и `completionTimestamp`, включая malformed VRR выше,
collector не удаляет.
Текущий VRR keeper не связан с созданным executor'ом restore target PVC, поэтому GC запроса не
удаляет этот PVC; implementation-specific детали ownership приведены в linked TTL page.

## Валидация и RBAC

Валидация выполняется CRD schema/CEL и контроллерами; admission webhook с
`SelfSubjectAccessReview` для VCR/VRR отсутствует.

| Actor | Фактический/request-specific доступ |
| --- | --- |
| Deckhouse `User` / RBAC v2 viewer | Cluster-wide `get`, `list`, `watch` VCR и VRR |
| Deckhouse `ClusterEditor` / RBAC v2 manager | Эффективный read-доступ плюс `create`, `update`, `patch`, `delete`, `deletecollection`; grant на request `/status` отсутствует |
| Capture-domain на snapshotsdk | Нужны VCR `get`/`create`/`patch` (и list/watch, если deployment их watch'ит); VCR status домен не пишет |
| Core state-snapshotter | Текущий template выдаёт VCR CRUD/delete и update/patch VCR `/status`, хотя служебный контракт отдаёт VCR status storage-foundation; после durable handoff core реапит VCR |
| Restore-consumer (текущий DataExport path) | Роль data-manager выдаёт VCR/VRR `create`, `delete`, `list`, `get`, `watch`, `update`; request `/status` не выдаётся |
| Patched CSI provisioner executor | Cluster-wide VRR `get`, `list`, `watch` и target-PVC `get`, `list`, `watch`, `create`, `update`, `patch`; VRR `/status` отсутствует |
| Controller storage-foundation | CRUD VCR/VRR плюс оба `/status` subresource; пишет request status и управляет следующими за request ObjectKeeper |

Из-за user-facing grants эти ресурсы фактически не controller-only, несмотря на предназначение
служебного API. Хук `040-vrr-provisioner-rbac` сейчас bind'ит executor-grant только ServiceAccount
`csi` в `d8-sds-local-volume`. Generic cross-driver VRR требует расширения общего
CSI deployment/RBAC-контракта.
