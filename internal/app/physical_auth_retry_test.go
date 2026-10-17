package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// A fake preparer implements the existing seam; no WAA/browser/network starts.
type offlineProtectedPreparer struct {
	state aistudio.StorageState
	sends int
}

func (p *offlineProtectedPreparer) Prepare(_ context.Context, r aistudio.ProtectedRequest) (aistudio.PreparedProtectedRequest, error) {
	return aistudio.PreparedProtectedRequest{Headers: r.Headers.Clone(), Body: append([]byte(nil), r.Body...)}, nil
}
func (p *offlineProtectedPreparer) BrowserStorageState(context.Context) (aistudio.StorageState, error) {
	return p.state, nil
}
func (p *offlineProtectedPreparer) SendProtected(_ context.Context, _ aistudio.ProtectedRequest) (*aistudio.RPCResponse, error) {
	p.sends++
	status, body := 200, `[]`
	if p.sends == 1 {
		status = 401
		body = `[16,"fixture authentication expired"]`
	}
	return &aistudio.RPCResponse{StatusCode: status, Header: http.Header{"Content-Type": []string{aistudio.JSONProtobufContentType}}, Body: io.NopCloser(bytes.NewBufferString(body))}, nil
}

func TestAuthReplayKeepsStrictChannelAndPhysicalAttemptIDs(t *testing.T) {
	for _, channel := range []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild} {
		t.Run(string(channel), func(t *testing.T) {
			directory := t.TempDir()
			storage := filepath.Join(directory, "fixture-storage.json")
			// Only invented local fixture values; never load a user's auth file.
			if err := os.WriteFile(storage, []byte(`{"cookies":[{"name":"SAPISID","value":"fixture","domain":".google.com","path":"/","expires":-1,"secure":true,"sameSite":"Lax"}],"origins":[]}`), 0600); err != nil {
				t.Fatal(err)
			}
			state, err := aistudio.LoadStorageState(storage)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"__Secure-1PAPISID", "__Secure-3PAPISID"} {
				cookie := state.Cookies[0]
				cookie.Name = name
				state.Cookies = append(state.Cookies, cookie)
			}
			model := aistudio.Model{ID: "fixture-model", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
			account := &aistudio.Account{ID: "fixture@example.invalid", Directory: directory, StoragePath: storage, RuntimePath: filepath.Join(directory, "fixture-runtime.json"), StorageState: state, Config: aistudio.DefaultAccountConfig("fixture"), State: aistudio.AccountReady, Models: []aistudio.Model{model}}
			account.Config.Enabled = true
			pool := aistudio.NewAccountPool([]*aistudio.Account{account}, 1)
			pool.SetUpstreamChannels([]aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild})
			if err := pool.SetBuildCatalog(account.ID, []aistudio.Model{model}); err != nil {
				t.Fatal(err)
			}
			lease, _, err := pool.TryAcquireFor(t.Context(), aistudio.AccountSelection{ModelID: model.ID, Method: "generateContent", Channel: channel, StrictChannel: true})
			if err != nil || lease == nil {
				t.Fatal(err)
			}
			defer lease.Release()
			plain, err := aistudio.NewMakerSuiteHTTPTransport(aistudio.HTTPTransportOptions{Pool: pool, Headers: aistudio.ProtocolHeaderProviderFunc(func(context.Context, string) (http.Header, error) {
				t.Fatal("unexpected ordinary RPC")
				return nil, errors.New("offline")
			})})
			if err != nil {
				t.Fatal(err)
			}
			fake := &offlineProtectedPreparer{state: state}
			worker, err := aistudio.NewWorkerProtectedTransport(aistudio.WorkerProtectedTransportOptions{Transport: plain, SetupTimeout: time.Second, Workers: aistudio.ProtectedPreparerProviderFunc(func(context.Context, string, string) (aistudio.ProtectedPreparer, error) { return fake, nil })})
			if err != nil {
				t.Fatal(err)
			}
			refreshes := 0
			refresher := &authRuntimeRefresher{pool: pool, requests: newRequestRegistry(t.Context()), chromeHas: func(string) bool { return true }, importCurrent: func(_ context.Context, source aistudio.AuthSource, _ *aistudio.ChromeOAuthMaterial, _ string) (aistudio.StorageState, error) {
				refreshes++
				updated := state
				err := updated.SetAuthExtension(aistudio.AuthExtension{Source: source, OAuth: &aistudio.ChromeOAuthMaterial{GaiaID: "fixture-gaia", RefreshToken: "fixture-not-real"}})
				return updated, err
			}, reset: func(string) error { return nil }, prepareHeaders: func(string) (func(bool), error) { return func(bool) {}, nil }}
			transport := &authRetryProtectedTransport{transport: worker, refresher: refresher}
			selection, _ := aistudio.ParseModelSelection(string(channel) + "/fixture-model")
			request := aistudio.GenerateRequest{ID: "logical-choice", Model: model.ID, AccountID: account.ID, Selection: selection, Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "fixture prompt"}}}}}
			var body []byte
			method := "GenerateContent"
			if channel == aistudio.ChannelBuild {
				method = "ProxyStreamedCall"
				path, data, encodeErr := aistudio.EncodeBuildGenerateRequest(request, aistudio.GenerationDefaults{MaxOutputTokens: 32768}, false, false)
				if encodeErr != nil {
					t.Fatal(encodeErr)
				}
				body, err = aistudio.EncodeBuildProxyRequest(path, data, false)
			} else {
				body, err = aistudio.EncodeGenerateContentRequest(request, aistudio.GenerationDefaults{MaxOutputTokens: 32768}, aistudio.RequestContext{})
			}
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			attempts := map[string]aistudio.GenerationAttempt{}
			ctx := aistudio.ContextWithGenerationAttemptObserver(aistudio.ContextWithAccountLease(t.Context(), lease), func(attempt aistudio.GenerationAttempt) {
				mu.Lock()
				defer mu.Unlock()
				if old, ok := attempts[attempt.ID]; !ok || attempt.Revision > old.Revision {
					attempts[attempt.ID] = attempt
				}
			})
			response, err := transport.DoProtected(ctx, request, aistudio.RPCRequest{Method: method, URL: "https://fixture.invalid", Header: make(http.Header), Body: body, Streaming: true})
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if fake.sends != 2 || refreshes != 1 {
				t.Fatalf("sends=%d refreshes=%d", fake.sends, refreshes)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(attempts) != 2 {
				t.Fatalf("physical attempts=%d", len(attempts))
			}
			var replay, unauthorized bool
			for _, attempt := range attempts {
				if attempt.RequestID != request.ID || attempt.RequestedChannel != channel || attempt.Channel != channel || attempt.AccountID != account.ID || !attempt.Sent || !attempt.ResponseReceived {
					t.Fatalf("lost physical provenance: %+v", attempt)
				}
				if attempt.Reason == "auth_replay" {
					replay = true
				}
				if attempt.StatusCode == 401 {
					unauthorized = true
				}
			}
			if !replay || !unauthorized {
				t.Fatal("replay/401 evidence missing")
			}
		})
	}
}
