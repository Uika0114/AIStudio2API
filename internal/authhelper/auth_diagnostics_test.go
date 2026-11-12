package authhelper

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/chromeauth"
)

func TestAuthenticationCountsAreResponseConfirmedLowerBound(t *testing.T) {
	counts := new(authCounts)
	for _, v := range []chromeauth.HTTPObservation{
		{Phase: "challenge", Stage: "transport", Category: "DNS_FAILURE", SendEvidence: "unknown"},
		{Phase: "assertion", Stage: "transport", Category: "TRANSPORT_FAILURE", SendEvidence: "unknown"},
		{Phase: "workflow", ResponseReceived: true},
	} {
		counts.observe(v)
	}
	if got := counts.snapshot(); got.Sent != 0 || got.Responses != 0 {
		t.Fatal("Do attempts or workflow completion counted as sending")
	}
	counts.observe(chromeauth.HTTPObservation{Phase: "challenge", ResponseReceived: true})
	counts.observe(chromeauth.HTTPObservation{Phase: "assertion", ResponseReceived: true, Category: "RESPONSE_READ_FAILURE"})
	got := counts.snapshot()
	if got.Sent != 2 || got.Responses != 2 {
		t.Fatal("confirmed response lower bound lost")
	}
	wire, err := json.Marshal(got)
	if err != nil || string(wire) != `{"sent":2,"responses":2}` {
		t.Fatal("strict legacy count shape changed", err)
	}
}

func TestBrokerZeroObservationsNeverBecomeNotStarted(t *testing.T) {
	for _, cause := range []error{io.EOF, context.Canceled, context.DeadlineExceeded, errors.New("DNS fixture-private-token"), failure("NOT_STARTED")} {
		t.Run(cause.Error(), func(t *testing.T) {
			b, m, count := fixtureBroker(t, func(_ context.Context, s aistudio.StorageState, _ Mapping, _ bool) (aistudio.StorageState, error) {
				return s, cause
			})
			req := fixtureRequest(m, "unknown_observation_0001", 0)
			if _, err := b.Refresh(t.Context(), m.Consumer, req); safeCode(err) != "INDETERMINATE" {
				t.Fatal("unproved backend failure became safe retry", err)
			}
			state, err := aistudio.LoadStorageState(m.StateFile)
			if err != nil {
				t.Fatal(err)
			}
			meta, _ := state.Session()
			op := meta.Authority.Operation
			if op.Stage != "indeterminate" || op.Failure != "INDETERMINATE" || op.HTTP == nil || op.HTTP.Sent != 0 || op.HTTP.Responses != 0 {
				t.Fatal("zero observations misclassified")
			}
			b.now = func() time.Time { return time.Now().Add(time.Hour) }
			req.LookupOnly = true
			if _, err := b.Refresh(t.Context(), m.Consumer, req); safeCode(err) != "INDETERMINATE" || count.Load() != 1 {
				t.Fatal("deadline or lookup erased unknown history", err)
			}
		})
	}
}

func TestBrokerDefinitiveFailuresRequireDurableSave(t *testing.T) {
	for _, code := range []string{"MATERIAL_REJECTED", "SOURCE_REBIND_REQUIRED", "UNSUPPORTED_KEY", "IDENTITY_MISMATCH", "VERSION_CONFLICT", "OPERATION_CONFLICT", "COOKIE_POLICY_FAILURE"} {
		for _, failSave := range []bool{false, true} {
			t.Run(code+map[bool]string{false: "-saved", true: "-save-failed"}[failSave], func(t *testing.T) {
				b, m, count := fixtureBroker(t, func(_ context.Context, s aistudio.StorageState, _ Mapping, _ bool) (aistudio.StorageState, error) {
					return s, failure(code)
				})
				if failSave {
					writes := 0
					write := b.write
					b.write = func(ctx context.Context, p string, s *aistudio.StorageState) error {
						writes++
						if writes == 2 {
							return errors.New("fixture-private-disk-error")
						}
						return write(ctx, p, s)
					}
				}
				want := code
				if failSave {
					want = "INDETERMINATE"
				}
				req := fixtureRequest(m, "definitive_failure_0001", 0)
				for i := 0; i < 2; i++ {
					if _, err := b.Refresh(t.Context(), m.Consumer, req); safeCode(err) != want {
						t.Fatalf("durability classification: %v", err)
					}
				}
				if count.Load() != 1 {
					t.Fatal("same failed operation authenticated again")
				}
				s, err := aistudio.LoadStorageState(m.StateFile)
				if err != nil {
					t.Fatal(err)
				}
				meta, _ := s.Session()
				if s.Cookies[0].Value != "initial-fixture" || meta.Authority.CookieRevision != 0 || meta.Authority.MaterialVersion != 1 {
					t.Fatal("failure changed valid material")
				}
				if !failSave && (meta.Authority.Operation.Stage != "failed" || meta.Authority.Operation.Failure != code) {
					t.Fatal("safe failure category was lost")
				}
			})
		}
	}
}

