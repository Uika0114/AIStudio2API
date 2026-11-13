package requestdb

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func quotaUnknown(t *testing.T, values ...*int64) {
	t.Helper()
	for _, value := range values {
		if value != nil {
			t.Fatalf("remaining must be unknown, got %d", *value)
		}
	}
}

func quotaStoredExhaustion(t *testing.T, store *Store, id string, want int) {
	t.Helper()
	if err := store.flushQuota(context.Background()); err != nil {
		t.Fatal(err)
	}
	var mask int
	if err := store.quotaConn.QueryRowContext(context.Background(),
		"SELECT limited FROM quota17_argon_attempts WHERE epoch=? AND id=?", store.quotaEpoch, id).Scan(&mask); err != nil {
		t.Fatal(err)
	}
	if mask != want {
		t.Fatalf("exhaustion mask=%d want=%d", mask, want)
	}
}

func TestQuotaExhaustionDimensionsIndependent(t *testing.T) {
	for _, test := range []struct {
		name      string
		requests  *int64
		tokens    *int64
		mask      int
		reqRemain int64
		tokRemain int64
	}{
		{"tokens only", nil, quotaInt(18000000), 2, 198, 0},
		{"requests only", quotaInt(150), nil, 4, 0, 20000000 - 100},
		{"both", quotaInt(150), quotaInt(18000000), 6, 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, _, now := quotaFixture(t)
			members := quotaMembers()[1:]
			store.RecordQuotaAttempt(quotaSend("used", "b", now, 100))
			attempt := observedLimitSend("limited", "b", argonQuota.model, "ultra", now, test.requests, test.tokens)
			store.RecordQuotaAttempt(attempt)
			report := quotaRead(t, store, members, now)
			quotaValue(t, report.Requests.Remaining, test.reqRemain)
			quotaValue(t, report.Tokens.Remaining, test.tokRemain)
			quotaValue(t, report.Requests.Used, 2)
			quotaValue(t, report.Tokens.Used, 100)
			if !report.Accounts[0].Exhausted429 || report.Unreported != 1 {
				t.Fatal("explicit exhaustion lost its 429 or usage quality metadata")
			}
			quotaStoredExhaustion(t, store, attempt.ID, test.mask)
		})
	}
}

func TestQuotaExhaustionEarlyHTTPUnknownRefinedByLaterRevision(t *testing.T) {
	store, _, now := quotaFixture(t)
	members := quotaMembers()[1:]
	attempt := quotaSend("same-physical-send", "b", now, -1)
	attempt.StatusCode = 429
	attempt.Stage = "http_error"
	store.RecordQuotaAttempt(attempt)
	early := quotaRead(t, store, members, now)
	quotaUnknown(t, early.Requests.Remaining, early.Tokens.Remaining)
	quotaStoredExhaustion(t, store, attempt.ID, 1)

	attempt.Revision++
	attempt.QuotaTier = "ultra"
	attempt.QuotaLimits = &aistudio.DailyQuotaLimits{Tokens: quotaInt(18000000), ObservedAt: now}
	store.RecordQuotaAttempt(attempt)
	refined := quotaRead(t, store, members, now)
	quotaValue(t, refined.Requests.Remaining, 199)
	quotaValue(t, refined.Tokens.Remaining, 0)
	quotaValue(t, refined.Requests.Used, 1)
	quotaStoredExhaustion(t, store, attempt.ID, 2)

	// A later usage-only snapshot cannot restore uncertainty or clear exhaustion.
	attempt.Revision++
	attempt.QuotaLimits = nil
	attempt.StatusCode = 200
	attempt.Usage = &aistudio.Usage{TotalTokens: 321}
	attempt.UsageSource = "upstream"
	store.RecordQuotaAttempt(attempt)
	late := quotaRead(t, store, members, now)
	quotaValue(t, late.Requests.Remaining, 199)
	quotaValue(t, late.Tokens.Remaining, 0)
	quotaValue(t, late.Tokens.Used, 321)
	quotaStoredExhaustion(t, store, attempt.ID, 2)

	attempt.Revision++
	attempt.StatusCode = 429
	store.RecordQuotaAttempt(attempt)
	quotaStoredExhaustion(t, store, attempt.ID, 2)
	quotaValue(t, quotaRead(t, store, members, now).Requests.Remaining, 199)

	// The other explicit dimension may arrive in a still later snapshot.
	attempt.Revision++
	attempt.QuotaLimits = &aistudio.DailyQuotaLimits{Requests: quotaInt(150), ObservedAt: now}
	store.RecordQuotaAttempt(attempt)
	both := quotaRead(t, store, members, now)
	quotaValue(t, both.Requests.Remaining, 0)
	quotaValue(t, both.Tokens.Remaining, 0)
	quotaStoredExhaustion(t, store, attempt.ID, 6)
}

