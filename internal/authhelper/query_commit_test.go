package authhelper

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func queryIntent(t *testing.T, m Mapping, reason string, advance bool) (aistudio.StorageState, string, RefreshRequest) {
	t.Helper()
	s, p := fixtureRemoteState(t, m)
	s.Cookies = fixtureCookies("local-private-sentinel")
	meta, _ := s.Session()
	r := meta.Remote
	r.EnrollmentRequired = reason == "bootstrap"
	r.RecoveryRequired = true
	r.Pending = &aistudio.SessionOperation{ID: "query_fixture_operation_0001", Reason: reason, Epoch: r.Epoch, BaseCookie: r.CookieRevision, BaseMaterial: r.MaterialVersion, StateRevision: meta.Revision + 1, Deadline: time.Now().Add(time.Minute).Unix()}
	if s.SetSession(meta) != nil || aistudio.WriteManagedSessionState(t.Context(), p, &s) != nil {
		t.Fatal("intent fixture")
	}
	if advance {
		s.Cookies = fixtureCookies("independent-private-sentinel")
		if e := aistudio.WriteStorageState(p, s); e != nil {
			t.Fatal(e)
		}
		s, _ = aistudio.LoadStorageState(p)
	}
	meta, _ = s.Session()
	return s, p, operationRequest(meta.Remote.Pending, m.Handle)
}

func TestQueryPendingBeforeDeliveryNeverAuthenticates(t *testing.T) {
	for _, advance := range []bool{false, true} {
		for _, reason := range []string{"reauth", "bootstrap"} {
			t.Run(reason+map[bool]string{false: "-same", true: "-advanced"}[advance], func(t *testing.T) {
				b, m, count := fixtureBroker(t, nil)
				c, _ := fixtureHTTP(t, b, m)
				s, p, req := queryIntent(t, m, reason, advance)
				before, _ := s.Session()
				cookies := append([]aistudio.StateCookie(nil), s.Cookies...)
				budgetCalls := 0
				err := c.PrepareRecovery(t.Context(), &s, p, reason, func(string) error { budgetCalls++; return failure("RECOVERY_BUDGET_EXHAUSTED") })
				if safeCode(err) != "NOT_STARTED" {
					t.Fatal("existing pending must only query and retire", err)
				}
				disk, e := aistudio.LoadStorageState(p)
				if e != nil {
					t.Fatal(e)
				}
				after, _ := disk.Session()
				if count.Load() != 0 || budgetCalls != 0 || after.Remote.Pending != nil || after.Remote.Ack != nil || !after.Remote.RecoveryRequired || after.Remote.EnrollmentRequired != before.Remote.EnrollmentRequired || !reflect.DeepEqual(disk.Cookies, cookies) || after.Remote.CookieRevision != before.Remote.CookieRevision || after.Remote.MaterialVersion != before.Remote.MaterialVersion {
					t.Fatal("retirement changed authentication/flags/cookies")
				}
				if len(after.Remote.RetiredOperations) != 1 || !operationMatches(&after.Remote.RetiredOperations[0], req) || after.Remote.RetiredOperations[0].StateRevision != before.Remote.Pending.StateRevision {
					t.Fatal("intent history lost")
				}
				a, _ := aistudio.LoadStorageState(m.StateFile)
				am, _ := a.Session()
				if am.Authority.Operation.Failure != "NOT_STARTED" {
					t.Fatal("missing durable no-send proof")
				}
			})
		}
	}
}

func TestQueryAdvancedPendingStillObservesIndeterminate(t *testing.T) {
	b, m, count := fixtureBroker(t, func(_ context.Context, s aistudio.StorageState, _ Mapping, _ bool) (aistudio.StorageState, error) {
		return s, errors.New("secret transport sentinel")
	})
	c, _ := fixtureHTTP(t, b, m)
	s, p, req := queryIntent(t, m, "reauth", true)
	if _, err := b.Refresh(t.Context(), m.Consumer, req); safeCode(err) != "INDETERMINATE" {
		t.Fatal(err)
	}
	before, _ := aistudio.LoadStorageState(p)
	budget := 0
	if err := c.PrepareRecovery(t.Context(), &s, p, "reauth", func(string) error { budget++; return nil }); safeCode(err) != "INDETERMINATE" {
		t.Fatal("local revision blocked remote observation", err)
	}
	after, _ := aistudio.LoadStorageState(p)
	if count.Load() != 1 || budget != 0 || !reflect.DeepEqual(before, after) {
		t.Fatal("unknown result changed state or authenticated")
	}
}

func TestQueryBootstrapKeepCurrentCannotFinishEnrollment(t *testing.T) {
	b, m, count := fixtureBroker(t, nil)
	c, _ := fixtureHTTP(t, b, m)
	s, p, _ := queryIntent(t, m, "bootstrap", true)
	err := c.ReconcileKeepingCurrent(t.Context(), &s, p)
	if err == nil {
		t.Fatal("unexecuted bootstrap falsely completed")
	}
	if safeCode(err) != "NOT_STARTED" {
		t.Fatal("bootstrap no-send should retire only", err)
	}
	disk, e := aistudio.LoadStorageState(p)
	if e != nil {
		t.Fatal(e)
	}
	meta, _ := disk.Session()
	if !meta.Remote.EnrollmentRequired || !meta.Remote.RecoveryRequired || meta.Remote.Pending != nil || meta.Remote.Ack != nil || count.Load() != 0 {
		t.Fatal("unexecuted bootstrap lost registration fence")
	}
}

func TestQueryWireIsLookupOnlyWithImmutableParameters(t *testing.T) {
	b, m, count := fixtureBroker(t, nil)
	c, _ := fixtureHTTP(t, b, m)
	s, p, req := queryIntent(t, m, "reauth", false)
	original := c.http.Transport
	seen := 0
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var actual RefreshRequest
		if e := json.NewDecoder(r.Body).Decode(&actual); e != nil {
			t.Fatal(e)
		}
		r.Body.Close()
		want := req
		want.LookupOnly = true
		if actual != want {
			t.Fatal("pending lookup mutated original operation or can execute")
		}
		seen++
		return nil, failure("HELPER_UNAVAILABLE")
	})
	defer func() { c.http.Transport = original }()
	err := c.PrepareRecovery(t.Context(), &s, p, "reauth", nil)
	if safeCode(err) != "HELPER_UNAVAILABLE" || seen != 1 || count.Load() != 0 {
		t.Fatal("query unexpectedly needs new auth budget", err, seen)
	}
}
