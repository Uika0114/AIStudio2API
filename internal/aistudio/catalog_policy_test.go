package aistudio

import "testing"

func TestPublicCatalogSeparatesNativeBuildTokenLimits(t *testing.T) {
	pool, account := strictFixturePool(t)
	base := account.Models[0]
	base.InputTokenLimit = 1048576
	base.OutputTokenLimit = 262144
	base.Capabilities["output_continuation"] = true
	account.Models = []Model{base}
	build := base
	build.InputTokenLimit = 131072
	build.OutputTokenLimit = 8192
	if err := pool.SetBuildCatalog(account.ID, []Model{build}); err != nil {
		t.Fatal(err)
	}
	models := pool.PublicModels([]Model{base})
	if len(models) != 1 {
		t.Fatal(models)
	}
	if got := models[0].ChannelTokenLimits[ChannelBuild]; got.Input != 131072 || got.Output != 8192 {
		t.Fatal(got)
	}
	if got := models[0].ChannelTokenLimits[ChannelPlayground]; got.Input != 1048576 || got.Output != 262144 {
		t.Fatal(got)
	}
	build.InputTokenLimit, build.OutputTokenLimit = 0, 0
	if err := pool.SetBuildCatalog(account.ID, []Model{build}); err != nil {
		t.Fatal(err)
	}
	if got := pool.PublicModels([]Model{base})[0].ChannelTokenLimits[ChannelBuild]; got.Input != 0 || got.Output != 0 {
		t.Fatal("unknown Build bound borrowed PG limit", got)
	}
}
