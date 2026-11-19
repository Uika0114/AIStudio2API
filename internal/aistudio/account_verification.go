package aistudio

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"strings"
	"time"
)

const (
	AccountVerificationRequiredCode    = "ACCOUNT_VERIFICATION_REQUIRED"
	accountVerificationRequiredMessage = "HTTP 403 · Google 要求验证账号：https://aistudio.google.com/verify-account"
)

// IsAccountVerificationRequired identifies the account-wide verification gate,
// not a generic permission denial or an expired credential that OAuth can fix.
func IsAccountVerificationRequired(err error) bool {
	var rpc *RPCError
	if !errors.As(err, &rpc) || rpc.StatusCode != http.StatusForbidden || rpc.Code != 7 || !modelBoundRPCMethod(rpc.Method) {
		return false
	}
	message := strings.ToLower(rpc.Message)
	if !strings.Contains(message, "your account must be verified before it can continue using the gemini api") {
		return false
	}
	for _, word := range strings.Fields(message) {
		if strings.Trim(word, ".,;:()[]<>!?\"") == "https://aistudio.google.com/verify-account" {
			return true
		}
	}
	return false
}

func (a *Account) verificationIDLocked() string {
	if a.verificationPending != "" {
		return a.verificationPending
	}
	return a.runtime.VerificationID
}

// MarkAccountVerificationRequired fences dispatch even if persisting the marker
// fails. Never let a catalog success or cancellation erase this observation.
func (l *AccountLease) MarkAccountVerificationRequired() error {
	if l == nil || l.account == nil || l.pool == nil {
		return errors.New("账户租约未初始化")
	}
	l.operation.Lock()
	authGeneration := l.authGeneration
	l.operation.Unlock()
	p, a := l.pool, l.account
	p.mu.Lock()
	if p.byID[a.ID] != a || a.authGeneration != authGeneration || (a.runtime.VerificationID != "" && a.verificationPending == "") {
		p.mu.Unlock()
		return nil
	}
	if a.verificationPending == "" {
		a.verificationPending = rand.Text()
		p.notifyLocked()
	}
	id := a.verificationPending
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := p.updateRuntimeContext(ctx, a.ID, func(account *Account, state *accountRuntimeState) (bool, func(*Account), error) {
		if account != a || account.verificationPending != id {
			return false, nil, nil
		}
		changed := state.VerificationID == ""
		if changed {
			state.VerificationID = id
		}
		return changed, func(a *Account) { a.verificationPending = "" }, nil
	})
	return err
}

// CheckAccountVerification rechecks a held lease before new data-plane work.
// Explicit management/catalog reads do not clear the marker.
func (l *AccountLease) CheckAccountVerification() error {
	if l == nil || l.account == nil || l.pool == nil || (l.purpose != leaseGeneration && l.purpose != leaseResourceResult) {
		return nil
	}
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	if l.account.verificationIDLocked() != "" {
		return SessionError(AccountVerificationRequiredCode)
	}
	return nil
}

// ClearAccountVerification is an operator acknowledgment of completed Google
// verification. Compare the exact occurrence before clearing it so an old UI
// submission cannot release a new hold.
func (l *AccountLease) ClearAccountVerification(ctx context.Context, expectedID string) error {
	if l == nil || l.account == nil || l.pool == nil || l.purpose != leaseManagement {
		return SessionError("ACCOUNT_MANAGEMENT_REQUIRED")
	}
	_, err := l.pool.updateRuntimeContext(ctx, l.account.ID, func(account *Account, state *accountRuntimeState) (bool, func(*Account), error) {
		if account != l.account || expectedID == "" || account.verificationIDLocked() != expectedID {
			return false, nil, SessionError("ACCOUNT_VERIFICATION_CHANGED")
		}
		changed := state.VerificationID != ""
		state.VerificationID = ""
		return changed, func(a *Account) { a.verificationPending = "" }, nil
	})
	return err
}
