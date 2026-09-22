# Format fixtures

These files are copied verbatim from the [JSON Schema Test Suite], the `optional/format`
cases for the formats Loom's helpers cover:

```
date        time        date-time   uuid
```

Source commit: `ab079cc2bace029fdbb483be28a6ade526bcfbc2`, the same revision the
keyword fixtures under `internal/toolcontract/testdata/jsonschema` were taken from.
License: MIT (see the upstream repository).

The suite keeps these apart from the keyword tests because the specification makes `format` an
annotation: an implementation asserts it only when it opts into the format-assertion vocabulary,
and the non-optional `format.json` file asserts exactly that. Loom follows the specification
there — `ValidateSchema` does not assert a format — and asserts these formats one layer up, in
the contract, where `Date`, `Time`, `DateTime`, and `UUID` attach the check.
`TestFormatCases` in the root package runs them.

Non-string instances are skipped: the specification's cases show that a format does not constrain
a non-string, and a contract says so with `type: string` before any format check runs.

[JSON Schema Test Suite]: https://github.com/json-schema-org/JSON-Schema-Test-Suite
