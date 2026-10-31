package app

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/authhelper"
)

// These mTLS certificates and cookies are generated synthetic fixtures. The
// service is the unchanged 8a2e49a Broker/Handler, not a mocked protocol server.
func queryFixtureTLS(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "query-fixture-ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, e := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	root, e = x509.ParseCertificate(der)
	if e != nil {
		t.Fatal(e)
	}
	pool := x509.NewCertPool()
	pool.AddCert(root)
	issue := func(name string, usage x509.ExtKeyUsage, n int64) tls.Certificate {
		k, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if e != nil {
			t.Fatal(e)
		}
		v := &x509.Certificate{SerialNumber: big.NewInt(n), DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
		b, e := x509.CreateCertificate(rand.Reader, v, root, &k.PublicKey, key)
		if e != nil {
			t.Fatal(e)
		}
		return tls.Certificate{Certificate: [][]byte{b, der}, PrivateKey: k}
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{issue("query-helper.fixture.invalid", x509.ExtKeyUsageServerAuth, 2)}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{issue("query-consumer.fixture.invalid", x509.ExtKeyUsageClientAuth, 3)}, RootCAs: pool, ServerName: "query-helper.fixture.invalid"}
}

type queryAppFixture struct {
	refresher *authRuntimeRefresher
	ctx       context.Context
	path      string
	pool      *aistudio.AccountPool
	account   *aistudio.Account
	client    *authhelper.Client
	broker    *authhelper.Broker
	mapping   authhelper.Mapping
	request   authhelper.RefreshRequest
	auth      atomic.Int32
	refresh   atomic.Int32
	ack       atomic.Int32
	lookups   atomic.Int32
	resets    int
	headers   int
}

func newQueryAppFixture(t *testing.T, outcome, reason string, advanced bool) *queryAppFixture {
	t.Helper()
	f := &queryAppFixture{}
	dir := t.TempDir()
	f.mapping = authhelper.Mapping{Handle: "query_fixture_handle_0001", Consumer: "query-consumer.fixture.invalid", GaiaID: "query_fixture_gaia", Email: "query@example.invalid", StateFile: filepath.Join(dir, "authority", "storage-state.json"), DirectAuth: true}
	m := f.mapping
	a := aistudio.StorageState{Cookies: fixtureRefreshCookies("authority-private-sentinel")}
	if e := a.SetAuthExtension(aistudio.AuthExtension{Source: aistudio.AuthSource{Browser: "chrome", ChromeRoot: filepath.Join(dir, "unmounted-chrome"), Profile: "Default", GaiaID: m.GaiaID, Email: m.Email}, OAuth: &aistudio.ChromeOAuthMaterial{GaiaID: m.GaiaID, RefreshToken: "fake-refresh-sentinel", WrappedBindingKey: []byte("fake-key-sentinel")}}); e != nil {
		t.Fatal(e)
	}
	if e := a.SetSession(&aistudio.SessionState{Version: 1, Authority: &aistudio.SessionAuthority{Handle: m.Handle, Consumer: m.Consumer, GaiaID: m.GaiaID, Email: m.Email, Epoch: 1, MaterialVersion: 1, Enabled: true}}); e != nil {
		t.Fatal(e)
	}
	if e := aistudio.WriteManagedSessionState(t.Context(), m.StateFile, &a); e != nil {
		t.Fatal(e)
	}
	b, e := authhelper.NewBroker(t.Context(), []authhelper.Mapping{m}, func(_ context.Context, s aistudio.StorageState, _ authhelper.Mapping, _ bool) (aistudio.StorageState, error) {
		f.auth.Add(1)
		if outcome == "indeterminate" {
			return s, errors.New("fake-transport-private-sentinel")
		}
		s.Cookies = fixtureRefreshCookies("remote-new-private-sentinel")
		return s, nil
	})
	if e != nil {
		t.Fatal(e)
	}
	f.broker = b
	handler := authhelper.Handler(b)
	st, ct := queryFixtureTLS(t)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/refresh" {
			data, e := io.ReadAll(r.Body)
			if e != nil {
				t.Error(e)
			}
			r.Body.Close()
			var q authhelper.RefreshRequest
			if json.Unmarshal(data, &q) != nil {
				t.Error("bad test request")
			}
			f.refresh.Add(1)
			if q.LookupOnly {
				f.lookups.Add(1)
			}
			r.Body = io.NopCloser(bytes.NewReader(data))
		}
		if r.URL.Path == "/v1/ack" {
			f.ack.Add(1)
		}
		handler.ServeHTTP(w, r)
	}))
	server.TLS = st
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)
	f.client, e = authhelper.NewClient(server.URL, m.Consumer, ct, nil)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(f.client.Close)
	s := aistudio.StorageState{Cookies: fixtureRefreshCookies("local-private-sentinel")}
	meta := &aistudio.SessionState{Version: 1, Remote: &aistudio.RemoteSession{Handle: m.Handle, Consumer: m.Consumer, GaiaID: m.GaiaID, Email: m.Email, Epoch: 1, MaterialVersion: 1, EnrollmentRequired: reason == "bootstrap"}}
	if e = s.SetSession(meta); e != nil {
		t.Fatal(e)
	}
	f.path = filepath.Join(dir, "consumer", "storage-state.json")
	if e = aistudio.WriteManagedSessionState(t.Context(), f.path, &s); e != nil {
		t.Fatal(e)
	}
	if outcome != "new" {
		meta, _ = s.Session()
		meta.Remote.Pending = &aistudio.SessionOperation{ID: "query_app_operation_0001", Reason: reason, Epoch: 1, BaseMaterial: 1, StateRevision: meta.Revision + 1, Deadline: time.Now().Add(time.Minute).Unix()}
		meta.Remote.RecoveryRequired = true
		if s.SetSession(meta) != nil || aistudio.WriteManagedSessionState(t.Context(), f.path, &s) != nil {
			t.Fatal("pending fixture")
		}
		f.request = authhelper.RefreshRequest{Version: 1, Handle: m.Handle, Epoch: 1, OperationID: meta.Remote.Pending.ID, BaseMaterial: 1, Reason: reason, Deadline: meta.Remote.Pending.Deadline}
		if outcome == "ready" || outcome == "indeterminate" {
			_, e = b.Refresh(t.Context(), m.Consumer, f.request)
			if outcome == "ready" && e != nil || outcome == "indeterminate" && !aistudio.IsSessionFailureCode(e, "INDETERMINATE") {
				t.Fatal(e)
			}
		}
	}
	model := aistudio.Model{ID: "query-fixture-model", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}, Channels: []string{"playground"}}
	cfg := aistudio.DefaultAccountConfig(m.Email)
	cfg.Enabled = true
	f.account = &aistudio.Account{ID: m.Email, Directory: filepath.Dir(f.path), StoragePath: f.path, RuntimePath: filepath.Join(dir, "runtime.json"), StorageState: s, Config: cfg, State: aistudio.AccountReady, Models: []aistudio.Model{model}}
	f.pool = aistudio.NewAccountPool([]*aistudio.Account{f.account}, 2)
	f.pool.SetUpstreamChannels([]aistudio.Channel{aistudio.ChannelPlayground})
	lease, e := f.pool.AcquireAccount(t.Context(), m.Email)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { lease.Release() })
	f.ctx = aistudio.WithRemoteAuthBudget(aistudio.ContextWithAccountLease(t.Context(), lease))
	f.refresher = &authRuntimeRefresher{pool: f.pool, remote: f.client, requests: newRequestRegistry(t.Context()), reset: func(string) error { f.resets++; return nil }, prepareHeaders: func(string) (func(bool), error) { f.headers++; return func(bool) {}, nil }}
	if advanced {
		if e = lease.ReplaceCookies(fixtureRefreshCookies("independent-private-sentinel")); e != nil {
			t.Fatal(e)
		}
	}
	return f
}

