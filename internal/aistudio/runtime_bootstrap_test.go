package aistudio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

type bootstrapRoundTrip func(*http.Request) (*http.Response, error)

func (f bootstrapRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type bootstrapClosingTransport struct {
	bootstrapRoundTrip
	closed int
}

func (t *bootstrapClosingTransport) CloseIdleConnections() { t.closed++ }

func bootstrapResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}
func bootstrapRuntime(transport http.RoundTripper) *goWAARuntime {
	state := StorageState{}
	for _, name := range []string{"SAPISID", "__Secure-1PAPISID", "__Secure-3PAPISID"} {
		state.Cookies = append(state.Cookies, StateCookie{Name: name, Value: "synthetic-account-secret", Domain: ".google.com", Path: "/", Secure: true, Expires: -1})
	}
	return &goWAARuntime{pageURL: aiStudioOrigin + "/prompts/new_chat", userAgent: "synthetic-account-agent", storage: state,
		client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func TestBootstrapRestrictedPageIsNotAuthenticationFailure(t *testing.T) {
	runtime := bootstrapRuntime(bootstrapRoundTrip(func(r *http.Request) (*http.Response, error) {
		response := bootstrapResponse(r, 302, "")
		response.Header.Set("Location", "https://accounts.google.com/info/servicerestricted?secret=never-log")
		return response, nil
	}))
	page, err := runtime.loadPage(context.Background())
	if err != nil || page.access != WebAccessRestricted || page.apiKey != "" {
		t.Fatalf("restricted page must be an observation, not authentication: %+v %v", page, err)
	}
}

func TestBootstrapRestrictionURL(t *testing.T) {
	for _, tc := range []struct {
		target string
		want   bool
	}{
		{"https://accounts.google.com/info/servicerestricted", true},
		{"HTTPS://ACCOUNTS.GOOGLE.COM:443/info/servicerestricted?secret=synthetic#fragment", true},
		{"http://accounts.google.com/info/servicerestricted", false},
		{"https://accounts.google.com:444/info/servicerestricted", false},
		{"https://accounts.google.com:/info/servicerestricted", false},
		{"https://u:p@accounts.google.com/info/servicerestricted", false},
		{"https://accounts.google.com.evil.invalid/info/servicerestricted", false},
		{"https://accounts.google.com./info/servicerestricted", false},
		{"https://accounts.google.com/ServiceLogin", false},
		{"https://accounts.google.com/info/servicerestricted/", false},
		{"https://accounts.google.com/info/%73ervicerestricted", false},
		{"https://accounts.google.com/info%2Fservicerestricted", false},
		{"https://accounts.google.com/info/ServiceRestricted", false},
		{"/info/servicerestricted", false},
		{"https://accounts.google.com:bad/info/servicerestricted", false},
	} {
		t.Run(tc.target, func(t *testing.T) {
			u, err := url.Parse(tc.target)
			got := err == nil && isServiceRestrictedLocation(u)
			if got != tc.want {
				t.Fatalf("match=%v want=%v", got, tc.want)
			}
		})
	}
	if isServiceRestrictedLocation(nil) {
		t.Fatal("nil URL")
	}
}

func TestBootstrapNormalNavigationUnchanged(t *testing.T) {
	calls := 0
	runtime := bootstrapRuntime(bootstrapRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if !strings.Contains(r.Header.Get("Cookie"), "synthetic-account-secret") || r.Header.Get("Sec-Fetch-Mode") != "navigate" || r.Header.Get("User-Agent") != "synthetic-account-agent" {
			t.Fatal("normal navigation lost original account headers")
		}
		response := bootstrapResponse(r, 200, `{"WIu0Nc":"synthetic-public-key"}`)
		if calls == 1 {
			response.StatusCode = 302
			response.Header.Set("Location", "/prompts/redirected")
		}
		response.Header.Add("Set-Cookie", "navigation=accepted; Domain=.google.com; Path=/; Secure")
		return response, nil
	}))
	var check WebAccessCheck
	ctx := ContextWithWebAccessObserver(context.Background(), func(v WebAccessCheck) { check = v })
	key, err := runtime.bootstrapAPIKey(ctx, func(string) (*http.Client, error) {
		t.Fatal("normal account must not use public fallback")
		return nil, nil
	})
	if err != nil || key != "synthetic-public-key" || calls != 2 || check.Access != WebAccessNormal || runtime.bootstrapMode != BootstrapLegacy {
		t.Fatalf("normal result: key=%q mode=%q check=%+v err=%v", key, runtime.bootstrapMode, check, err)
	}
	if value, ok := runtime.cookies().CookieValue("navigation", aiStudioOrigin+"/", time.Now()); !ok || value != "accepted" {
		t.Fatal("navigation cookie update lost")
	}
}

