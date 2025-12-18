# Deckhouse Fire-and-Forget Operation Controller Pattern

Этот документ описывает общие архитектурные паттерны, используемые в контроллерах Deckhouse Storage:
- `state-snapshotter` (ManifestCaptureRequest, ManifestCheckpoint)
- `storage-foundation` (VolumeCaptureRequest, VolumeRestoreRequest)

**Важно:** Все описанные здесь паттерны применяются к обоим модулям. Если паттерн касается обоих модулей, он описан как общий принцип без разделений на конкретные реализации.

## Название паттерна

**Deckhouse Fire-and-Forget Operation Controller**

### Что означает Fire-and-Forget

Fire-and-Forget означает:
- контроллер инициирует операцию один раз
- не управляет её выполнением
- не делает повторных попыток
- не переходит в state machine
- не переиспользует Request

Контроллер не "забывает" объект, а **не возвращается к нему логически** после достижения терминального состояния.

---

## 1. Контроллер — оркестратор, а не исполнитель

### Общее

Контроллер не делает реальную работу. Он:
- создаёт/связывает Kubernetes-объекты
- ждёт внешние контроллеры / CSI / kube-GC
- агрегирует состояние в Status

### Следствие

- Никакой «бизнес-логики» в reconcile
- Никаких ретраев внешних операций
- Нет попыток «лечить» ошибки нижнего уровня

**Контроллер описывает что должно быть, а не как это делать.**

### Реализация

Контроллеры создают артефакты (VSC/PV, ManifestCheckpoint, ObjectKeeper) и транслируют статусы внешних объектов в Status Request-ресурса.

**Важно:** Контроллеры **не создают** некоторые артефакты напрямую:
- `VolumeRestoreRequestController` **не создаёт** PVC — это делает external-provisioner
- `VolumeCaptureRequestController` создаёт VSC напрямую (без VolumeSnapshot)
- `VolumeRestoreRequestController` **не создаёт** VolumeSnapshot (запрещено архитектурой)

**Примеры:**
- `VolumeCaptureRequestController`, `VolumeRestoreRequestController` (storage-foundation)
- `ManifestCheckpointController` (state-snapshotter)

---

## 2. Fire-and-forget ресурсы

### Общее

Все Request-ресурсы:
- `VolumeCaptureRequest`
- `VolumeRestoreRequest`
- `ManifestCaptureRequest`
- `ManifestCheckpoint`

Все они:
- представляют одну операцию
- имеют терминальное состояние
- никогда не переисполняются

**Жизненный цикл:** `Pending → Ready=True | Ready=False → TTL → GC`

**Важно:** Используется только один condition — `Ready`:
- `Ready=True` — успешное завершение
- `Ready=False` — финальная ошибка
- Condition выставляется только в финальном состоянии (терминальный успех или терминальная ошибка)

### Запрещено

- пересоздание артефактов
- повторные попытки после ошибки
- «исправление» существующих объектов

### Реализация

После достижения терминального состояния (Ready=True или Ready=False) reconcile должен немедленно завершаться без обработки. Проверка терминальности выполняется в начале reconcile через проверку наличия Ready condition с финальным статусом.

---

## 3. Терминальное состояние — источник истины

### Общее

После установки Ready condition объект считается **immutable**.

Reconcile обязан немедленно завершаться без обработки. Единственное допустимое последующее действие — TTL cleanup через scanner.

### Ошибка = терминальная

Если внешний объект (VSC / Job / Pod / Checkpoint) сказал Error, контроллер транслирует в `Ready=False` с соответствующим `Reason`, а не интерпретирует.

Все ошибки транслируются в `Ready=False`. Детали ошибки передаются через `Message`, категоризация — через соответствующие `Reason` константы из `api/v1alpha1/conditions.go`.

---

## 4. Разделение ролей владения (ownership split)

### Общее

