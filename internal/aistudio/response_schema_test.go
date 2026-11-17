package aistudio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const responseSchemaCanonicalUnion = `{"anyOf":[{"type":"OBJECT","properties":{"source_fact_ids":{"type":"ARRAY","minItems":1,"items":{"type":"STRING","minLength":1}}},"required":["source_fact_ids"]},{"type":"OBJECT","properties":{"any_of":{"type":"STRING","minLength":1}},"required":["any_of"]},{"type":"OBJECT","properties":{"accepted":{"type":"BOOLEAN"}},"required":["accepted"]}]}`
const responseSchemaSDKUnion = `{"any_of":[{"type":"OBJECT","properties":{"source_fact_ids":{"type":"ARRAY","min_items":1,"items":{"type":"STRING","min_length":1}}},"required":["source_fact_ids"]},{"type":"OBJECT","properties":{"any_of":{"type":"STRING","min_length":1}},"required":["any_of"]},{"type":"OBJECT","properties":{"accepted":{"type":"BOOLEAN"}},"required":["accepted"]}]}`

func responseSchemaRequest(t *testing.T, model, schema string) GenerateRequest {
	t.Helper()
	request, err := NormalizeGenerateRequestModel(GenerateRequest{
		ID: t.Name(), Model: model,
		Contents: []Content{{Role: RoleUser, Parts: []Part{{Text: "synthetic schema fixture"}}}},
		Config:   GenerationConfig{ResponseMIMEType: "application/json", ResponseSchema: json.RawMessage(schema)},
	})
	if err != nil {
		t.Fatal(err)
	}
	before := bytes.Clone(request.Config.ResponseSchema)
	t.Cleanup(func() {
		if !bytes.Equal(before, request.Config.ResponseSchema) {
			t.Fatal("caller RawMessage changed")
		}
	})
	return request
}

func TestResponseSchemaSDKUnion(t *testing.T) {
	for _, test := range []struct{ name, schema string }{
		{"canonical", responseSchemaCanonicalUnion},
		{"sdk", responseSchemaSDKUnion},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := responseSchemaRequest(t, "playground/fixture-model", test.schema)
			selection, err := GenerationAccountSelection(request, "", 1)
			if err != nil || selection.Channel != ChannelPlayground || !selection.StrictChannel {
				t.Errorf("typed union did not retain strict PG: %+v, %v", selection, err)
			}
			if needs, err := RequestNeedsBuildSchema(request); err != nil || needs {
				t.Fatalf("typed union still needs Build: %v, %v", needs, err)
			}
			encoded, err := EncodeGenerateContentRequest(request, GenerationDefaults{MaxOutputTokens: 32}, RequestContext{})
			if err != nil {
				t.Fatal(err)
			}
			var sent []any
			if err := json.Unmarshal(encoded, &sent); err != nil {
				t.Fatal(err)
			}
			if root := sent[3].([]any)[8].([]any)[0]; root != float64(6) {
				t.Errorf("real PG encoder projected a typed union to %v", root)
			}
			wire, err := encodeResponseSchema(request.Config.ResponseSchema)
			if err != nil || len(wire) <= 17 || wire[0] != int64(6) {
				t.Fatalf("union lost: %#v, %v", wire, err)
			}
			first := wire[17].([]any)[0].([]any)
			property := first[6].([]any)[0].([]any)
			array := property[1].([]any)
			if property[0] != "source_fact_ids" || array[21] != int64(1) || array[5].([]any)[12] != int64(1) {
				t.Fatalf("nested constraints changed: %#v", property)
			}
			build, err := encodeBuildGenerationConfig(request.Config, GenerationDefaults{MaxOutputTokens: 32})
			if err != nil || !reflect.DeepEqual(build["responseSchema"], buildResponseSchema(wire)) {
				t.Fatalf("Build did not use the same response schema: %#v, %v", build, err)
			}
			buildRequest := responseSchemaRequest(t, "build/fixture-model", test.schema)
			_, body, err := EncodeBuildGenerateRequest(buildRequest, GenerationDefaults{MaxOutputTokens: 32}, false, false)
			if err != nil {
				t.Fatal(err)
			}
			var sentBuild map[string]json.RawMessage
			if err := json.Unmarshal(body, &sentBuild); err != nil {
				t.Fatal(err)
			}
			var generation map[string]json.RawMessage
			if err := json.Unmarshal(sentBuild["generationConfig"], &generation); err != nil {
				t.Fatal(err)
			}
			projected, _ := json.Marshal(buildResponseSchema(wire))
			assertResponseSchemaJSON(t, generation["responseSchema"], string(projected))
		})
	}
}

