package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Schema is the JSON Schema subset tools may use. It is small on purpose:
// every provider accepts it, and it can be checked locally without a full
// JSON Schema library. A keyword outside the subset is an error when a
// schema is parsed.
type Schema struct {
	// SchemaURI is the "$schema" keyword. It is kept but has no effect.
	SchemaURI   string `json:"$schema,omitempty"`
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
	// Properties, Required and AdditionalProperties apply to an object.
	// When AdditionalProperties is nil, unlisted properties are allowed.
	Properties           map[string]*Schema `json:"properties,omitempty"`
	Required             []string           `json:"required,omitempty"`
	AdditionalProperties *bool              `json:"additionalProperties,omitempty"`
	// Items applies to an array.
	Items *Schema `json:"items,omitempty"`
	// Enum applies to a string.
	Enum []string `json:"enum,omitempty"`
	// Minimum and Maximum apply to an integer or a number.
	Minimum *float64 `json:"minimum,omitempty"`
	Maximum *float64 `json:"maximum,omitempty"`
	// Default documents the value used when the property is absent. It is
	// not applied by ValidateArgs.
	Default json.RawMessage `json:"default,omitempty"`
}

// The types a Schema can have.
const (
	TypeObject  = "object"
	TypeString  = "string"
	TypeInteger = "integer"
	TypeNumber  = "number"
	TypeBoolean = "boolean"
	TypeArray   = "array"
)

// maxSchemaDepth bounds nesting so a hostile schema or value cannot exhaust
// the stack.
const maxSchemaDepth = 16

// Dialect names the schema flavour a kind of provider accepts.
type Dialect string

// The dialects.
const (
	// DialectFull keeps every keyword of the subset.
	DialectFull Dialect = "full"
	// DialectGemini drops the keywords the Gemini API rejects:
	// additionalProperties, $schema and default.
	DialectGemini Dialect = "gemini"
)

// ParseSchema parses and checks a schema. The error names the path of the
// first problem.
func ParseSchema(raw json.RawMessage) (*Schema, error) {
	var s Schema
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("schema: %w", cleanDecodeError(err))
	}
	if dec.More() {
		return nil, errors.New("schema: more than one JSON value")
	}
	if err := s.check("$", 0); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &s, nil
}

// cleanDecodeError turns encoding/json's unknown-field message into one that
// says what is wrong in schema terms.
func cleanDecodeError(err error) error {
	if name, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		return fmt.Errorf("keyword %s is not in the supported subset", name)
	}
	return err
}

