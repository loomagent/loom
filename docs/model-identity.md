# Model identity and snapshot references

`ChatModel` keeps its existing required methods. The optional `ModelInfoProvider`
interface and `DescribeModel(model)` expose a value snapshot of the configured
request target:

```go
model, err := modelfactory.Build(modelfactory.Config{
    Provider: modelfactory.ProviderOpenRouter,
    APIKey: apiKey,
    Model: "provider/alias",
    Ref: loom.ModelRef{InstanceID: instanceID, SnapshotID: persistedSnapshotID},
})
if err != nil { return err }
info := loom.DescribeModel(model)
// info.Provider: "openrouter" (the adapter)
// info.RequestModel: "provider/alias" (the configured request target)
// info.Ref: the exact opaque reference supplied above
```

All four built-in chat providers accept `Config.Ref`; `modelfactory.Build` and
`Factory.Build` pass it through. Provider and request model are derived from the
actual constructor, including DeepSeek's resolved default. They are never derived
from a response model, display name or a centralized model list. `Capabilities()`
remains the sole capability contract.

Loom does not allocate, persist, validate existence of, or resolve host references.
A host must only attach a SnapshotID when it has the corresponding immutable
configuration record, and must retain it for as long as audit records refer to it.
Never put secrets or credential fingerprints in these public references. Local
configuration identity does not pin an alias to a fixed remote model version.

Without a supplied Ref, a built-in provider still identifies its adapter and
requested model, but its host identity stays empty. A custom model without
`ModelInfoProvider` returns zero information, even if its Name looks like a known
provider. A wrapper must explicitly forward `ModelInfo()` if it preserves the
underlying binding; embedding `ChatModel` alone does not forward optional methods.
No generic wrapper is inserted by this feature.

## Call and attempt evidence

Synchronous, research streaming and final-answer streaming helpers capture
ModelInfo before invoking the selected model. `LLMCalledEvent.ModelInfo` contains
that captured target. `ProviderReportedModel` separately records the provider's
response identifier; absence remains empty. The existing `Model` display field
and usage accounting keep their existing behavior. Failover events capture the
selected model's own reference, not the primary's reference.

`FailoverAttempt.ModelInfo` also describes failed model invocations with no usage.
At the physical-request seam, `AttemptMeta.ModelInfo` is fixed by each provider at
construction and reaches `AttemptLimiter.Acquire` on every Loom retry, independently
of the private quota key. Caller retry configuration is copied before binding it.
The Ark SDK's internal retries are disabled so that every HTTP attempt passes
through Loom's retry/admission layer.

ModelInfo itself does not create a lifecycle audit log. The optional
[call lifecycle](call-lifecycle.md) supplies CallID/InvocationID/AttemptID and
mandatory observer error propagation; hosts still own durable storage.
LLMCalled remains a usage event and is not emitted for unmetered failures. A host
requiring durable audit must persist call starts, failed attempts and snapshot
references through its own lifecycle/persistence boundary, including persistence
failure and fencing rules. Do not claim complete auditing from usage events alone.

## Probe evidence

`modelprobe.Options.TargetRef` supplies an explicit host target. It is copied before
Builder runs and retained as `Report.TargetRef`, including a partial measured
report. Known conflicting bindings are rejected before requests: two diagnostic
models with the same Name but different refs are not interchangeable, and known
refs must match an explicit TargetRef. A builder without optional metadata relies
on the host's explicit assertion; this is not independent verification of binding.
Without an explicit TargetRef, no binding is inferred.

`Report.StartedAt` and `FinishedAt` bound a measured probe invocation, including
partial results. Builder/validation failures that produce no measured report
remain empty. Older serialized reports keep unknown binding and times; they are
not backfilled. A bound report records the business target supplied by the host,
not the synthetic probe capabilities, and does not automatically rewrite any
administrator declaration.
