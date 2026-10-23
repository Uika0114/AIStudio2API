package api

import (
	"fmt"
	"strings"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// channelOrder 是公开目录中通道变体的固定顺序。
var channelOrder = []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild, aistudio.ChannelAssistant}

// modelHasChannel 判断目录项是否把通道列为可调用的资格候选。
// 空 Channels 表示没有已知可用通道，绝不据此编造变体。
func modelHasChannel(model aistudio.Model, channel aistudio.Channel) bool {
	for _, candidate := range model.Channels {
		if strings.EqualFold(strings.TrimSpace(candidate), string(channel)) {
			return true
		}
	}
	return false
}

func modelHasGenerateMethod(model aistudio.Model) bool {
	for _, method := range model.Methods {
		if method == "generateContent" {
			return true
		}
	}
	return false
}

// modelSupportsAntiTruncation 判断基础模型能否承载完整参数工具运输。
// 运输需要真实的函数声明，因此缺少 function_declarations 或 generateContent
// 的模型不能暴露 anti-truncation ID。
func modelSupportsAntiTruncation(model aistudio.Model) bool {
	if !model.Capabilities["function_declarations"] {
		return false
	}
	for _, method := range model.Methods {
		if method == "generateContent" {
			return true
		}
	}
	return false
}

// publicModelCatalog 把 Service.Models 提供的基础目录/别名展开为服务端实际
// 接受的公开 ID：裸名保留原有 auto，每个拥有该通道的模型再给出严格通道变体，
// 具备函数声明能力的模型额外给出 anti-truncation 变体。Channels 为空的条目
// 不产生任何变体，目录不会宣称不存在的通道。
func publicModelCatalog(models []aistudio.Model) []aistudio.Model {
	catalog := make([]aistudio.Model, 0, len(models))
	seen := make(map[string]bool, len(models))
	appendModel := func(model aistudio.Model) {
		if model.ID == "" || seen[model.ID] {
			return
		}
		seen[model.ID] = true
		catalog = append(catalog, model)
	}
	for _, model := range models {
		appendModel(model)
		if !modelHasGenerateMethod(model) {
			continue
		}
		for _, channel := range channelOrder {
			if modelHasChannel(model, channel) {
				appendModel(publicChannelVariant(model, channel, false))
			}
		}
		for _, channel := range channelOrder {
			if modelHasChannel(model, channel) && modelSupportsAntiTruncation(publicChannelVariant(model, channel, false)) {
				appendModel(publicChannelVariant(model, channel, true))
			}
		}
	}
	return catalog
}

// publicChannelVariant 把目录项固定到单一通道，并记录其基础模型来源。
// Build 没有原生续写循环，因此 Build 变体不保留 Playground 的
// output_continuation 能力，避免目录把 PG 专属能力标到 Build 上。
func publicChannelVariant(model aistudio.Model, channel aistudio.Channel, anti bool) aistudio.Model {
	selection := aistudio.ModelSelection{BaseModel: model.ID, Channel: channel, AntiTruncation: anti}
	if channel == aistudio.ChannelAssistant {
		model = aistudio.AssistantModelVariant(model)
	}
	variant := model
	variant.ID = selection.PublicModelID(model.ID)
	if variant.BaseModel == "" {
		variant.BaseModel = model.ID
	}
	variant.Channels = []string{string(channel)}
	if limits, known := model.ChannelTokenLimits[channel]; known {
		variant.InputTokenLimit, variant.OutputTokenLimit = limits.Input, limits.Output
	}
	if channel == aistudio.ChannelBuild && variant.Capabilities["output_continuation"] {
		capabilities := make(map[string]bool, len(variant.Capabilities))
		for key, value := range variant.Capabilities {
			if key == "output_continuation" {
				continue
			}
			capabilities[key] = value
		}
		variant.Capabilities = capabilities
	}
	return variant
}

