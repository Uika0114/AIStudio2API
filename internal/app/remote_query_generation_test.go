package app

import (
	"context"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func TestRemoteQueryStaleLeaseCannotBypassUnfinishedRecovery(t *testing.T) {
	for _, resolved := range []bool{false, true} {
		t.Run(map[bool]string{false: "still-pending", true: "other-lease-committed"}[resolved], func(t *testing.T) {
			f := newQueryAppFixture(t, "not-started", "reauth", false)
			old, _ := aistudio.AccountLeaseFromContext(f.ctx)
			if e := old.Release(); e != nil {
				t.Fatal(e)
			}
			selection := aistudio.AccountSelection{AccountID: f.account.ID, ModelID: "query-fixture-model", Method: "generateContent", Channel: aistudio.ChannelPlayground, StrictChannel: true}
			stale, e := f.pool.AcquireFor(t.Context(), selection)
			if e != nil {
				t.Fatal(e)
			}
			defer stale.Release()
			other, e := f.pool.AcquireFor(t.Context(), selection)
			if e != nil {
				t.Fatal(e)
			}
			if resolved {
				ctx := aistudio.WithRemoteAuthBudget(t.Context())
				if e = other.RefreshManagedStorageState(ctx, func(s *aistudio.StorageState) error {
					meta, _ := s.Session()
					meta.Remote.Pending = nil
					meta.Remote.RecoveryRequired = false
					meta.Remote.CookieRevision++
					s.Cookies = fixtureRefreshCookies("other-success-private-sentinel")
					return s.SetSession(meta)
				}, func() (func(bool), error) { return func(bool) {}, nil }); e != nil {
					t.Fatal(e)
				}
			} else {
				s, e := other.ReloadStorageState()
				if e != nil {
					t.Fatal(e)
				}
				s.Cookies = fixtureRefreshCookies("new-local-still-unresolved")
				if e = other.SaveStorageState(s); e != nil {
					t.Fatal(e)
				}
			}
			if e = other.Release(); e != nil {
				t.Fatal(e)
			}
			ctx := aistudio.WithRemoteAuthBudget(aistudio.ContextWithAccountLease(t.Context(), stale))
			sends := 0
			_, err := f.refresher.do(ctx, "GenerateContent", func() (*aistudio.RPCResponse, error) {
				sends++
				if sends == 1 {
					return nil, &aistudio.RPCError{StatusCode: 401, Code: 16}
				}
				return &aistudio.RPCResponse{StatusCode: 200}, nil
			})
			if resolved {
				if err != nil || sends != 2 {
					t.Fatal("legitimate concurrent refresh reuse broke", err)
				}
			} else if !aistudio.IsSessionFailureCode(err, "VERSION_CONFLICT") || sends != 1 {
				t.Fatal("stale lease turned unresolved state into replay", err)
			}
			if f.refresh.Load() != 0 || f.auth.Load() != 0 || f.resets != 0 {
				t.Fatal("deduplicated branch did unexpected recovery")
			}
		})
	}
}

type queryBidiTransport struct{ opens int }

func (q *queryBidiTransport) DoProtected(context.Context, aistudio.GenerateRequest, aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
	return nil, aistudio.SessionError("INVALID_REQUEST")
}
func (q *queryBidiTransport) OpenBidiProtected(_ context.Context, _ aistudio.BidiRequest, _ aistudio.RequestContext, _ *aistudio.AccountLease, release func() error) (*aistudio.BidiSession, error) {
	q.opens++
	_ = release()
	return nil, &aistudio.RPCError{StatusCode: 401, Code: 16}
}

func TestRemoteQueryBidiDoesNotReopenUnknownOrUnstarted(t *testing.T) {
	for _, outcome := range []string{"not-started", "indeterminate"} {
		t.Run(outcome, func(t *testing.T) {
			f := newQueryAppFixture(t, outcome, "reauth", true)
			fake := &queryBidiTransport{}
			transport := &authRetryProtectedTransport{transport: fake, refresher: f.refresher}
			lease, _ := aistudio.AccountLeaseFromContext(f.ctx)
			released := 0
			_, err := transport.OpenBidiProtected(f.ctx, aistudio.BidiRequest{}, aistudio.RequestContext{}, lease, func() error { released++; return nil })
			want := map[string]string{"not-started": "NOT_STARTED", "indeterminate": "INDETERMINATE"}[outcome]
			// The failed-open gate deliberately keeps the lease for auth recovery.
			// On terminal recovery failure its outer caller, not the abandoned gate,
			// owns the final release. No second open or double-release is permitted.
			if !aistudio.IsSessionFailureCode(err, want) || fake.opens != 1 || released != 0 || f.refresh.Load() != 1 || f.ack.Load() != 0 {
				t.Fatal("Bidi reopened or released recovery lease early", err, fake.opens, released)
			}
			released++
			if released != 1 {
				t.Fatal("caller cleanup did not retain sole ownership")
			}
		})
	}
}