func TestResponseSchemaNestedAliasesSurviveFallback(t *testing.T) {
	canonical := `{"type":"object","properties":{"any_of":{"type":"array","minItems":1,"items":{"anyOf":[{"type":"string","minLength":1},{"type":"string","minLength":2}]}}},"required":["any_of"],"propertyOrdering":["any_of"]`
	aliases := `{"type":"object","properties":{"any_of":{"type":"array","min_items":1,"items":{"any_of":[{"type":"string","min_length":1},{"type":"string","min_length":2}]}}},"required":["any_of"],"property_ordering":["any_of"]`
	for _, fallback := range []bool{false, true} {
		name, suffix := "direct", `}`
		if fallback {
			name, suffix = "shape", `,"$defs":{"unused":{"type":"string"}}}`
		}
		t.Run(name, func(t *testing.T) {
			want, err := encodeResponseSchema(json.RawMessage(canonical + suffix))
			if err != nil {
				t.Fatal(err)
			}
			raw := json.RawMessage(aliases + suffix)
			got, err := encodeResponseSchema(raw)
			if err != nil {
				t.Fatal(err)
			}
			if fallback {
				if len(got) <= 2 || !strings.Contains(got[2].(string), responseSchemaNote+string(raw)) {
					t.Fatalf("original schema not retained in fallback: %#v", got)
				}
				want[2], got[2] = nil, nil // Only intentional original-input notes may differ.
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("alias carrier differs from canonical\ngot  %#v\nwant %#v", got, want)
			}
		})
	}
}

func TestResponseSchemaInvalidBeforeAccounts(t *testing.T) {
	for _, schema := range []string{
		`{"type":"array","items":{"type":"string"},"minItems":-1}`,
		`{"type":"array","items":{"type":"string"},"min_items":2,"minItems":1}`,
		`{"type":"object","properties":{"value":{"min_length":null,"minLength":1}}}`,
		`{"type":"array","items":{"type":"string"},"min_items":9007199254740992,"minItems":9007199254740993}`,
		`{"type":"array","items":{"type":"string"},"min_items":true}`,
		`{"all_of":[{"type":"string"},false]}`,
		`false`,
	} {
		for _, model := range []string{"fixture-model", "playground/fixture-model", "build/fixture-model"} {
			t.Run(model+"/"+schema, func(t *testing.T) {
				request := responseSchemaRequest(t, model, schema)
				if _, err := RequestNeedsBuildSchema(request); !errors.Is(err, ErrInvalidArgument) {
					t.Errorf("response judgment swallowed invalid schema: %v", err)
				}
				if _, err := GenerationAccountSelection(request, "", 1); !errors.Is(err, ErrInvalidArgument) {
					t.Errorf("selection swallowed invalid schema: %v", err)
				}
				service := &PooledService{pool: NewAccountPool(nil, 1), client: &Client{}}
				if err := service.ValidateGenerateRequest(request); !errors.Is(err, ErrInvalidArgument) {
					t.Errorf("prevalidation swallowed invalid schema: %v", err)
				}
				// An empty real pool must not mask local parameter errors with no-account errors.
				if _, err := service.Generate(context.Background(), request); !errors.Is(err, ErrInvalidArgument) {
					t.Errorf("generation reached account selection: %v", err)
				}
				if _, err := EncodeGenerateContentRequest(request, GenerationDefaults{MaxOutputTokens: 32}, RequestContext{}); err == nil {
					t.Error("real PG encoding accepted invalid schema")
				}
				if _, err := encodeBuildGenerationConfig(request.Config, GenerationDefaults{MaxOutputTokens: 32}); err == nil {
					t.Error("Build encoding accepted invalid schema")
				}
			})
		}
	}
}

