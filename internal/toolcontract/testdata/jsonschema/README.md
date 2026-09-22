# JSON Schema Test Suite fixtures

These files are copied verbatim from the [JSON Schema Test Suite], the subset of
`tests/draft2020-12` for the keywords Loom implements:

```
additionalProperties  allOf          const            enum
exclusiveMaximum      exclusiveMinimum items           maximum
maxItems              maxLength      minimum          minItems
minLength             pattern        properties       required
type                  uniqueItems
```

Source commit: `ab079cc2bace029fdbb483be28a6ade526bcfbc2`.
License: MIT (see the upstream repository).

[`TestJSONSchemaSuiteSubset`](../suite_test.go) runs the cases whose schema
decodes into Loom's model and skips the rest, so these files also record what
Loom's subset deliberately does not cover.

[JSON Schema Test Suite]: https://github.com/json-schema-org/JSON-Schema-Test-Suite

## What the skipped cases are

`TestJSONSchemaSuiteSubset` skips every group whose schema does not decode into Loom's model, and
those skips are three different things. The counts are asserted there, so a change to any of them
has to update this section.

- **`unmodeledKeyword` (18)** — Loom refuses a keyword it does not model, with an error. The
  specification says unknown keywords should be treated as annotations, so this is a deliberate
  policy rather than a gap: a contract that advertises a constraint must be able to enforce it.
  It covers `$defs`, `$comment`, `dependentRequired`/`dependentSchemas`, `patternProperties`,
  `propertyNames`, `prefixItems`, and `multipleOf`.
- **`keywordSpelling` (4)** — Loom requires a keyword value the specification defines as an
  integer to be written as one, so `minLength: 2.0` is refused although the specification defines
  `2.0` as an integer. An *instance* is matched by value instead: `5.0` is read as 5.
- **`unimplementedConstruct` (14)** — a construct Loom does not implement, so a schema using it
  cannot be expressed: a boolean schema (`true`/`false`), a `type` union, and a schema-valued
  `additionalProperties`. These are gaps, not policies, and implementing one turns its groups on.
