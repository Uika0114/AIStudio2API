package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func assistantRoutingModel() aistudio.Model {
	model := routingTestModel()
	model.Channels = append(model.Channels, "assistant")
	model.ChannelTokenLimits = map[aistudio.Channel]aistudio.ModelTokenLimits{aistudio.ChannelAssistant: {}}
	return model
}

func TestAssistantRoutingCatalogDoesNotBorrowToolOrContinuationCapabilities(t *testing.T) {
	model := assistantRoutingModel()
	catalog := publicModelCatalog([]aistudio.Model{model})
	var assistant *aistudio.Model
	for i := range catalog {
		if catalog[i].ID == "assistant/gemini-4-argon" {
			assistant = &catalog[i]
		}
		if strings.HasPrefix(catalog[i].ID, "assistant/anti-truncation/") {
			t.Fatal("advertised unmapped transport")
		}
	}
	if assistant == nil || assistant.Capabilities["function_declarations"] || assistant.Capabilities["output_continuation"] || assistant.InputTokenLimit != 0 || assistant.OutputTokenLimit != 0 {
		t.Fatalf("assistant projection: %+v", assistant)
	}
	if !model.Capabilities["function_declarations"] || !model.Capabilities["output_continuation"] {
		t.Fatal("mutated legacy model")
	}
	if _, ok := LookupPublicModel([]aistudio.Model{model}, "assistant/anti-truncation/gemini-4-argon"); ok {
		t.Fatal("lookup admitted unimplemented transport")
	}
}

// Fake service validates adapter plumbing only, never Google's acceptance.
func TestAssistantRoutingThreeProtocolsTextAndStreams(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "gemini"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", protocol, stream), func(t *testing.T) {
				service := &fakeService{models: []aistudio.Model{assistantRoutingModel()}}
				handler := newRoutingTestHandler(t, service)
				model := "assistant/gemini-4-argon"
				path, body := "", ""
				switch protocol {
				case "chat":
					path = "/v1/chat/completions"
					body = fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"fixture"}],"stream":%v}`, model, stream)
				case "responses":
					path = "/v1/responses"
					body = fmt.Sprintf(`{"model":%q,"input":"fixture","stream":%v}`, model, stream)
				case "gemini":
					path = "/v1beta/models/" + model + ":generateContent"
					if stream {
						path = "/v1beta/models/" + model + ":streamGenerateContent?alt=sse"
					}
					body = `{"contents":[{"role":"user","parts":[{"text":"fixture"}]}]}`
				}
				response := doJSON(t, handler, http.MethodPost, path, body)
				if response.Code != 200 {
					t.Fatalf("%d: %s", response.Code, response.Body.String())
				}
				request := service.lastGenerate(t)
				if request.Model != "gemini-4-argon" || request.Selection.Channel != aistudio.ChannelAssistant || request.Selection.RequestedModel != model {
					t.Fatalf("policy lost: %+v", request)
				}
				if !strings.Contains(response.Body.String(), model) {
					t.Fatalf("public ID not retained: %s", response.Body.String())
				}
			})
		}
	}
}

func TestAssistantRoutingToolPayloadIsNotStrippedByAdapters(t *testing.T) {
	service := &fakeService{models: []aistudio.Model{assistantRoutingModel()}, generateErr: fmt.Errorf("%w: fixture core rejection", aistudio.ErrInvalidArgument)}
	handler := newRoutingTestHandler(t, service)
	payloads := []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"assistant/gemini-4-argon","messages":[{"role":"user","content":"fixture"}],"tools":[{"type":"function","function":{"name":"read_fixture","parameters":{"type":"object"}}}],"tool_choice":"required"}`},
		{"/v1/responses", `{"model":"assistant/gemini-4-argon","input":"fixture","tools":[{"type":"function","name":"read_fixture","parameters":{"type":"object"}}],"tool_choice":"required"}`},
		{"/v1beta/models/assistant/gemini-4-argon:generateContent", `{"contents":[{"role":"user","parts":[{"text":"fixture"}]}],"tools":[{"functionDeclarations":[{"name":"read_fixture","parameters":{"type":"object"}}]}],"toolConfig":{"functionCallingConfig":{"mode":"ANY"}}}`},
	}
	for _, item := range payloads {
		response := doJSON(t, handler, http.MethodPost, item.path, item.body)
		if response.Code != 400 || !strings.Contains(response.Body.String(), "fixture core rejection") {
			t.Fatalf("%s: %d %s", item.path, response.Code, response.Body.String())
		}
		request := service.lastGenerate(t)
		if len(request.Tools.Functions) != 1 || request.Tools.Functions[0].Name != "read_fixture" || request.Tools.ToolConfig.Mode != "required" {
			t.Fatalf("adapter stripped contract: %+v", request.Tools)
		}
	}
}

func TestAssistantRoutingHistoryChecksAndUnsupportedEndpoints(t *testing.T) {
	previous := responseHistory{ID: "fixture-response", Channel: "build", Contents: []aistudio.Content{{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{{ThoughtSignature: "fixture-opaque"}}}}}
	selection := aistudio.ModelSelection{Channel: aistudio.ChannelAssistant, BaseModel: "gemini-4-argon"}
	if crossChannelOpaqueStateError(previous, selection) == nil {
		t.Fatal("cross-channel signature accepted")
	}
	if rejectRoutedModel("assistant/gemini-4-argon") == nil {
		t.Fatal("unsupported endpoint ignored assistant prefix")
	}
}