func assertResponseSchemaJSON(t *testing.T, raw json.RawMessage, expected string) {
	t.Helper()
	var values [2]any
	for index, value := range []json.RawMessage{raw, json.RawMessage(expected)} {
		decoder := json.NewDecoder(bytes.NewReader(value))
		decoder.UseNumber()
		if err := decoder.Decode(&values[index]); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(values[0], values[1]) {
		t.Fatalf("schema JSON changed\ngot  %s\nwant %s", raw, expected)
	}
}

func TestResponseSchemaAliasTable(t *testing.T) {
	for _, test := range []struct{ alias, canonical, base, value string }{
		{"any_of", "anyOf", ``, `[{"type":"string","minLength":1}]`},
		{"one_of", "oneOf", ``, `[{"type":"string","maxLength":9}]`},
		{"all_of", "allOf", ``, `[{"type":"string","minLength":1}]`},
		{"min_items", "minItems", `"type":"array","items":{"type":"string"},`, `"1"`},
		{"max_items", "maxItems", `"type":"array","items":{"type":"string"},`, `9`},
		{"min_length", "minLength", `"type":"string",`, `1.0`},
		{"max_length", "maxLength", `"type":"string",`, `9`},
		{"min_properties", "minProperties", `"type":"object",`, `1`},
		{"max_properties", "maxProperties", `"type":"object",`, `9`},
		{"property_ordering", "propertyOrdering", `"type":"object","properties":{"any_of":{"type":"string"}},`, `["any_of"]`},
		{"additional_properties", "additionalProperties", `"type":"object",`, `false`},
		{"prefix_items", "prefixItems", `"type":"array",`, `[{"type":"string","minLength":1}]`},
		{"property_names", "propertyNames", `"type":"object",`, `{"type":"string","minLength":1}`},
		{"exclusive_minimum", "exclusiveMinimum", `"type":"number",`, `1`},
	} {
		t.Run(test.alias, func(t *testing.T) {
			for _, value := range []string{test.value, `null`} {
				canonical := `{` + test.base + `"` + test.canonical + `":` + value + `}`
				for _, dual := range []bool{false, true} {
					input := `{` + test.base + `"` + test.alias + `":` + value
					if dual {
						input += `,"` + test.canonical + `":` + value
					}
					input += `}`
					raw := json.RawMessage(input)
					before := bytes.Clone(raw)
					normalized, err := normalizeResponseSchema(raw, false)
					if err != nil {
						t.Fatal(err)
					}
					assertResponseSchemaJSON(t, normalized, canonical)
					again, err := normalizeResponseSchema(normalized, false)
					if err != nil || !bytes.Equal(normalized, again) || !bytes.Equal(raw, before) {
						t.Fatalf("not idempotent or mutated input: %s, %v", again, err)
					}
					want, err := encodeResponseSchema(json.RawMessage(canonical))
					if err != nil {
						t.Fatal(err)
					}
					got, err := encodeResponseSchema(raw)
					if err != nil || !reflect.DeepEqual(got, want) {
						t.Fatalf("carrier mismatch for %s: %#v != %#v, %v", input, got, want, err)
					}
				}
			}
			conflict := `{` + test.base + `"` + test.alias + `":null,"` + test.canonical + `":` + test.value + `,"x-extension":true}`
			if _, err := encodeResponseSchema(json.RawMessage(conflict)); err == nil || !strings.Contains(err.Error(), "conflicts") {
				t.Fatalf("conflict entered shape fallback: %s, %v", conflict, err)
			}
		})
	}
}

func TestResponseSchemaStructuralPositionsAndData(t *testing.T) {
	const data = `{"any_of":[{"min_items":9007199254740993,"minItems":1}],"one_of":false,"min_length":null}`
	raw := json.RawMessage(`{"properties":{"any_of":{"min_length":1},"min_items":{"max_length":9},"source_fact_ids":{"items":{"min_length":2}}},"required":["any_of","min_items"],"property_ordering":["source_fact_ids","any_of"],"items":{"any_of":[{"min_length":3}]},"not":{"max_length":4},"additional_properties":{"min_properties":1},"property_names":{"min_length":5},"prefix_items":[true,false,null,{"max_items":6}],"$defs":{"any_of":{"min_length":7}},"definitions":{"min_items":{"max_length":8}},"$ref":"#/$defs/any_of","description":"any_of min_items","enum":[` + data + `],"const":` + data + `,"default":` + data + `,"example":` + data + `,"x-extension":` + data + `}`)
	want := `{"properties":{"any_of":{"minLength":1},"min_items":{"maxLength":9},"source_fact_ids":{"items":{"minLength":2}}},"required":["any_of","min_items"],"propertyOrdering":["source_fact_ids","any_of"],"items":{"anyOf":[{"minLength":3}]},"not":{"maxLength":4},"additionalProperties":{"minProperties":1},"propertyNames":{"minLength":5},"prefixItems":[true,false,null,{"maxItems":6}],"$defs":{"any_of":{"minLength":7}},"definitions":{"min_items":{"maxLength":8}},"$ref":"#/$defs/any_of","description":"any_of min_items","enum":[` + data + `],"const":` + data + `,"default":` + data + `,"example":` + data + `,"x-extension":` + data + `}`
	before := bytes.Clone(raw)
	normalized, err := normalizeResponseSchema(raw, false)
	if err != nil {
		t.Fatal(err)
	}
	assertResponseSchemaJSON(t, normalized, want)
	again, err := normalizeResponseSchema(normalized, false)
	if err != nil || !bytes.Equal(again, normalized) || !bytes.Equal(before, raw) {
		t.Fatalf("normalization changed input or is not idempotent: %v", err)
	}
	wire, err := encodeResponseSchema(raw)
	if err != nil || !strings.Contains(wire[2].(string), responseSchemaNote+string(raw)) {
		t.Fatalf("original data/contract lost in fallback: %#v, %v", wire, err)
	}
}

func TestResponseSchemaDuplicateValues(t *testing.T) {
	for _, field := range []struct {
		name  string
		index int
	}{
		{"minItems", 21}, {"maxItems", 20}, {"minLength", 12},
		{"maxLength", 13}, {"minProperties", 8}, {"maxProperties", 9},
	} {
		name := field.name
		alias := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(name, "Items", "_items"), "Length", "_length"), "Properties", "_properties")
		for _, value := range []string{`1`, `1.0`, `1e0`, `"1"`, `9007199254740993`, `"9007199254740993"`} {
			expected, integer := `1`, int64(1)
			if strings.Contains(value, "9007199254740993") {
				expected, integer = `9007199254740993`, 9007199254740993
			}
			raw := json.RawMessage(`{"type":"object","` + alias + `":` + value + `,"` + name + `":` + expected + `}`)
			normalized, err := normalizeResponseSchema(raw, false)
			if err != nil {
				t.Fatalf("equal counter forms rejected: %s, %v", raw, err)
			}
			assertResponseSchemaJSON(t, normalized, `{"type":"object","`+name+`":`+expected+`}`)
			wire, err := encodeResponseSchema(raw)
			if err != nil || len(wire) <= field.index || wire[field.index] != integer {
				t.Fatalf("exact int64 counter rejected or rounded: %s, %#v, %v", raw, wire, err)
			}
		}
	}
	for _, test := range []struct {
		schema string
		ok     bool
	}{
		{`{"any_of":[{"type":"string","min_length":"1","example":{"n":9007199254740993}}],"anyOf":[{"example":{"n":9007199254740993},"minLength":1.0,"type":"string"}]}`, true},
		{`{"additional_properties":{"properties":{"any_of":{"min_length":1}},"type":"object"},"additionalProperties":{"type":"object","properties":{"any_of":{"minLength":1}}}}`, true},
		{`{"min_items":9007199254740992,"minItems":9007199254740993}`, false},
		{`{"min_items":9007199254740992.0,"minItems":9007199254740992}`, true},
		{`{"min_items":9007199254740993.0,"minItems":9007199254740992}`, false},
		{`{"min_items":"9007199254740993.0","minItems":9007199254740992}`, false},
		{`{"min_items":1e-400,"minItems":0}`, false},
		{`{"any_of":[{"type":"string","example":{"n":9007199254740992}}],"anyOf":[{"type":"string","example":{"n":9007199254740993}}]}`, false},
		{`{"any_of":[{"type":"string"},{"type":"object"}],"anyOf":[{"type":"object"},{"type":"string"}]}`, false},
		{`{"property_ordering":["a","b"],"propertyOrdering":["b","a"]}`, false},
		{`{"exclusive_minimum":1,"exclusiveMinimum":1.0}`, false},
	} {
		_, err := normalizeResponseSchema(json.RawMessage(test.schema), false)
		if (err == nil) != test.ok {
			t.Fatalf("duplicate comparison: %s, error=%v", test.schema, err)
		}
	}
}