func TestBootstrapRestrictedAnonymousAndAccountRPCBoundaries(t *testing.T) {
	pageCalls, loggingCalls, waaCalls, discoveryCalls := 0, 0, 0, 0
	runtime := bootstrapRuntime(bootstrapRoundTrip(func(r *http.Request) (*http.Response, error) {
		if !strings.Contains(r.Header.Get("Cookie"), "synthetic-account-secret") {
			t.Fatal("account cookie missing")
		}
		if r.Method == http.MethodGet {
			pageCalls++
			response := bootstrapResponse(r, 302, "")
			if pageCalls == 1 {
				response.Header.Set("Location", "/prompts/bridge")
			} else {
				response.Header.Set("Location", "https://ACCOUNTS.GOOGLE.COM:443/info/servicerestricted?secret=never-log")
			}
			return response, nil
		}
		if !strings.Contains(r.Header.Get("Authorization"), "SAPISIDHASH ") || r.Header.Get("User-Agent") != "synthetic-account-agent" {
			t.Fatal("RPC account signature/fingerprint missing")
		}
		if r.URL.String() == loggingContextURL {
			loggingCalls++
			if r.Header.Get("X-Goog-Api-Key") != "synthetic-public-key" {
				t.Fatal("wrong public key")
			}
			return bootstrapResponse(r, 200, `[1]`), nil
		}
		waaCalls++
		return bootstrapResponse(r, 403, `[7,"synthetic-sensitive-body"]`), nil
	}))
	runtime.options.Proxy = "http://fixture-proxy.invalid:9010"
	runtime.profile.PageURL = runtime.pageURL
	anonymousTransport := &bootstrapClosingTransport{bootstrapRoundTrip: func(r *http.Request) (*http.Response, error) {
		discoveryCalls++
		if r.URL.String() != aiStudioOrigin+"/" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" || strings.Contains(fmt.Sprint(r.Header), "synthetic-account") {
			t.Fatal("public discovery leaked account state")
		}
		response := bootstrapResponse(r, 200, `{"WIu0Nc":"synthetic-public-key"}`)
		response.Header.Set("Set-Cookie", "anonymous=must-not-merge; Domain=.google.com; Path=/")
		return response, nil
	}}
	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse(aiStudioOrigin)
	jar.SetCookies(u, []*http.Cookie{{Name: "accidental", Value: "synthetic-account-secret"}})
	client := &http.Client{Transport: anonymousTransport, Jar: jar}
	var check WebAccessCheck
	ctx := ContextWithWebAccessObserver(context.Background(), func(v WebAccessCheck) { check = v })
	key, err := runtime.bootstrapAPIKey(ctx, func(proxy string) (*http.Client, error) {
		if proxy != runtime.options.Proxy {
			t.Fatal("effective account proxy changed")
		}
		return client, nil
	})
	if err != nil || check.Access != WebAccessRestricted || runtime.bootstrapMode != BootstrapRestrictedFallback || discoveryCalls != 1 || pageCalls != 2 || anonymousTransport.closed != 1 {
		t.Fatalf("fallback: mode=%q check=%+v calls=%d/%d close=%d err=%v", runtime.bootstrapMode, check, discoveryCalls, pageCalls, anonymousTransport.closed, err)
	}
	if client.Jar != jar || runtime.profile.PageURL != runtime.pageURL {
		t.Fatal("factory client/profile mutated")
	}
	if _, exists := runtime.cookies().CookieValue("anonymous", aiStudioOrigin+"/", time.Now()); exists {
		t.Fatal("anonymous cookie entered account")
	}
	if _, err := runtime.loggingContextHeader(ctx, key); err != nil {
		t.Fatal(err)
	}
	_, err = runtime.createChallenge(ctx, nil)
	if err == nil || DefinitiveAuthenticationFailure(err) || strings.Contains(err.Error(), "synthetic-sensitive-body") || loggingCalls != 1 || waaCalls != 1 {
		t.Fatalf("WAA retained auth/rejection boundary: %v", err)
	}
}

func TestBootstrapNonRestrictionNeverFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name, location, body string
		status               int
		auth                 bool
	}{
		{"login", "https://accounts.google.com/ServiceLogin?token=synthetic-secret", "", 302, true},
		{"wrong-scheme", "http://accounts.google.com/info/servicerestricted", "", 302, true},
		{"wrong-port", "https://accounts.google.com:444/info/servicerestricted", "", 302, true},
		{"userinfo", "https://u:synthetic-secret@accounts.google.com/info/servicerestricted", "", 302, true},
		{"wrong-host", "https://accounts.google.com.evil.invalid/info/servicerestricted", "", 302, false},
		{"missing-location", "", "", 302, false},
		{"invalid-location", "https://accounts.google.com:bad/a?token=synthetic-secret", "", 302, false},
		{"loop", "/prompts/new_chat", "", 302, false},
		{"text", "", "servicerestricted synthetic-secret", 200, false},
		{"forbidden", "", "synthetic-secret", 403, false},
		{"server-error", "", "synthetic-secret", 500, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			runtime := bootstrapRuntime(bootstrapRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls > 5 {
					t.Fatal("unbounded navigation")
				}
				response := bootstrapResponse(r, tc.status, tc.body)
				if tc.location != "" {
					response.Header.Set("Location", tc.location)
				}
				return response, nil
			}))
			var check WebAccessCheck
			ctx := ContextWithWebAccessObserver(context.Background(), func(v WebAccessCheck) { check = v })
			_, err := runtime.bootstrapAPIKey(ctx, func(string) (*http.Client, error) { t.Fatal("unexpected public discovery"); return nil, nil })
			if err == nil || DefinitiveAuthenticationFailure(err) != tc.auth || check.Access != WebAccessUnknown || strings.Contains(err.Error(), "synthetic-secret") {
				t.Fatalf("non-restriction error=%v check=%+v", err, check)
			}
		})
	}
}

func TestBootstrapDiscoveryFailuresStopAndStayNonAuth(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		err    error
	}{
		{"timeout", 0, "", fmt.Errorf("synthetic-secret: %w", context.DeadlineExceeded)},
		{"transport", 0, "", errors.New("proxy user:synthetic-secret")},
		{"unauthorized", 401, `[16,"synthetic-secret"]`, nil},
		{"missing-key", 200, "synthetic-secret", nil},
		{"forbidden", 403, "synthetic-secret", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtime := bootstrapRuntime(bootstrapRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Method != "GET" {
					t.Fatal("discovery failure proceeded to WAA/RPC")
				}
				response := bootstrapResponse(r, 302, "")
				response.Header.Set("Location", "https://accounts.google.com/info/servicerestricted")
				return response, nil
			}))
			calls := 0
			transport := &bootstrapClosingTransport{bootstrapRoundTrip: func(r *http.Request) (*http.Response, error) {
				calls++
				if tc.err != nil {
					return nil, tc.err
				}
				return bootstrapResponse(r, tc.status, tc.body), nil
			}}
			var check WebAccessCheck
			ctx := ContextWithWebAccessObserver(context.Background(), func(v WebAccessCheck) { check = v })
			_, err := runtime.bootstrapAPIKey(ctx, func(string) (*http.Client, error) { return &http.Client{Transport: transport}, nil })
			if err == nil || DefinitiveAuthenticationFailure(err) || calls != 1 || transport.closed != 1 || check.Access != WebAccessRestricted || runtime.bootstrapMode != "" || strings.Contains(err.Error(), "synthetic-secret") {
				t.Fatalf("discovery boundary: %v %+v calls=%d close=%d", err, check, calls, transport.closed)
			}
			if tc.name == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("lost deadline identity")
			}
		})
	}
}

func TestBootstrapRPCErrorsStructuredAndRedacted(t *testing.T) {
	for _, method := range []string{"GetLoggingContext", "Waa/Create"} {
		for _, tc := range []struct {
			status int
			body   string
			auth   bool
			code   int64
		}{
			{401, `synthetic-secret`, true, 0},
			{403, `[16,"synthetic-secret"]`, true, 16},
			{403, `[7,"synthetic-secret"]`, false, 7},
			{400, `[3,"synthetic-secret"]`, false, 3},
			{429, `[8,"synthetic-secret"]`, false, 8},
			{500, `[13,"synthetic-secret"]`, false, 13},
		} {
			t.Run(fmt.Sprintf("%s/%d/%d", method, tc.status, tc.code), func(t *testing.T) {
				runtime := bootstrapRuntime(bootstrapRoundTrip(func(r *http.Request) (*http.Response, error) { return bootstrapResponse(r, tc.status, tc.body), nil }))
				var err error
				if method == "GetLoggingContext" {
					_, err = runtime.loggingContextHeader(context.Background(), "public")
				} else {
					_, err = runtime.createChallenge(context.Background(), nil)
				}
				var rpc *RPCError
				if !errors.As(err, &rpc) || rpc.Method != method || rpc.StatusCode != tc.status || rpc.Code != tc.code || rpc.Metadata != nil || DefinitiveAuthenticationFailure(err) != tc.auth || strings.Contains(err.Error(), "synthetic-secret") {
					t.Fatalf("classification/redaction: %+v %v", rpc, err)
				}
			})
		}
	}
	for _, method := range []string{"logging", "waa"} {
		runtime := bootstrapRuntime(bootstrapRoundTrip(func(*http.Request) (*http.Response, error) { return nil, errors.New("synthetic-secret") }))
		var err error
		if method == "logging" {
			_, err = runtime.loggingContextHeader(context.Background(), "public")
		} else {
			_, err = runtime.createChallenge(context.Background(), nil)
		}
		if err == nil || DefinitiveAuthenticationFailure(err) || strings.Contains(err.Error(), "synthetic-secret") {
			t.Fatalf("transport classified as auth: %v", err)
		}
	}
}