func TestRemoteQueryActualAppHelperOutcomes(t *testing.T) {
	for _, outcome := range []string{"not-started", "indeterminate", "ready", "new"} {
		t.Run(outcome, func(t *testing.T) {
			f := newQueryAppFixture(t, outcome, "reauth", outcome != "new")
			before, _ := aistudio.LoadStorageState(f.path)
			bm, _ := before.Session()
			sends := 0
			_, err := f.refresher.do(f.ctx, "GenerateContent", func() (*aistudio.RPCResponse, error) {
				sends++
				return nil, &aistudio.RPCError{StatusCode: 401, Code: 16}
			})
			disk, e := aistudio.LoadStorageState(f.path)
			if e != nil {
				t.Fatal(e)
			}
			m, _ := disk.Session()
			if outcome == "new" {
				if f.auth.Load() != 1 || f.resets != 1 || f.headers != 1 || f.lookups.Load() != 0 || sends != 2 {
					t.Fatal("new intent changed recovery/send count", err)
				}
				return
			}
			want := map[string]string{"not-started": "NOT_STARTED", "indeterminate": "INDETERMINATE", "ready": "VERSION_CONFLICT"}[outcome]
			if !aistudio.IsSessionFailureCode(err, want) || sends != 1 || f.resets != 0 || f.headers != 0 || f.ack.Load() != 0 || f.refresh.Load() != 1 || f.lookups.Load() != 1 || !reflect.DeepEqual(before.Cookies, disk.Cookies) || m.Remote.CookieRevision != bm.Remote.CookieRevision {
				t.Fatal("query reached success/replay/commit", err)
			}
			if outcome == "not-started" {
				if m.Remote.Pending != nil || !m.Remote.RecoveryRequired || len(m.Remote.RetiredOperations) != 1 {
					t.Fatal("retirement incomplete")
				}
			} else if m.Remote.Pending == nil {
				t.Fatal("unknown/ready conflict was discarded")
			}
			lease, _ := aistudio.AccountLeaseFromContext(f.ctx)
			latest, e := lease.ReloadStorageState()
			if e != nil {
				t.Fatal(e)
			}
			lm, _ := latest.Session()
			if !reflect.DeepEqual(m, lm) {
				t.Fatal("runtime did not observe metadata-only disposition")
			}
			for _, entry := range f.refresher.requests.logs {
				if strings.Contains(entry.Message, "private-sentinel") {
					t.Fatal("secret in diagnosis")
				}
			}
		})
	}
}