func TestResponseSchemaChannelBoundaries(t *testing.T) {
	for _, schema := range []string{`true`, `{"type":"array"}`, `{"any_of":[{"type":"string"},{"type":"object"}]}`, `{"any_of":[true,{"type":"string"}]}`} {
		for _, model := range []string{"fixture-model", "playground/fixture-model", "build/fixture-model"} {
			request := responseSchemaRequest(t, model, schema)
			if needs, err := RequestNeedsBuildSchema(request); err != nil || !needs {
				t.Fatalf("genuine open/mixed schema no longer needs Build: %s, %v", schema, err)
			}
			selection, err := GenerationAccountSelection(request, "", 1)
			if request.Selection.Channel == ChannelPlayground {
				if !errors.Is(err, ErrInvalidArgument) {
					t.Fatalf("strict PG escaped guard: %v", err)
				}
			} else if err != nil || selection.Channel != request.Selection.Channel {
				t.Fatalf("auto/Build policy changed: %+v, %v", selection, err)
			}
		}
	}
	assistant := responseSchemaRequest(t, "assistant/fixture-model", responseSchemaSDKUnion)
	if err := (&PooledService{client: &Client{}}).ValidateGenerateRequest(assistant); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("Assistant accepted unsupported response schema: %v", err)
	}
	for _, raw := range []string{`null`, `true`, `{"type":"string","not":"denied"}`, `{"type":"string","not":["a","b"]}`, `{"type":"string","min_length":null}`, `{"type":"object","any_of":42}`, `{"type":"object","x-unknown":{"any_of":42}}`} {
		canonical := strings.ReplaceAll(strings.ReplaceAll(raw, `"min_length":null`, `"minLength":null`), `"any_of":42`, `"anyOf":42`)
		got, err := encodeResponseSchema(json.RawMessage(raw))
		want, wantErr := encodeResponseSchema(json.RawMessage(canonical))
		if (err == nil) != (wantErr == nil) {
			t.Fatalf("canonical error boundary changed for %s: %v / %v", raw, err, wantErr)
		}
		if err == nil {
			if len(got) > 2 {
				description, _ := got[2].(string)
				if strings.Contains(description, responseSchemaNote) {
					got[2], want[2] = nil, nil
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("canonical carrier boundary changed for %s", raw)
			}
		}
	}
}

func TestResponseSchemaAliasesDoNotLeakIntoTools(t *testing.T) {
	raw := json.RawMessage(`{"type":"array","items":{"type":"string"},"min_items":1}`)
	declaration := FunctionDeclaration{Name: "inspect", Parameters: raw}
	wire, err := encodeFunctionDeclaration(declaration)
	if err != nil {
		t.Fatal(err)
	}
	parameters := wire[2].([]any)
	if len(parameters) > 21 && parameters[21] != nil || !strings.Contains(wire[1].(string), `"min_items":1`) {
		t.Fatalf("response-specific aliases changed the tool contract: %#v", wire)
	}
	build, _, err := encodeBuildTools(Tools{Functions: []FunctionDeclaration{declaration}})
	if err != nil {
		t.Fatal(err)
	}
	function := build[0].(map[string]any)["functionDeclarations"].([]any)[0].(map[string]any)
	assertResponseSchemaJSON(t, function["parametersJsonSchema"].(json.RawMessage), string(raw))
}
