package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// fakeService 是公开协议适配器的离线替身：只记录收到的规范请求并返回虚构事件，
// 不启动 runtime、不发网络请求。
type fakeService struct {
	mu             sync.Mutex
	models         []aistudio.Model
	generateReqs   []aistudio.GenerateRequest
	countReqs      []aistudio.TokenCountRequest
	generated      int
	generateEvents []aistudio.Event
	generateErr    error
}

func (service *fakeService) Models(context.Context) ([]aistudio.Model, error) {
	return service.models, nil
}

func (service *fakeService) CountTokens(_ context.Context, request aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	service.mu.Lock()
	service.countReqs = append(service.countReqs, request)
	service.mu.Unlock()
	return aistudio.TokenCount{InputTokens: 7}, nil
}

func (service *fakeService) Generate(_ context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	service.mu.Lock()
	service.generateReqs = append(service.generateReqs, request)
	service.generated++
	events := service.generateEvents
	err := service.generateErr
	service.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if events == nil {
		events = fakeGenerationEvents(request)
	}
	channel := make(chan aistudio.Event, len(events))
	for _, event := range events {
		channel <- event
	}
	close(channel)
	return channel, nil
}

func (service *fakeService) lastGenerate(t *testing.T) aistudio.GenerateRequest {
	t.Helper()
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.generateReqs) == 0 {
		t.Fatal("no Generate request was recorded")
	}
	return service.generateReqs[len(service.generateReqs)-1]
}

func (service *fakeService) generateCount() int {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.generated
}

func (service *fakeService) lastCount(t *testing.T) aistudio.TokenCountRequest {
	t.Helper()
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.countReqs) == 0 {
		t.Fatal("no CountTokens request was recorded")
	}
	return service.countReqs[len(service.countReqs)-1]
}

// fakeGenerationEvents 返回一次正常结束的虚构生成，实际通道与请求政策一致。
func fakeGenerationEvents(request aistudio.GenerateRequest) []aistudio.Event {
	channel := request.Selection.Channel
	if channel == "" {
		channel = aistudio.ChannelPlayground
	}
	return []aistudio.Event{
		{Kind: aistudio.EventText, Text: "hello", Channel: channel},
		{Kind: aistudio.EventUsage, Usage: &aistudio.Usage{InputTokens: 3, OutputTokens: 2, TotalTokens: 5}, Channel: channel},
		{Kind: aistudio.EventFinish, FinishReason: "stop", Channel: channel},
	}
}

// routingTestModel 返回同时拥有 Playground 与 Build 且支持函数声明的目录项。
func routingTestModel() aistudio.Model {
	return aistudio.Model{
		ID:               "gemini-4-argon",
		Name:             "Gemini 4 Argon",
		Methods:          []string{"generateContent", "streamGenerateContent"},
		InputTokenLimit:  1048576,
		OutputTokenLimit: 32768,
		Capabilities:     map[string]bool{"function_declarations": true, "output_continuation": true},
		CapabilityOptions: map[string][]string{
			"aliases": {"gemini-4-argon-latest"},
		},
		Channels: []string{"playground", "build"},
	}
}

func newRoutingTestHandler(t *testing.T, service aistudio.Service) http.Handler {
	t.Helper()
	return NewHandler(service, Config{})
}

