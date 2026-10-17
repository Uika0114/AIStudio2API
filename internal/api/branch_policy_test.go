package api

import (
	"fmt"
	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"strings"
	"testing"
)

func TestExplicitPolicySurvivesMultipleChoicesAndCandidates(t *testing.T) {
	for _, channel := range []string{"playground", "build"} {
		for _, stream := range []bool{false, true} {
			for _, protocol := range []string{"chat", "gemini"} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", channel, protocol, stream), func(t *testing.T) {
					service := &fakeService{models: []aistudio.Model{routingTestModel()}}
					handler := newRoutingTestHandler(t, service)
					model := channel + "/anti-truncation/gemini-4-argon"
					path := "/v1/chat/completions"
					body := fmt.Sprintf(`{"model":%q,"stream":%t,"n":3,"messages":[{"role":"user","content":"fixture"}]}`, model, stream)
					if protocol == "gemini" {
						action := "generateContent"
						if stream {
							action = "streamGenerateContent"
						}
						path = "/v1beta/models/" + model + ":" + action
						body = `{"contents":[{"role":"user","parts":[{"text":"fixture"}]}],"generationConfig":{"candidateCount":3}}`
					}
					response := doJSON(t, handler, "POST", path, body)
					if response.Code != 200 {
						t.Fatal(response.Code, response.Body.String())
					}
					service.mu.Lock()
					defer service.mu.Unlock()
					if len(service.generateReqs) != 3 {
						t.Fatal("candidate count changed", len(service.generateReqs))
					}
					ids := map[string]bool{}
					for _, request := range service.generateReqs {
						if request.Selection.RequestedModel != model || string(request.Selection.Channel) != channel || !request.Selection.AntiTruncation || request.Model != "gemini-4-argon" || ids[request.ID] {
							t.Fatalf("choice policy/id lost: %+v", request)
						}
						ids[request.ID] = true
					}
					if stream && !strings.Contains(response.Body.String(), "hello") {
						t.Fatal("no stream output")
					}
				})
			}
		}
	}
}

func TestResponsesBackgroundStreamKeepsPolicyAndRetrievalDoesNotGenerate(t *testing.T) {
	for _, channel := range []string{"playground", "build"} {
		t.Run(channel, func(t *testing.T) {
			service := &fakeService{models: []aistudio.Model{routingTestModel()}}
			handler := newRoutingTestHandler(t, service)
			model := channel + "/anti-truncation/gemini-4-argon"
			response := doJSON(t, handler, "POST", "/v1/responses", fmt.Sprintf(`{"model":%q,"input":"fixture","background":true,"stream":true}`, model))
			if response.Code != 200 || !strings.Contains(response.Body.String(), "response.completed") {
				t.Fatal(response.Code, response.Body.String())
			}
			objects := protocolObjects(t, response.Body.String(), true)
			id := ""
			for _, object := range objects {
				if value, ok := object["response"].(map[string]any); ok {
					if got, ok := value["id"].(string); ok {
						id = got
					}
				}
			}
			if id == "" {
				t.Fatal("missing saved response")
			}
			request := service.lastGenerate(t)
			if request.Selection.RequestedModel != model || string(request.Selection.Channel) != channel || !request.Selection.AntiTruncation {
				t.Fatal(request.Selection)
			}
			before := service.generateCount()
			for _, operation := range []struct{ method, path string }{{"GET", "/v1/responses/" + id}, {"GET", "/v1/responses/" + id + "?stream=true"}, {"POST", "/v1/responses/" + id + "/cancel"}} {
				doJSON(t, handler, operation.method, operation.path, "")
			}
			if service.generateCount() != before {
				t.Fatal("retrieval/cancel generated another request")
			}
		})
	}
}