| Роль | Кто |
|------|-----|
| Инициатор | VCR / VRR / MCR |
| Владение артефактами | ObjectKeeper |
| Создание артефактов | VCR (VSC/PV) / external-provisioner (PVC/PV для restore) / MCR (ManifestCheckpoint) |
| Исполнение | CSI / snapshot-controller / external-provisioner / kubelet |
| Очистка | kube-GC |

### Инварианты

- Request не владеет артефактами
- ObjectKeeper — единственный controller owner артефактов
- GC всегда стандартный, без финалайзеров
- **VRR не создаёт PVC** — это делает external-provisioner (патченный)
- **VCR создаёт VSC напрямую** (без VolumeSnapshot)

### Реализация

Request создаёт ObjectKeeper, который становится controller owner артефактов (VSC/PV, PVC, ManifestCheckpoint). При удалении Request автоматически удаляются ObjectKeeper и все артефакты через стандартный Kubernetes GC.

**Важно:** Разные Request-ресурсы создают разные артефакты:
- `VolumeCaptureRequest` создаёт VSC/PV напрямую
- `VolumeRestoreRequest` **не создаёт** PVC — это делает external-provisioner, который наблюдает за VRR
- `ManifestCaptureRequest` создаёт ManifestCheckpoint

```
Request
   |
   v
ObjectKeeper (FollowObject)
   |
   v
Artifacts (VSC / PV / PVC / MCP / Chunks)
```

---

## 5. ObjectKeeper как якорь владения

### Общее

ObjectKeeper — ownership anchor:
- режим `FollowObject`
- не имеет TTL
- живёт ровно столько же, сколько Request

### Зачем

- безопасная цепочка ownerRef
- можно удалять Request → GC гарантирован
- нет финалайзеров → нет дедлоков

### Реализация

ObjectKeeper создается с режимом `FollowObject` и не имеет TTL. Он живёт ровно столько же, сколько Request, и обеспечивает безопасную цепочку ownerRef для артефактов.

---

## 6. ObjectKeeper UID — обязательный барьер после создания

### Критически важно

**После создания ObjectKeeper его UID должен быть гарантированно получен перед использованием в OwnerReference.**

### Проблема

После `Create()`:
- UID гарантированно существует в API server
- но локальный объект или cache может не содержать UID
- использование пустого UID в OwnerReference приведёт к ошибке

### Решение

После `Create()` необходимо гарантировать получение UID перед использованием в OwnerReference.

**Единый подход, реализованный в обоих модулях (storage-foundation и state-snapshotter):**

```go
if err := r.Create(ctx, objectKeeper); err != nil {
    return nil, ctrl.Result{}, fmt.Errorf("failed to create ObjectKeeper: %w", err)
}
l.Info("Created ObjectKeeper", "name", name)

// Re-read ObjectKeeper to get UID populated by apiserver/fake client
if err := r.Get(ctx, client.ObjectKey{Name: name}, objectKeeper); err != nil {
    return nil, ctrl.Result{}, fmt.Errorf("failed to re-read ObjectKeeper after Create: %w", err)
}

// HARD BARRIER: UID must exist before creating artifacts with OwnerReference
// If UID is still not populated (shouldn't happen with real apiserver, but possible with fake client), requeue
if objectKeeper.UID == "" {
    l.Info("ObjectKeeper UID not assigned yet, requeue", "name", name)
    return nil, ctrl.Result{RequeueAfter: time.Second}, nil
}

// UID гарантированно заполнен, можно использовать
artifact.OwnerReferences = []metav1.OwnerReference{{
    UID: objectKeeper.UID,
    // ...
}}
```

**Идемпотентность:**
- При повторном reconcile ObjectKeeper уже существует (проверка `IsNotFound`)
- Если ObjectKeeper существует, но UID пустой — requeue с `RequeueAfter: time.Second`
- На следующем reconcile ObjectKeeper уже будет иметь UID (либо из cache, либо из API server)

### Требования