func doJSON(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// TestPublicModelCatalogProjectsSelectableIDs 验证目录只给出真实可选的通道变体。
func TestPublicModelCatalogProjectsSelectableIDs(t *testing.T) {
	restricted := aistudio.Model{
		ID: "no-channel-model", Methods: []string{"generateContent"},
		Capabilities: map[string]bool{"function_declarations": true},
	}
	noTools := aistudio.Model{
		ID: "no-tools-model", Methods: []string{"generateContent"},
		Capabilities: map[string]bool{}, Channels: []string{"playground"},
	}
	catalog := publicModelCatalog([]aistudio.Model{routingTestModel(), restricted, noTools})
	ids := make(map[string]aistudio.Model, len(catalog))
	for _, model := range catalog {
		ids[model.ID] = model
	}
	for _, want := range []string{
		"gemini-4-argon",
		"playground/gemini-4-argon",
		"build/gemini-4-argon",
		"playground/anti-truncation/gemini-4-argon",
		"build/anti-truncation/gemini-4-argon",
		"no-channel-model",
		"no-tools-model",
		"playground/no-tools-model",
	} {
		if _, ok := ids[want]; !ok {
			t.Fatalf("catalog missing %q: %v", want, catalogIDs(catalog))
		}
	}
	for _, unwanted := range []string{
		"playground/no-channel-model",
		"build/no-channel-model",
		"playground/anti-truncation/no-tools-model",
		"build/no-tools-model",
	} {
		if _, ok := ids[unwanted]; ok {
			t.Fatalf("catalog invented %q: %v", unwanted, catalogIDs(catalog))
		}
	}
	if got := ids["playground/anti-truncation/gemini-4-argon"].Channels; len(got) != 1 || got[0] != "playground" {
		t.Fatalf("variant channels = %v, want [playground]", got)
	}
	if got := ids["playground/gemini-4-argon"].BaseModel; got != "gemini-4-argon" {
		t.Fatalf("variant BaseModel = %q, want gemini-4-argon", got)
	}
	if !ids["playground/gemini-4-argon"].Capabilities["output_continuation"] {
		t.Fatal("playground variant should keep native output_continuation")
	}
	if ids["build/gemini-4-argon"].Capabilities["output_continuation"] {
		t.Fatal("build variant must not advertise Playground output_continuation")
	}
	if ids["build/anti-truncation/gemini-4-argon"].Capabilities["output_continuation"] {
		t.Fatal("build anti variant must not advertise Playground output_continuation")
	}
}

func catalogIDs(models []aistudio.Model) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids
}

// TestLookupPublicModelValidatesEligibility 验证前缀查找按通道与 anti 能力判定。
func TestLookupPublicModelValidatesEligibility(t *testing.T) {
	buildOnly := routingTestModel()
	buildOnly.ID = "build-only"
	buildOnly.Channels = []string{"build"}
	noTools := aistudio.Model{
		ID: "no-tools", Methods: []string{"generateContent"},
		Capabilities: map[string]bool{}, Channels: []string{"playground", "build"},
	}
	models := []aistudio.Model{routingTestModel(), buildOnly, noTools}

	cases := []struct {
		id      string
		wantID  string
		channel aistudio.Channel
		anti    bool
		ok      bool
	}{
		{id: "gemini-4-argon", wantID: "gemini-4-argon", ok: true},
		{id: "playground/gemini-4-argon", wantID: "playground/gemini-4-argon", channel: aistudio.ChannelPlayground, ok: true},
		{id: "build/anti-truncation/gemini-4-argon", wantID: "build/anti-truncation/gemini-4-argon", channel: aistudio.ChannelBuild, anti: true, ok: true},
		{id: "playground/gemini-4-argon-latest", wantID: "playground/gemini-4-argon-latest", channel: aistudio.ChannelPlayground, ok: true},
		{id: "playground/build-only", ok: false},
		{id: "build/build-only", wantID: "build/build-only", channel: aistudio.ChannelBuild, ok: true},
		{id: "playground/anti-truncation/no-tools", ok: false},
		{id: "build/no-tools", wantID: "build/no-tools", channel: aistudio.ChannelBuild, ok: true},
		{id: "playground/missing", ok: false},
		{id: "auto/gemini-4-argon", ok: false},
		{id: "anti-truncation/gemini-4-argon", ok: false},
	}
	for _, testCase := range cases {
		model, ok := LookupPublicModel(models, testCase.id)
		if ok != testCase.ok {
			t.Fatalf("LookupPublicModel(%q) ok = %v, want %v", testCase.id, ok, testCase.ok)
		}
		if !ok {
			continue
		}
		if model.ID != testCase.wantID {
			t.Fatalf("LookupPublicModel(%q).ID = %q, want %q", testCase.id, model.ID, testCase.wantID)
		}
		if testCase.channel != "" {
			if len(model.Channels) != 1 || model.Channels[0] != string(testCase.channel) {
				t.Fatalf("LookupPublicModel(%q).Channels = %v, want [%s]", testCase.id, model.Channels, testCase.channel)
			}
		}
	}
}

