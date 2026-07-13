# ReAct examples

These commands progress from a minimal local tool loop to multi-turn deep
research. They intentionally use the public Loom packages directly so the
composition is visible.

## Environment

All examples use an OpenRouter chat model:

```bash
export OPENROUTER_API_KEY=...
export OPENROUTER_MODEL=...
```

The research examples also require:

```bash
export SERPER_API_KEY=...
export UNIFUNCS_API_KEY=...
```

## 1. Local tool loop

This is the smallest ReAct example. The model uses `get_time` and `calculator`:

```bash
go run ./examples/react/tool_loop
```

## 2. One-turn deep research

This combines Serper search, Unifuncs reading, calculator/time utilities, a
read-only workspace shell, `loomfs`, and stable source references:

```bash
go run ./examples/react/deep_research \
  -question "Compare current evidence for and against small modular reactor cost competitiveness before 2035" \
  -workspace ./research-workspace
```

Search results receive `SRC-N` identifiers immediately. Reading the same URL
upgrades that source with full content at `raw/SRC-N.md`; it does not allocate a
new identifier.

## 3. Multi-turn research chat

The interactive command keeps one conversation alive across turns:

```bash
go run ./examples/react/deep_research_chat -workspace ./research-chat
```

Try this sequence:

1. `Map the strongest evidence about direct air capture costs through 2030.`
2. `Now challenge the assumptions using primary sources we have not considered.`
3. `Reconcile both answers. Reuse prior evidence and tell me which SRC references changed your conclusion.`

The application retains the Loom turn history, one conversation-scoped source
registry, and one `loomfs` workspace. Later turns can inspect
`prior_turns.jsonl`, `queries.jsonl`, `sources.jsonl`,
`context/turn_contexts.jsonl`, and saved `raw/SRC-N.md` files with the read-only
`bash` tool. A normalized URL always maps to the same `SRC-N` during the running
conversation.

The example uses `MemoryStore` for source numbering, so references survive all
turns in the process but are not reconstructed after a process restart. A
production application should provide a transactional `sourceregistry.Store`;
its implementation can be validated with `sourceregistrytest.TestStore`.
