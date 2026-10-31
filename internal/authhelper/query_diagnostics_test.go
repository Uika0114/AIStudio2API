package authhelper

import (
	"context"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func TestQueryDiagnosticsAreStageScopedAndDoNotInventRevision(t *testing.T) {
	b, m, count := fixtureBroker(t, nil)
	c, _ := fixtureHTTP(t, b, m)
	s, p := fixtureRemoteState(t, m)
	var events []RecoveryProgress
	ctx := WithRecoveryObserver(t.Context(), func(v RecoveryProgress) { events = append(events, v) })
	if e := c.PrepareRecovery(ctx, &s, p, "reauth", func(string) error { return nil }); e != nil {
		t.Fatal(e)
	}
	if e := commitNativeProjection(ctx, c, &s, p); e != nil {
		t.Fatal(e)
	}
	query, commit, ack := 0, 0, 0
	for _, v := range events {
		switch v.Phase {
		case "query":
			query++
			if v.LookupOnly == nil || *v.LookupOnly {
				t.Fatal("new authentication query mislabeled lookup")
			}
		case "commit":
			commit++
			if v.LookupOnly != nil || v.Revision == nil {
				t.Fatal("commit misrepresented as lookup or unknown revision")
			}
		case "ack":
			ack++
			if v.LookupOnly != nil || v.Revision != nil {
				t.Fatal("ack invented file revision or lookup applicability")
			}
		}
	}
	if query != 1 || commit != 1 || ack != 1 || count.Load() != 1 {
		t.Fatal("missing stage observations")
	}
}

func TestQueryKeepCurrentReauthRetiresOnlyAndBootstrapReadyCannotFinish(t *testing.T) {
	for _, reason := range []string{"reauth", "bootstrap"} {
		t.Run(reason, func(t *testing.T) {
			b, m, count := fixtureBroker(t, nil)
			c, _ := fixtureHTTP(t, b, m)
			s, p, req := queryIntent(t, m, reason, true)
			if reason == "bootstrap" {
				if _, e := b.Refresh(t.Context(), m.Consumer, req); e != nil {
					t.Fatal(e)
				}
			}
			cookies := s.Cookies
			e := reconcileProjection(context.Background(), c, p, true)
			state, x := aistudio.LoadStorageState(p)
			if x != nil {
				t.Fatal(x)
			}
			meta, _ := state.Session()
			if reason == "bootstrap" {
				if !aistudio.IsSessionFailureCode(e, "BOOTSTRAP_REQUIRED") || !meta.Remote.EnrollmentRequired || meta.Remote.Pending == nil || meta.Remote.Ack != nil || count.Load() != 0 {
					t.Fatal("operator invented completed bootstrap", e)
				}
			} else {
				if e != nil || meta.Remote.Pending != nil || meta.Remote.RecoveryRequired || meta.Remote.EnrollmentRequired || meta.Remote.Ack != nil || count.Load() != 0 || meta.Remote.CookieRevision != 0 {
					t.Fatal("explicit reauth disposition did extra work", e)
				}
			}
			if len(state.Cookies) != len(cookies) || state.Cookies[0].Value != cookies[0].Value {
				t.Fatal("operator changed local cookies")
			}
		})
	}
}
