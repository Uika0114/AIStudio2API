package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func TestAssistantResponsesBackgroundHistoryAndCompactionRemainPinned(t *testing.T) {
	service := &fakeService{models: []aistudio.Model{assistantRoutingModel()}}
	handler := newRoutingTestHandler(t, service)
	model := "assistant/gemini-4-argon"
	response := doJSON(t, handler, http.MethodPost, "/v1/responses", `{"model":"`+model+`","input":"fixture","store":true}`)
	if response.Code != 200 {
		t.Fatalf("%d: %s", response.Code, response.Body.String())
	}
	var saved map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	id, _ := saved["id"].(string)
	if id == "" {
		t.Fatal("missing response ID")
	}
	response = doJSON(t, handler, http.MethodPost, "/v1/responses", `{"previous_response_id":"`+id+`","input":"next fixture"}`)
	if response.Code != 200 {
		t.Fatalf("history: %d: %s", response.Code, response.Body.String())
	}
	request := service.lastGenerate(t)
	if request.Selection.Channel != aistudio.ChannelAssistant || request.Selection.RequestedModel != model || len(request.Contents) < 2 {
		t.Fatalf("history lost route: %+v", request)
	}
	response = doJSON(t, handler, http.MethodPost, "/v1/responses", `{"model":"`+model+`","input":"background fixture","background":true,"stream":true}`)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "response.completed") {
		t.Fatalf("background: %d: %s", response.Code, response.Body.String())
	}
	request = service.lastGenerate(t)
	if request.Selection.Channel != aistudio.ChannelAssistant {
		t.Fatal("background fell back")
	}
	before := service.generateCount()
	for _, operation := range []struct{ method, path string }{{http.MethodGet, "/v1/responses/" + id}, {http.MethodPost, "/v1/responses/" + id + "/cancel"}} {
		doJSON(t, handler, operation.method, operation.path, "")
	}
	if service.generateCount() != before {
		t.Fatal("retrieval/cancel generated again")
	}
	server := &server{service: service, thoughtSignatures: newThoughtSignatureStore()}
	if _, _, err := server.compactionSummary(context.Background(), responsesRequest{Model: model}, []json.RawMessage{json.RawMessage(`{"role":"user","content":"fixture"}`)}, "", "fixture-compaction"); err != nil {
		t.Fatal(err)
	}
	request = service.lastGenerate(t)
	if request.Selection.Channel != aistudio.ChannelAssistant || request.Selection.RequestedModel != model || request.Model != "gemini-4-argon" {
		t.Fatalf("compaction lost route: %+v", request)
	}
}

func TestAssistantResponsesRejectsForeignSignedHistoryBeforeGenerate(t *testing.T) {
	service := &fakeService{models: []aistudio.Model{assistantRoutingModel()}, generateEvents: []aistudio.Event{
		{Kind: aistudio.EventText, Text: "fixture", Channel: aistudio.ChannelBuild, ThoughtSignature: "fixture-build-signature"},
		{Kind: aistudio.EventFinish, FinishReason: "stop", Channel: aistudio.ChannelBuild},
	}}
	handler := newRoutingTestHandler(t, service)
	response := doJSON(t, handler, http.MethodPost, "/v1/responses", `{"model":"build/gemini-4-argon","input":"fixture","store":true}`)
	if response.Code != 200 {
		t.Fatalf("%d: %s", response.Code, response.Body.String())
	}
	var saved map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	id, _ := saved["id"].(string)
	before := service.generateCount()
	response = doJSON(t, handler, http.MethodPost, "/v1/responses", `{"model":"assistant/gemini-4-argon","previous_response_id":"`+id+`","input":"next fixture"}`)
	if response.Code != 400 || service.generateCount() != before {
		t.Fatalf("foreign signature forwarded: %d %s", response.Code, response.Body.String())
	}
}
