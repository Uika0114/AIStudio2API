package api

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func TestChatRestoresStoredThoughtSignature(t *testing.T) {
	store := newThoughtSignatureStore()
	store.Remember([]aistudio.FunctionCall{{ID: "call_sig_1", Name: "get_weather", Arguments: json.RawMessage(`{"city":"Berlin"}`), ThoughtSignature: "sig-abc"}}, aistudio.ModelSelection{}, "", "")

	var request chatRequest
	raw := `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_sig_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Berlin\"}"}}]}]}`
	if err := json.Unmarshal([]byte(raw), &request); err != nil {
		t.Fatal(err)
	}
	generate, err := request.toGenerateRequest("id")
	if err != nil {
		t.Fatal(err)
	}
	store.Restore(generate.Contents, generate.Selection)
	if got := generate.Contents[1].Parts[0].FunctionCall.ThoughtSignature; got != "sig-abc" {
		t.Fatalf("stored signature not restored: got %q", got)
	}

	request.Messages[1].ToolCalls[0].ExtraContent.Google.ThoughtSignature = "sig-from-client"
	generate, err = request.toGenerateRequest("id")
	if err != nil {
		t.Fatal(err)
	}
	store.Restore(generate.Contents, generate.Selection)
	if got := generate.Contents[1].Parts[0].FunctionCall.ThoughtSignature; got != "sig-from-client" {
		t.Fatalf("client-supplied signature must win: got %q", got)
	}
}

// TestStrictThoughtSignatureIsolatedByChannel 验证严格请求只补同基础模型、同实际通道的来源。
func TestStrictThoughtSignatureIsolatedByChannel(t *testing.T) {
	call := aistudio.FunctionCall{ID: "call_iso", Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`), ThoughtSignature: "sig-build"}
	store := newThoughtSignatureStore()
	store.Remember([]aistudio.FunctionCall{call}, aistudio.ModelSelection{BaseModel: "gemini-4-argon", Channel: aistudio.ChannelBuild}, aistudio.ChannelBuild, "gemini-4-argon")

	buildSelection := aistudio.ModelSelection{BaseModel: "gemini-4-argon", Channel: aistudio.ChannelBuild}
	playgroundSelection := aistudio.ModelSelection{BaseModel: "gemini-4-argon", Channel: aistudio.ChannelPlayground}
	autoSelection := aistudio.ModelSelection{BaseModel: "gemini-4-argon"}

	content := func() []aistudio.Content {
		restored := call
		restored.ThoughtSignature = ""
		return []aistudio.Content{{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{{FunctionCall: &restored}}}}
	}

	// 同通道同基础模型：补全。
	same := content()
	store.Restore(same, buildSelection)
	if got := same[0].Parts[0].FunctionCall.ThoughtSignature; got != "sig-build" {
		t.Fatalf("same channel/base signature should be restored: got %q", got)
	}
	// 另一通道：绝不盲填。
	other := content()
	store.Restore(other, playgroundSelection)
	if got := other[0].Parts[0].FunctionCall.ThoughtSignature; got != "" {
		t.Fatalf("other channel signature must not be restored: got %q", got)
	}
	// 另一基础模型：绝不盲填。
	otherBase := aistudio.ModelSelection{BaseModel: "gemini-3.8-flash", Channel: aistudio.ChannelBuild}
	other = content()
	store.Restore(other, otherBase)
	if got := other[0].Parts[0].FunctionCall.ThoughtSignature; got != "" {
		t.Fatalf("other base signature must not be restored: got %q", got)
	}
	// auto（裸名）保留旧的通道无关缓存兼容，但严格来源不写入旧缓存。
	auto := content()
	store.Restore(auto, autoSelection)
	if got := auto[0].Parts[0].FunctionCall.ThoughtSignature; got != "" {
		t.Fatalf("strict origin must not leak into auto restore: got %q", got)
	}
}

// TestStrictThoughtSignatureMatchesProviderModelBase 验证别名请求生成的签名
// 可由 canonical 基础模型请求在同一通道补回，但不跨通道。
func TestStrictThoughtSignatureMatchesProviderModelBase(t *testing.T) {
	call := aistudio.FunctionCall{ID: "call_alias", Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`), ThoughtSignature: "sig-alias"}
	store := newThoughtSignatureStore()
	store.Remember([]aistudio.FunctionCall{call}, aistudio.ModelSelection{BaseModel: "my-alias", Channel: aistudio.ChannelBuild}, aistudio.ChannelBuild, "gemini-4-argon")

	restored := call
	restored.ThoughtSignature = ""
	content := []aistudio.Content{{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{{FunctionCall: &restored}}}}
	store.Restore(content, aistudio.ModelSelection{BaseModel: "gemini-4-argon", Channel: aistudio.ChannelBuild})
	if got := content[0].Parts[0].FunctionCall.ThoughtSignature; got != "sig-alias" {
		t.Fatalf("canonical base restore = %q, want sig-alias", got)
	}

	other := call
	other.ThoughtSignature = ""
	content = []aistudio.Content{{Role: aistudio.RoleAssistant, Parts: []aistudio.Part{{FunctionCall: &other}}}}
	store.Restore(content, aistudio.ModelSelection{BaseModel: "gemini-4-argon", Channel: aistudio.ChannelPlayground})
	if got := content[0].Parts[0].FunctionCall.ThoughtSignature; got != "" {
		t.Fatalf("other channel must not restore alias signature: got %q", got)
	}
}

func TestThoughtSignatureStoreIsBounded(t *testing.T) {
	store := newThoughtSignatureStore()
	for i := 0; i < thoughtSignatureCapacity+10; i++ {
		store.Remember([]aistudio.FunctionCall{{ID: fmt.Sprintf("call_%d", i), Name: "f", Arguments: json.RawMessage(`{}`), ThoughtSignature: "s"}}, aistudio.ModelSelection{}, "", "")
	}
	if len(store.legacy) != thoughtSignatureCapacity || len(store.legacyOrder) != thoughtSignatureCapacity {
		t.Fatalf("store size = %d/%d, want %d", len(store.legacy), len(store.legacyOrder), thoughtSignatureCapacity)
	}
}
