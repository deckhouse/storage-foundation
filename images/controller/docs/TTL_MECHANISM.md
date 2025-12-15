# TTL Mechanism for VCR/VRR

## Overview

TTL (Time-To-Live) is a mechanism that automatically deletes completed one-time request resources (VolumeCaptureRequest and VolumeRestoreRequest) after a specified duration following their completion.

**Important**: TTL applies only to request resources. Artifacts (VolumeSnapshotContent, PersistentVolume) and IRetainer resources have their own independent TTL policies, unrelated to request TTL.

## TTL Storage

TTL is stored as an annotation on the object:

```yaml
metadata:
  annotations:
    storage.deckhouse.io/ttl: "10m"
```

### TTL Value Source

The TTL annotation value comes from:
- Module configuration: `REQUEST_TTL` environment variable
- Default: `"10m"` (10 minutes)

The annotation is set uniformly when transitioning to Ready/Failed state.

## When TTL is Set

TTL annotation must be set:

**Always on any final state:**
- `Condition Ready=True` → successful completion
- `Condition Ready=False` → error (Failed)

The TTL is set **first time** when the request completes.

**Behavior:**
- If annotation already exists → do not overwrite (idempotent)
- If not exists → create with TTL from configuration

## TTL Countdown Logic

TTL starts counting from:

```
status.completionTimestamp
```

`CompletionTimestamp` is mandatory when Ready/Failed is set.

TTL calculation:

```
expiration = completionTimestamp + TTL
```

## When Resource is Deleted

The controller deletes the object only if:

1. TTL annotation exists
2. `CompletionTimestamp` exists
3. Current time > expiration

**Deletion is performed before any Reconcile logic.**

## Behavior When TTL Not Expired

On each Reconcile, if TTL has not expired, the controller must:

**Return RequeueAfter** to come back later.

**Rules:**
- `requeue = min(timeLeft, 1 minute)`
- But not less than 30 seconds
- Add jitter ±10% (to avoid reconcile storms)

## Behavior on Invalid TTL

If TTL annotation is invalid (e.g., "10minutes"):

1. Controller sets:
   - `Condition Ready=False`
   - `Reason: InvalidTTL`
   - `Message: "Invalid TTL annotation format: X (error: Y)"`

2. Object is **not** automatically deleted

3. Reconcile does **not** loop (no RequeueAfter)

4. TTL annotation remains unchanged

## Final States

Final states are:

| State | Conditions |
|-------|------------|
| Ready | `Condition Ready=True` |
| Failed | `Condition Ready=False` and `Reason != Pending/Running` |
| InvalidTTL | `Condition Ready=False` and `Reason=InvalidTTL` |

TTL is applied uniformly to all these states.

## Unified Reconcile Cycle

At the start of Reconcile:

```go
obj := get()

// 1. If final state — check TTL
if isReady(obj) OR isFailed(obj):
    if shouldDelete(obj):
        delete(obj)
        return
    if ttlNotExpired:
        return RequeueAfter
```

If object is not completed, TTL is not applied, and controller continues normal execution.

## Relationship with IRetainer TTL

**Important**: Request TTL and IRetainer TTL are **independent**:

- **VCR/VRR TTL** (10m): Controls when the request resource is deleted (short-lived, cleanup API noise)
- **IRetainer TTL** (24h+): Controls when artifacts (VSC/PV) are garbage collected (long-lived, retention policy)

After VCR/VRR deletion, IRetainer continues to live and guard artifacts until its own TTL expires.

## Implementation Details

### TTL Annotation Key

```go
const AnnotationKeyTTL = "storage.deckhouse.io/ttl"
```

### Configuration

```go
// Default TTL for request resources
const DefaultRequestTTL = 10 * time.Minute
const DefaultRequestTTLStr = "10m"

// Environment variable
const RequestTTLEnvName = "REQUEST_TTL"
```

### Functions

- `setTTLAnnotation(obj)` - Sets TTL annotation (idempotent)
- `checkAndHandleTTL(ctx, obj)` - Checks TTL and deletes if expired, returns (shouldDelete, requeueAfter, error)

### Retry on Conflict

All TTL annotation updates use `retry.RetryOnConflict` to handle concurrent updates safely.

## Testing Requirements

See [TTL Tests](../../../test/ttl_test.go) for unit tests covering:

1. ✅ TTL is written on Ready
2. ✅ TTL is written on Failed
3. ✅ TTL is not overwritten if already exists
4. ✅ TTL is not applied until CompletionTimestamp exists
5. ✅ TTL expired → object deleted
6. ✅ TTL not expired → RequeueAfter (30s-60s with jitter)
7. ✅ Invalid TTL → Condition InvalidTTL
8. ✅ TTL independent from IRetainer TTL
9. ✅ Same behavior for VCR and VRR

## See Also

- [ADR: TTL for Request Resources](../../../../docs/adr-ttl-requests.md)
- [State-Snapshotter TTL Documentation](../../../../../state-snapshotter/images/state-snapshotter-controller/docs/TTL_MECHANISM.md)
