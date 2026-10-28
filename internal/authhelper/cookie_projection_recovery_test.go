package authhelper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/chromeauth"
)

func installProjectionFullState(t *testing.T, m Mapping) aistudio.StorageState {
	t.Helper()
	old, err := aistudio.LoadStorageState(m.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	full := projectionFullState(t, m, "initial-fixture", 140)
	meta, err := old.Session()
	if err != nil || full.SetSession(meta) != nil {
		t.Fatal("fixture metadata", err)
	}
	if err := aistudio.WriteManagedSessionState(t.Context(), m.StateFile, &full); err != nil {
		t.Fatal(err)
	}
	return full
}

func assertProjectionResponse(t *testing.T, result *RefreshResponse, full aistudio.StorageState) {
	t.Helper()
	if result == nil || !reflect.DeepEqual(result.Cookies, full.Cookies[:4]) {
		t.Fatal("response was not the exact selected subset")
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	ext, _, _ := full.AuthExtension()
	for _, forbidden := range []string{"OUTSIDE_SENTINEL", "OAUTH_SENTINEL", "KEY_SENTINEL", "ORIGIN_SENTINEL", "UNKNOWN_SENTINEL", "oauth", "wrapped_binding_key", "chrome_root", "origins", "fixture_unknown", ext.Source.ChromeRoot} {
		if bytes.Contains(data, []byte(forbidden)) {
			t.Fatal("response leaked complete native state")
		}
	}
}

func TestCookieProjectionReauthRetrievalRestartAndHistory(t *testing.T) {
	b, m, count := fixtureBroker(t, func(_ context.Context, s aistudio.StorageState, _ Mapping, _ bool) (aistudio.StorageState, error) {
		s.Cookies = projectionFullState(t, mForProjection(s), "renewed-fixture", 141).Cookies
		ext, _, _ := s.AuthExtension()
		ext.OAuth.RefreshToken = "OAUTH_SENTINEL_ROTATED"
		if err := s.SetAuthExtension(ext); err != nil {
			t.Fatal(err)
		}
		return s, nil
	})
	full := installProjectionFullState(t, m)
	bootstrap := fixtureRequest(m, "projection_bootstrap_2", 0)
	bootstrap.Reason = "bootstrap"
	first, err := b.Refresh(t.Context(), m.Consumer, bootstrap)
	if err != nil || count.Load() != 0 {
		t.Fatal("bootstrap did Google work", err)
	}
	assertProjectionResponse(t, first, full)
	ackResult(t, b, m, first)
	req := fixtureRequest(m, "projection_reauth_0001", 1)
	result, err := b.Refresh(t.Context(), m.Consumer, req)
	if err != nil || count.Load() != 1 || result.CookieRevision != 2 || result.MaterialVersion != 2 {
		t.Fatal("complete renewal rejected or version changed incorrectly", err)
	}
	committed, err := aistudio.LoadStorageState(m.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	want := full
	want.Cookies = projectionFullState(t, m, "renewed-fixture", 141).Cookies
	ext, _, _ := want.AuthExtension()
	ext.OAuth.RefreshToken = "OAUTH_SENTINEL_ROTATED"
	if err := want.SetAuthExtension(ext); err != nil {
		t.Fatal(err)
	}
	assertProjectionFullState(t, want, committed)
	assertProjectionResponse(t, result, committed)
	restarted, err := NewBroker(context.Background(), []Mapping{m}, b.renew)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(m.StateFile)
	for _, lookup := range []bool{false, true} {
		replay := req
		replay.LookupOnly = lookup
		again, err := restarted.Refresh(t.Context(), m.Consumer, replay)
		if err != nil || !reflect.DeepEqual(again, result) || count.Load() != 1 {
			t.Fatal("same-ID retrieval changed subset or authenticated again", err)
		}
	}
	after, _ := os.ReadFile(m.StateFile)
	if !bytes.Equal(before, after) {
		t.Fatal("projection retrieval wrote authority")
	}
	ackResult(t, restarted, m, result)
	// A later NOT_STARTED disposition moves the still-current result into history.
	late := fixtureRequest(m, "projection_not_started", 2)
	late.BaseMaterial = 2
	late.Deadline = time.Now().Add(-time.Minute).Unix()
	if _, err := restarted.Refresh(t.Context(), m.Consumer, late); safeCode(err) != "NOT_STARTED" {
		t.Fatal(err)
	}
	again, err := restarted.Refresh(t.Context(), m.Consumer, req)
	if err != nil || !reflect.DeepEqual(again, result) || count.Load() != 1 {
		t.Fatal("current history did not use the common projection outlet", err)
	}
	collision := req
	collision.BaseMaterial++
	if _, err := restarted.Refresh(t.Context(), m.Consumer, collision); safeCode(err) != "OPERATION_CONFLICT" {
		t.Fatal("historical parameters changed", err)
	}
	next := fixtureRequest(m, "projection_reauth_0002", 2)
	next.BaseMaterial = 2
	if _, err := restarted.Refresh(t.Context(), m.Consumer, next); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Refresh(t.Context(), m.Consumer, req); safeCode(err) != "RESULT_SUPERSEDED" || count.Load() != 2 {
		t.Fatal("superseded historical result was reused/re-authenticated", err)
	}
}

// Read only synthetic identity fields already present in the injected native result.
func mForProjection(s aistudio.StorageState) Mapping {
	ext, _, _ := s.AuthExtension()
	return Mapping{GaiaID: ext.OAuth.GaiaID, Email: ext.Source.Email}
}

func TestCookieProjectionClientLostResponseAndOrdinaryCommitAck(t *testing.T) {
	b, m, count := fixtureBroker(t, func(_ context.Context, s aistudio.StorageState, _ Mapping, _ bool) (aistudio.StorageState, error) {
		s.Cookies = projectionFullState(t, mForProjection(s), "renewed-fixture", 140).Cookies
		return s, nil
	})
	installProjectionFullState(t, m)
	c, _ := fixtureHTTP(t, b, m)
	state, path := fixtureRemoteState(t, m)
	if err := c.PrepareRecovery(t.Context(), &state, path, "bootstrap", nil); err != nil {
		t.Fatal(err)
	}
	if err := commitNativeProjection(t.Context(), c, &state, path); err != nil || count.Load() != 0 {
		t.Fatal("bootstrap/commit/ordinary ack", err)
	}
	original := c.http.Transport
	lose := true
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		response, err := original.RoundTrip(r)
		if err == nil && lose && strings.HasSuffix(r.URL.Path, "/refresh") {
			lose = false
			response.Body.Close()
			return nil, errors.New("synthetic lost response after server completion")
		}
		return response, err
	})
	ctx := aistudio.WithRemoteAuthBudget(t.Context())
	budget := func(id string) error { return aistudio.TakeRemoteAuthBudget(ctx, id) }
	if err := c.PrepareRecovery(ctx, &state, path, "reauth", budget); safeCode(err) != "HELPER_UNAVAILABLE" {
		t.Fatal(err)
	}
	pending, err := aistudio.LoadStorageState(path)
	if err != nil {
		t.Fatal(err)
	}
	pm, _ := pending.Session()
	id := pm.Remote.Pending.ID
	if count.Load() != 1 || pm.Remote.Ack != nil || pending.Cookies[0].Value != "initial-fixture" {
		t.Fatal("lost response prematurely committed/acked")
	}
	restarted, err := NewBroker(context.Background(), []Mapping{m}, b.renew)
	if err != nil {
		t.Fatal(err)
	}
	// A new mTLS endpoint uses the rebuilt Broker and the same persisted operation.
	recovery, _ := fixtureHTTP(t, restarted, m)
	if err := recovery.PrepareRecovery(ctx, &pending, path, "reauth", budget); err != nil {
		t.Fatal(err)
	}
	meta, _ := pending.Session()
	if meta.Remote.Ack.ID != id || count.Load() != 1 {
		t.Fatal("recovery changed operation or repeated authentication")
	}
	if err := commitNativeProjection(ctx, recovery, &pending, path); err != nil {
		t.Fatal(err)
	}
	local, _ := aistudio.LoadStorageState(path)
	windows, _ := aistudio.LoadStorageState(m.StateFile)
	lm, _ := local.Session()
	wm, _ := windows.Session()
	if lm.Remote.Pending != nil || lm.Remote.Ack != nil || lm.Remote.Epoch != wm.Authority.Epoch || lm.Remote.CookieRevision != wm.Authority.CookieRevision || lm.Remote.MaterialVersion != wm.Authority.MaterialVersion || wm.Authority.Operation.Stage != "acked" || wm.Authority.Operation.AckMode != "" || len(windows.Cookies) != 144 || len(local.Cookies) != 4 || !reflect.DeepEqual(local.Cookies, windows.Cookies[:4]) {
		t.Fatal("full authority/subset or ordinary ACK contract broken")
	}
	if _, exists, err := local.AuthExtension(); err != nil || exists || len(local.Origins) != 0 {
		t.Fatal("consumer received native material", err)
	}
}

func TestCookieProjectionRegisterFailureNeverWritesAuthority(t *testing.T) {
	for _, mode := range []string{"source-missing", "identity", "invalid-subset", "missing-signer"} {
		t.Run(mode, func(t *testing.T) {
			_, m, _ := fixtureBroker(t, nil)
			m.StateFile = filepath.Join(t.TempDir(), "unregistered", "storage-state.json")
			full := projectionFullState(t, m, "initial-fixture", 2)
			ext, _, _ := full.AuthExtension()
			code := "COOKIE_POLICY_FAILURE"
			switch mode {
			case "source-missing":
				ext.Source.ChromeRoot = ""
				code = "SOURCE_REBIND_REQUIRED"
			case "identity":
				ext.OAuth.GaiaID = "different-fixture-gaia"
				code = "IDENTITY_MISMATCH"
			case "invalid-subset":
				full.Cookies[0].PartitionKey = "synthetic"
			case "missing-signer":
				full.Cookies = full.Cookies[1:]
			}
			if full.SetAuthExtension(ext) != nil || aistudio.WriteStorageState(m.StateFile, full) != nil {
				t.Fatal("fixture setup")
			}
			before, _ := os.ReadFile(m.StateFile)
			if err := registerAuthority(t.Context(), m, "fixture-only-sid"); safeCode(err) != code {
				t.Fatal("registration failure changed", err)
			}
			after, _ := os.ReadFile(m.StateFile)
			if !bytes.Equal(before, after) {
				t.Fatal("failed register wrote authority")
			}
		})
	}
}

func TestCookieProjectionReauthFailureKeepsCompleteMaterial(t *testing.T) {
	for _, mode := range []string{"source", "gaia", "invalid-subset", "invalid-complete-state", "cancel", "save", "cas"} {
		t.Run(mode, func(t *testing.T) {
			life, cancel := context.WithCancel(context.Background())
			defer cancel()
			b, m, count := fixtureBroker(t, func(ctx context.Context, s aistudio.StorageState, _ Mapping, _ bool) (aistudio.StorageState, error) {
				s.Cookies = projectionFullState(t, mForProjection(s), "partial-must-not-commit", 2).Cookies
				ext, _, _ := s.AuthExtension()
				ext.OAuth.RefreshToken = "partial-material-must-not-commit"
				switch mode {
				case "source":
					ext.Source.Profile = "Profile 9"
				case "gaia":
					ext.OAuth.GaiaID = "different-fixture-gaia"
				case "invalid-subset":
					s.Cookies[0].PartitionKey = "synthetic"
				case "invalid-complete-state":
					s.Cookies[4].Path = "invalid-native-path"
				case "cancel":
					cancel()
					return s, ctx.Err()
				}
				if err := s.SetAuthExtension(ext); err != nil {
					t.Fatal(err)
				}
				return s, nil
			})
			b.lifecycle = life
			original := installProjectionFullState(t, m)
			if mode == "save" || mode == "cas" {
				writes := 0
				b.write = func(ctx context.Context, path string, s *aistudio.StorageState) error {
					writes++
					if writes == 2 {
						if mode == "save" {
							return errors.New("synthetic persistence failure")
						}
						current, err := aistudio.LoadStorageState(path)
						if err != nil || aistudio.WriteManagedSessionState(ctx, path, &current) != nil {
							t.Fatal("synthetic concurrent native write", err)
						}
					}
					return aistudio.WriteManagedSessionState(ctx, path, s)
				}
			}
			req := fixtureRequest(m, "projection_failed_0001", 0)
			for range 2 {
				if _, err := b.Refresh(t.Context(), m.Consumer, req); safeCode(err) != "INDETERMINATE" {
					t.Fatal("failure/unknown semantics changed", err)
				}
			}
			got, err := aistudio.LoadStorageState(m.StateFile)
			if err != nil {
				t.Fatal(err)
			}
			assertProjectionFullState(t, original, got)
			meta, _ := got.Session()
			if count.Load() != 1 || meta.Authority.CookieRevision != 0 || meta.Authority.MaterialVersion != 1 || (meta.Authority.Operation.Stage != "pending" && meta.Authority.Operation.Stage != "indeterminate") {
				t.Fatal("failed operation changed material/version or was repeated")
			}
		})
	}
}

func TestCookieProjectionExistingUnsettledOperationsStayUnsettled(t *testing.T) {
	for _, stage := range []string{"pending", "indeterminate", "failed"} {
		t.Run(stage, func(t *testing.T) {
			b, m, count := fixtureBroker(t, nil)
			full := installProjectionFullState(t, m)
			req := fixtureRequest(m, "projection_old_unknown", 0)
			meta, _ := full.Session()
			o := &aistudio.SessionOperation{ID: req.OperationID, Reason: req.Reason, Epoch: req.Epoch, BaseCookie: req.BaseCookie, BaseMaterial: req.BaseMaterial, Deadline: req.Deadline, Stage: stage}
			want := "INDETERMINATE"
			if stage == "failed" {
				o.Failure = "MATERIAL_REJECTED"
				want = o.Failure
			}
			meta.Authority.Operation = o
			if full.SetSession(meta) != nil || aistudio.WriteManagedSessionState(t.Context(), m.StateFile, &full) != nil {
				t.Fatal("unsettled fixture save")
			}
			before, _ := os.ReadFile(m.StateFile)
			restarted, err := NewBroker(context.Background(), []Mapping{m}, b.renew)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := restarted.Refresh(t.Context(), m.Consumer, req); safeCode(err) != want || count.Load() != 0 {
				t.Fatal("projection settled old unknown/failed operation", err)
			}
			after, _ := os.ReadFile(m.StateFile)
			if !bytes.Equal(before, after) {
				t.Fatal("retrieval rewrote unsettled state")
			}
		})
	}
}

func TestCookieProjectionAuthorityRebindKeepsFullStateAndGuards(t *testing.T) {
	for _, mode := range []string{"success", "invalid-subset", "changed-gaia", "wrong-sid", "wrong-epoch", "enabled", "pending-unacknowledged", "pending-explicit"} {
		t.Run(mode, func(t *testing.T) {
			_, m, _ := fixtureBroker(t, nil)
			if mode != "enabled" {
				if err := SetAuthorityPolicy(t.Context(), m, 1, false, false); err != nil {
					t.Fatal(err)
				}
			}
			original := installProjectionFullState(t, m)
			meta, _ := original.Session()
			epoch := meta.Authority.Epoch
			if strings.HasPrefix(mode, "pending-") {
				meta.Authority.Operation = &aistudio.SessionOperation{ID: "prior_projection_unknown", Reason: "reauth", Epoch: epoch, BaseMaterial: 1, Deadline: time.Now().Add(time.Minute).Unix(), Stage: "indeterminate", Failure: "INDETERMINATE"}
				if original.SetSession(meta) != nil || aistudio.WriteManagedSessionState(t.Context(), m.StateFile, &original) != nil {
					t.Fatal("pending fixture save")
				}
			}
			source := aistudio.AuthSource{Browser: "chrome", ChromeRoot: filepath.Join(t.TempDir(), "explicit-synthetic-root"), Profile: "Profile 2", GaiaID: m.GaiaID, Email: m.Email}
			calls := 0
			imported := projectionFullState(t, m, "rebound-fixture", 140)
			ext, _, _ := imported.AuthExtension()
			ext.Source = source
			if mode == "changed-gaia" {
				ext.OAuth.GaiaID = "different-fixture-gaia"
			}
			if imported.SetAuthExtension(ext) != nil {
				t.Fatal("fixture extension")
			}
			if mode == "invalid-subset" {
				imported.Cookies[0].PartitionKey = "synthetic"
			}
			importer := func(_ context.Context, o chromeauth.ImportOptions) ([]chromeauth.ImportResult, error) {
				calls++
				if o.ExactSource == nil || !chromeauth.SameSource(source, *o.ExactSource) || o.ChromeRoot != source.ChromeRoot {
					t.Fatal("rebind selected another source")
				}
				return []chromeauth.ImportResult{{State: imported}}, nil
			}
			sid := ""
			if mode == "wrong-sid" {
				sid = "wrong-synthetic-sid"
			}
			if mode == "wrong-epoch" {
				epoch++
			}
			before, _ := os.ReadFile(m.StateFile)
			err := rebindAuthoritySource(t.Context(), m, epoch, source, mode == "pending-explicit", sid, importer)
			got, loadErr := aistudio.LoadStorageState(m.StateFile)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			gm, _ := got.Session()
			switch mode {
			case "success", "pending-explicit":
				want := original
				want.Cookies = imported.Cookies
				if want.SetAuthExtension(ext) != nil {
					t.Fatal("expected extension")
				}
				assertProjectionFullState(t, want, got)
				if err != nil || calls != 1 || gm.Authority.Enabled || gm.Authority.Epoch != epoch+1 || gm.Authority.CookieRevision != 1 || gm.Authority.MaterialVersion != 2 || gm.Authority.Operation.Stage != "acked" {
					t.Fatal("rebind changed full authority contract", err)
				}
				if mode == "pending-explicit" && (len(gm.Authority.History) != 1 || gm.Authority.History[0].Stage != "indeterminate") {
					t.Fatal("explicit rebind erased prior unknown fact")
				}
				if _, err := projectCookies(got.Cookies, m.CookieDomains); err != nil {
					t.Fatal(err)
				}
			case "invalid-subset", "changed-gaia":
				assertProjectionFullState(t, original, got)
				if safeCode(err) != "INDETERMINATE" || calls != 1 || gm.Authority.Epoch != epoch || gm.Authority.Operation.Stage != "indeterminate" {
					t.Fatal("failed rebind discarded prior material/unknown fact", err)
				}
			default:
				after, _ := os.ReadFile(m.StateFile)
				if err == nil || calls != 0 || !bytes.Equal(before, after) {
					t.Fatal("rebind guard allowed native import or state write", err)
				}
			}
		})
	}
}

func TestCookieProjectionReturnLocalAndOrdinaryRoundTripRemainComplete(t *testing.T) {
	_, m, _ := fixtureBroker(t, nil)
	if err := SetAuthorityPolicy(t.Context(), m, 1, false, false); err != nil {
		t.Fatal(err)
	}
	full := installProjectionFullState(t, m)
	if err := returnLocalAuthority(t.Context(), m, 2, true, ""); err != nil {
		t.Fatal(err)
	}
	local, err := aistudio.LoadStorageState(m.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	assertProjectionFullState(t, full, local)
	if err := aistudio.WriteStorageState(m.StateFile, local); err != nil {
		t.Fatal(err)
	}
	got, err := aistudio.LoadStorageState(m.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	assertProjectionFullState(t, full, got)
}

func TestCookieProjectionClientAndKeepCurrentStillRejectUntrustedExtraDomain(t *testing.T) {
	for _, mode := range []string{"prepare", "keep-current-result", "keep-current-local"} {
		t.Run(mode, func(t *testing.T) {
			b, m, count := fixtureBroker(t, nil)
			c, _ := fixtureHTTP(t, b, m)
			state, path := fixtureRemoteState(t, m)
			original := c.http.Transport
			lose := mode != "prepare"
			requests := 0
			c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				requests++
				response, err := original.RoundTrip(r)
				if err != nil {
					return response, err
				}
				defer response.Body.Close()
				if lose {
					lose = false
					return nil, errors.New("synthetic lost response")
				}
				var result Result
				if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
					t.Fatal(err)
				}
				if result.Session == nil {
					t.Fatal("unexpected acknowledgement or missing result")
				}
				result.Session.Cookies = append(result.Session.Cookies, aistudio.StateCookie{Name: "UNTRUSTED", Value: "OUTSIDE_SENTINEL", Domain: ".youtube.com", Path: "/", Expires: -1})
				data, err := json.Marshal(result)
				if err != nil {
					t.Fatal(err)
				}
				response.Body = io.NopCloser(bytes.NewReader(data))
				response.ContentLength = int64(len(data))
				return response, nil
			})
			err := c.PrepareRecovery(t.Context(), &state, path, "reauth", func(string) error { return nil })
			if mode == "prepare" {
				if safeCode(err) != "COOKIE_POLICY_FAILURE" || len(state.Cookies) != 0 {
					t.Fatal("consumer filtered malicious RPC instead of rejecting", err)
				}
			} else {
				if safeCode(err) != "HELPER_UNAVAILABLE" {
					t.Fatal(err)
				}
				state, err = aistudio.LoadStorageState(path)
				if err != nil {
					t.Fatal(err)
				}
				state.Cookies = fixtureCookies("independently-updated-local")
				if mode == "keep-current-local" {
					state.Cookies = append(state.Cookies, aistudio.StateCookie{Name: "LOCAL_EXTRA", Domain: ".youtube.com", Path: "/", Expires: -1})
				}
				if err := aistudio.WriteStorageState(path, state); err != nil {
					t.Fatal(err)
				}
				state, _ = aistudio.LoadStorageState(path)
				before, _ := os.ReadFile(path)
				preserved := append([]aistudio.StateCookie(nil), state.Cookies...)
				priorRequests := requests
				err = c.ReconcileKeepingCurrent(t.Context(), &state, path)
				after, _ := os.ReadFile(path)
				if safeCode(err) != "COOKIE_POLICY_FAILURE" || !reflect.DeepEqual(state.Cookies, preserved) || !bytes.Equal(before, after) || (mode == "keep-current-local" && requests != priorRequests) {
					t.Fatal("keep-current widened strict local/result policy", err)
				}
			}
			persisted, _ := aistudio.LoadStorageState(path)
			pm, _ := persisted.Session()
			authority, _ := aistudio.LoadStorageState(m.StateFile)
			am, _ := authority.Session()
			if pm.Remote.Pending == nil || pm.Remote.Ack != nil || am.Authority.Operation.Stage != "ready" || count.Load() != 1 {
				t.Fatal("invalid result committed/acked/re-authenticated")
			}
		})
	}
}
