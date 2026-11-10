package llm_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aryanpappu/patchbot/internal/llm"
)

const editFileSchema = `{
	"$schema": "https://json-schema.org/draft/2020-12/schema",
	"type": "object",
	"description": "Arguments of edit_file.",
	"properties": {
		"path": {"type": "string", "description": "File to edit."},
		"mode": {"type": "string", "enum": ["replace", "append"], "default": "replace"},
		"line": {"type": "integer", "minimum": 1, "maximum": 100000},
		"ratio": {"type": "number", "minimum": 0, "maximum": 1},
		"all": {"type": "boolean", "default": false},
		"tags": {"type": "array", "items": {"type": "string"}},
		"range": {
			"type": "object",
			"properties": {"start": {"type": "integer"}, "end": {"type": "integer"}},
			"required": ["start"],
			"additionalProperties": false
		}
	},
	"required": ["path"],
	"additionalProperties": false
}`

func TestParseSchemaRejects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		schema  string
		wantErr string
	}{
		{"not json", `{"type":`, "schema:"},
		{"not an object", `"string"`, "schema:"},
		{"null", `null`, "$: no type"},
		{"two values", `{"type":"object"} {"type":"object"}`, "unexpected data after the JSON value"},
		{"trailing bracket", `{"type":"object"}]`, "unexpected data after the JSON value"},
		{"trailing brace", `{"type":"object"}}`, "unexpected data after the JSON value"},
		{"trailing text", `{"type":"object"} x`, "unexpected data after the JSON value"},
		{"trailing comma", `{"type":"object"},`, "unexpected data after the JSON value"},
		{"no type", `{}`, "$: no type"},
		{"unknown type", `{"type":"null"}`, `$: type "null" is not one of`},
		{"type list", `{"type":["string","null"]}`, "schema:"},
		{"unsupported keyword", `{"type":"object","oneOf":[]}`, `keyword "oneOf" is not in the supported subset`},
		{"unsupported keyword nested", `{"type":"object","properties":{"a":{"type":"string","pattern":"^a"}}}`, `keyword "pattern" is not in the supported subset`},
		{"ref", `{"$ref":"#/defs/x"}`, `keyword "$ref" is not in the supported subset`},
		{"properties on a string", `{"type":"string","properties":{}}`, "apply only to an object"},
		{"required on a string", `{"type":"string","required":["a"]}`, "apply only to an object"},
		{"items on an object", `{"type":"object","items":{"type":"string"}}`, "items applies only to an array"},
		{"array without items", `{"type":"array"}`, "$: an array needs items"},
		{"enum on an integer", `{"type":"integer","enum":["1"]}`, "enum applies only to a string"},
		{"empty enum", `{"type":"string","enum":[]}`, "enum is empty"},
		{"minimum on a string", `{"type":"string","minimum":1}`, "minimum and maximum apply only"},
		{"minimum above maximum", `{"type":"integer","minimum":5,"maximum":1}`, "minimum is above maximum"},
		{"required property not declared", `{"type":"object","properties":{"a":{"type":"string"}},"required":["b"]}`, `$: required property "b" is not in properties`},
		{"bad nested property", `{"type":"object","properties":{"a":{"type":"object","properties":{"b":{"type":"wat"}}}}}`, `$.a.b: type "wat"`},
		{"bad array item", `{"type":"object","properties":{"a":{"type":"array","items":{}}}}`, "$.a[]: no type"},
		{"null property", `{"type":"object","properties":{"a":null}}`, "$.a: schema is null"},
		{"too deep", strings.Repeat(`{"type":"array","items":`, 20) + `{"type":"string"}` + strings.Repeat(`}`, 20), "nested deeper than 16 levels"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := llm.ParseSchema(json.RawMessage(tt.schema))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("ParseSchema error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateArgs(t *testing.T) {
	t.Parallel()
	schema, err := llm.ParseSchema(json.RawMessage(editFileSchema))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		args    string
		wantErr string // empty means valid
	}{
		{"minimal", `{"path":"a.go"}`, ""},
		{"everything", `{"path":"a.go","mode":"append","line":7,"ratio":0.5,"all":true,"tags":["x","y"],"range":{"start":1,"end":2}}`, ""},
		{"empty array", `{"path":"a.go","tags":[]}`, ""},
		{"boundary values", `{"path":"","line":1,"ratio":1}`, ""},
		{"number as integer literal", `{"path":"a","ratio":1}`, ""},
		{"number with exponent", `{"path":"a","ratio":5e-1}`, ""},

		{"not json", `{"path":`, "arguments are not valid JSON"},
		{"empty", ``, "arguments are not valid JSON"},
		{"array", `["a.go"]`, "arguments must be a JSON object, got array"},
		{"string", `"a.go"`, "arguments must be a JSON object, got string"},
		{"null", `null`, "arguments must be a JSON object, got null"},
		{"two values", `{"path":"a"}{"path":"b"}`, "unexpected data after the JSON value"},
		{"trailing bracket", `{"path":"a"}]`, "unexpected data after the JSON value"},
		{"trailing brace", `{"path":"a"}}`, "unexpected data after the JSON value"},
		{"trailing text", `{"path":"a"} null`, "unexpected data after the JSON value"},
		{"trailing space is fine", "{\"path\":\"a\"} \n\t", ""},
		{"missing required", `{}`, "path: missing required property"},
		{"missing nested required", `{"path":"a","range":{"end":2}}`, "range.start: missing required property"},
		{"unknown property", `{"path":"a","file":"b"}`, "file: unknown property; known properties: all, line, mode, path, range, ratio, tags"},
		{"unknown nested property", `{"path":"a","range":{"start":1,"stop":2}}`, "range.stop: unknown property"},
		{"string given a number", `{"path":5}`, "path: want string, got number"},
		{"string given null", `{"path":null}`, "path: want string, got null"},
		{"integer given a string", `{"path":"a","line":"7"}`, "line: want integer, got string"},
		{"integer given a fraction", `{"path":"a","line":7.5}`, "line: want a whole number, got 7.5"},
		{"integer given 7.0", `{"path":"a","line":7.0}`, "line: want a whole number, got 7.0"},
		{"integer given an exponent", `{"path":"a","line":1e3}`, "line: want a whole number, got 1e3"},
		{"integer beyond int64", `{"path":"a","line":99999999999999999999}`, "line: want a whole number"},
		{"integer below minimum", `{"path":"a","line":0}`, "line: 0 is below the minimum 1"},
		{"integer above maximum", `{"path":"a","line":100001}`, "line: 100001 is above the maximum 100000"},
		{"number above maximum", `{"path":"a","ratio":1.01}`, "ratio: 1.01 is above the maximum 1"},
		{"number below minimum", `{"path":"a","ratio":-0.1}`, "ratio: -0.1 is below the minimum 0"},
		{"number out of range", `{"path":"a","ratio":1e999}`, "ratio: 1e999 is out of range"},
		{"boolean given a string", `{"path":"a","all":"true"}`, "all: want boolean, got string"},
		{"enum mismatch", `{"path":"a","mode":"delete"}`, `mode: "delete" is not one of replace, append`},
		{"array given a string", `{"path":"a","tags":"x"}`, "tags: want array, got string"},
		{"array item of the wrong type", `{"path":"a","tags":["x",2]}`, "tags[1]: want string, got number"},
		{"object given an array", `{"path":"a","range":[1,2]}`, "range: want object, got array"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := schema.ValidateArgs(json.RawMessage(tt.args))
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("ValidateArgs: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("ValidateArgs error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateArgsOpenObjectAndRoot(t *testing.T) {
	t.Parallel()
	open, err := llm.ParseSchema(json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	// Without additionalProperties: false, unlisted properties pass, as in
	// JSON Schema.
	if err := open.ValidateArgs(json.RawMessage(`{"a":"x","extra":[1,{"deep":null}]}`)); err != nil {
		t.Errorf("ValidateArgs with an extra property: %v", err)
	}
	if err := open.ValidateArgs(json.RawMessage(`{}`)); err != nil {
		t.Errorf("ValidateArgs with no properties: %v", err)
	}

	notObject, err := llm.ParseSchema(json.RawMessage(`{"type":"string"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := notObject.ValidateArgs(json.RawMessage(`"x"`)); err == nil || !strings.Contains(err.Error(), "want object") {
		t.Errorf("ValidateArgs on a non-object schema = %v", err)
	}
}

func TestSchemaForDialect(t *testing.T) {
	t.Parallel()
	full, err := llm.SchemaFor(json.RawMessage(editFileSchema), llm.DialectFull)
	if err != nil {
		t.Fatal(err)
	}
	gemini, err := llm.SchemaFor(json.RawMessage(editFileSchema), llm.DialectGemini)
	if err != nil {
		t.Fatal(err)
	}

	for _, keyword := range []string{`"$schema"`, `"additionalProperties"`, `"default"`} {
		if !strings.Contains(string(full), keyword) {
			t.Errorf("the full dialect lost %s: %s", keyword, full)
		}
		if strings.Contains(string(gemini), keyword) {
			t.Errorf("the gemini dialect kept %s: %s", keyword, gemini)
		}
	}
	const wantGemini = `{"type":"object","description":"Arguments of edit_file.","properties":{` +
		`"all":{"type":"boolean"},` +
		`"line":{"type":"integer","minimum":1,"maximum":100000},` +
		`"mode":{"type":"string","enum":["replace","append"]},` +
		`"path":{"type":"string","description":"File to edit."},` +
		`"range":{"type":"object","properties":{"end":{"type":"integer"},"start":{"type":"integer"}},"required":["start"]},` +
		`"ratio":{"type":"number","minimum":0,"maximum":1},` +
		`"tags":{"type":"array","items":{"type":"string"}}},` +
		`"required":["path"]}`
	if string(gemini) != wantGemini {
		t.Errorf("gemini schema:\n got %s\nwant %s", gemini, wantGemini)
	}

	// Output is stable, and cleaning does not change the parsed original.
	again, _ := llm.SchemaFor(json.RawMessage(editFileSchema), llm.DialectGemini)
	if string(again) != string(gemini) {
		t.Error("SchemaFor output is not stable")
	}
	parsed, _ := llm.ParseSchema(json.RawMessage(editFileSchema))
	cleaned := parsed.For(llm.DialectGemini)
	cleaned.Properties["path"].Description = "changed"
	cleaned.Required[0] = "changed"
	if parsed.AdditionalProperties == nil || parsed.Properties["range"].AdditionalProperties == nil ||
		parsed.Properties["path"].Description != "File to edit." || parsed.Required[0] != "path" {
		t.Error("For changed the schema it was called on")
	}

	if _, err := llm.SchemaFor(json.RawMessage(`{"type":"wat"}`), llm.DialectFull); err == nil {
		t.Error("SchemaFor accepted an invalid schema")
	}
	// A property that happens to be called "default" is a property, not the keyword.
	out, err := llm.SchemaFor(json.RawMessage(`{"type":"object","properties":{"default":{"type":"string"},"additionalProperties":{"type":"boolean"}}}`), llm.DialectGemini)
	if err != nil || !strings.Contains(string(out), `"default":{"type":"string"}`) || !strings.Contains(string(out), `"additionalProperties":{"type":"boolean"}`) {
		t.Errorf("SchemaFor dropped a property named like a keyword: %s, %v", out, err)
	}
}

func TestToolSpecValidate(t *testing.T) {
	t.Parallel()
	object := json.RawMessage(`{"type":"object"}`)
	tests := []struct {
		name    string
		tool    llm.ToolSpec
		wantErr string
	}{
		{"valid", llm.ToolSpec{Name: "read_file", Description: "Read a file.", Schema: object}, ""},
		{"valid with dash and digits", llm.ToolSpec{Name: "run-cmd2", Description: "Run.", Schema: json.RawMessage(editFileSchema)}, ""},
		{"empty name", llm.ToolSpec{Description: "d", Schema: object}, `"" is not a tool name`},
		{"name with a dot", llm.ToolSpec{Name: "fs.read", Description: "d", Schema: object}, `"fs.read" is not a tool name`},
		{"name too long", llm.ToolSpec{Name: strings.Repeat("a", 65), Description: "d", Schema: object}, "is not a tool name"},
		{"no description", llm.ToolSpec{Name: "t", Description: "  ", Schema: object}, "tool t has no description"},
		{"no schema", llm.ToolSpec{Name: "t", Description: "d"}, "tool t: schema:"},
		{"root is not an object", llm.ToolSpec{Name: "t", Description: "d", Schema: json.RawMessage(`{"type":"array","items":{"type":"string"}}`)}, `the root type is "array", want object`},
		{"unsupported keyword", llm.ToolSpec{Name: "t", Description: "d", Schema: json.RawMessage(`{"type":"object","anyOf":[]}`)}, `keyword "anyOf" is not in the supported subset`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.tool.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("Validate: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("Validate error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// FuzzValidateArgs checks that no arguments, however malformed or deeply
// nested, make validation panic.
func FuzzValidateArgs(f *testing.F) {
	schema, err := llm.ParseSchema(json.RawMessage(editFileSchema))
	if err != nil {
		f.Fatal(err)
	}
	for _, seed := range []string{`{"path":"a"}]`, `{"path":"a"}}`, `{"path":"a"}`, `{"path":1}`, `[]`, `{"range":{"start":1e9}}`, strings.Repeat("[", 5000), `{"tags":[[[[[[]]]]]]}`, ``} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, args string) {
		err := schema.ValidateArgs(json.RawMessage(args))
		if err == nil && !json.Valid([]byte(args)) {
			t.Errorf("ValidateArgs accepted invalid JSON %q", args)
		}
	})
}