func TestRemoteQueryWorkerAndRPCShareTrackedBudget(t *testing.T) {
	f := newQueryAppFixture(t, "new", "reauth", false)
	// Use the actual tracked lifecycle budget root, not a budget created by this test.
	tracked := &trackedService{lifecycle: t.Context(), pool: f.pool, requests: f.refresher.requests, timeout: time.Minute}
	tracked.dataContext = t.Context()
	tracked.state.Store(serviceRunning)
	lease, _ := aistudio.AccountLeaseFromContext(f.ctx)
	ctx, cancel, e := tracked.dataRequestContext(aistudio.ContextWithAccountLease(t.Context(), lease))
	if e != nil {
		t.Fatal(e)
	}
	defer cancel()
	if e = f.refresher.Recover(ctx, &aistudio.RPCError{StatusCode: 401, Code: 16}); e != nil {
		t.Fatal(e)
	}
	sends := 0
	_, e = f.refresher.do(ctx, "GenerateContent", func() (*aistudio.RPCResponse, error) {
		sends++
		return nil, &aistudio.RPCError{StatusCode: 401, Code: 16}
	})
	if !aistudio.IsSessionFailureCode(e, "RECOVERY_BUDGET_EXHAUSTED") || f.auth.Load() != 1 || sends != 1 {
		t.Fatal("RPC escaped tracked logical budget", e)
	}
}

func TestRemoteQueryJoinedFailureStopsPhysicalReplayAndBidi(t *testing.T) {
	for _, code := range []string{"NOT_STARTED", "INDETERMINATE"} {
		t.Run(code, func(t *testing.T) {
			f := newQueryAppFixture(t, map[string]string{"NOT_STARTED": "not-started", "INDETERMINATE": "indeterminate"}[code], "reauth", true)
			cause := &aistudio.RPCError{StatusCode: 401, Code: 16}
			err := f.refresher.Recover(f.ctx, cause)
			mixed := errors.Join(err, cause, &url.Error{Op: "POST", URL: "https://fixture.invalid", Err: context.DeadlineExceeded}, &accountWorkerInitError{err: errors.New("worker fixture"), authHandled: true})
			if !aistudio.IsSessionFailureCode(mixed, code) || retryableGenerateAccountError(f.ctx, mixed) || proxyNetworkError(f.ctx, mixed) || aistudio.DefinitiveAuthenticationFailure(mixed) {
				t.Fatal("classification lost safety marker", mixed)
			}
		})
	}
}

// Test-only injection of the HTTP boundary, retaining real lease acquisition,
// request preparation, response handling and Set-Cookie persistence.
type queryHTTPFunc func(*http.Request) (*http.Response, error)

