package aistudio

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

// Check the model-visible declaration independently of the optional system prompt.
func assertReplySubmissionSchema(t *testing.T, declaration FunctionDeclaration) {
	t.Helper()
	raw, err := os.ReadFile("testdata/anti-reply-v2.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
		System      string         `json:"system_template"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	if declaration.Description != golden.Description {
		t.Fatal("unified description differs from decision golden")
	}
	if got := fmt.Sprintf(antiTruncationSystemInstruction, declaration.Name); got != strings.ReplaceAll(golden.System, "__TRANSPORT_TOOL_NAME__", declaration.Name) {
		t.Fatal("unified system differs from decision golden")
	}
	var schema map[string]any
	if err := json.Unmarshal(declaration.Parameters, &schema); err != nil {
		t.Fatal(err)
	}
	want := golden.Parameters
	if !reflect.DeepEqual(schema, want) {
		t.Fatalf("unexpected parameter schema: %s", declaration.Parameters)
	}
}

func TestAntiTruncationReplySchemaIndependentOfSystemPrompt(t *testing.T) {
	for _, appendPrompt := range []bool{false, true} {
		for _, collision := range []bool{false, true} {
			t.Run(fmt.Sprintf("append=%t/collision=%t", appendPrompt, collision), func(t *testing.T) {
				request := GenerateRequest{System: "caller system", Tools: Tools{ToolConfig: ToolConfig{Mode: "auto"}}}
				wantName := antiTruncationTransportBaseName
				if collision {
					request.Tools.Functions = []FunctionDeclaration{{Name: wantName}}
					wantName += "_2"
				}
				upstream, transport, err := prepareAntiTruncation(request, appendPrompt)
				if err != nil {
					t.Fatal(err)
				}
				if transport.Name() != wantName {
					t.Fatal("tool identity/collision behavior changed")
				}
				if !appendPrompt && upstream.System != request.System {
					t.Fatal("schema update injected a disabled system instruction")
				}
				declaration := upstream.Tools.Functions[len(upstream.Tools.Functions)-1]
				assertReplySubmissionSchema(t, declaration)
				if declaration.Name != transport.Name() {
					t.Fatal("schema and projector name differ")
				}
			})
		}
	}
}

func TestUnifiedAntiReplyActualWire(t *testing.T) {
	for _, prompt := range []bool{false, true} {
		for _, collision := range []bool{false, true} {
			for _, channel := range []Channel{ChannelPlayground, ChannelBuild} {
				t.Run(fmt.Sprintf("%s/prompt=%t/collision=%t", channel, prompt, collision), func(t *testing.T) {
					r := GenerateRequest{Model: "fixture-model", System: "caller-system", Contents: []Content{{Role: RoleUser, Parts: []Part{{Text: "fixture"}}}}}
					if collision {
						r.Tools.Functions = []FunctionDeclaration{{Name: antiTruncationTransportBaseName, Parameters: json.RawMessage(`{"type":"object"}`)}}
					}
					up, transport, err := prepareAntiTruncation(r, prompt)
					if err != nil {
						t.Fatal(err)
					}
					declaration := up.Tools.Functions[len(up.Tools.Functions)-1]
					assertReplySubmissionSchema(t, declaration)
					if channel == ChannelBuild {
						_, body, err := EncodeBuildGenerateRequest(up, GenerationDefaults{MaxOutputTokens: 32768}, false, false)
						if err != nil {
							t.Fatal(err)
						}
						var wire struct {
							Tools []struct {
								Functions []struct {
									Name        string         `json:"name"`
									Description string         `json:"description"`
									Parameters  map[string]any `json:"parametersJsonSchema"`
								} `json:"functionDeclarations"`
							} `json:"tools"`
							Config struct {
								Calling struct {
									Mode    string   `json:"mode"`
									Allowed []string `json:"allowedFunctionNames"`
								} `json:"functionCallingConfig"`
							} `json:"toolConfig"`
							System struct {
								Parts []struct {
									Text string `json:"text"`
								} `json:"parts"`
							} `json:"systemInstruction"`
						}
						if err = json.Unmarshal(body, &wire); err != nil {
							t.Fatal(err)
						}
						last := wire.Tools[0].Functions[len(wire.Tools[0].Functions)-1]
						if last.Name != transport.Name() || last.Description != declaration.Description || last.Parameters["additionalProperties"] != false || wire.Config.Calling.Mode != "ANY" {
							t.Fatalf("Build mapping missing: %s", body)
						}
						if len(wire.System.Parts) != 1 || wire.System.Parts[0].Text != up.System {
							t.Fatal("Build system changed")
						}
						if (len(wire.Config.Calling.Allowed) == 1 && wire.Config.Calling.Allowed[0] != transport.Name()) || len(wire.Config.Calling.Allowed) != len(up.Tools.Functions) {
							t.Fatal("Build allowlist changed")
						}
					} else {
						body, err := EncodeGenerateContentRequest(up, GenerationDefaults{MaxOutputTokens: 32768}, RequestContext{})
						if err != nil {
							t.Fatal(err)
						}
						var wire []any
						if err = json.Unmarshal(body, &wire); err != nil {
							t.Fatal(err)
						}
						functions := wire[6].([]any)[0].([]any)[1].([]any)
						last := functions[len(functions)-1].([]any)
						if last[0] != transport.Name() {
							t.Fatal("PG collision name lost")
						}
						parts := strings.SplitN(last[1].(string), "\nArguments must follow this JSON Schema: ", 2)
						if len(parts) != 2 || parts[0] != declaration.Description {
							t.Fatal("PG full schema instruction missing with system off")
						}
						var schema map[string]any
						if err = json.Unmarshal([]byte(parts[1]), &schema); err != nil || schema["additionalProperties"] != false {
							t.Fatal("PG full schema lost", err)
						}
						// This observed baseline has no wire ANY mapping. Do not invent one.
						if len(wire) != 11 || up.Tools.ToolConfig.Mode != "required" {
							t.Fatalf("PG native mode baseline changed: %s", body)
						}
					}
				})
			}
		}
	}
}

func TestUnifiedAntiExtraFieldCannotFallbackAndLongReplyUnchanged(t *testing.T) {
	_, transport := antiTestPrepare(t, GenerateRequest{})
	antiTestProject(t, transport, Event{Kind: EventText, Text: "ordinary draft"})
	if _, err := transport.Project(antiTestCall(transport.Name(), `{"content":"reply","extra":"sensitive-key"}`)); !errors.Is(err, errAntiTruncationExtraField) || strings.Contains(err.Error(), "sensitive-key") {
		t.Fatal("bad transport masked or leaked", err)
	}
	content := strings.Repeat("中文 😀\nfirst draft/revision ", 10000)
	_, transport = antiTestPrepare(t, GenerateRequest{})
	raw, _ := json.Marshal(map[string]string{"content": content})
	events := antiTestProject(t, transport, antiTestCall(transport.Name(), string(raw)))
	if len(events) != 1 || events[0].Text != content {
		t.Fatal("legal long reply edited or truncated")
	}
}