func TestBootstrapObservationLastKnownAndRecovery(t *testing.T) {
	start := time.Unix(1000, 0)
	var observation WebAccessObservation
	observation.Observe(WebAccessCheck{Access: WebAccessRestricted, CheckedAt: start})
	observation.Observe(WebAccessCheck{Access: WebAccessUnknown, CheckedAt: start.Add(time.Minute)})
	if observation.Access != WebAccessRestricted || observation.ObservedAt != start || observation.LastCheck.Access != WebAccessUnknown {
		t.Fatalf("failed check erased/freshened evidence: %+v", observation)
	}
	observation.Observe(WebAccessCheck{Access: WebAccessNormal, CheckedAt: start.Add(-time.Minute)})
	if observation.Access != WebAccessRestricted {
		t.Fatal("stale check replaced last known")
	}
	observation.Observe(WebAccessCheck{Access: WebAccessNormal, CheckedAt: start.Add(2 * time.Minute)})
	if observation.Access != WebAccessNormal {
		t.Fatal("recovered page did not change observation")
	}
	var restarted WebAccessObservation
	restarted.Observe(WebAccessCheck{CheckedAt: start})
	if restarted.Access != WebAccessUnknown {
		t.Fatal("fresh process invented normal")
	}
}

type bootstrapFakeRuntime struct{}

func (bootstrapFakeRuntime) Proof(context.Context, string, string) (string, error) {
	return "synthetic-proof", nil
}
func (bootstrapFakeRuntime) ProtocolHeaders(context.Context) (http.Header, error) {
	return http.Header{}, nil
}
func (bootstrapFakeRuntime) SendProtected(context.Context, string, http.Header, []byte) (*camoufoxnative.ProtectedResponse, error) {
	return nil, errors.New("not used")
}
func (bootstrapFakeRuntime) StorageCookies(context.Context) ([]byte, error) { return []byte(`[]`), nil }
func (bootstrapFakeRuntime) State() camoufoxnative.State                    { return camoufoxnative.State{} }
func (bootstrapFakeRuntime) Close() error                                   { return nil }

func TestBootstrapActualWorkerModeFollowsAttempt(t *testing.T) {
	for _, mode := range []BootstrapMode{BootstrapLegacy, BootstrapRestrictedFallback, ""} {
		var snapshot GenerationAttempt
		ctx := ContextWithGenerationAttemptObserver(context.Background(), func(a GenerationAttempt) { snapshot = a })
		ctx, attempt := beginPhysicalAttempt(ctx, GenerateRequest{ID: "fixture", Model: "fixture-model"}, RPCRequest{Method: "GenerateContent"})
		worker := &NativeWorker{accountID: "fixture", bootstrapMode: mode, runtime: bootstrapFakeRuntime{}}
		if _, err := worker.Prepare(ctx, ProtectedRequest{Body: []byte(`[null]`), ProofField: 1}); err != nil {
			t.Fatal(err)
		}
		if worker.BootstrapMode() != mode || snapshot.BootstrapMode != mode {
			t.Fatalf("mode=%q attempt=%q", mode, snapshot.BootstrapMode)
		}
		if mode != "" {
			replacement := &NativeWorker{bootstrapMode: BootstrapRestrictedFallback, runtime: bootstrapFakeRuntime{}}
			if _, err := replacement.Prepare(ctx, ProtectedRequest{Body: []byte(`[null]`), ProofField: 1}); err != nil {
				t.Fatal(err)
			}
			attempt.fail(errors.New("fixture failed"))
			if snapshot.BootstrapMode != mode {
				t.Fatal("replacement relabeled original attempt")
			}
		}
	}
}
