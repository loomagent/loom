package loom

// Schema is the JSON Schema model Loom builds, sends to providers, and validates
// against. It carries only the keywords the declared-argument builder emits,
// which keeps the model — and the validator that walks it — a closed, testable
// subset instead of a general JSON Schema implementation.
//
// A schema is a plain value: build it as a struct literal and marshal it with
// encoding/json/v2. The zero value is an empty schema, which accepts anything.
type Schema struct {
	// Identity and dialect. Loom does not emit these, but a caller may.
	ID     string `json:"$id,omitempty"`
	Schema string `json:"$schema,omitempty"`

	// Reference keywords. Loom's builder emits no references, but a caller may
	// hand Loom a schema that uses them, and the validator resolves them.
	Ref  string             `json:"$ref,omitempty"`
	Defs map[string]*Schema `json:"$defs,omitempty"`

	// type is a single type name. Loom never emits a type union, so there is no
	// Types field; every schema it builds has at most one type.
	Type string `json:"type,omitempty"`

	// Object keywords.
	Properties map[string]*Schema `json:"properties,omitempty"`
	// PropertyOrder preserves the author's declaration order for diagnostics.
	// It is not part of the JSON representation.
	PropertyOrder []string `json:"-"`
	Required      []string `json:"required,omitempty"`
	// AdditionalProperties is a boolean. Loom sets false; nil means unset, so
	// the keyword is omitted entirely.
	AdditionalProperties *bool `json:"additionalProperties,omitempty"`

	// Array keywords.
	Items *Schema `json:"items,omitempty"`

	// Value keywords.
	Enum             []any    `json:"enum,omitempty"`
	Const            *any     `json:"const,omitempty"`
	Format           string   `json:"format,omitempty"`
	Pattern          string   `json:"pattern,omitempty"`
	Minimum          *float64 `json:"minimum,omitempty"`
	Maximum          *float64 `json:"maximum,omitempty"`
	ExclusiveMinimum *float64 `json:"exclusiveMinimum,omitempty"`
	ExclusiveMaximum *float64 `json:"exclusiveMaximum,omitempty"`
	MinLength        *int     `json:"minLength,omitempty"`
	MaxLength        *int     `json:"maxLength,omitempty"`
	MinItems         *int     `json:"minItems,omitempty"`
	MaxItems         *int     `json:"maxItems,omitempty"`
	UniqueItems      bool     `json:"uniqueItems,omitzero"`
	MinProperties    *int     `json:"minProperties,omitempty"`
	MaxProperties    *int     `json:"maxProperties,omitempty"`
	Description      string   `json:"description,omitempty"`
	Examples         []any    `json:"examples,omitempty"`

	// Applicator keywords. Loom's builder does not emit these, but diagnostics
	// and hand-built schemas may carry them.
	Not   *Schema   `json:"not,omitempty"`
	AllOf []*Schema `json:"allOf,omitempty"`
	AnyOf []*Schema `json:"anyOf,omitempty"`
	OneOf []*Schema `json:"oneOf,omitempty"`
}

// boolPtr returns a pointer to v, for the boolean schema keywords.
func boolPtr(v bool) *bool { return &v }
