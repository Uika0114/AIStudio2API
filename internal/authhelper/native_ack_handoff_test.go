package authhelper

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func TestNativeRecoveryHandoffAfterNotStarted(t *testing.T) {
	for _, mode := range []string{"reconcile", "already-committed"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			broker, mapping, renew := fixtureBroker(t, nil)
			client, _ := fixtureHTTP(t, broker, mapping)
			state, path, old := queryIntent(t, mapping, "reauth", false)
			meta, _ := state.Session()
			before := *meta.Remote.Pending
			clean := *meta.Remote
			clean.Pending, clean.RecoveryRequired = nil, false
			if err := aistudio.ReserveRemoteRecoveryAuth(ctx, path, &clean, old.OperationID, false, time.Now().Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			controlBefore, err := os.ReadFile(path + ".recovery-control.json")
			if err != nil {
				t.Fatal(err)
			}
			if err := reconcileProjection(ctx, client, path); safeCode(err) != "NOT_STARTED" || renew.Load() != 0 {
				t.Fatal("old intent must only be queried and retired", err, renew.Load())
			}
			controlAfter, _ := os.ReadFile(path + ".recovery-control.json")
			if string(controlBefore) != string(controlAfter) {
				t.Fatal("metadata retirement alone changed the expired budget")
			}
			if mode == "already-committed" {
				// Emulate the old native writer up to the durable commit, before ACK.
				state, err = aistudio.LoadStorageState(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := client.PrepareRecovery(ctx, &state, path, "reauth", func(string) error { return nil }); err != nil {
					t.Fatal(err)
				}
				if err := aistudio.WriteManagedSessionState(ctx, path, &state); err != nil {
					t.Fatal(err)
				}
			}
			transport := client.http.Transport
			var requests []AckRequest
			var refreshes int
			client.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path != "/v1/ack" {
					refreshes++
					return transport.RoundTrip(req)
				}
				var ack AckRequest
				body, err := req.GetBody()
				if err != nil {
					return nil, err
				}
				err = json.NewDecoder(body).Decode(&ack)
				body.Close()
				if err != nil {
					return nil, err
				}
				requests = append(requests, ack)
				response, err := transport.RoundTrip(req)
				if err == nil && len(requests) == 1 {
					response.Body.Close()
					return nil, errors.New("fixture response loss after ACK")
				}
				return response, err
			})
			if err := reconcileProjection(ctx, client, path); safeCode(err) != "HELPER_UNAVAILABLE" || len(requests) != 1 {
				t.Fatal("expected lost ACK response after durable handoff", err, len(requests))
			}
			state, _ = aistudio.LoadStorageState(path)
			meta, _ = state.Session()
			if meta.Remote.Ack == nil || requests[0].OperationID != meta.Remote.Ack.ID || requests[0].OperationID == old.OperationID {
				t.Fatal("lost ACK response did not preserve the exact new ACK")
			}
			if err := reconcileProjection(ctx, client, path); err != nil {
				t.Fatal("retry exact persisted ACK", err)
			}
			wantRefreshes := 0
			if mode == "reconcile" {
				wantRefreshes = 1
			}
			if len(requests) != 2 || requests[0] != requests[1] || refreshes != wantRefreshes || renew.Load() != 1 {
				t.Fatal("ACK retry reissued authentication or acknowledged another operation", len(requests), refreshes, renew.Load())
			}
			state, _ = aistudio.LoadStorageState(path)
			meta, _ = state.Session()
			r := meta.Remote
			status, err := aistudio.ReadRemoteRecoveryStatus(path, r, true, time.Now())
			if err != nil || status.Code != "OK" || r.Pending != nil || r.Ack != nil || r.RecoveryRequired || r.EnrollmentRequired || r.CookieRevision != old.BaseCookie+1 || !reflect.DeepEqual(r.RetiredOperations, []aistudio.SessionOperation{before}) {
				t.Fatal("handoff did not close the budget or preserve the retired intent", status, err)
			}
		})
	}
}

func TestNativeRecoveryDoesNotCreateIntentAgainstUnretiredRound(t *testing.T) {
	ctx := t.Context()
	broker, mapping, renew := fixtureBroker(t, nil)
	client, _ := fixtureHTTP(t, broker, mapping)
	state, path := fixtureRemoteState(t, mapping)
	meta, _ := state.Session()
	if err := aistudio.ReserveRemoteRecoveryAuth(ctx, path, meta.Remote, "unretired_fixture_operation", false, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	meta.Remote.RecoveryRequired = true
	if state.SetSession(meta) != nil || aistudio.WriteManagedSessionState(ctx, path, &state) != nil {
		t.Fatal("fixture recovery fence")
	}
	before, _ := os.ReadFile(path)
	var requests int
	original := client.http.Transport
	client.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		return original.RoundTrip(req)
	})
	if err := reconcileProjection(ctx, client, path); safeCode(err) != "RECOVERY_BUDGET_REQUIRED" {
		t.Fatal("missing retirement did not block before intent creation", err)
	}
	if after, _ := os.ReadFile(path); string(before) != string(after) || renew.Load() != 0 || requests != 0 {
		t.Fatal("preflight rejection created work", renew.Load(), requests)
	}
}
