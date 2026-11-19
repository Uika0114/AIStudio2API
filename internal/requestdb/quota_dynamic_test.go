package requestdb

import (
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

func TestDynamicGemini4QuotaSurvivesReopenAndKeepsLegacyIsolated(t *testing.T) {
	store, path, now := quotaFixture(t)
	const model = "gemini-4-neon-preview"
	const another = "gemini-4-luna"
	members := quotaMembers()
	read := func(s *Store, name string, ms []api.QuotaMember) api.QuotaReport {
		t.Helper()
		r, err := s.QuotaSnapshotForModel(t.Context(), ms, now, name)
		if err != nil {
			t.Fatal(err)
		}
		if r.Model != name || r.Channel != "playground" {
			t.Fatalf("wrong pool: %+v", r)
		}
		return r
	}
	before := read(store, model, members)
	quotaValue(t, before.Tokens.Limit, 35_000_000)
	quotaValue(t, before.Tokens.Remaining, 35_000_000)
	a := quotaSend("same-id", "a", now, 2_500)
	a.BaseModel, a.QuotaTier, a.Method = "models/"+model, "pro", "GenerateContent"
	store.RecordQuotaAttempt(a)
	store.RecordQuotaAttempt(a)
	b := quotaSend("same-id", "a", now, 800)
	b.BaseModel, b.QuotaTier, b.Method = another, "pro", "GenerateContent"
	store.RecordQuotaAttempt(b)
	store.RecordQuotaAttempt(quotaSend("same-id", "a", now, 100))
	daily := observedLimitSend("daily", "b", model, "ultra", now.Add(time.Second), nil, quotaInt(17_000_000))
	daily.Method = "GenerateContent"
	store.RecordQuotaAttempt(daily)
	nonText := quotaSend("non-text", "a", now, 400)
	nonText.BaseModel, nonText.Method = model, "GenerateVideo"
	store.RecordQuotaAttempt(nonText)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := read(s, model, members)
	quotaValue(t, r.Tokens.Used, 2_500)
	quotaValue(t, r.Tokens.Limit, 32_000_000)
	quotaValue(t, r.Requests.Limit, 200)
	quotaValue(t, r.Requests.Used, 2)
	quotaValue(t, quotaAccount(t, r, "b").Tokens.Remaining, 0)
	quotaValue(t, quotaAccount(t, r, "b").Requests.Remaining, 149)
	quotaValue(t, read(s, another, members).Tokens.Used, 800)
	quotaValue(t, read(s, argonQuota.model, members).Tokens.Used, 100)
	quotaValue(t, read(s, carbonQuota.model, members).Tokens.Limit, 20_000_000)
	members[1].BlockCode = "ACCOUNT_VERIFICATION_REQUIRED"
	blocked := read(s, model, members)
	quotaValue(t, blocked.Tokens.Limit, 15_000_000)
	if quotaAccount(t, blocked, "b").Included {
		t.Fatal("blocked account remained in the pool")
	}
}

func TestDynamicGemini4QuotaRejectsAliasAndUnrelatedFamily(t *testing.T) {
	s, _, now := quotaFixture(t)
	for _, model := range []string{"gemini-4-neon-anti", "gemini-3.8-flash", "gemini-4-neon;DROP", "playground/gemini-4-neon"} {
		if _, err := s.QuotaSnapshotForModel(t.Context(), quotaMembers(), now, model); err == nil {
			t.Fatalf("unexpected quota pool: %s", model)
		}
	}
}