func TestQuotaExhaustionUnknownEvidenceAndLegacyMask(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*aistudio.GenerationAttempt)
	}{
		{"missing tier", func(a *aistudio.GenerationAttempt) { a.QuotaTier = "" }},
		{"missing metric", func(a *aistudio.GenerationAttempt) { a.QuotaLimits = nil }},
		{"empty dimensions", func(a *aistudio.GenerationAttempt) { a.QuotaLimits.Tokens = nil }},
		{"invalid limit", func(a *aistudio.GenerationAttempt) { a.QuotaLimits.Tokens = quotaInt(-1) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, _, now := quotaFixture(t)
			attempt := observedLimitSend("unknown", "b", argonQuota.model, "ultra", now, nil, quotaInt(18000000))
			test.mutate(&attempt)
			store.RecordQuotaAttempt(attempt)
			report := quotaRead(t, store, quotaMembers()[1:], now)
			quotaUnknown(t, report.Tokens.Remaining, report.Requests.Remaining)
			quotaValue(t, report.Tokens.Limit, 20000000)
			quotaStoredExhaustion(t, store, attempt.ID, 1)
		})
	}

	t.Run("legacy persisted one", func(t *testing.T) {
		store, path, now := quotaFixture(t)
		_, err := store.quotaConn.ExecContext(context.Background(), `INSERT INTO quota17_argon_attempts
 (epoch,id,revision,account,window,sent_at,tokens,reported,ended,limited) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			"legacy-epoch", "old-429", 1, "b", QuotaWindow(now).UnixMilli(), now.UnixMilli(), 321, 1, 1, 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := Open(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		report := quotaRead(t, reopened, quotaMembers()[1:], now)
		quotaUnknown(t, report.Tokens.Remaining, report.Requests.Remaining)
		quotaValue(t, report.Tokens.Used, 321)
		quotaValue(t, report.Requests.Used, 1)
		if !report.Accounts[0].Exhausted429 {
			t.Fatal("legacy 429 history disappeared")
		}
	})
}

func TestQuotaExhaustionAggregatesEachDimensionSeparately(t *testing.T) {
	for _, requests := range []bool{false, true} {
		for _, exhaustSecond := range []bool{false, true} {
			name := "tokens"
			if requests {
				name = "requests"
			}
			if exhaustSecond {
				name += "-all-exhausted"
			} else {
				name += "-other-account-available"
			}
			t.Run(name, func(t *testing.T) {
				store, _, now := quotaFixture(t)
				unknown := quotaSend("unknown", "a", now, -1)
				unknown.StatusCode = 429
				unknown.Stage = "http_error"
				store.RecordQuotaAttempt(unknown)
				var requestLimit, tokenLimit *int64
				if requests {
					requestLimit = quotaInt(50)
				} else {
					tokenLimit = quotaInt(15000000)
				}
				store.RecordQuotaAttempt(observedLimitSend("known-a", "a", argonQuota.model, "pro", now, requestLimit, tokenLimit))
				if exhaustSecond {
					if requests {
						requestLimit = quotaInt(200)
					} else {
						tokenLimit = quotaInt(20000000)
					}
					store.RecordQuotaAttempt(observedLimitSend("known-b", "b", argonQuota.model, "ultra", now, requestLimit, tokenLimit))
				} else {
					store.RecordQuotaAttempt(quotaSend("used-b", "b", now, 100))
				}
				report := quotaRead(t, store, quotaMembers(), now)
				want := int64(0)
				if requests {
					if !exhaustSecond {
						want = 199
					}
					quotaValue(t, report.Requests.Remaining, want)
					quotaUnknown(t, report.Tokens.Remaining)
				} else {
					if !exhaustSecond {
						want = 20000000 - 100
					}
					quotaValue(t, report.Tokens.Remaining, want)
					quotaUnknown(t, report.Requests.Remaining)
				}
			})
		}
	}
}

func TestQuotaExhaustionKnownDimensionSurvivesPartialCoverage(t *testing.T) {
	store, _, now := quotaFixture(t)
	store.quotaSince = QuotaWindow(now).Add(time.Minute)
	store.RecordQuotaAttempt(observedLimitSend("a-token", "a", argonQuota.model, "pro", now, nil, quotaInt(15000000)))
	store.RecordQuotaAttempt(observedLimitSend("b-token", "b", argonQuota.model, "ultra", now, nil, quotaInt(20000000)))
	report := quotaRead(t, store, quotaMembers(), now)
	if report.Coverage != "partial" {
		t.Fatal("fixture is not partial")
	}
	quotaValue(t, report.Tokens.Remaining, 0)
	quotaUnknown(t, report.Requests.Remaining)

	// Separate attempts must combine by dimension, not numeric MAX(mask).
	store.RecordQuotaAttempt(observedLimitSend("a-request", "a", argonQuota.model, "pro", now, quotaInt(50), nil))
	store.RecordQuotaAttempt(observedLimitSend("b-request", "b", argonQuota.model, "ultra", now, quotaInt(200), nil))
	report = quotaRead(t, store, quotaMembers(), now)
	quotaValue(t, report.Tokens.Remaining, 0)
	quotaValue(t, report.Requests.Remaining, 0)
}

func TestQuotaExhaustionResetPreservesCapsWithoutCrossModelPollution(t *testing.T) {
	store, path, now := carbonQuotaFixture(t)
	store.RecordQuotaAttempt(observedLimitSend("carbon-token", "b", carbonQuota.model, "ultra", now, nil, quotaInt(17000000)))
	store.RecordQuotaAttempt(observedLimitSend("argon-request", "b", argonQuota.model, "ultra", now, quotaInt(150), nil))
	carbon := carbonRead(t, store, quotaMembers(), now)
	quotaValue(t, carbon.Tokens.Remaining, 0)
	quotaValue(t, carbon.Requests.Remaining, 199)
	argon := quotaRead(t, store, quotaMembers()[1:], now)
	quotaValue(t, argon.Requests.Remaining, 0)
	quotaValue(t, argon.Tokens.Remaining, 20000000)
	hydrogen := hydrogenRead(t, store, quotaMembers(), now)
	if hydrogen.Accounts[0].Exhausted429 {
		t.Fatal("other models exhausted Hydrogen")
	}
	quotaValue(t, hydrogen.Tokens.Limit, 30000000)

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	next := QuotaWindow(now).Add(24 * time.Hour)
	carbon = carbonRead(t, reopened, quotaMembers(), next)
	quotaValue(t, carbon.Tokens.Remaining, 17000000)
	quotaValue(t, carbon.Requests.Remaining, 200)
	quotaValue(t, carbon.Tokens.Used, 0)
	argon = quotaRead(t, reopened, quotaMembers()[1:], next)
	quotaValue(t, argon.Requests.Remaining, 150)
	quotaValue(t, argon.Tokens.Remaining, 20000000)
	if carbon.Accounts[0].Exhausted429 || argon.Accounts[0].Exhausted429 {
		t.Fatal("exhaustion survived its reset window")
	}
}

func TestQuotaExhaustionTierSwitchAndColdRead(t *testing.T) {
	store, path, now := quotaFixture(t)
	members := quotaMembers()[:1]
	used := quotaSend("measured-pro", "a", now, 100)
	used.QuotaTier = "pro"
	store.RecordQuotaAttempt(used)
	store.RecordQuotaAttempt(observedLimitSend("pro-token", "a", argonQuota.model, "pro", now, nil, quotaInt(12000000)))
	pro := quotaRead(t, store, members, now)
	quotaValue(t, pro.Tokens.Remaining, 0)
	quotaValue(t, pro.Requests.Remaining, 48)

	members[0].Tier = "Ultra"
	ultra := quotaRead(t, store, members, now)
	quotaValue(t, ultra.Tokens.Remaining, 20000000-100)
	quotaValue(t, ultra.Requests.Remaining, 198)
	if ultra.Accounts[0].Exhausted429 {
		t.Fatal("old Pro exhaustion was assigned to the current Ultra tier")
	}
	store.RecordQuotaAttempt(observedLimitSend("ultra-request", "a", argonQuota.model, "ultra", now.Add(time.Second), quotaInt(180), nil))
	ultra = quotaRead(t, store, members, now)
	quotaValue(t, ultra.Requests.Remaining, 0)
	quotaValue(t, ultra.Tokens.Remaining, 20000000-100)

	verify := func(store *Store) {
		t.Helper()
		members[0].Tier = "Pro"
		pro := quotaRead(t, store, members, now)
		quotaValue(t, pro.Tokens.Limit, 12000000)
		quotaValue(t, pro.Tokens.Remaining, 0)
		quotaValue(t, pro.Requests.Remaining, 47)
		quotaValue(t, pro.Requests.Used, 3)
		quotaValue(t, pro.Tokens.Used, 100)
		members[0].Tier = "Ultra"
		ultra := quotaRead(t, store, members, now)
		quotaValue(t, ultra.Requests.Limit, 180)
		quotaValue(t, ultra.Requests.Remaining, 0)
		quotaValue(t, ultra.Tokens.Remaining, 20000000-100)
		quotaValue(t, ultra.Requests.Used, 3)
		for _, model := range []string{carbonQuota.model, hydrogenQuota.model} {
			other, err := store.QuotaSnapshotForModel(context.Background(), members, now, model)
			if err != nil {
				t.Fatal(err)
			}
			quotaValue(t, other.Requests.Used, 0)
			quotaValue(t, other.Tokens.Used, 0)
			if other.Accounts[0].Exhausted429 {
				t.Fatalf("Argon tier-specific exhaustion leaked into %s", model)
			}
		}
	}
	verify(store)
	for id, want := range map[string]string{"measured-pro": "pro", "pro-token": "pro", "ultra-request": "ultra"} {
		var tier string
		if err := store.quotaConn.QueryRowContext(context.Background(), "SELECT quota_tier FROM quota17_argon_attempts WHERE id=?", id).Scan(&tier); err != nil || tier != want {
			t.Fatalf("persisted tier for %s=%q want=%q: %v", id, tier, want, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	verify(reopened)
}

func TestQuotaExhaustionLegacyTypedMaskWithoutTierRemainsUnknown(t *testing.T) {
	for _, mask := range []int{2, 4, 6} {
		store, _, now := quotaFixture(t)
		_, err := store.quotaConn.ExecContext(context.Background(), `INSERT INTO quota17_argon_attempts
 (epoch,id,revision,account,window,sent_at,tokens,reported,ended,limited) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			"legacy-epoch", "typed-without-tier", 1, "a", QuotaWindow(now).UnixMilli(), now.UnixMilli(), 321, 1, 1, mask)
		if err != nil {
			t.Fatal(err)
		}
		members := quotaMembers()[:1]
		for _, tier := range []string{"Pro", "Ultra"} {
			members[0].Tier = tier
			report := quotaRead(t, store, members, now)
			quotaUnknown(t, report.Tokens.Remaining, report.Requests.Remaining)
			quotaValue(t, report.Tokens.Used, 321)
			if !report.Accounts[0].Exhausted429 {
				t.Fatalf("unattributed legacy mask %d lost its 429 signal for %s", mask, tier)
			}
		}
	}
}

