package authhelper

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func TestQueryReadyWithNewerLocalCookiesDoesNotCommitOrAck(t *testing.T) {
	for _, advance := range []bool{false, true} {
		t.Run(map[bool]string{false: "ready-current", true: "ready-newer-local"}[advance], func(t *testing.T) {
			b, m, count := fixtureBroker(t, nil)
			c, _ := fixtureHTTP(t, b, m)
			s, p, req := queryIntent(t, m, "reauth", advance)
			if _, err := b.Refresh(t.Context(), m.Consumer, req); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(p)
			cookies := append([]aistudio.StateCookie(nil), s.Cookies...)
			err := c.PrepareRecovery(t.Context(), &s, p, "reauth", func(string) error { t.Fatal("lookup used authentication budget"); return nil })
			after, _ := os.ReadFile(p)
			if string(before) != string(after) || count.Load() != 1 {
				t.Fatal("prepare wrote cookies or reauthenticated")
			}
			meta, _ := s.Session()
			if advance {
				if safeCode(err) != "VERSION_CONFLICT" || meta.Remote.Pending == nil || meta.Remote.Ack != nil || !reflect.DeepEqual(cookies, s.Cookies) {
					t.Fatal("independent local state overwritten", err)
				}
			} else {
				if err != nil || meta.Remote.Pending != nil || meta.Remote.Ack == nil {
					t.Fatal("ready not prepared", err)
				}
				if err := commitNativeProjection(t.Context(), c, &s, p); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestQueryCASChangeDuringLookupNeverDisposesOrOverwrites(t *testing.T) {
	for _, kind := range []string{"ready", "not-started", "keep-current"} {
		t.Run(kind, func(t *testing.T) {
			b, m, _ := fixtureBroker(t, nil)
			c, _ := fixtureHTTP(t, b, m)
			s, p, req := queryIntent(t, m, "reauth", true)
			if kind != "not-started" {
				if _, err := b.Refresh(t.Context(), m.Consumer, req); err != nil {
					t.Fatal(err)
				}
			}
			original := c.http.Transport
			c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				res, err := original.RoundTrip(r)
				disk, e := aistudio.LoadStorageState(p)
				if e != nil {
					t.Fatal(e)
				}
				disk.Cookies = fixtureCookies("concurrent-writer-private")
				if e = aistudio.WriteStorageState(p, disk); e != nil {
					t.Fatal(e)
				}
				return res, err
			})
			var err error
			if kind == "keep-current" {
				err = c.ReconcileKeepingCurrent(t.Context(), &s, p)
			} else {
				err = c.PrepareRecovery(t.Context(), &s, p, "reauth", nil)
			}
			disk, _ := aistudio.LoadStorageState(p)
			meta, _ := disk.Session()
			if safeCode(err) != "VERSION_CONFLICT" || disk.Cookies[0].Value != "concurrent-writer-private" || meta.Remote.Pending == nil || meta.Remote.Ack != nil || len(meta.Remote.RetiredOperations) != 0 {
				t.Fatal("CAS allowed obsolete disposition", err)
			}
		})
	}
}

func TestQueryCancellationDoesNotCommitLateResult(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run(map[bool]string{false: "not-started", true: "ready"}[ready], func(t *testing.T) {
			b, m, count := fixtureBroker(t, nil)
			c, _ := fixtureHTTP(t, b, m)
			s, p, req := queryIntent(t, m, "reauth", false)
			if ready {
				if _, err := b.Refresh(t.Context(), m.Consumer, req); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			original := c.http.Transport
			c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				res, err := original.RoundTrip(r)
				cancel()
				return res, err
			})
			before, _ := os.ReadFile(p)
			err := c.PrepareRecovery(ctx, &s, p, "reauth", nil)
			after, _ := os.ReadFile(p)
			if safeCode(err) != "CANCELED" || string(before) != string(after) {
				t.Fatal("canceled lookup committed", err)
			}
			if ready && count.Load() != 1 || !ready && count.Load() != 0 {
				t.Fatal("cancellation repeated authentication")
			}
		})
	}
}

func TestQueryRejectsMismatchedReceiptAndChangedResult(t *testing.T) {
	for _, kind := range []string{"receipt", "epoch", "handle", "identity", "cookie-domain", "cookie-partition", "result-version", "result-cookie"} {
		t.Run(kind, func(t *testing.T) {
			b, m, count := fixtureBroker(t, nil)
			c, _ := fixtureHTTP(t, b, m)
			s, p, req := queryIntent(t, m, "reauth", false)
			if kind != "receipt" {
				if _, err := b.Refresh(t.Context(), m.Consumer, req); err != nil {
					t.Fatal(err)
				}
			}
			original := c.http.Transport
			c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				res, err := original.RoundTrip(r)
				if err != nil {
					return res, err
				}
				defer res.Body.Close()
				var j Result
				if e := json.NewDecoder(res.Body).Decode(&j); e != nil {
					t.Fatal(e)
				}
				switch kind {
				case "receipt":
					j.NotStarted.OperationID = "other_operation_0001"
				case "epoch":
					j.Session.Epoch++
				case "handle":
					j.Session.Handle = "other_handle_000001"
				case "identity":
					j.Session.GaiaID = "other_gaia"
				case "cookie-domain":
					j.Session.Cookies[0].Domain = ".evil.invalid"
				case "cookie-partition":
					j.Session.Cookies[0].PartitionKey = "partition"
				case "result-version":
					j.Session.Version++
				case "result-cookie":
					j.Session.CookieRevision++
				}
				data, _ := json.Marshal(j)
				res.Body = io.NopCloser(strings.NewReader(string(data)))
				res.ContentLength = int64(len(data))
				return res, nil
			})
			before, _ := os.ReadFile(p)
			err := c.PrepareRecovery(t.Context(), &s, p, "reauth", nil)
			after, _ := os.ReadFile(p)
			if err == nil || string(before) != string(after) || count.Load() > 1 {
				t.Fatal("untrusted result accepted", err)
			}
		})
	}
}

