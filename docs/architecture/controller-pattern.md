# Deckhouse Fire-and-Forget Operation Controller Pattern

Этот документ описывает общие архитектурные паттерны, используемые в контроллерах Deckhouse Storage:
- `state-snapshotter` (ManifestCaptureRequest, ManifestCheckpoint)
- `storage-foundation` (VolumeCaptureRequest, VolumeRestoreRequest)

## Название паттерна

**Deckhouse Fire-and-Forget Operation Controller**

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

✅ **storage-foundation**: `VolumeCaptureRequestController`, `VolumeRestoreRequestController`
- Создают VSC/PVC, ждут CSI
- Не ретрают операции
- Транслируют статусы внешних объектов

✅ **state-snapshotter**: `ManifestCheckpointController`
- Создает ObjectKeeper, ManifestCheckpoint
- Ждёт внешние контроллеры
- Агрегирует состояние в Status

**Файлы:**
- `images/controller/internal/controllers/volumecapturerequest_controller.go`
- `images/controller/internal/controllers/volumerestorerequest_controller.go`
- `images/state-snapshotter-controller/internal/controllers/manifestcheckpoint_controller.go`

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

✅ **storage-foundation**: 
- VCR/VRR после Ready не пересоздаются
- Проверка `isConditionTrue(vcr.Status.Conditions, ConditionTypeReady)` → skip reconcile
- `images/controller/internal/controllers/volumecapturerequest_controller.go:112-116`

✅ **state-snapshotter**:
- MCR после Ready не пересоздаются
- Проверка `readyCondition.Status == metav1.ConditionTrue` → skip reconcile
- `images/state-snapshotter-controller/internal/controllers/manifestcheckpoint_controller.go:103`

---

## 3. Терминальное состояние — источник истины

### Общее

Статус → `Ready=True | Ready=False`

После этого:
- `ObservedGeneration` фиксируется
- reconcile больше ничего не делает

**Важно:** Используется только один condition — `Ready`:
- `Ready=True` — успешное завершение операции
- `Ready=False` — финальная ошибка (с соответствующим `Reason`)
- Condition выставляется только в финальном состоянии

### Ошибка = терминальная

Если внешний объект (VSC / Job / Pod / Checkpoint) сказал Error, контроллер транслирует в `Ready=False` с соответствующим `Reason`, а не интерпретирует.

### Реализация

✅ **storage-foundation**:
- `ObservedGeneration` фиксируется при `Ready=True` или `Ready=False`
- `images/controller/internal/controllers/volumecapturerequest_controller.go:112-123`
- Ошибки VSC транслируются в `Ready=False` с соответствующим `Reason`
- Константы условий: `storagev1alpha1.ConditionTypeReady`, `storagev1alpha1.ConditionReason*` из `api/v1alpha1/conditions.go`

✅ **state-snapshotter**:
- `ObservedGeneration` фиксируется при `Ready=True` или `Ready=False`
- Ошибки внешних объектов транслируются в `Ready=False` с соответствующим `Reason`
- Константы условий: `storagev1alpha1.ConditionTypeReady`, `storagev1alpha1.ConditionReason*` из `api/v1alpha1/conditions.go`

---

## 4. Разделение ролей владения (ownership split)

### Общее

| Роль | Кто |
|------|-----|
| Инициатор | VCR / MCR |
| Владение артефактами | ObjectKeeper |
| Исполнение | CSI / snapshot-controller / kubelet |
| Очистка | kube-GC |

### Инварианты

- Request не владеет артефактами
- ObjectKeeper — единственный controller owner
- GC всегда стандартный, без финалайзеров

### Реализация

✅ **storage-foundation**:
- VCR не владеет VSC/PV напрямую
- ObjectKeeper — controller owner VSC/PV
- `images/controller/internal/controllers/volumecapturerequest_controller.go:388-396`

✅ **state-snapshotter**:
- MCR не владеет ManifestCheckpoint напрямую
- ObjectKeeper — controller owner ManifestCheckpoint
- `images/state-snapshotter-controller/internal/controllers/manifestcheckpoint_controller.go:556-558`

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

✅ **storage-foundation**:
- ObjectKeeper создается с `FollowObject` режимом
- `images/controller/internal/controllers/volumecapturerequest_controller.go:320-340`
- Нет TTL у ObjectKeeper

✅ **state-snapshotter**:
- ObjectKeeper создается с `FollowObject` режимом
- `images/state-snapshotter-controller/internal/controllers/manifestcheckpoint_controller.go:442-494`
- Нет TTL у ObjectKeeper

---

## 6. ObjectKeeper UID — обязательный барьер после создания

### Критически важно

**После создания ObjectKeeper его UID должен быть гарантированно получен перед использованием в OwnerReference.**

### Проблема