func (s *Schema) check(path string, depth int) error {
	if s == nil {
		return fmt.Errorf("%s: schema is null", path)
	}
	if depth > maxSchemaDepth {
		return fmt.Errorf("%s: nested deeper than %d levels", path, maxSchemaDepth)
	}
	isObject, isArray := s.Type == TypeObject, s.Type == TypeArray
	isNumeric := s.Type == TypeInteger || s.Type == TypeNumber
	switch s.Type {
	case TypeObject, TypeString, TypeInteger, TypeNumber, TypeBoolean, TypeArray:
	case "":
		return fmt.Errorf("%s: no type", path)
	default:
		return fmt.Errorf("%s: type %q is not one of object, string, integer, number, boolean, array", path, s.Type)
	}
	switch {
	case !isObject && (s.Properties != nil || s.Required != nil || s.AdditionalProperties != nil):
		return fmt.Errorf("%s: properties, required and additionalProperties apply only to an object", path)
	case !isArray && s.Items != nil:
		return fmt.Errorf("%s: items applies only to an array", path)
	case isArray && s.Items == nil:
		return fmt.Errorf("%s: an array needs items", path)
	case s.Type != TypeString && s.Enum != nil:
		return fmt.Errorf("%s: enum applies only to a string", path)
	case s.Enum != nil && len(s.Enum) == 0:
		return fmt.Errorf("%s: enum is empty", path)
	case !isNumeric && (s.Minimum != nil || s.Maximum != nil):
		return fmt.Errorf("%s: minimum and maximum apply only to an integer or a number", path)
	case s.Minimum != nil && s.Maximum != nil && *s.Minimum > *s.Maximum:
		return fmt.Errorf("%s: minimum is above maximum", path)
	}
	for _, name := range s.Required {
		if _, ok := s.Properties[name]; !ok {
			return fmt.Errorf("%s: required property %q is not in properties", path, name)
		}
	}
	if isArray {
		return s.Items.check(path+"[]", depth+1)
	}
	for _, name := range sortedKeys(s.Properties) {
		if err := s.Properties[name].check(path+"."+name, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// For returns a copy of the schema with the keywords a dialect rejects
// removed.
func (s *Schema) For(d Dialect) *Schema {
	if s == nil {
		return nil
	}
	out := *s
	out.Required = slices.Clone(s.Required)
	out.Enum = slices.Clone(s.Enum)
	out.Default = slices.Clone(s.Default)
	if d == DialectGemini {
		out.SchemaURI = ""
		out.AdditionalProperties = nil
		out.Default = nil
	}
	if s.Properties != nil {
		out.Properties = make(map[string]*Schema, len(s.Properties))
		for name, prop := range s.Properties {
			out.Properties[name] = prop.For(d)
		}
	}
	out.Items = s.Items.For(d)
	return &out
}

// SchemaFor parses a tool schema and returns it as JSON for a dialect, with
// keys in a stable order.
func SchemaFor(raw json.RawMessage, d Dialect) (json.RawMessage, error) {
	s, err := ParseSchema(raw)
	if err != nil {
		return nil, err
	}
	out, err := json.Marshal(s.For(d))
	if err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return out, nil
}

// Validate checks a tool definition.
func (t ToolSpec) Validate() error {
	if !toolNameRE.MatchString(t.Name) {
		return fmt.Errorf("%q is not a tool name: use 1 to 64 letters, digits, _ or -", t.Name)
	}
	if strings.TrimSpace(t.Description) == "" {
		return fmt.Errorf("tool %s has no description", t.Name)
	}
	s, err := ParseSchema(t.Schema)
	if err != nil {
		return fmt.Errorf("tool %s: %w", t.Name, err)
	}
	if s.Type != TypeObject {
		return fmt.Errorf("tool %s: schema: the root type is %q, want object", t.Name, s.Type)
	}
	return nil
}

// ValidateArgs checks the arguments of a tool call against the schema. It is
// run locally for every call, whether or not the provider enforces schemas.
// The error is written for the model: it names the argument and what is
// wrong, so the model can correct the call.
func (s *Schema) ValidateArgs(args json.RawMessage) error {
	if s.Type != TypeObject {
		return fmt.Errorf("schema root type is %q, want object", s.Type)
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return fmt.Errorf("arguments are not valid JSON: %w", err)
	}
	if dec.More() {
		return errors.New("arguments hold more than one JSON value")
	}
	if _, ok := value.(map[string]any); !ok {
		return fmt.Errorf("arguments must be a JSON object, got %s", jsonType(value))
	}
	return s.validate("", value, 0)
}

func (s *Schema) validate(path string, value any, depth int) error {
	if depth > maxSchemaDepth {
		return fmt.Errorf("%s: nested deeper than %d levels", display(path), maxSchemaDepth)
	}
	mismatch := func() error {
		return fmt.Errorf("%s: want %s, got %s", display(path), s.Type, jsonType(value))
	}
	switch s.Type {
	case TypeObject:
		object, ok := value.(map[string]any)
		if !ok {
			return mismatch()
		}
		return s.validateObject(path, object, depth)
	case TypeArray:
		items, ok := value.([]any)
		if !ok {
			return mismatch()
		}
		for i, item := range items {
			if err := s.Items.validate(path+"["+strconv.Itoa(i)+"]", item, depth+1); err != nil {
				return err
			}
		}
	case TypeString:
		text, ok := value.(string)
		if !ok {
			return mismatch()
		}
		if s.Enum != nil && !slices.Contains(s.Enum, text) {
			return fmt.Errorf("%s: %q is not one of %s", display(path), text, strings.Join(s.Enum, ", "))
		}
	case TypeBoolean:
		if _, ok := value.(bool); !ok {
			return mismatch()
		}
	case TypeInteger, TypeNumber:
		number, ok := value.(json.Number)
		if !ok {
			return mismatch()
		}
		return s.validateNumber(path, number)
	}
	return nil
}

func (s *Schema) validateObject(path string, object map[string]any, depth int) error {
	for _, name := range s.Required {
		if _, ok := object[name]; !ok {
			return fmt.Errorf("%s: missing required property", display(join(path, name)))
		}
	}
	for _, name := range sortedKeys(object) {
		prop, ok := s.Properties[name]
		if !ok {
			if s.AdditionalProperties != nil && !*s.AdditionalProperties {
				return fmt.Errorf("%s: unknown property; known properties: %s",
					display(join(path, name)), orNone(sortedKeys(s.Properties)))
			}
			continue
		}
		if err := prop.validate(join(path, name), object[name], depth+1); err != nil {
			return err
		}
	}
	return nil
}

func (s *Schema) validateNumber(path string, number json.Number) error {
	var f float64
	if s.Type == TypeInteger {
		// Only a plain integer literal counts. "1.0" and "1e3" are integers
		// in JSON Schema, but the tools decode into Go ints, which reject
		// them, so they are refused here with a clear message.
		i, err := strconv.ParseInt(number.String(), 10, 64)
		if err != nil {
			return fmt.Errorf("%s: want a whole number, got %s", display(path), number)
		}
		f = float64(i)
	} else {
		parsed, err := number.Float64()
		if err != nil || math.IsInf(parsed, 0) {
			return fmt.Errorf("%s: %s is out of range", display(path), number)
		}
		f = parsed
	}
	if s.Minimum != nil && f < *s.Minimum {
		return fmt.Errorf("%s: %s is below the minimum %v", display(path), number, *s.Minimum)
	}
	if s.Maximum != nil && f > *s.Maximum {
		return fmt.Errorf("%s: %s is above the maximum %v", display(path), number, *s.Maximum)
	}
	return nil
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func display(path string) string {
	if path == "" {
		return "arguments"
	}
	return path
}

func jsonType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number:
		return "number"
	default:
		return fmt.Sprintf("%T", v)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func orNone(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", ")
}