1. **Обязательно:** После `Create()` выполнить `Get()` для получения UID из API server
2. **Обязательно:** Проверить `objectKeeper.UID == ""` и вернуть `ctrl.Result{RequeueAfter: time.Second}` если пустой
3. **Обязательно:** Использовать UID только после гарантированного получения
4. **Запрещено:** Использование пустого UID в OwnerReference (приведёт к некорректной работе ownerRef и Kubernetes GC, иногда без явной ошибки)

### Почему это важно

- OwnerReference с пустым UID не работает
- Kubernetes GC игнорирует ownerRef без правильного UID
- Это может привести к утечкам ресурсов

---

## 7. TTL — только у Request

### Общее

- TTL начинается с `CompletionTimestamp`
- TTL — политика контроллера, а не пользователя
- annotation — только informational

### Важно

- reconcile не занимается TTL
- TTL scanner:
  - только List + Delete
  - работает leader-only
  - не трогает Status

### Реализация

TTL scanner запускается как leader-only `manager.RunnableFunc` в `Add*ControllerToManager`. Scanner периодически сканирует терминальные Request-ресурсы и удаляет те, у которых истёк TTL (с момента `CompletionTimestamp`).

Annotation TTL игнорируется, используется значение из конфига контроллера.

---

## 8. Cache — только для динамических объектов

### Общее правило

| Тип объекта | Как читаем |
|-------------|-----------|
| PVC / PV / Pod / VSC | cache |
| StorageClass / SnapshotClass | APIReader |
| CRD-конфиг | APIReader |

### Принцип

**Cluster-level config — не watch, не cache**

### Причины

- RBAC проще
- меньше гонок
- конфиг должен существовать до Request

### Исключение

Cluster-scoped артефакты, создаваемые самим контроллером (ManifestCheckpoint, VolumeSnapshotContent), могут читаться через cache как динамические объекты.

### APIReader — обязательная зависимость

APIReader должен быть:
- обязательным полем контроллера
- валидирован при создании контроллера
- использован напрямую без проверок на nil

```go
type Controller struct {
    client.Client
    APIReader client.Reader // Required: for reading cluster-level config
    // ...
}

func AddControllerToManager(mgr ctrl.Manager, cfg *config.Options) error {
    apiReader := mgr.GetAPIReader()
    if apiReader == nil {
        return fmt.Errorf("APIReader must not be nil: controllers require APIReader")
    }
    // ...
}
```

### Реализация

APIReader используется для чтения cluster-level конфигурации (StorageClass, SnapshotClass, CRD-конфиг). 

**Примечание:** APIReader может использоваться для UID barrier после создания ObjectKeeper, но в текущих модулях (storage-foundation и state-snapshotter) выбран requeue-based barrier через обычный `Get()` с проверкой пустого UID. Валидация APIReader выполняется при создании контроллера или в `AddControllerToManager`.

---

## 9. Read-after-write consistency не требуется

### Общее

После `Create()`:
- UID может быть пуст
- Status может отсутствовать
- это нормально

### Паттерн

- проверка → requeue
- жёсткие барьеры (UID before ownerRef)

### Реализация

Полная read-after-write consistency не требуется. Требуются только точечные жёсткие барьеры (например, UID barrier перед использованием в OwnerReference) и requeue при необходимости.

---

## 10. Ошибки инфраструктуры ≠ пользовательские ошибки

### Общее сопоставление

| Ситуация | Outcome |
|----------|---------|
| NotFound | Ready=False (terminal) |
| Forbidden | Ready=False (terminal) |
| Invalid config | Ready=False (terminal) |
| External controller error | Ready=False (terminal) |
| Temporary отсутствие status | Requeue |

### Реализация

Все ошибки транслируются в `Ready=False` с соответствующими `Reason` константами из `api/v1alpha1/conditions.go`. Временные ситуации (отсутствие статуса) обрабатываются через requeue.

**Примечание:** NotFound считается терминальной ошибкой, так как Request — декларация мгновенного среза состояния. Если объект появится позже, требуется новый Request.

---

## 11. Минимальный reconcile

### Общее

- нет циклов ожидания внутри reconcile
- нет `for {}` / `sleep`
- всегда быстрый выход

### Реализация

