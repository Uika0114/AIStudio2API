package aistudio

// publicTokenLimitsLocked prevents PG continuation limits from leaking into a
// Build variant. Use the common published bound of eligible Build accounts, not
// a guessed bound when a native catalog omits it. Caller holds pool.mu.
func (p *AccountPool) publicTokenLimitsLocked(model Model) map[Channel]ModelTokenLimits {
	limits := map[Channel]ModelTokenLimits{ChannelPlayground: {Input: model.InputTokenLimit, Output: model.OutputTokenLimit}}
	selection := AccountSelection{ModelID: model.ID, Method: "generateContent", Channel: ChannelBuild, StrictChannel: true}
	found := false
	build := ModelTokenLimits{}
	for _, account := range p.accounts {
		if account == nil || !account.Config.Enabled || !p.channelSupportsLocked(account, ChannelBuild, selection) {
			continue
		}
		for _, native := range account.buildModels {
			if native.ID != model.ID {
				continue
			}
			if !found {
				build = ModelTokenLimits{Input: native.InputTokenLimit, Output: native.OutputTokenLimit}
				found = true
			} else {
				build.Input = min(build.Input, native.InputTokenLimit)
				build.Output = min(build.Output, native.OutputTokenLimit)
			}
		}
	}
	if p.channelEnabledLocked(ChannelAssistant) {
		// Assistant has no verified native token-limit catalog.
		limits[ChannelAssistant] = ModelTokenLimits{}
	}
	limits[ChannelBuild] = build
	return limits
}
