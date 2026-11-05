# Call and attempt lifecycle

A host can persist metadata for successful, failed and interrupted model calls:

```go
ctx = loom.WithLifecycleObserver(ctx, loom.LifecycleObserverFunc(
    func(ctx context.Context, event loom.LifecycleEvent) error {
        return journal.Append(ctx, event) // never persist event.Err verbatim
    },
))
response, err := loom.CallModel(ctx, "research.review", model, request)
```

`CallModel`, `StreamLLMToStep` and `StreamLLMToFinalAnswer` create a fresh logical
CallID and purpose. Failover within CallModel retains that CallID and creates a
new InvocationID for the selected model. `ChatWithRetry`/`StreamWithRetry` create a
new AttemptID per admitted physical attempt; custom models that do not use those
helpers must not infer physical attempts from the number of logical calls.

`InvocationFromContext` reads the current selected-model identity. An HTTP callback
inside the retry helpers can read `AttemptIDFromContext`. These are opaque IDs,
not sequence numbers or provider request IDs. They never reach request parameters.

The observer receives `call_started`, `attempt_started`, `attempt_finished` and
`call_finished`. Start is recorded before model/HTTP work. A failed call-start
record prevents model work; a failed attempt-start record releases its quota
permit and prevents HTTP work. An attempt-end write failure stops retries before
provider backoff. `ObservationError` is permanent and does not trigger failover.
A host must propagate it and must not treat an accompanying response as success.

Only metadata, available usage and provider-reported model identifiers are
serializable. Err remains an in-memory classification input with `json:"-"`.
`call_started` additionally provides a transient, read-only `Request` when called
through the standard model helpers. Custom instrumentation can opt in with
`WithObservedRequest(request)` on ObserveChat/ObserveStream. A trusted observer
can synchronously compute a private fingerprint; it must not retain or persist
raw input. Request is always excluded from JSON, like Err. This is the selected
helper-level ChatRequest, including failover rebuilding, not final HTTP bytes.
Nested observers still reuse the original invocation and produce one start.
Neither prompts nor generated text are part of the serialized event. ModelInfo is captured
before invocation; response model identifiers never replace its request target.

For streaming, a normal EOF records completed, an error records failed/cancelled,
and an early Close records interrupted. One terminal record is produced; repeated
Close does not duplicate it. Terminal persistence failure is returned as an error
rather than EOF, so the final-answer helper cannot commit false success. A killed
process or an abandoned stream can leave a start without a terminal event: the
host must keep that unknown instead of manufacturing success or zero usage.

Record must be concurrent-safe, bounded, and must not reenter the same model or
stream. Cancellation of provider work does not authorize dropping audit data: a
host may use a short context.WithoutCancel cleanup deadline for terminal writes,
while still enforcing the original execution generation and lease in the same
transaction as its append. No database transaction should span network work.

A storage-bound factory may expose optional `ModelObserverProvider` to preserve
its mandatory observer through standard helpers. Transparent wrappers must forward
both it and ModelInfoProvider explicitly. A nested wrapper for the same active
model reuses the invocation, including Chat-to-Stream adaptation. Changing the
selected model creates a new invocation; nested independent calls should use a
fresh WithCallScope.

Direct calls to Chat/Stream on an uninstrumented custom/native model do not
implicitly invoke a context observer. Hosts can wrap their invocation with
ObserveChat/ObserveStream, or use a factory wrapper implementing the two optional
interfaces. Observe helpers remain no-ops for persistence when no observer is bound.

Loom supplies no database or snapshot store. The host must persist the referenced
configuration before first use, enforce ownership/fencing atomically with writes,
keep referenced snapshots through its retention policy, and provide its own
resolution and bounded query APIs. Usage events and accounting are separate from
this lifecycle and continue to omit unmetered calls.
