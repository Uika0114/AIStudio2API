package aistudio

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestAntiTruncationSystemPromptOnlyChangesSystem(t *testing.T) {
	for _, mode := range []string{"", "auto", "none", "required", "validated"} {
		for _, system := range []string{"", "  ", "caller system\n  "} {
			t.Run(fmt.Sprintf("%s/%q", mode, system), func(t *testing.T) {
				request := GenerateRequest{System: system, Tools: Tools{
					Functions:  []FunctionDeclaration{{Name: antiTruncationTransportBaseName, Parameters: json.RawMessage(`{"type":"object"}`)}},
					ToolConfig: ToolConfig{Mode: mode},
				}}
				before, _ := json.Marshal(request)
				on, onTransport, err := PrepareAntiTruncation(request)
				if err != nil {
					t.Fatal(err)
				}
				off, offTransport, err := prepareAntiTruncation(request, false)
				if err != nil {
					t.Fatal(err)
				}
				if off.System != system {
					t.Fatal("disabled append changed caller system")
				}
				if on.System != appendSystemInstruction(system, fmt.Sprintf(antiTruncationSystemInstruction, onTransport.Name())) {
					t.Fatal("legacy prompt changed")
				}
				if onTransport.Name() != offTransport.Name() {
					t.Fatal("toggle changed collision handling")
				}
				on.System = system
				if !reflect.DeepEqual(on, off) {
					t.Fatal("toggle changed fields other than system")
				}
				after, _ := json.Marshal(request)
				if !bytes.Equal(before, after) {
					t.Fatal("mutated caller request")
				}
				if off.Tools.ToolConfig.Mode != "required" {
					t.Fatal("disabled append weakened ANY")
				}
				if mode == "required" && reflect.DeepEqual(off.Tools.ToolConfig.AllowedFunctionNames, []string{offTransport.Name()}) {
					t.Fatal("transport satisfied required real tool")
				}
				for _, transport := range []*AntiTruncationTransport{onTransport, offTransport} {
					events := antiTestProject(t, transport, antiTestCall(transport.Name(), `{"content":"fixture answer"}`))
					if len(events) != 1 || events[0].Kind != EventText || events[0].Text != "fixture answer" {
						t.Fatal("text projection changed")
					}
				}
			})
		}
	}
}

func TestAntiTruncationSystemPromptClientPipeline(t *testing.T) {
	for _, channel := range []Channel{ChannelPlayground, ChannelBuild} {
		for _, disabled := range []bool{false, true} {
			for _, anti := range []bool{false, true} {
				for _, unary := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/off=%t/anti=%t/unary=%t", channel, disabled, anti, unary), func(t *testing.T) {
						fixture := newPipelineFixture(t, channel)
						old := fixture.client
						client, err := NewClient(ClientOptions{Transport: old.transport, Protected: old.protected, DisableAntiTruncationSystemPrompt: disabled})
						if err != nil {
							t.Fatal(err)
						}
						client.catalogs = old.catalogs
						fixture.client = client
						fixture.reply = func(request GenerateRequest, rpc RPCRequest, _ int) ([]byte, error) {
							const caller = "caller-system-sentinel"
							want := caller
							if anti && !disabled {
								want = appendSystemInstruction(caller, fmt.Sprintf(antiTruncationSystemInstruction, antiTruncationTransportBaseName))
							}
							if request.System != want {
								t.Fatalf("system: %q want %q", request.System, want)
							}
							// Both encoders must carry the same policy, including Build's nested JSON string.
							if !bytes.Contains(rpc.Body, []byte(caller)) {
								t.Fatal("caller system missing on wire")
							}
							if bytes.Contains(rpc.Body, []byte("Honor the caller requirements")) != (anti && !disabled) {
								t.Fatal("wire append policy wrong")
							}
							if bytes.Contains(rpc.Body, []byte("Every user-facing reply MUST")) != anti {
								t.Fatal("new reply schema missing on wire or leaked into plain route")
							}
							parts := []Part{{Text: "fixture answer"}}
							if anti {
								if request.Tools.ToolConfig.Mode != "required" {
									t.Fatal("tool choice changed")
								}
								assertReplySubmissionSchema(t, request.Tools.Functions[len(request.Tools.Functions)-1])
								parts = []Part{fixtureTransportPart(t, request, "fixture answer")}
							} else if len(request.Tools.Functions) != 0 {
								t.Fatal("plain route gained transport")
							}
							return fixtureResponse(t, rpc, parts, "stop", &Usage{InputTokens: 5, OutputTokens: 2, TotalTokens: 7}, ""), nil
						}
						model := string(channel) + "/"
						if anti {
							model += "anti-truncation/"
						}
						model += "fixture-model"
						events := fixture.generate(t, GenerateRequest{Model: model, System: "caller-system-sentinel", Unary: unary})
						if pipelineError(events) != nil || pipelineFinish(events) == nil || pipelineText(events) != "fixture answer" {
							t.Fatalf("pipeline failed: %+v", events)
						}
						if usage := pipelineUsage(events); usage == nil || usage.TotalTokens != 7 {
							t.Fatal("usage changed")
						}
						if len(fixture.sends) != 1 {
							t.Fatal("toggle caused extra generation")
						}
						for _, event := range events {
							if event.ToolCall != nil || strings.Contains(event.Text, antiTruncationTransportBaseName) {
								t.Fatal("transport leaked")
							}
						}
					})
				}
			}
		}
	}
}
