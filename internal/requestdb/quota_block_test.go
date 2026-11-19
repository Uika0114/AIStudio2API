package requestdb

import (
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

func TestQuotaAccountDispatchBlockExcludesBalancesAndPreservesUsage(t *testing.T) {
	for _, tc := range []struct {
		model   string
		fixture func(*testing.T) (*Store, string, time.Time)
	}{{"gemini-4-argon", quotaFixture}, {"gemini-4-carbon", carbonQuotaFixture}, {"gemini-4-hydrogen", hydrogenQuotaFixture}} {
		t.Run(tc.model, func(t *testing.T) {
			s, _, now := tc.fixture(t)
			add := func(id, account string, tokens int64) {
				a := quotaSend(id, account, now, tokens)
				a.BaseModel, a.QuotaTier = tc.model, "Ultra"
				s.RecordQuotaAttempt(a)
			}
			add("fixture-normal", "normal", 150)
			add("fixture-blocked", "blocked", 250)
			add("fixture-auth-required", "auth", 350)
			members := []api.QuotaMember{
				{ID: "normal", Tier: "Ultra", State: "cooldown", Enabled: true},
				{ID: "blocked", Tier: "Ultra", State: "auth_required", Enabled: true, BlockCode: aistudio.AccountVerificationRequiredCode},
				{ID: "auth", Tier: "Ultra", State: "auth_required", Enabled: true, BlockCode: "AUTH_REQUIRED"},
				{ID: "disabled", Tier: "Ultra", State: "disabled", Enabled: false, BlockCode: aistudio.AccountVerificationRequiredCode},
			}
			read := func(ms []api.QuotaMember) api.QuotaReport {
				t.Helper()
				r, err := s.QuotaSnapshotForModel(t.Context(), ms, now, tc.model)
				if err != nil {
					t.Fatal(err)
				}
				return r
			}
			expected := read(members[:1])
			r := read(members)
			if len(r.Accounts) != 3 || r.ExcludedAccounts != 2 {
				t.Fatalf("account list: %+v", r)
			}
			quotaValue(t, r.Tokens.Limit, *expected.Tokens.Limit)
			quotaValue(t, r.Tokens.Remaining, *expected.Tokens.Remaining)
			quotaValue(t, r.Requests.Used, 1)
			quotaValue(t, r.Tokens.Used, 150)
			if !quotaAccount(t, r, "normal").Included {
				t.Fatal("ordinary cooldown removed from quota pool")
			}
			b := quotaAccount(t, r, "blocked")
			if b.Included || b.BlockCode != aistudio.AccountVerificationRequiredCode || b.Tokens.Limit != nil || b.Tokens.Remaining != nil || b.Requests.Limit != nil || b.Requests.Remaining != nil {
				t.Fatalf("blocked showed capacity: %+v", b)
			}
			quotaValue(t, b.Tokens.Used, 250)
			quotaValue(t, b.Requests.Used, 1)
			a := quotaAccount(t, r, "auth")
			if a.Included || a.BlockCode != "AUTH_REQUIRED" {
				t.Fatalf("auth block: %+v", a)
			}
			allBlocked := read(members[1:])
			if allBlocked.Tokens.Limit != nil || allBlocked.Requests.Remaining != nil || len(allBlocked.Accounts) != 2 {
				t.Fatalf("all blocked: %+v", allBlocked)
			}
			members[1].State, members[1].BlockCode = "ready", ""
			resumed := read(members)
			quotaValue(t, resumed.Tokens.Used, 400)
			quotaValue(t, quotaAccount(t, resumed, "blocked").Tokens.Remaining, *expected.Tokens.Limit-250)
		})
	}
}