Все reconcile функции должны возвращаться быстро. Нет циклов ожидания, нет внутренних retry циклов (кроме `RetryOnConflict` для `Status().Patch()` при конфликтах записи статуса).

---

## 12. Request-style модель

### Общее

Все контроллеры используют request-style модель:
- Один reconcile = одна попытка
- Ошибка → terminal Ready=False → нет retry
- Для retry пользователь должен удалить и пересоздать Request
- Нет retry state machine, нет внутренних retry циклов
- Retry только для `Status().Patch()` (конфликты записи статуса)

### Ключевые принципы

1. **Fail-fast семантика:**
   - `Create(ObjectKeeper)` → fail-fast
   - `Create(Checkpoint/VSC)` → fail-fast (+ AlreadyExists как идемпотентность)
   - `Create(Chunk)` → fail-fast (+ AlreadyExists validation с checksum при необходимости)

2. **Терминальность:**
   - Проверка терминальности выполняется в начале reconcile
   - Terminal Request = immutable (кроме TTL annotation в post-restart сценарии)

3. **Единая финализация:**
   - Все пути финализации используют единую функцию финализации
   - Устраняет дублирование кода
   - Гарантирует консистентность

4. **Обработка ошибок:**
   - Все ошибки транслируются в `Ready=False`
   - Детали передаются через `Message`, категоризация — через соответствующие `Reason` константы

### Реализация

Early-return по терминальности в начале reconcile. Fail-fast создание объектов без внутренних retry циклов. Единая функция финализации для всех путей завершения.

---

## Краткая формула (чтобы «запомнить»)

```
Request — это декларация операции
ObjectKeeper — это владение
Контроллер — это оркестратор
GC — это уборщик
TTL — это политика, а не логика
UID — это обязательный барьер после Create()
Terminal = immutable (request-style)
```

---

## Lifecycle diagram

```
Create Request
   ↓
Create ObjectKeeper
   ↓ (UID barrier)
Create Artifact (VSC/PV/ManifestCheckpoint)
   ↓
External controller works (CSI / snapshot-controller / external-provisioner)
   ↓
Ready=True / Ready=False (terminal state)
   ↓
TTL scanner (leader-only)
   ↓
Delete Request → GC (ObjectKeeper → Artifacts)
```

---

## Антипаттерны (запрещено)

### Ownership

- ❌ **OwnerReference от Request напрямую к артефактам**
  - Правильно: Request → ObjectKeeper → Artifacts
  - Неправильно: Request → Artifacts

- ❌ **Finalizer вместо GC**
  - Правильно: стандартный Kubernetes GC через ownerRef
  - Неправильно: кастомные finalizers для управления жизненным циклом

### Retry и state machine

- ❌ **Retry state machine внутри reconcile**
  - Правильно: ошибка → Ready=False (terminal)
  - Неправильно: попытки "исправить" или повторить операцию

- ❌ **Пересоздание артефактов после ошибки**
  - Правильно: транслировать ошибку в Ready=False
  - Неправильно: удалять и пересоздавать артефакты

### Архитектурные нарушения

- ❌ **Использование VolumeSnapshot в VCR**
  - Правильно: VCR создаёт VSC напрямую
  - Неправильно: создание VolumeSnapshot как промежуточного объекта

- ❌ **Patch Status из внешних контроллеров**
  - Правильно: внешние контроллеры обновляют статусы своих объектов (PVC/PV/VSC)
  - Неправильно: external-provisioner обновляет VRR.status напрямую

- ❌ **Использование пустого UID в OwnerReference**
  - Правильно: UID barrier после Create() → Get() → проверка → requeue если пустой
  - Неправильно: использование objectKeeper.UID сразу после Create()

### TTL и cleanup

- ❌ **TTL логика в reconcile**
  - Правильно: отдельный TTL scanner (leader-only)
  - Неправильно: проверка TTL в reconcile loop

- ❌ **Использование annotation TTL для timing**
  - Правильно: TTL из конфига контроллера, annotation только informational
  - Неправильно: чтение TTL из annotation для принятия решений

