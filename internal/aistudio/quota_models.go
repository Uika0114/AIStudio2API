package aistudio

import (
	"regexp"
	"slices"
	"strings"
)

const MaxGemini4QuotaModels = 64

var gemini4QuotaID = regexp.MustCompile(`^gemini-4(?:-[a-z0-9]+){1,8}$`)

func IsGemini4QuotaModel(id string) bool {
	return len(id) <= 96 && gemini4QuotaID.MatchString(id) && !strings.HasSuffix(id, "-anti")
}

func Gemini4PlaygroundQuotaBase(model Model) string {
	if !slices.Contains(model.Channels, string(ChannelPlayground)) || !slices.ContainsFunc(model.Methods, func(method string) bool { return strings.EqualFold(method, "generateContent") }) {
		return ""
	}
	id := strings.TrimPrefix(model.BaseModel, "models/")
	if id == "" {
		id = strings.TrimPrefix(model.ID, "models/")
	}
	if !IsGemini4QuotaModel(id) {
		return ""
	}
	return id
}