// LookupPublicModel 按正式 ID 优先于别名解析实时目录，并识别保留的通道/
// 抗截断前缀。严格前缀只有在基础模型确实拥有该通道、anti 前缀还需要函数声明
// 能力时才解析成功，否则返回 false，不会把不支持的组合当成基础模型。
func LookupPublicModel(models []aistudio.Model, id string) (aistudio.Model, bool) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(id), "models/")
	if trimmed == "" {
		return aistudio.Model{}, false
	}
	selection, err := aistudio.ParseModelSelection(trimmed)
	if err != nil {
		return aistudio.Model{}, false
	}
	model, ok := lookupCatalogBaseModel(models, selection.BaseModel)
	if !ok {
		return aistudio.Model{}, false
	}
	if selection.Strict() {
		if !modelHasGenerateMethod(model) || !modelHasChannel(model, selection.Channel) {
			return aistudio.Model{}, false
		}
		if selection.AntiTruncation && !modelSupportsAntiTruncation(publicChannelVariant(model, selection.Channel, false)) {
			return aistudio.Model{}, false
		}
		model = publicChannelVariant(model, selection.Channel, selection.AntiTruncation)
		model.ID = selection.PublicModelID(selection.BaseModel)
	}
	return model, true
}

// lookupCatalogBaseModel 按正式 ID 优先于别名查找基础模型条目。
func lookupCatalogBaseModel(models []aistudio.Model, id string) (aistudio.Model, bool) {
	for _, model := range models {
		if model.ID == id {
			return model, true
		}
	}
	for _, model := range models {
		for _, alias := range model.CapabilityOptions["aliases"] {
			if strings.TrimPrefix(strings.TrimSpace(alias), "models/") == id {
				return model, true
			}
		}
	}
	return aistudio.Model{}, false
}

// normalizeRoutedModel 把保留的通道/抗截断前缀解析进 Selection，并把 Model
// 归约为基础身份。没有模型的请求保持原样，供辅助入口稍后回退。
func normalizeRoutedModel(request aistudio.GenerateRequest) (aistudio.GenerateRequest, error) {
	if strings.TrimSpace(request.Model) == "" && strings.TrimSpace(request.Selection.RequestedModel) == "" {
		return request, nil
	}
	return aistudio.NormalizeGenerateRequestModel(request)
}

// rejectRoutedModel 让尚未实现通道选择的入口显式拒绝保留前缀，
// 而不是把前缀当作基础模型继续，也不静默忽略。
func rejectRoutedModel(modelID string) error {
	if aistudio.HasModelSelectionPrefix(modelID) {
		return fmt.Errorf("%w: model %q selects a channel or anti-truncation policy that this endpoint does not support", aistudio.ErrInvalidArgument, modelID)
	}
	return nil
}

// requestedPublicModel 返回协议响应中应回显的公开模型 ID，
// 优先使用客户端提交的带前缀 ID，保证前缀可 round-trip。
func requestedPublicModel(request aistudio.GenerateRequest) string {
	if request.Selection.RequestedModel != "" {
		return request.Selection.RequestedModel
	}
	return request.Model
}

// crossChannelOpaqueStateError 拒绝把带上游思考签名的 Responses 历史
// 续接到另一个严格通道：签名是不可移植的不透明状态，不能偷偷换通道或删除。
// 只含可移植文本/真实工具结果的历史不在此列。
func crossChannelOpaqueStateError(previous responseHistory, selection aistudio.ModelSelection) error {
	if !selection.Strict() || previous.Channel == "" || previous.Channel == string(selection.Channel) {
		return nil
	}
	if !contentsHaveThoughtSignature(previous.Contents) {
		return nil
	}
	return fmt.Errorf("%w: response %s carries upstream thought signatures from channel %s and cannot continue on %s", aistudio.ErrInvalidArgument, previous.ID, previous.Channel, selection.Channel)
}

// contentsHaveThoughtSignature 判断历史内容是否携带上游思考签名。
func generationResponseModel(request aistudio.GenerateRequest, actual string) string {
	if request.Selection.Strict() || actual == "" {
		return requestedPublicModel(request)
	}
	return actual
}

func contentsHaveThoughtSignature(contents []aistudio.Content) bool {
	for _, content := range contents {
		for _, part := range content.Parts {
			if part.ThoughtSignature != "" || (part.FunctionCall != nil && part.FunctionCall.ThoughtSignature != "") {
				return true
			}
		}
	}
	return false
}