---

## Общие моменты реализации

### 1. Структура контроллера

Все контроллеры используют одинаковую структуру:

```go
type Controller struct {
    client.Client
    APIReader client.Reader // Required dependency
    Scheme    *runtime.Scheme
    Config    *config.Options
    // + специфичные поля (Logger и т.д.)
}
```

### 2. Инициализация контроллера

Все контроллеры валидируют зависимости при создании:

```go
func AddControllerToManager(mgr ctrl.Manager, cfg *config.Options) error {
    apiReader := mgr.GetAPIReader()
    if apiReader == nil {
        return fmt.Errorf("APIReader must not be nil: controllers require APIReader")
    }
    // ... создание контроллера с валидацией зависимостей
}
```

Или через конструктор с валидацией:

```go
func NewController(
    client client.Client,
    apiReader client.Reader,
    scheme *runtime.Scheme,
    cfg *config.Options,
) (*Controller, error) {
    if apiReader == nil {
        return nil, fmt.Errorf("APIReader must not be nil")
    }
    // ... валидация всех зависимостей
    return &Controller{...}, nil
}
```

### 3. TTL Scanner паттерн

Все контроллеры используют идентичный паттерн:
- Leader-only через `manager.RunnableFunc`
- Запуск в `Add*ControllerToManager`
- Graceful shutdown через context cancellation
- Игнорирование annotation TTL, использование конфига

### 4. Терминальное состояние

Все контроллеры проверяют Ready одинаково:
- Проверка наличия Ready condition с финальным статусом (True или False)
- Skip reconcile после терминального состояния (Ready=True или Ready=False)
- TTL cleanup вынесен в scanner
- Константы условий определены в `api/v1alpha1/conditions.go` и используются через префикс `storagev1alpha1.`

### 5. ObjectKeeper создание

Все контроллеры создают ObjectKeeper одинаково:
- Режим `FollowObject`
- Нет TTL у ObjectKeeper
- UID barrier после создания (разные подходы, но одинаковый принцип)

### 6. Conditions — только Ready, константы в API пакете

Все контроллеры используют одинаковый подход к conditions:
- **Только один condition:** `Ready` (см. разделы 2 и 3)
- **Константы определены в API пакете:**
  - `storage-foundation`: `github.com/deckhouse/storage-foundation/api/v1alpha1/conditions.go`
  - `state-snapshotter`: `github.com/deckhouse/state-snapshotter/api/v1alpha1/conditions.go`
- **Использование:** Все константы используются через префикс `storagev1alpha1.`
- **Запрещено:** Использование захардкоженных строк вместо констант

### 7. Single-writer contract для статусов Request-ресурсов

**Критически важно:** Статусы Request-ресурсов (VCR, VRR, MCR) управляются **исключительно** их контроллерами.

#### Правило

- **Единственный writer:** Только контроллер Request-ресурса может обновлять его Status
- **Внешние контроллеры:** Не должны обновлять Status Request-ресурсов
- **Коммуникация:** Внешние контроллеры должны использовать статусы своих объектов (PVC/PV/VSC) или Kubernetes events

#### Примеры

**Правильно:**
- `VolumeRestoreRequestController` обновляет VRR.status на основе наблюдения за PVC/PV
- `external-provisioner` создаёт PVC/PV и обновляет их статусы
- `VolumeRestoreRequestController` читает PVC.status и транслирует в VRR.status

**Неправильно:**
- `external-provisioner` обновляет VRR.status напрямую (запрещено)
- Любой другой контроллер обновляет VCR.status (запрещено)

#### Зачем это нужно

- Предотвращение race conditions
- Предсказуемое поведение
- Единая точка ответственности за статус
- Упрощение отладки

#### Реализация

Если внешний контроллер (например, external-provisioner) должен сообщить об ошибке:
1. Обновить статус своего объекта (PVC/PV)
2. Использовать Kubernetes events
3. Request-контроллер наблюдает за этими изменениями и транслирует в свой Status