// TestChatPrefixRouting 验证 Chat 三协议前缀在 HTTP 层解析进 Selection 且基础模型不带前缀。
func TestChatPrefixRouting(t *testing.T) {
	service := &fakeService{models: []aistudio.Model{routingTestModel()}}
	handler := newRoutingTestHandler(t, service)

	recorder := doJSON(t, handler, http.MethodPost, "/v1/chat/completions",
		`{"model":"playground/anti-truncation/gemini-4-argon","messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	request := service.lastGenerate(t)
	if request.Model != "gemini-4-argon" {
		t.Fatalf("generate model = %q, want base gemini-4-argon", request.Model)
	}
	if request.Selection.RequestedModel != "playground/anti-truncation/gemini-4-argon" ||
		request.Selection.Channel != aistudio.ChannelPlayground || !request.Selection.AntiTruncation {
		t.Fatalf("selection = %#v", request.Selection)
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["model"] != "playground/anti-truncation/gemini-4-argon" {
		t.Fatalf("response model = %v, want prefixed round-trip", response["model"])
	}

	// 裸名保持 auto：不设置通道政策。
	recorder = doJSON(t, handler, http.MethodPost, "/v1/chat/completions",
		`{"model":"gemini-4-argon","messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("bare status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	request = service.lastGenerate(t)
	if request.Model != "gemini-4-argon" || request.Selection.Strict() {
		t.Fatalf("bare selection = %#v model = %q", request.Selection, request.Model)
	}
}

// TestChatStreamPrefixRouting 验证 Chat 流式路径同样携带前缀政策并回显前缀。
func TestChatStreamPrefixRouting(t *testing.T) {
	service := &fakeService{models: []aistudio.Model{routingTestModel()}}
	handler := newRoutingTestHandler(t, service)
	recorder := doJSON(t, handler, http.MethodPost, "/v1/chat/completions",
		`{"model":"playground/gemini-4-argon","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "[DONE]") {
		t.Fatalf("stream missing DONE: %s", recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"model":"playground/gemini-4-argon"`) {
		t.Fatalf("stream chunk model missing prefix: %s", recorder.Body.String())
	}
	request := service.lastGenerate(t)
	if request.Model != "gemini-4-argon" || request.Selection.Channel != aistudio.ChannelPlayground {
		t.Fatalf("stream selection = %#v model = %q", request.Selection, request.Model)
	}
}

// TestGeminiStreamPrefixRouting 验证 Gemini streamGenerateContent 携带前缀政策并回显前缀。
func TestGeminiStreamPrefixRouting(t *testing.T) {
	service := &fakeService{models: []aistudio.Model{routingTestModel()}}
	handler := newRoutingTestHandler(t, service)
	recorder := doJSON(t, handler, http.MethodPost, "/v1beta/models/build/gemini-4-argon:streamGenerateContent?alt=sse",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"modelVersion":"build/gemini-4-argon"`) {
		t.Fatalf("gemini stream modelVersion missing prefix: %s", recorder.Body.String())
	}
	request := service.lastGenerate(t)
	if request.Model != "gemini-4-argon" || request.Selection.Channel != aistudio.ChannelBuild {
		t.Fatalf("gemini stream selection = %#v model = %q", request.Selection, request.Model)
	}
}

// TestMalformedPrefixFailsLocally 验证保留前缀的畸形组合在本地失败，不落到 auto。
func TestMalformedPrefixFailsLocally(t *testing.T) {
	service := &fakeService{models: []aistudio.Model{routingTestModel()}}
	handler := newRoutingTestHandler(t, service)
	for _, model := range []string{
		"playground/", "playground/anti-truncation/", "playground/anti-truncation/auto",
		"auto/gemini-4-argon", "anti-truncation/gemini-4-argon", "playground/build/gemini-4-argon",
	} {
		recorder := doJSON(t, handler, http.MethodPost, "/v1/chat/completions",
			`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("model %q status = %d, want 400 body = %s", model, recorder.Code, recorder.Body.String())
		}
	}
	if service.generateCount() != 0 {
		t.Fatalf("malformed prefixes reached Generate %d times", service.generateCount())
	}
}

// TestResponsesPrefixRouting 验证 Responses 生成、后台与压缩触发的生成都携带选择政策。
func TestResponsesPrefixRouting(t *testing.T) {
	service := &fakeService{models: []aistudio.Model{routingTestModel()}}
	handler := newRoutingTestHandler(t, service)

	recorder := doJSON(t, handler, http.MethodPost, "/v1/responses",
		`{"model":"build/gemini-4-argon","input":"hello","store":true}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	request := service.lastGenerate(t)
	if request.Model != "gemini-4-argon" || request.Selection.Channel != aistudio.ChannelBuild {
		t.Fatalf("responses selection = %#v model = %q", request.Selection, request.Model)
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["model"] != "build/gemini-4-argon" {
		t.Fatalf("response model = %v, want prefixed", response["model"])
	}
	responseID, _ := response["id"].(string)
	if responseID == "" {
		t.Fatal("response id missing")
	}

	// input_tokens 继承前序响应前缀，但辅助入口用基础模型。
	recorder = doJSON(t, handler, http.MethodPost, "/v1/responses/input_tokens",
		`{"previous_response_id":"`+responseID+`","input":"more"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("input_tokens status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if got := service.lastCount(t).Model; got != "gemini-4-argon" {
		t.Fatalf("countTokens model = %q, want base gemini-4-argon", got)
	}

	// 后台生成仍携带选择政策。
	recorder = doJSON(t, handler, http.MethodPost, "/v1/responses",
		`{"model":"playground/gemini-4-argon","input":"hello","background":true}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("background status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	waitForBackground(t, &service.mu, func() bool {
		service.mu.Lock()
		defer service.mu.Unlock()
		return len(service.generateReqs) >= 2
	})
	service.mu.Lock()
	background := service.generateReqs[len(service.generateReqs)-1]
	service.mu.Unlock()
	if background.Selection.Channel != aistudio.ChannelPlayground || background.Model != "gemini-4-argon" {
		t.Fatalf("background selection = %#v model = %q", background.Selection, background.Model)
	}

	// 压缩触发的摘要生成继承同一选择政策。
	server := &server{service: service, thoughtSignatures: newThoughtSignatureStore()}
	compactionRequest := responsesRequest{Model: "build/anti-truncation/gemini-4-argon"}
	if _, _, err := server.compactionSummary(context.Background(), compactionRequest, []json.RawMessage{json.RawMessage(`{"role":"user","content":"hi"}`)}, "", "cmp"); err != nil {
		t.Fatalf("compactionSummary error: %v", err)
	}
	compaction := service.lastGenerate(t)
	if compaction.Model != "gemini-4-argon" || compaction.Selection.Channel != aistudio.ChannelBuild || !compaction.Selection.AntiTruncation {
		t.Fatalf("compaction selection = %#v model = %q", compaction.Selection, compaction.Model)
	}
}

// waitForBackground 在有界时间内等待条件成立，避免测试挂起。
func waitForBackground(t *testing.T, _ *sync.Mutex, condition func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition was not met in time")
}

// TestResponsesGetAndCancelDoNotGenerate 验证获取/取消已有响应不重新生成。
func TestResponsesGetAndCancelDoNotGenerate(t *testing.T) {
	service := &fakeService{models: []aistudio.Model{routingTestModel()}}
	handler := newRoutingTestHandler(t, service)
	recorder := doJSON(t, handler, http.MethodPost, "/v1/responses",
		`{"model":"build/gemini-4-argon","input":"hello","store":true,"background":true}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	id, _ := response["id"].(string)
	waitForBackground(t, &service.mu, func() bool { return service.generateCount() >= 1 })
	before := service.generateCount()
	doJSON(t, handler, http.MethodGet, "/v1/responses/"+id, "")
	doJSON(t, handler, http.MethodPost, "/v1/responses/"+id+"/cancel", "")
	if after := service.generateCount(); after != before {
		t.Fatalf("get/cancel triggered generation: before %d after %d", before, after)
	}
}

// TestGeminiPrefixRouting 验证 Gemini 原生 generate/streamGenerate/countTokens 与前缀 round-trip。
func TestGeminiPrefixRouting(t *testing.T) {
	service := &fakeService{models: []aistudio.Model{routingTestModel()}}
	handler := newRoutingTestHandler(t, service)

	recorder := doJSON(t, handler, http.MethodPost, "/v1beta/models/playground/anti-truncation/gemini-4-argon:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	request := service.lastGenerate(t)
	if request.Model != "gemini-4-argon" || request.Selection.Channel != aistudio.ChannelPlayground || !request.Selection.AntiTruncation {
		t.Fatalf("gemini selection = %#v model = %q", request.Selection, request.Model)
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["modelVersion"] != "playground/anti-truncation/gemini-4-argon" {
		t.Fatalf("modelVersion = %v, want prefixed round-trip", response["modelVersion"])
	}

	// URL 编码的斜杠前缀同样解析。
	recorder = doJSON(t, handler, http.MethodPost, "/v1beta/models/build%2Fgemini-4-argon:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("encoded status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	request = service.lastGenerate(t)
	if request.Selection.Channel != aistudio.ChannelBuild || request.Model != "gemini-4-argon" {
		t.Fatalf("encoded selection = %#v model = %q", request.Selection, request.Model)
	}

	// countTokens 是辅助入口：用基础模型，不当作跨通道生成。
	recorder = doJSON(t, handler, http.MethodPost, "/v1beta/models/playground/gemini-4-argon:countTokens",
		`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("countTokens status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if got := service.lastCount(t).Model; got != "gemini-4-argon" {
		t.Fatalf("gemini countTokens model = %q, want base", got)
	}
}

// TestGeminiModelGetAndListRecognizePrefix 验证模型 get/list 暴露可真实选择的前缀 ID。
func TestGeminiModelGetAndListRecognizePrefix(t *testing.T) {
	service := &fakeService{models: []aistudio.Model{routingTestModel()}}
	handler := newRoutingTestHandler(t, service)

	recorder := doJSON(t, handler, http.MethodGet, "/v1beta/models/playground/gemini-4-argon", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("get status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	var model map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &model); err != nil {
		t.Fatal(err)
	}
	if model["name"] != "models/playground/gemini-4-argon" {
		t.Fatalf("model name = %v", model["name"])
	}

	recorder = doJSON(t, handler, http.MethodGet, "/v1beta/models", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("list status = %d", recorder.Code)
	}
	var list struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if !containsModelName(list.Models, "models/playground/anti-truncation/gemini-4-argon") {
		t.Fatalf("gemini list missing anti variant: %v", list.Models)
	}

	// 不具备函数声明能力的基础模型不能暴露 anti 前缀。
	noTools := aistudio.Model{ID: "no-tools", Methods: []string{"generateContent"}, Capabilities: map[string]bool{}, Channels: []string{"playground"}}
	service.models = []aistudio.Model{noTools}
	recorder = doJSON(t, handler, http.MethodGet, "/v1beta/models/playground/anti-truncation/no-tools", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("ineligible anti status = %d body = %s", recorder.Code, recorder.Body.String())
	}
}

func containsModelName(models []map[string]any, name string) bool {
	for _, model := range models {
		if model["name"] == name {
			return true
		}
	}
	return false
}

// TestOpenAIModelListExposesPrefixes 验证 OpenAI 模型目录给出可真实选择的前缀 ID。
func TestOpenAIModelListExposesPrefixes(t *testing.T) {
	service := &fakeService{models: []aistudio.Model{routingTestModel()}}
	handler := newRoutingTestHandler(t, service)
	recorder := doJSON(t, handler, http.MethodGet, "/v1/models", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if !containsModelID(list.Data, "playground/gemini-4-argon") || !containsModelID(list.Data, "build/anti-truncation/gemini-4-argon") {
		t.Fatalf("openai list missing prefixed IDs: %v", list.Data)
	}
}

func containsModelID(models []map[string]any, id string) bool {
	for _, model := range models {
		if model["id"] == id {
			return true
		}
	}
	return false
}

// TestUncoveredRoutesRejectPrefixes 验证未覆盖入口明确拒绝保留前缀并保留旧裸名行为。
func TestUncoveredRoutesRejectPrefixes(t *testing.T) {
	service := &fakeService{models: []aistudio.Model{routingTestModel()}}
	handler := newRoutingTestHandler(t, service)

	recorder := doJSON(t, handler, http.MethodPost, "/v1/messages",
		`{"model":"playground/gemini-4-argon","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("anthropic status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "does not support") {
		t.Fatalf("anthropic error = %s", recorder.Body.String())
	}

	recorder = doJSON(t, handler, http.MethodPost, "/v1/images/generations",
		`{"model":"playground/gemini-4-argon","prompt":"cat"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("images status = %d body = %s", recorder.Code, recorder.Body.String())
	}

	recorder = doJSON(t, handler, http.MethodPost, "/v1beta/models/playground/gemini-4-argon:embedContent",
		`{"content":{"parts":[{"text":"hi"}]}}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("embed status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	if service.generateCount() != 0 {
		t.Fatalf("uncovered routes reached Generate %d times", service.generateCount())
	}
}

// TestStrictModelPreservesFinishReasonAndUsage 验证严格模型如实传递 length 与 usage，不改写为正常 stop。
func TestStrictModelPreservesFinishReasonAndUsage(t *testing.T) {
	service := &fakeService{
		models: []aistudio.Model{routingTestModel()},
		generateEvents: []aistudio.Event{
			{Kind: aistudio.EventText, Text: "partial", Channel: aistudio.ChannelBuild},
			{Kind: aistudio.EventUsage, Usage: &aistudio.Usage{InputTokens: 3, OutputTokens: 9, TotalTokens: 12}, Channel: aistudio.ChannelBuild},
			{Kind: aistudio.EventFinish, FinishReason: "length", Channel: aistudio.ChannelBuild},
		},
	}
	handler := newRoutingTestHandler(t, service)
	recorder := doJSON(t, handler, http.MethodPost, "/v1/chat/completions",
		`{"model":"build/gemini-4-argon","messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	choices, _ := response["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %v", response["choices"])
	}
	choice, _ := choices[0].(map[string]any)
	if choice["finish_reason"] != "length" {
		t.Fatalf("finish_reason = %v, want length", choice["finish_reason"])
	}
	usage, _ := response["usage"].(map[string]any)
	if usage == nil || usage["completion_tokens"].(float64) == 0 {
		t.Fatalf("usage was dropped: %v", response["usage"])
	}
}

// TestStrictModelMissingFinishDoesNotFakeSuccess 验证缺 finish 的上游 EOF 不输出伪成功。
func TestStrictModelMissingFinishDoesNotFakeSuccess(t *testing.T) {
	service := &fakeService{
		models: []aistudio.Model{routingTestModel()},
		generateEvents: []aistudio.Event{
			{Kind: aistudio.EventText, Text: "partial", Channel: aistudio.ChannelBuild},
			{Kind: aistudio.EventUsage, Usage: &aistudio.Usage{InputTokens: 3, OutputTokens: 9, TotalTokens: 12}, Channel: aistudio.ChannelBuild},
		},
	}
	handler := newRoutingTestHandler(t, service)
	recorder := doJSON(t, handler, http.MethodPost, "/v1/chat/completions",
		`{"model":"build/gemini-4-argon","messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code == http.StatusOK {
		t.Fatalf("missing finish must not return success: %s", recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "\"finish_reason\":\"stop\"") {
		t.Fatalf("missing finish faked a normal stop: %s", recorder.Body.String())
	}
}

// TestCrossChannelOpaqueStateRejected 验证带签名历史不能跨严格通道续接，纯文本可移植。
func TestCrossChannelOpaqueStateRejected(t *testing.T) {
	signature := aistudio.Content{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{{ThoughtSignature: "sig"}}}
	previous := responseHistory{ID: "resp_1", Channel: "build", Contents: []aistudio.Content{signature}}
	if err := crossChannelOpaqueStateError(previous, aistudio.ModelSelection{BaseModel: "m", Channel: aistudio.ChannelPlayground}); err == nil {
		t.Fatal("cross-channel signature continuation should fail")
	}
	portable := responseHistory{ID: "resp_2", Channel: "build", Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}}}
	if err := crossChannelOpaqueStateError(portable, aistudio.ModelSelection{BaseModel: "m", Channel: aistudio.ChannelPlayground}); err != nil {
		t.Fatalf("portable text should continue across channels: %v", err)
	}
	if err := crossChannelOpaqueStateError(previous, aistudio.ModelSelection{BaseModel: "m"}); err != nil {
		t.Fatalf("auto continuation should not be blocked: %v", err)
	}
}
