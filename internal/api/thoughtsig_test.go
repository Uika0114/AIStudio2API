package api

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
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

func TestThoughtSignatureKeyNormalization(t *testing.T) {
	for _, tc := range []struct {
		name, left, right string
		equal             bool
	}{
		{"object_order_and_space", " {\"b\":2, \"a\":1} ", "{\"a\":1,\"b\":2}", true},
		{"nested_objects", "{\"x\":[{\"b\":2,\"a\":1}]}", "{\"x\":[{\"a\":1,\"b\":2}]}", true},
		{"array_order", "[1,2]", "[2,1]", false},
		{"integer_decimal", "1", "1.0", false},
		{"integer_exponent", "1", "1e0", false},
		{"exponent_spelling", "1e+0", "1e0", false},
		{"large_integers", "9007199254740992", "9007199254740993", false},
		{"negative_zero", "-0", "0", false},
		{"duplicate_last_wins", "{\"a\":1,\"a\":2}", "{\"a\":2}", true},
		{"duplicate_not_first", "{\"a\":1,\"a\":2}", "{\"a\":1}", false},
		{"trailing_value", "{\"a\":1}{\"a\":2}", "{\"a\":1}", true},
		{"trailing_junk", "{\"a\":1}garbage", "{\"a\":1}", true},
		{"leading_zero_one_decode", "01", "0", true},
		{"unicode_escape", "\"\\u4f60\\u597d\"", "\"你好\"", true},
		{"html_escape", "\"\\u003c\"", "\"<\"", true},
		{"invalid_same", "{\"a\":", "{\"a\":", true},
		{"invalid_different", "{\"a\":", "{\"a\": ", false},
		{"invalid_trailing_comma", "{\"a\":1,}", "{\"a\":1}", false},
		{"empty_raw_space", "", " ", false},
		{"null_empty_raw", "null", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			left := aistudio.FunctionCall{ID: "call", Name: "lookup", Arguments: json.RawMessage(tc.left)}
			right := left
			right.Arguments = json.RawMessage(tc.right)
			if equal := thoughtSignatureKey(left) == thoughtSignatureKey(right); equal != tc.equal {
				t.Fatalf("key equality = %t, want %t", equal, tc.equal)
			}
		})
	}
}

func TestThoughtSignatureStoreMatchesFullCall(t *testing.T) {
	store := newThoughtSignatureStore()
	call := aistudio.FunctionCall{ID: "call", Name: "lookup", Arguments: json.RawMessage("{\"a\":1,\"b\":2}"), ThoughtSignature: "stored"}
	store.Remember([]aistudio.FunctionCall{call}, aistudio.ModelSelection{}, "", "")
	for _, tc := range []struct {
		name, id, function, arguments, want string
	}{
		{"normalized", "call", "lookup", " {\"b\":2,\"a\":1} ", "stored"},
		{"different_id", "other", "lookup", "{\"a\":1,\"b\":2}", ""},
		{"different_function", "call", "other", "{\"a\":1,\"b\":2}", ""},
		{"different_arguments", "call", "lookup", "{\"a\":2,\"b\":2}", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			history := aistudio.FunctionCall{ID: tc.id, Name: tc.function, Arguments: json.RawMessage(tc.arguments)}
			store.Restore([]aistudio.Content{{Parts: []aistudio.Part{{FunctionCall: &history}}}}, aistudio.ModelSelection{})
			if history.ThoughtSignature != tc.want {
				t.Fatalf("signature = %q, want %q", history.ThoughtSignature, tc.want)
			}
		})
	}
}

func TestThoughtSignatureStoreUpdateAndEviction(t *testing.T) {
	store := newThoughtSignatureStore()
	selection := aistudio.ModelSelection{BaseModel: "model"}
	call := func(i int) aistudio.FunctionCall {
		return aistudio.FunctionCall{ID: fmt.Sprintf("call_%d", i), Name: "f", Arguments: json.RawMessage("{}"), ThoughtSignature: fmt.Sprintf("sig_%d", i)}
	}
	for i := 0; i < thoughtSignatureCapacity; i++ {
		store.Remember([]aistudio.FunctionCall{call(i)}, selection, aistudio.ChannelBuild, "model")
	}
	updated := call(0)
	updated.Arguments = json.RawMessage(" { } ")
	updated.ThoughtSignature = call(1).ThoughtSignature
	store.Remember([]aistudio.FunctionCall{updated}, selection, aistudio.ChannelBuild, "model")
	firstKey := thoughtSignatureKey(call(0))
	firstRouted := routedThoughtSignatureKey("model", "build", firstKey)
	if store.legacy[firstKey] != updated.ThoughtSignature || store.routed[firstRouted] != updated.ThoughtSignature {
		t.Fatal("existing entry was not updated")
	}
	if len(store.legacyOrder) != thoughtSignatureCapacity || len(store.routedOrder) != thoughtSignatureCapacity || len(store.originOrder) != thoughtSignatureCapacity {
		t.Fatal("update appended a duplicate entry")
	}
	if store.legacyOrder[0] != firstKey || store.routedOrder[0] != firstRouted {
		t.Fatal("update changed insertion order")
	}
	store.Remember([]aistudio.FunctionCall{call(thoughtSignatureCapacity)}, selection, aistudio.ChannelBuild, "model")
	if _, ok := store.legacy[firstKey]; ok {
		t.Fatal("oldest legacy entry survived eviction")
	}
	if _, ok := store.routed[firstRouted]; ok {
		t.Fatal("oldest routed entry survived eviction")
	}
	if _, ok := store.origins[sha256.Sum256([]byte(call(0).ThoughtSignature))]; ok {
		t.Fatal("oldest provenance survived eviction")
	}
	for name, size := range map[string]int{
		"legacy": len(store.legacy), "legacy_order": len(store.legacyOrder),
		"routed": len(store.routed), "routed_order": len(store.routedOrder),
		"origins": len(store.origins), "origin_order": len(store.originOrder),
	} {
		if size != thoughtSignatureCapacity {
			t.Fatalf("%s size = %d, want %d", name, size, thoughtSignatureCapacity)
		}
	}
}