func TestBrokerRPCNeverEnablesChromeSourceRecovery(t *testing.T) {
	b, m, count := fixtureBroker(t, func(_ context.Context, s aistudio.StorageState, _ Mapping, allow bool) (aistudio.StorageState, error) {
		if allow {
			t.Fatal("RPC inherited legacy permission to read Chrome")
		}
		return s, failure("MATERIAL_REJECTED")
	})
	s, err := aistudio.LoadStorageState(m.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := s.Session()
	meta.Authority.AllowSourceRecovery = true
	if s.SetSession(meta) != nil || aistudio.WriteManagedSessionState(t.Context(), m.StateFile, &s) != nil {
		t.Fatal("legacy policy fixture")
	}
	if _, err := b.Refresh(t.Context(), m.Consumer, fixtureRequest(m, "saved_material_only_0001", 0)); safeCode(err) != "MATERIAL_REJECTED" || count.Load() != 1 {
		t.Fatal("saved material renewal contract changed", err)
	}
}

func TestBrokerAuthenticationDiagnosticsAreSafeAndIntentScoped(t *testing.T) {
	b, m, _ := fixtureBroker(t, func(_ context.Context, s aistudio.StorageState, _ Mapping, _ bool) (aistudio.StorageState, error) {
		return s, errors.New("fixture-private-cookie-token-key-url")
	})
	var events []AuthenticationProgress
	b.lifecycle = WithAuthenticationObserver(t.Context(), func(v AuthenticationProgress) { events = append(events, v) })
	req := fixtureRequest(m, "diagnostic_fixture_0001", 0)
	if _, err := b.Refresh(t.Context(), m.Consumer, req); safeCode(err) != "INDETERMINATE" {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].OperationID != req.OperationID || events[0].Epoch != req.Epoch || events[0].BaseCookie != req.BaseCookie || events[0].BaseMaterial != req.BaseMaterial || events[0].HTTP.Category != "INDETERMINATE" {
		t.Fatalf("missing intent-scoped failure: %+v", events)
	}
	encoded, _ := json.Marshal(events)
	if strings.Contains(string(encoded), "fixture-private") {
		t.Fatal("secret escaped diagnostics")
	}
}

func TestBrokerUnstartedReceiptIsOnlyEmittedAfterExactDurableRecord(t *testing.T) {
	for _, failSave := range []bool{false, true} {
		t.Run(map[bool]string{false: "durable", true: "save-failed"}[failSave], func(t *testing.T) {
			b, m, count := fixtureBroker(t, nil)
			if failSave {
				b.write = func(context.Context, string, *aistudio.StorageState) error {
					return errors.New("fixture-private-write-error")
				}
			}
			req := fixtureRequest(m, "unstarted_receipt_0001", 0)
			req.LookupOnly = true
			_, err := b.Refresh(t.Context(), m.Consumer, req)
			var receipt *notStartedError
			if failSave {
				if errors.As(err, &receipt) || safeCode(err) != "STATE_UNAVAILABLE" {
					t.Fatal("save failure emitted safe receipt", err)
				}
			} else if !errors.As(err, &receipt) || receipt.receipt != req {
				t.Fatal("lost exact durable receipt", err)
			}
			if count.Load() != 0 {
				t.Fatal("lookup started authentication")
			}
		})
	}
}

func TestNativeDeviceKeyFailureIsDefinitiveWithoutChrome(t *testing.T) {
	_, mapping, _ := fixtureBroker(t, nil)
	state, err := aistudio.LoadStorageState(mapping.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	refresh := func(context.Context, aistudio.ChromeOAuthMaterial, string) ([]aistudio.StateCookie, error) {
		return nil, chromeauth.ErrDeviceKeyUnavailable
	}
	importer := func(context.Context, chromeauth.ImportOptions) ([]chromeauth.ImportResult, error) {
		t.Fatal("device-key failure attempted Chrome recovery")
		return nil, nil
	}
	result, err := nativeRenewWith(t.Context(), state, mapping, true, refresh, importer)
	if safeCode(err) != "UNSUPPORTED_KEY" || result.Cookies[0].Value != state.Cookies[0].Value {
		t.Fatal("device-key classification or material changed", err)
	}
}