После `Create()` объект может быть создан в API-сервере, но:
- `objectKeeper.UID` может быть пустым в локальном объекте
- controller-runtime не всегда заполняет UID сразу после Create()
- использование пустого UID в OwnerReference приведёт к ошибке

### Решение

Два допустимых подхода:

#### Подход A: Requeue (storage-foundation)

```go
if err := r.Create(ctx, objectKeeper); err != nil {
    return ctrl.Result{}, fmt.Errorf("failed to create ObjectKeeper: %w", err)
}

// HARD BARRIER: UID must exist before creating VSC with OwnerReference
if objectKeeper.UID == "" {
    l.Info("ObjectKeeper UID not yet populated, requeueing", "name", retainerName)
    return ctrl.Result{Requeue: true}, nil
}

// UID гарантированно заполнен, можно использовать
vsc.OwnerReferences = []metav1.OwnerReference{{
    UID: objectKeeper.UID,
    // ...
}}
```

#### Подход B: APIReader (state-snapshotter)

```go
if err := r.Create(ctx, objectKeeper); err != nil {
    return ctrl.Result{}, fmt.Errorf("failed to create ObjectKeeper: %w", err)
}

// After Create(), controller-runtime populates objectKeeper.UID automatically
// Use it directly if available, otherwise read via APIReader (bypasses cache)
if objectKeeper.UID == "" {
    // UID not populated - read directly from API server
    if err := r.APIReader.Get(ctx, client.ObjectKey{Name: retainerName}, objectKeeper); err != nil {
        return ctrl.Result{RequeueAfter: 200 * time.Millisecond}, nil
    }
}

// UID гарантированно заполнен, можно использовать
```

### Требования

1. **Обязательно:** Проверка `objectKeeper.UID == ""` после Create()
2. **Обязательно:** Получение UID перед использованием в OwnerReference
3. **Запрещено:** Использование пустого UID в OwnerReference (приведёт к ошибке)

### Почему это важно

- OwnerReference с пустым UID не работает
- Kubernetes GC игнорирует ownerRef без правильного UID
- Это может привести к утечкам ресурсов

### Реализация

✅ **storage-foundation**: Подход A (Requeue)
- `images/controller/internal/controllers/volumecapturerequest_controller.go:345-352`
- `images/controller/internal/controllers/volumecapturerequest_controller.go:736-743`

✅ **state-snapshotter**: Подход B (APIReader)
- `images/state-snapshotter-controller/internal/controllers/manifestcheckpoint_controller.go:497-511`

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

✅ **storage-foundation**:
- TTL scanner: `StartTTLScanner()` в `add_reconcilers.go:51-60`
- Реализация: `images/controller/internal/controllers/volumecapturerequest_controller.go:1056-1130`
- Leader-only через `manager.RunnableFunc`
- Annotation игнорируется, используется `config.RequestTTL`

✅ **state-snapshotter**:
- TTL scanner: `StartTTLScanner()` в `add_reconcilers.go:56-65`
- Реализация: `images/state-snapshotter-controller/internal/controllers/manifestcheckpoint_controller.go:1567-1634`
- Leader-only через `manager.RunnableFunc`
- Annotation игнорируется, используется `config.DefaultTTL`

**Общее:**
- Оба используют одинаковый паттерн leader-only RunnableFunc
- Оба игнорируют annotation TTL, используют конфиг
- TTL начинается с `CompletionTimestamp`

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

✅ **storage-foundation**:
- APIReader для StorageClass: `images/controller/internal/controllers/volumecapturerequest_controller.go:268`
- APIReader для StorageClass: `images/controller/internal/controllers/volumerestorerequest_controller.go:260, 552`
- Валидация: `images/controller/internal/controllers/add_reconcilers.go:35-37, 70-72`
- Структура: `images/controller/internal/controllers/volumecapturerequest_controller.go:83`

✅ **state-snapshotter**:
- APIReader для ObjectKeeper после создания: `images/state-snapshotter-controller/internal/controllers/manifestcheckpoint_controller.go:507`
- Валидация: `images/state-snapshotter-controller/internal/controllers/add_reconcilers.go:41-43`
- Структура: `images/state-snapshotter-controller/internal/controllers/manifestcheckpoint_controller.go:76`

**Общее:**
- Оба валидируют APIReader при создании контроллера
- Оба используют APIReader напрямую без nil-проверок
- Оба имеют обязательное поле APIReader в структуре контроллера

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

✅ **storage-foundation**:
- UID barrier с requeue: `images/controller/internal/controllers/volumecapturerequest_controller.go:349-352`
- Нет ожидания статусов внешних объектов

✅ **state-snapshotter**:
- UID barrier с APIReader: `images/state-snapshotter-controller/internal/controllers/manifestcheckpoint_controller.go:504-510`
- Нет ожидания статусов внешних объектов

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

