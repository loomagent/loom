# ReAct host integration

The public `react` package owns the loop, model/tool history, budget reservations,
terminal-tool isolation, irreversible finalization, and final-answer delivery.
Hosts own persistence, provider admission, business review, prompts and tool
concurrency. Host adapters must not implement a second iteration loop.

## Extension contracts

- `CallModel` replaces non-delivery calls only (including buffered final rounds).
  `RunToFinalAnswer` always delivers its final round through Loom's streaming
  writer; the host cannot replace it with a buffered call or replay it on failure.
- `PrepareModelCallContext` runs once per iteration after all step policies and
  final instructions, before either `CallModel` or streamed final delivery.
  Its value-only `ModelCallInfo` contains the zero-based step, span purpose,
  settled final-phase flag and finalization reason. Return a child context to
  attach request metadata, or an error to stop before the model is called. A nil
  context is rejected. Policies, tools and later iterations retain the original
  run context; provider retries and fallback stay inside the prepared call.
  This hook does not capture the provider's final wire request or count tokens.
- `ExecuteTools` receives only allowed, budget-reserved calls and the current
  registry subset. It may schedule concurrently, but returns exactly one result
  per call in input order. The core validates result identity before history use.
- `MaxTokens` applies to research and final requests alike.
- `TerminalToolName` assigns the terminal protocol to any existing registered
  tool by name. Configured and `EndsToolPhase` tools use the same state machine:
  exclusive batch, no research budget charge, transition only on success.
  Explicitly configured terminals remain available after research budget exhaustion.
  Their descriptions are replaced with the shared `loom.TerminalToolDescription`.
  The shared registry is unchanged; naming is local to each run.
- `AfterToolsDecision.Stop` retains buffered early-return semantics for business
  pipelines. Streaming delivery always enters a final model round.
- Existing step policies may keep schemas stable for prompt cache reuse. Actual
  execution is checked against the current visible registry and budget.

## Acceptance scenarios (defined before implementation)

1. user receives final reasoning and content before provider EOF after either a
   terminal tool or an accepted natural draft, even with a custom research caller.
2. user cannot execute a hidden tool or exceed a budget through a custom parallel
   scheduler; tool results preserve call identity and order.
3. user can choose any terminal name, including a report-specific name. Mixed
   batches execute nothing; a failed terminal stays retryable. One successful
   terminal ends tools and enters a tool-free completion.
4. user gets an explicit failure for malformed scheduler results, sink failure or
   provider failure; cancellation never becomes a successful answer.
5. user retains backend review, fallback reasoning configuration, dated context,
   budget feedback, per-provider concurrency and bounded no-progress behavior.

No paid provider calls are needed: use protocol fakes, existing real HTTP provider
contract tests and the backend PostgreSQL streaming integration test.

## Internal helpers and research budgets

Register trusted internal helpers with `loom.WithBudgetExempt()`. The default is
budgeted, even for offline tools; networking and budget classification are
independent. The marker bypasses `MaxToolCalls`, `ToolCallLimits` and the new
`MaxToolRounds` research budget, but never terminal isolation or final closure.

`MaxToolRounds` counts a batch once when it reserves at least one budgeted tool.
Mixed batches count only their budgeted calls; failed executions keep their
reservations. Internal-only and entirely refused batches consume no research
round. `State.ToolRoundsUsed` reports this counter separately from physical
`State.Step`. When research budgets run out, helpers and terminals stay available
until a normal finish, terminal success, policy stop or safety boundary.

Set `MaxSteps` independently to bound all model rounds, including internal-only
loops and refused finishes. It preserves its existing semantics and allows one
final tool-free call. Deadlines, cancellation and consecutive refusal limits are
unchanged. Neither budget exemption nor tool-round counting changes actual
provider usage or accounting. Host schedulers receive only admitted calls and
must place persistent reservations before invoking those calls, rather than
charging tool-call diagnostic events that also represent rejected requests.
