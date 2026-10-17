package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// thoughtSignatureCapacity 是工具调用思考签名的保留条数。
const thoughtSignatureCapacity = 4096

// thoughtSignatureStore 保存本进程响应中工具调用的思考签名。
// auto（裸名）请求只使用旧的、与通道无关的缓存以保持兼容；严格请求
// 只按 canonical base + actual channel 隔离的来源补全，不会用另一个通道
// 或来源未知的缓存盲填。
type thoughtSignatureStore struct {
	mu          sync.Mutex
	legacy      map[string]string
	legacyOrder []string
	routed      map[string]string
	routedOrder []string
	origins     map[[32]byte][]thoughtSignatureOrigin
	originOrder [][32]byte
}

type thoughtSignatureOrigin struct {
	base    string
	channel string
	strict  bool
}

func newThoughtSignatureStore() *thoughtSignatureStore {
	return &thoughtSignatureStore{legacy: make(map[string]string), routed: make(map[string]string), origins: make(map[[32]byte][]thoughtSignatureOrigin)}
}

// Remember 保存一次响应中带签名的工具调用。selection 给出请求的基础模型与
// 通道政策，channel 是核心报告的实际通道，providerModel 是核心报告的实际
// 上游基础模型；来源未知（空通道）时不写路由缓存。base 与 providerModel 都
// 记入路由缓存，使别名与 canonical 之间切换时仍能按同通道命中，但不跨通道。
func (store *thoughtSignatureStore) Remember(calls []aistudio.FunctionCall, selection aistudio.ModelSelection, channel aistudio.Channel, providerModel string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	actual := strings.TrimSpace(string(channel))
	bases := make([]string, 0, 2)
	for _, candidate := range []string{strings.TrimSpace(selection.BaseModel), strings.TrimSpace(providerModel)} {
		if candidate == "" || slices.Contains(bases, candidate) {
			continue
		}
		bases = append(bases, candidate)
	}
	for _, call := range calls {
		if call.ThoughtSignature == "" {
			continue
		}
		store.rememberOriginsLocked(call.ThoughtSignature, bases, actual, selection.Strict())
		if call.ID == "" {
			continue
		} // Gemini IDs are optional; provenance is not.
		key := thoughtSignatureKey(call)
		if actual != "" {
			for _, base := range bases {
				routedKey := routedThoughtSignatureKey(base, actual, key)
				if _, exists := store.routed[routedKey]; !exists {
					store.routedOrder = append(store.routedOrder, routedKey)
				}
				store.routed[routedKey] = call.ThoughtSignature
			}
		}
		if !selection.Strict() {
			if _, exists := store.legacy[key]; !exists {
				store.legacyOrder = append(store.legacyOrder, key)
			}
			store.legacy[key] = call.ThoughtSignature
		}
	}
	store.trimLocked()
}

// Restore 为客户端未回传签名的历史工具调用补回本进程保存的签名。
// 严格请求只接受同基础模型、同实际通道的来源；auto 请求沿用旧的通道无关缓存。
func (store *thoughtSignatureStore) Restore(contents []aistudio.Content, selection aistudio.ModelSelection) {
	store.mu.Lock()
	defer store.mu.Unlock()
	base := strings.TrimSpace(selection.BaseModel)
	channel := strings.TrimSpace(string(selection.Channel))
	for _, content := range contents {
		for _, part := range content.Parts {
			call := part.FunctionCall
			if call == nil || call.ID == "" || call.ThoughtSignature != "" || part.ThoughtSignature != "" {
				continue
			}
			key := thoughtSignatureKey(*call)
			signature := ""
			if selection.Strict() {
				if base != "" && channel != "" {
					signature = store.routed[routedThoughtSignatureKey(base, channel, key)]
				}
			} else {
				signature = store.legacy[key]
			}
			if signature != "" {
				call.ThoughtSignature = signature
			}
		}
	}
}

// trimLocked 分别限制路由缓存与旧缓存的条数。
func (store *thoughtSignatureStore) trimLocked() {
	for len(store.originOrder) > thoughtSignatureCapacity {
		delete(store.origins, store.originOrder[0])
		store.originOrder = store.originOrder[1:]
	}
	for len(store.routedOrder) > thoughtSignatureCapacity {
		delete(store.routed, store.routedOrder[0])
		store.routedOrder = store.routedOrder[1:]
	}
	for len(store.legacyOrder) > thoughtSignatureCapacity {
		delete(store.legacy, store.legacyOrder[0])
		store.legacyOrder = store.legacyOrder[1:]
	}
}

// routedThoughtSignatureKey 以基础模型、实际通道与调用标识隔离来源。
func routedThoughtSignatureKey(base, channel, callKey string) string {
	return base + "\x00" + channel + "\x00" + callKey
}