✅ **storage-foundation**:
- NotFound → Ready=False: `images/controller/internal/controllers/volumecapturerequest_controller.go:261-262`
- Forbidden → Ready=False: `images/controller/internal/controllers/volumecapturerequest_controller.go:265-266`
- Invalid config → Ready=False: `images/controller/internal/controllers/volumecapturerequest_controller.go:274-277`
- Temporary status → Requeue: используется в различных местах
- Константы: `storagev1alpha1.ConditionReasonNotFound`, `storagev1alpha1.ConditionReasonRBACDenied`, и т.д. из `api/v1alpha1/conditions.go`

✅ **state-snapshotter**:
- NotFound → Ready=False: различные места в `manifestcheckpoint_controller.go`
- Forbidden → Ready=False: различные места
- Invalid config → Ready=False: различные места
- Temporary status → Requeue: используется в различных местах
- Константы: `storagev1alpha1.ConditionReasonNotFound`, `storagev1alpha1.ConditionReasonInternalError`, и т.д. из `api/v1alpha1/conditions.go`

---

## 11. Минимальный reconcile

### Общее

- нет циклов ожидания внутри reconcile
- нет `for {}` / `sleep`
- всегда быстрый выход

### Реализация

✅ **storage-foundation**:
- Все reconcile функции возвращают быстро
- Нет циклов ожидания
- `images/controller/internal/controllers/volumecapturerequest_controller.go:82-1207`

✅ **state-snapshotter**:
- Все reconcile функции возвращают быстро
- Нет циклов ожидания
- `images/state-snapshotter-controller/internal/controllers/manifestcheckpoint_controller.go:57-1634`

---

## Краткая формула (чтобы «запомнить»)

```
Request — это декларация операции
ObjectKeeper — это владение
Контроллер — это оркестратор
GC — это уборщик
TTL — это политика, а не логика
UID — это обязательный барьер после Create()
```

---

## Общие моменты реализации

### 1. Структура контроллера

Оба проекта используют одинаковую структуру:

```go
type Controller struct {
    client.Client
    APIReader client.Reader // Required dependency
    Scheme    *runtime.Scheme
    Config    *config.Options
    // + специфичные поля (Logger для state-snapshotter)
}
```

### 2. Инициализация контроллера

Оба проекта валидируют APIReader одинаково:

```go
func AddControllerToManager(mgr ctrl.Manager, cfg *config.Options) error {
    apiReader := mgr.GetAPIReader()
    if apiReader == nil {
        return fmt.Errorf("APIReader must not be nil: controllers require APIReader")
    }
    // ...
}
```

### 3. TTL Scanner паттерн

Оба проекта используют идентичный паттерн:
- Leader-only через `manager.RunnableFunc`
- Запуск в `Add*ControllerToManager`
- Graceful shutdown через context cancellation
- Игнорирование annotation TTL, использование конфига

### 4. Терминальное состояние

Оба проекта проверяют Ready одинаково:
- Проверка `isConditionTrue(vcr.Status.Conditions, storagev1alpha1.ConditionTypeReady)` для успеха
- Проверка `isConditionFalse(vcr.Status.Conditions, storagev1alpha1.ConditionTypeReady)` для ошибки
- Или проверка `readyCondition.Status == metav1.ConditionTrue` / `readyCondition.Status == metav1.ConditionFalse`
- Skip reconcile после терминального состояния (Ready=True или Ready=False)
- TTL cleanup вынесен в scanner
- Константы условий определены в `api/v1alpha1/conditions.go` и используются через префикс `storagev1alpha1.`

### 5. ObjectKeeper создание

Оба проекта создают ObjectKeeper одинаково:
- Режим `FollowObject`
- Нет TTL у ObjectKeeper
- UID barrier после создания (разные подходы, но одинаковый принцип)

### 6. Conditions — только Ready, константы в API пакете

Оба проекта используют одинаковый подход к conditions:
- **Только один condition:** `Ready`
  - `Ready=True` — успешное завершение операции
  - `Ready=False` — финальная ошибка (с соответствующим `Reason`)
- **Condition выставляется только в финальном состоянии** (терминальный успех или терминальная ошибка)
- **Константы определены в API пакете:**
  - `storage-foundation`: `github.com/deckhouse/storage-foundation/api/v1alpha1/conditions.go`
  - `state-snapshotter`: `github.com/deckhouse/state-snapshotter/api/v1alpha1/conditions.go`
- **Использование:** Все константы используются через префикс `storagev1alpha1.`:
  - `storagev1alpha1.ConditionTypeReady`
  - `storagev1alpha1.ConditionReasonCompleted`
  - `storagev1alpha1.ConditionReasonInternalError`
  - и т.д.
- **Запрещено:** Использование захардкоженных строк вместо констант