func (f queryHTTPFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func queryInjectHTTP(t *aistudio.MakerSuiteHTTPTransport, c *http.Client) {
	v := reflect.ValueOf(t).Elem().FieldByName("clients")
	reflect.NewAt(v.Type(), unsafe.Pointer(v.UnsafeAddr())).Elem().Set(reflect.ValueOf(map[string]*http.Client{"": c}))
}

func TestRemoteQueryRealOrdinaryCookieWriters(t *testing.T) {
	for _, writer := range []string{"http-set-cookie", "worker-replace"} {
		for _, outcome := range []string{"not-started", "indeterminate", "ready"} {
			t.Run(writer+"/"+outcome, func(t *testing.T) {
				f := newQueryAppFixture(t, outcome, "reauth", false)
				before, _ := aistudio.LoadStorageState(f.path)
				bm, _ := before.Session()
				if writer == "http-set-cookie" {
					transport, e := aistudio.NewMakerSuiteHTTPTransport(aistudio.HTTPTransportOptions{Pool: f.pool, Headers: aistudio.ProtocolHeaderProviderFunc(func(context.Context, string) (http.Header, error) {
						return http.Header{"User-Agent": {"fixture"}, "X-Goog-Api-Key": {"fixture"}, "X-Goog-Authuser": {"0"}, "X-User-Agent": {"fixture"}}, nil
					})})
					if e != nil {
						t.Fatal(e)
					}
					queryInjectHTTP(transport, &http.Client{Transport: queryHTTPFunc(func(r *http.Request) (*http.Response, error) {
						return &http.Response{StatusCode: 200, Header: http.Header{"Set-Cookie": []string{"SAPISID=writer-private-sentinel; Domain=.google.com; Path=/; Secure; SameSite=None"}}, Body: io.NopCloser(strings.NewReader(`[]`))}, nil
					})})
					response, e := transport.Do(f.ctx, aistudio.RPCRequest{AccountID: f.account.ID, Method: "SyntheticCookieWriter", URL: "https://aistudio.google.com/synthetic", Body: []byte(`[]`)})
					if e != nil {
						t.Fatal(e)
					}
					response.Body.Close()
				} else {
					plain, e := aistudio.NewMakerSuiteHTTPTransport(aistudio.HTTPTransportOptions{Pool: f.pool, Headers: aistudio.ProtocolHeaderProviderFunc(func(context.Context, string) (http.Header, error) {
						return http.Header{"User-Agent": {"fixture"}, "X-Goog-Api-Key": {"fixture"}, "X-Goog-Authuser": {"0"}, "X-User-Agent": {"fixture"}}, nil
					})})
					if e != nil {
						t.Fatal(e)
					}
					fake := &offlineProtectedPreparer{state: aistudio.StorageState{Cookies: fixtureRefreshCookies("writer-private-sentinel")}}
					worker, e := aistudio.NewWorkerProtectedTransport(aistudio.WorkerProtectedTransportOptions{Transport: plain, SetupTimeout: time.Second, Workers: aistudio.ProtectedPreparerProviderFunc(func(context.Context, string, string) (aistudio.ProtectedPreparer, error) { return fake, nil })})
					if e != nil {
						t.Fatal(e)
					}
					selection, _ := aistudio.ParseModelSelection("playground/query-fixture-model")
					request := aistudio.GenerateRequest{ID: "query-writer", Model: "query-fixture-model", AccountID: f.account.ID, Selection: selection, Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "invented"}}}}}
					body, e := aistudio.EncodeGenerateContentRequest(request, aistudio.GenerationDefaults{MaxOutputTokens: 256}, aistudio.RequestContext{})
					if e != nil {
						t.Fatal(e)
					}
					response, e := worker.DoProtected(f.ctx, request, aistudio.RPCRequest{Method: "GenerateContent", URL: "https://aistudio.google.com/synthetic", Header: http.Header{}, Body: body, Streaming: true})
					if e != nil {
						t.Fatal(e)
					}
					response.Body.Close()
				}
				advanced, _ := aistudio.LoadStorageState(f.path)
				am, _ := advanced.Session()
				if am.Revision != bm.Revision+1 || am.Remote.Pending.StateRevision != bm.Remote.Pending.StateRevision {
					t.Fatal("real writer did not advance local revision only")
				}
				err := f.refresher.Refresh(f.ctx)
				want := map[string]string{"not-started": "NOT_STARTED", "indeterminate": "INDETERMINATE", "ready": "VERSION_CONFLICT"}[outcome]
				if !aistudio.IsSessionFailureCode(err, want) || f.refresh.Load() != 1 || f.lookups.Load() != 1 || f.resets != 0 || f.ack.Load() != 0 {
					t.Fatal("writer recovery contract", err)
				}
				after, _ := aistudio.LoadStorageState(f.path)
				if !reflect.DeepEqual(advanced.Cookies, after.Cookies) {
					t.Fatal("writer cookies overwritten")
				}
			})
		}
	}
}
