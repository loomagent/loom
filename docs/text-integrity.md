# Observing output text integrity

Loom can report suspected encoding damage without rejecting, repairing, retrying,
or replacing output. Enable the observer once at process startup:

```go
loom.SetTextIntegrityObserver(func(ctx context.Context, event loom.TextIntegrityObservation) {
    // Record the value snapshot or enqueue it for bounded, asynchronous delivery.
    // This function must return promptly and support concurrent calls.
})
defer loom.SetTextIntegrityObserver(nil)
```

`WithTextIntegrityObserver(ctx, observer)` overrides the process observer for a
context. Passing nil explicitly disables diagnostics for that context. With no
observer, Loom does not collect stream content for this feature.

One detector receives text from two existing boundaries:

| Boundary | Inspected fields | Source |
| --- | --- | --- |
| `ObserveChat` / `ObserveStream` | Content, reasoning content, structured reasoning details, tool call names, IDs, and arguments | `model` |
| Finished runtime item | Text, labels, tool arguments/output, tool names/IDs, and item error code/message | `runtime_item` or `tool_result` |

Built-in providers and the model helpers use the model boundary. A custom model
called directly must use `ObserveChat` / `ObserveStream` to participate. Finished
items also cover text from host code or custom tool schedulers. The same content
can therefore produce both a model observation and a runtime item observation;
`Source` distinguishes them. Nested model wrappers share one capture per
invocation, while concurrent calls retain separate captures.

The detector reports:

- Invalid UTF-8 bytes.
- The Unicode replacement character U+FFFD.
- UTF-8 decoded as CP1252/Latin-1 (`cp1252_mojibake`). Each rune is mapped back
  to the byte such a decoder consumed (U+FFFD stands for one of the five bytes
  CP1252 leaves undefined), and the bytes must re-form well-formed UTF-8.
  Evidence is a sequence that restores a three- or four-byte character (CJK,
  emoji, punctuation such as `’` or `→`) or a Latin-1 letter (`Ã©`). Other
  two-byte sequences are not evidence, because an accented letter followed by
  CP1252 punctuation is normal text (`“FETÖ”`, `Sévaré–Konna`). Two pieces of
  evidence report a finding; one does when every non-ASCII rune of the string
  belongs to a well-formed sequence, which covers a single damaged character
  or emoji (`äºº`, `ðŸ‘€`).

Valid JSON is decoded for inspection of keys and string values, including escaped
characters such as `\ufffd`. Malformed JSON is inspected as text; it does not
become a validation error. JSON field paths identify the decoded string, and
`ByteOffset` is relative to that string. Each field/kind is reported once per
response or item. Normal multilingual text is accepted unchanged. Question marks,
unfamiliar scripts, and arbitrary unreadable text are not reliable encoding clues.

Streaming text and tool argument deltas are assembled per channel and tool index
before inspection. This requires an additional in-memory copy proportional to
the streamed text while diagnostics are enabled. Structured reasoning portions
are inspected individually. Findings are emitted at EOF, a receive error, or
Close. A stream closed before EOF has `Partial=true` unless a frame with a
finish reason was received. An incomplete UTF-8 suffix on an
interrupted stream is ignored. UTF-8 and JSON escape sequences split across
chunks do not generate premature findings. Consumers must close abandoned
streams, as required by the normal Stream contract.

The event carries model invocation metadata, tool identity, or runtime item
identity when available, plus an excerpt of at most 64 runes. It carries no
mutable response pointer. Observer panics are contained. HTTP delivery, timeouts,
queue limits, alert throttling, and log retention belong to the host application;
Loom has no webhook dependency. Do not send webhooks synchronously in the observer.

A finding is a suspicion: a model can quote already-damaged source text or
intentionally discuss U+FFFD. These diagnostics do not attribute the corruption
to the model or prove an upstream decoding error. Image/audio/video bytes are not
text and are not scanned; future textual transcripts or captions should enter
the same detector when Loom gains those fields.

The separation of model and tool boundaries follows the approach in
[tRPC-Agent-Go model callbacks](https://github.com/trpc-group/trpc-agent-go/blob/90b8c46f000e75b254a1e35abc11b46f876c64c5/model/callbacks.go)
and [tool callbacks](https://github.com/trpc-group/trpc-agent-go/blob/90b8c46f000e75b254a1e35abc11b46f876c64c5/tool/callbacks.go).
Loom's diagnostic callback deliberately returns no replacement or error, keeping
this feature separate from control-flow callbacks and mandatory lifecycle audits.