// thoughtSignatureKey 以调用 ID、函数名和规范化参数标识一次工具调用。
func thoughtSignatureKey(call aistudio.FunctionCall) string {
	arguments := []byte(call.Arguments)
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) == nil {
		if canonical, err := json.Marshal(value); err == nil {
			arguments = canonical
		}
	}
	return call.ID + "\x00" + call.Name + "\x00" + string(arguments)
}

// RememberEvent sees only public projected events. Internal transport calls and
// their signatures have already been removed by the core.
func (store *thoughtSignatureStore) RememberEvent(event aistudio.Event, selection aistudio.ModelSelection) {
	if event.ToolCall != nil {
		call := *event.ToolCall
		if call.ThoughtSignature == "" {
			call.ThoughtSignature = event.ThoughtSignature
		}
		store.Remember([]aistudio.FunctionCall{call}, selection, event.Channel, event.ProviderModel)
	}
	if event.ThoughtSignature != "" {
		store.mu.Lock()
		store.rememberOriginsLocked(event.ThoughtSignature, []string{selection.BaseModel, event.ProviderModel}, string(event.Channel), selection.Strict())
		store.trimLocked()
		store.mu.Unlock()
	}
}

func (store *thoughtSignatureStore) rememberOriginsLocked(signature string, bases []string, channel string, strict bool) {
	if signature == "" || channel == "" {
		return
	}
	digest := sha256.Sum256([]byte(signature))
	if store.origins == nil {
		store.origins = make(map[[32]byte][]thoughtSignatureOrigin)
	}
	if _, exists := store.origins[digest]; !exists {
		store.originOrder = append(store.originOrder, digest)
	}
	for _, base := range bases {
		base = strings.TrimPrefix(strings.TrimSpace(base), "models/")
		if base == "" {
			continue
		}
		origin := thoughtSignatureOrigin{base: base, channel: channel, strict: strict}
		if !slices.Contains(store.origins[digest], origin) {
			store.origins[digest] = append(store.origins[digest], origin)
		}
	}
}

func opaqueHistoryError(reason string) error {
	return fmt.Errorf("%w: cannot safely reuse upstream thought signature: %s; use its matching explicit model/channel or portable history", aistudio.ErrInvalidArgument, reason)
}

// RestoreChecked validates explicit native signatures as well as omitted OpenAI
// signatures. A missing cache hit is not permission to discard known opaque state.
// Bare-to-bare legacy histories retain their old behavior; known strict-origin
// state cannot be sent through unpinned auto.
func (store *thoughtSignatureStore) RestoreChecked(contents []aistudio.Content, selection aistudio.ModelSelection) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	base, channel := strings.TrimSpace(selection.BaseModel), string(selection.Channel)
	for _, content := range contents {
		for _, part := range content.Parts {
			call := part.FunctionCall
			signature := part.ThoughtSignature
			if call != nil && call.ThoughtSignature != "" {
				if selection.Strict() && signature != "" && signature != call.ThoughtSignature {
					return opaqueHistoryError("conflicting signatures")
				}
				signature = call.ThoughtSignature
			}
			if call != nil && call.ID != "" {
				key := thoughtSignatureKey(*call)
				if !selection.Strict() && store.strictCallOriginLocked(key) {
					return opaqueHistoryError("strict-origin tool history cannot become auto")
				}
				if signature == "" {
					if selection.Strict() {
						signature = store.routed[routedThoughtSignatureKey(base, channel, key)]
						if signature == "" && store.knownCallOriginLocked(key) {
							return opaqueHistoryError("tool call belongs to another model/channel")
						}
					} else {
						signature = store.legacy[key]
					}
					if signature != "" {
						call.ThoughtSignature = signature
					}
				}
			}
			if signature == "" {
				continue
			}
			origins := store.origins[sha256.Sum256([]byte(signature))]
			if selection.Strict() {
				match := slices.ContainsFunc(origins, func(origin thoughtSignatureOrigin) bool { return origin.base == base && origin.channel == channel })
				if !match {
					return opaqueHistoryError("unknown or incompatible provenance")
				}
			} else if slices.ContainsFunc(origins, func(origin thoughtSignatureOrigin) bool { return origin.strict }) {
				return opaqueHistoryError("strict-origin signature cannot become auto")
			}
		}
	}
	return nil
}

func (store *thoughtSignatureStore) knownCallOriginLocked(key string) bool {
	for routedKey := range store.routed {
		if strings.HasSuffix(routedKey, "\x00"+key) {
			return true
		}
	}
	return false
}

func (store *thoughtSignatureStore) strictCallOriginLocked(key string) bool {
	for routedKey, signature := range store.routed {
		if strings.HasSuffix(routedKey, "\x00"+key) {
			if slices.ContainsFunc(store.origins[sha256.Sum256([]byte(signature))], func(origin thoughtSignatureOrigin) bool { return origin.strict }) {
				return true
			}
		}
	}
	return false
}
