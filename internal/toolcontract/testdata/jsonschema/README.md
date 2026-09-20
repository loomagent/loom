# JSON Schema Test Suite fixtures

These files are copied verbatim from the [JSON Schema Test Suite], the subset of
`tests/draft2020-12` for the keywords Loom implements:

```
additionalProperties  const          enum             exclusiveMaximum
exclusiveMinimum      items          maximum          maxItems
maxLength             minimum        minItems         minLength
pattern               properties     required         type
uniqueItems
```

Source commit: `ab079cc2bace029fdbb483be28a6ade526bcfbc2`.
License: MIT (see the upstream repository).

[`TestJSONSchemaSuiteSubset`](../suite_test.go) runs the cases whose schema
decodes into Loom's model and skips the rest, so these files also record what
Loom's subset deliberately does not cover.

[JSON Schema Test Suite]: https://github.com/json-schema-org/JSON-Schema-Test-Suite
