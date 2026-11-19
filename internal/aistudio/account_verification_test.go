package aistudio

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
)

const verificationFixtureMessage = "Your account must be verified before it can continue using the Gemini API. Verify your account at https://aistudio.google.com/verify-account."

func TestAccountVerificationRequiredClassification(t *testing.T) {
	for _, tc := range []struct {
		name, method, message string
		status                int
		code                  int64
		want                  bool
	}{
		{"exact", "GenerateContent", verificationFixtureMessage, 403, 7, true},
		{"bidi", "BidiGenerateContent", verificationFixtureMessage, 403, 7, true},
		{"not exact URL", "GenerateContent", strings.ReplaceAll(verificationFixtureMessage, "verify-account.", "verify-account-mock."), 403, 7, false},
		{"plain forbidden", "GenerateContent", "The caller does not have permission", 403, 7, false},
		{"different RPC", "FetchAvailableModels", verificationFixtureMessage, 403, 7, false},
		{"different status", "GenerateContent", verificationFixtureMessage, 429, 7, false},
		{"different code", "GenerateContent", verificationFixtureMessage, 403, 16, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := fmt.Errorf("fixture wrapper: %w", &RPCError{Method: tc.method, StatusCode: tc.status, Code: tc.code, Message: tc.message})
			if got := IsAccountVerificationRequired(err); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			if tc.want && DefinitiveAuthenticationFailure(err) {
				t.Fatal("verification should not enter OAuth renewal")
			}
		})
	}
}

func TestAccountVerificationPersistsUntilExactOperatorConfirmation(t *testing.T) {
	ctx := t.Context()
	pool, account := remoteQuarantineFixture(t, nil)
	if err := pool.BindResource("fixture-file", account.ID); err != nil {
		t.Fatal(err)
	}
	first, err := pool.AcquireFor(ctx, AccountSelection{AccountID: account.ID})
	if err != nil {
		t.Fatal(err)
	}
	second, err := pool.AcquireFor(ctx, AccountSelection{AccountID: account.ID})
	if err != nil {
		t.Fatal(err)
	}
	// The real direct generation failure handler must persist the exact error,
	// not fall through to its ordinary, time-limited failure cooldown.
	svc := &PooledService{pool: pool}
	upstream := &RPCError{Method: "GenerateContent", StatusCode: http.StatusForbidden, Code: 7, Message: verificationFixtureMessage + " PRIVATE_SENTINEL"}
	if err := svc.markRetryableFailure(first, "fixture-model", upstream); err != nil {
		t.Fatal(err)
	}
	s := pool.Status()[0]
	if s.VerificationID == "" || s.BlockCode != AccountVerificationRequiredCode || s.State != AccountAuthRequired || !s.Enabled || !strings.Contains(s.Message, "403") || strings.Contains(s.Message, "PRIVATE_SENTINEL") || len(s.Cooldowns) != 0 {
		t.Fatalf("incorrect verification diagnosis: %+v", s)
	}
	if err := second.MarkAuthenticationValid(); err != nil {
		t.Fatal(err)
	}
	if err := second.CheckAccountVerification(); !IsSessionFailureCode(err, AccountVerificationRequiredCode) {
		t.Fatalf("held lease bypass: %v", err)
	}
	if err := validateLeaseSelection(second, AccountSelection{AccountID: account.ID}); !IsSessionFailureCode(err, AccountVerificationRequiredCode) {
		t.Fatalf("reused lease bypass: %v", err)
	}
	assertNoRemoteGeneration(t, pool, account)
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
	if err := pool.SetCatalog(account.ID, BenefitTierPro, account.Models); err != nil {
		t.Fatal(err)
	}
	fresh, err := loadAccount(account.Directory)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt := NewAccountPool([]*Account{fresh}, 2)
	if got := rebuilt.Status()[0]; got.VerificationID != s.VerificationID || got.State != AccountAuthRequired {
		t.Fatalf("hold lost on restart: %+v", got)
	}
	if next, _, err := rebuilt.TryAcquireFor(ctx, AccountSelection{AccountID: account.ID}); next != nil || err != nil {
		t.Fatalf("persisted hold allowed selection: %v %v", next, err)
	}
	admin, err := rebuilt.AcquireAccount(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.ClearAccountVerification(ctx, "stale-incident"); !IsSessionFailureCode(err, "ACCOUNT_VERIFICATION_CHANGED") {
		t.Fatalf("stale confirmation: %v", err)
	}
	if err := admin.ClearAccountVerification(ctx, s.VerificationID); err != nil {
		t.Fatal(err)
	}
	if err := admin.Release(); err != nil {
		t.Fatal(err)
	}
	if got := rebuilt.Status()[0]; got.VerificationID != "" || got.State != AccountReady {
		t.Fatalf("existing readiness not restored: %+v", got)
	}
	fresh, err = loadAccount(account.Directory)
	if err != nil || fresh.runtime.VerificationID != "" {
		t.Fatalf("clear did not persist: %v", err)
	}
}

func TestAccountVerificationWriteFailureStillFencesDispatch(t *testing.T) {
	pool, account := remoteQuarantineFixture(t, nil)
	lease, err := pool.AcquireFor(t.Context(), AccountSelection{AccountID: account.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if err := os.Mkdir(account.RuntimePath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := lease.MarkAccountVerificationRequired(); err == nil {
		t.Fatal("expected persistent mark failure")
	}
	if err := lease.CheckAccountVerification(); !IsSessionFailureCode(err, AccountVerificationRequiredCode) {
		t.Fatalf("write failure allowed further dispatch: %v", err)
	}
	if pool.Status()[0].BlockCode != AccountVerificationRequiredCode {
		t.Fatal("RAM hold not visible")
	}
}