func TestThoughtSignatureStoreConcurrentRestore(t *testing.T) {
	store := newThoughtSignatureStore()
	selection := aistudio.ModelSelection{BaseModel: "model", Channel: aistudio.ChannelBuild}
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Go(func() {
			for i := 0; i < 32; i++ {
				call := aistudio.FunctionCall{ID: fmt.Sprintf("call_%d_%d", worker, i), Name: "f", Arguments: json.RawMessage("{\"b\":2,\"a\":1}"), ThoughtSignature: fmt.Sprintf("sig_%d_%d", worker, i)}
				store.RememberEvent(aistudio.Event{ToolCall: &call, Channel: aistudio.ChannelBuild, ProviderModel: "model"}, selection)
				call.Arguments = json.RawMessage("{\"a\":1,\"b\":2}")
				want := call.ThoughtSignature
				call.ThoughtSignature = ""
				contents := []aistudio.Content{{Parts: []aistudio.Part{{FunctionCall: &call}}}}
				if err := store.RestoreChecked(contents, selection); err != nil || call.ThoughtSignature != want {
					t.Errorf("restore = %q, %v; want %q", call.ThoughtSignature, err, want)
					return
				}
				call.ThoughtSignature = ""
				if err := store.RestoreChecked(contents, aistudio.ModelSelection{BaseModel: "model"}); err == nil {
					t.Error("strict-origin call escaped into auto")
					return
				}
			}
		})
	}
	workers.Wait()
}

func TestThoughtSignatureKeyDigestBounded(t *testing.T) {
	for _, size := range []int{0, 1 << 10, 16 << 10, 64 << 10} {
		for _, valid := range []bool{false, true} {
			t.Run(fmt.Sprintf("bytes_%d/valid_%t", size, valid), func(t *testing.T) {
				arguments := strings.Repeat("x", size)
				if valid {
					arguments = fmt.Sprintf("{\"payload\":%q}", arguments)
				}
				call := aistudio.FunctionCall{ID: "call", Name: "lookup", Arguments: json.RawMessage(arguments), ThoughtSignature: "signature"}
				prefix := call.ID + "\x00" + call.Name + "\x00"
				want := prefix + fmt.Sprintf("%x", sha256.Sum256([]byte(arguments)))
				key := thoughtSignatureKey(call)
				if key != want || len(key) != len(prefix)+64 {
					t.Fatalf("key must retain ID/name and the full SHA-256 digest; length = %d", len(key))
				}
				store := newThoughtSignatureStore()
				store.Remember([]aistudio.FunctionCall{call}, aistudio.ModelSelection{BaseModel: "model"}, aistudio.ChannelBuild, "model")
				routed := routedThoughtSignatureKey("model", "build", key)
				if store.legacyOrder[0] != key || store.routedOrder[0] != routed || store.legacy[key] != "signature" || store.routed[routed] != "signature" {
					t.Fatal("cache map/order must use the bounded key")
				}
			})
		}
	}
}

func TestThoughtSignatureRestoreCheckedProvenance(t *testing.T) {
	store := newThoughtSignatureStore()
	selection := aistudio.ModelSelection{BaseModel: "model", Channel: aistudio.ChannelBuild}
	original := aistudio.FunctionCall{ID: "call", Name: "lookup", Arguments: json.RawMessage("{\"a\":1}"), ThoughtSignature: "stored"}
	explicit := original
	explicit.ID, explicit.ThoughtSignature = "other", "explicit"
	store.Remember([]aistudio.FunctionCall{original, explicit}, selection, aistudio.ChannelBuild, "canonical")
	for _, tc := range []struct {
		name, base, channel, function, arguments, callSignature, partSignature, want string
		wantError                                                                    bool
	}{
		{"same", "model", "build", "lookup", " {\"a\":1} ", "", "", "stored", false},
		{"canonical", "canonical", "build", "lookup", "{\"a\":1}", "", "", "stored", false},
		{"cross_base", "other", "build", "lookup", "{\"a\":1}", "", "", "", true},
		{"cross_channel", "model", "playground", "lookup", "{\"a\":1}", "", "", "", true},
		{"strict_to_auto", "model", "", "lookup", "{\"a\":1}", "", "", "", true},
		{"explicit_wins", "model", "build", "lookup", "{\"a\":1}", "explicit", "", "explicit", false},
		{"conflict", "model", "build", "lookup", "{\"a\":1}", "explicit", "stored", "explicit", true},
		{"unknown", "model", "build", "lookup", "{\"a\":1}", "unknown", "", "unknown", true},
		{"different_arguments", "model", "playground", "lookup", "{\"a\":2}", "", "", "", false},
		{"different_function", "model", "playground", "other", "{\"a\":1}", "", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call := aistudio.FunctionCall{ID: "call", Name: tc.function, Arguments: json.RawMessage(tc.arguments), ThoughtSignature: tc.callSignature}
			contents := []aistudio.Content{{Parts: []aistudio.Part{{FunctionCall: &call, ThoughtSignature: tc.partSignature}}}}
			err := store.RestoreChecked(contents, aistudio.ModelSelection{BaseModel: tc.base, Channel: aistudio.Channel(tc.channel)})
			if (err != nil) != tc.wantError || call.ThoughtSignature != tc.want {
				t.Fatalf("restore = %q, %v; want %q, error=%t", call.ThoughtSignature, err, tc.want, tc.wantError)
			}
		})
	}
}