func TestQuotaExhaustionTierColumnMigrationPreservesEveryModel(t *testing.T) {
	store, path, now := quotaFixture(t)
	scopes := []quotaScope{argonQuota, carbonQuota, hydrogenQuota}
	var starts [3]time.Time
	for i, scope := range scopes {
		starts[i] = QuotaWindow(now).Add(-24*time.Hour + time.Duration(i)*time.Minute)
		since, _ := store.quotaTracking(scope)
		*since = starts[i]
		if _, err := store.quotaConn.ExecContext(context.Background(), "UPDATE "+scope.tracking+" SET observed_since=? WHERE id=1", starts[i].UnixMilli()); err != nil {
			t.Fatal(err)
		}
		attempt := observedLimitSend(scope.model, "b", scope.model, "ultra", now, nil, quotaInt(17000000+int64(i)))
		attempt.Usage = &aistudio.Usage{TotalTokens: 100 + int64(i)}
		attempt.UsageSource = "upstream"
		store.RecordQuotaAttempt(attempt)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Only this temporary fixture is altered to reproduce the pre-tier schema.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range scopes {
		if _, err := db.Exec("ALTER TABLE " + scope.attempts + " DROP COLUMN quota_tier"); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		reopened, err := Open(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for i, scope := range scopes {
			since, _ := reopened.quotaTracking(scope)
			if !since.Equal(starts[i]) {
				t.Fatalf("%s migration changed observation start: %s want %s", scope.model, since, starts[i])
			}
			var tier string
			var mask int
			if err := reopened.quotaConn.QueryRowContext(context.Background(),
				"SELECT quota_tier,limited FROM "+scope.attempts+" WHERE id=?", scope.model).Scan(&tier, &mask); err != nil {
				t.Fatal(err)
			}
			if tier != "" || mask != 2 {
				t.Fatalf("%s migration backfilled tier or rewrote old evidence: tier=%q mask=%d", scope.model, tier, mask)
			}
			report, err := reopened.QuotaSnapshotForModel(context.Background(), quotaMembers()[1:], now, scope.model)
			if err != nil {
				t.Fatal(err)
			}
			quotaValue(t, report.Requests.Used, 1)
			quotaValue(t, report.Tokens.Used, 100+int64(i))
			quotaValue(t, report.Tokens.Limit, 17000000+int64(i))
			quotaUnknown(t, report.Requests.Remaining, report.Tokens.Remaining)
			if report.Coverage != "complete" || !report.Accounts[0].Exhausted429 {
				t.Fatalf("%s migration lost coverage/history: %+v", scope.model, report)
			}
		}
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