func TestQueryPureAckLossAndLocalSaveFailureOnlyRepeatAck(t *testing.T) {
	b, m, count := fixtureBroker(t, nil)
	c, _ := fixtureHTTP(t, b, m)
	s, p := fixtureRemoteState(t, m)
	if err := c.PrepareRecovery(t.Context(), &s, p, "reauth", func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := aistudio.WriteManagedSessionState(t.Context(), p, &s); err != nil {
		t.Fatal(err)
	}
	original := c.http.Transport
	acks, refresh := 0, 0
	lose := true
	saveFail := false
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/v1/refresh" {
			refresh++
		}
		if r.URL.Path == "/v1/ack" {
			acks++
		}
		res, err := original.RoundTrip(r)
		if err != nil {
			return res, err
		}
		if r.URL.Path == "/v1/ack" && lose {
			lose = false
			res.Body.Close()
			return nil, errors.New("lost ack response")
		}
		if r.URL.Path == "/v1/ack" && saveFail {
			saveFail = false
			disk, _ := aistudio.LoadStorageState(p)
			disk.Cookies = fixtureCookies("independent-after-ack")
			if e := aistudio.WriteStorageState(p, disk); e != nil {
				t.Fatal(e)
			}
		}
		return res, nil
	})
	if err := reconcileProjection(t.Context(), c, p); safeCode(err) != "HELPER_UNAVAILABLE" {
		t.Fatal("lost ack was hidden", err)
	}
	saveFail = true
	if err := reconcileProjection(t.Context(), c, p); safeCode(err) != "STATE_UNAVAILABLE" {
		t.Fatal("clear-ack persistence failure was hidden", err)
	}
	if err := reconcileProjection(t.Context(), c, p); err != nil {
		t.Fatal(err)
	}
	disk, _ := aistudio.LoadStorageState(p)
	meta, _ := disk.Session()
	if count.Load() != 1 || refresh != 0 || acks != 3 || meta.Remote.Ack != nil || disk.Cookies[0].Value != "independent-after-ack" {
		t.Fatal("ack-only reconciliation reauthenticated or overwrote cookies")
	}
}

func TestQueryHistoryRetrievalDoesNotPermitArbitraryOldAck(t *testing.T) {
	b, m, count := fixtureBroker(t, nil)
	c, _ := fixtureHTTP(t, b, m)
	s, p, req := queryIntent(t, m, "reauth", false)
	result, err := b.Refresh(t.Context(), m.Consumer, req)
	if err != nil {
		t.Fatal(err)
	}
	ackResult(t, b, m, result)
	// Keep the same current versions while moving a definitely-unstarted new ID
	// into the current slot; the prior ready result is now in History.
	other := req
	other.OperationID = "other_unstarted_000001"
	other.BaseCookie = result.CookieRevision
	other.BaseMaterial = result.MaterialVersion
	other.LookupOnly = true
	if _, e := b.Refresh(t.Context(), m.Consumer, other); safeCode(e) != "NOT_STARTED" {
		t.Fatal(e)
	}
	if e := c.PrepareRecovery(t.Context(), &s, p, "reauth", nil); e != nil {
		t.Fatal("current-version history should be observable", e)
	}
	if e := commitNativeProjection(t.Context(), c, &s, p); safeCode(e) != "VERSION_CONFLICT" {
		t.Fatal("old helper ack constraint changed", e)
	}
	if count.Load() != 1 {
		t.Fatal("history lookup authenticated")
	}
}
