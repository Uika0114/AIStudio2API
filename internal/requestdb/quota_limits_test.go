package requestdb

import (
	"context"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

func observedLimitSend(id, account, model, tier string, at time.Time, requests, tokens *int64) aistudio.GenerationAttempt {
	attempt := quotaSend(id, account, at, -1)
	attempt.BaseModel = model
	attempt.QuotaTier = tier
	attempt.RateLimited = true
	attempt.Stage = "failed"
	attempt.QuotaLimits = &aistudio.DailyQuotaLimits{Requests: requests, Tokens: tokens, ObservedAt: at}
	return attempt
}

func quotaAccount(t *testing.T, report api.QuotaReport, id string) api.QuotaAccount {
	t.Helper()
	for _, account := range report.Accounts {
		if account.ID == id {
			return account
		}
	}
	t.Fatalf("account %s missing", id)
	return api.QuotaAccount{}
}

func TestQuotaObservedLimitsIsolateModelTierAccountAndDimension(t *testing.T) {
	store, _, now := quotaFixture(t)
	members := append(quotaMembers(), api.QuotaMember{ID: "c", Tier: "Ultra", Enabled: true})
	store.RecordQuotaAttempt(observedLimitSend("token", "b", argonQuota.model, "Ultra", now, nil, quotaInt(18000000)))
	report := quotaRead(t, store, members, now)
	b := quotaAccount(t, report, "b")
	quotaValue(t, b.Tokens.Limit, 18000000)
	quotaValue(t, b.Requests.Limit, 150)
	quotaValue(t, quotaAccount(t, report, "c").Tokens.Limit, 20000000)
	quotaValue(t, quotaAccount(t, report, "a").Tokens.Limit, 15000000)
	quotaValue(t, report.Tokens.Limit, 53000000)
	quotaValue(t, carbonRead(t, store, quotaMembers(), now).Tokens.Limit, 20000000)
	quotaValue(t, hydrogenRead(t, store, quotaMembers(), now).Tokens.Limit, 20000000)

	// A later request-count observation changes only that field.
	store.RecordQuotaAttempt(observedLimitSend("requests", "b", argonQuota.model, "ultra", now.Add(time.Second), quotaInt(137), nil))
	report = quotaRead(t, store, members, now)
	b = quotaAccount(t, report, "b")
	quotaValue(t, b.Tokens.Limit, 18000000)
	quotaValue(t, b.Requests.Limit, 137)
	quotaValue(t, quotaAccount(t, report, "c").Requests.Limit, 150)

	store.RecordQuotaAttempt(observedLimitSend("carbon", "b", carbonQuota.model, "ultra", now.Add(2*time.Second), nil, quotaInt(17000000)))
	quotaValue(t, carbonRead(t, store, quotaMembers(), now).Tokens.Limit, 17000000)
	quotaValue(t, quotaAccount(t, quotaRead(t, store, members, now), "b").Tokens.Limit, 18000000)

	// Changing the current tier must not reuse the old permission's observed cap.
	members[1].Tier = "Pro"
	report = quotaRead(t, store, members, now)
	quotaValue(t, quotaAccount(t, report, "b").Tokens.Limit, 15000000)
	quotaValue(t, quotaAccount(t, report, "b").Requests.Limit, 50)
	store.RecordQuotaAttempt(observedLimitSend("pro", "b", argonQuota.model, "pro", now.Add(3*time.Second), nil, quotaInt(12000000)))
	quotaValue(t, quotaAccount(t, quotaRead(t, store, members, now), "b").Tokens.Limit, 12000000)
	members[1].Tier = "Ultra"
	quotaValue(t, quotaAccount(t, quotaRead(t, store, members, now), "b").Tokens.Limit, 18000000)
}

func TestQuotaObservedLimitsPersistAcrossWindowsAndRejectOlderEvidence(t *testing.T) {
	store, path, now := quotaFixture(t)
	newer := observedLimitSend("newer", "b", argonQuota.model, "ultra", now.Add(time.Second), quotaInt(137), quotaInt(20000000))
	store.RecordQuotaAttempt(newer)
	store.RecordQuotaAttempt(newer)
	older := observedLimitSend("older", "b", argonQuota.model, "ultra", now, quotaInt(200), quotaInt(30000000))
	store.RecordQuotaAttempt(older)
	// A delayed snapshot of an earlier send cannot rewind either dimension.
	older.Revision += 10
	store.RecordQuotaAttempt(older)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	next := QuotaWindow(now).Add(24 * time.Hour)
	b := quotaAccount(t, quotaRead(t, reopened, quotaMembers(), next), "b")
	quotaValue(t, b.Requests.Limit, 137)
	quotaValue(t, b.Tokens.Limit, 20000000)
	quotaValue(t, b.Requests.Remaining, 137)
	quotaValue(t, b.Tokens.Remaining, 20000000)

	// A zero limit is an explicit observation, not an unknown or excluded account.
	reopened.RecordQuotaAttempt(observedLimitSend("zero", "b", argonQuota.model, "ultra", next, nil, quotaInt(0)))
	b = quotaAccount(t, quotaRead(t, reopened, quotaMembers(), next), "b")
	if !b.Included {
		t.Fatal("observed zero limit excluded an eligible member")
	}
	quotaValue(t, b.Tokens.Limit, 0)
	quotaValue(t, b.Requests.Limit, 137)
}

func TestQuotaObservedLimitsIgnoreUnknownEvidenceAndCopyNumericValues(t *testing.T) {
	store, _, now := quotaFixture(t)
	base := observedLimitSend("invalid", "b", argonQuota.model, "ultra", now, nil, quotaInt(1))
	for _, mutate := range []func(*aistudio.GenerationAttempt){
		func(a *aistudio.GenerationAttempt) { a.QuotaTier = "" },
		func(a *aistudio.GenerationAttempt) { a.QuotaTier = "unknown" },
		func(a *aistudio.GenerationAttempt) { a.QuotaLimits.ObservedAt = time.Time{} },
		func(a *aistudio.GenerationAttempt) { a.RateLimited = false },
		func(a *aistudio.GenerationAttempt) { a.Sent = false },
		func(a *aistudio.GenerationAttempt) { a.Channel = aistudio.ChannelBuild },
		func(a *aistudio.GenerationAttempt) { a.QuotaLimits.Tokens = quotaInt(-1) },
		func(a *aistudio.GenerationAttempt) { a.QuotaLimits.Tokens = quotaInt(1 << 53) },
	} {
		attempt := base
		evidence := *base.QuotaLimits
		attempt.QuotaLimits = &evidence
		mutate(&attempt)
		store.RecordQuotaAttempt(attempt)
	}
	b := quotaAccount(t, quotaRead(t, store, quotaMembers(), now), "b")
	quotaValue(t, b.Tokens.Limit, 20000000)
	quotaValue(t, b.Requests.Limit, 150)

	value := int64(19000000)
	attempt := observedLimitSend("copied", "b", argonQuota.model, "ultra", now.Add(time.Second), nil, &value)
	store.RecordQuotaAttempt(attempt)
	value = 42
	b = quotaAccount(t, quotaRead(t, store, quotaMembers(), now), "b")
	quotaValue(t, b.Tokens.Limit, 19000000)
	var count int
	if err := store.quotaConn.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM quota_observed_limits").Scan(&count); err != nil || count != 1 {
		t.Fatalf("unexpected observations: count=%d error=%v", count, err)
	}
}
