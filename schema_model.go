package loom

import "github.com/loomagent/loom/internal/schema"

// Schema is the JSON Schema model Loom builds and validates against. See the
// internal/schema package for the definition; it is aliased here so callers can
// build and inspect schemas without importing an internal package.
type Schema = schema.Schema

// boolPtr returns a pointer to v, for the boolean schema keywords.
func boolPtr(v bool) *bool { return &v }
