package aistudio

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// A fake WAA preparer exercises only the real protected-transport seam. These
// cookie values are invented in memory, not read from an account or a browser.
type assistantReviewPreparer struct {
	prepared ProtectedRequest
	sends    int
}

func (p *assistantReviewPreparer) Prepare(_ context.Context, r ProtectedRequest) (PreparedProtectedRequest, error) {
	p.prepared = r
	return PreparedProtectedRequest{Headers: r.Headers.Clone(), Body: append([]byte(nil), r.Body...)}, nil
}
func (*assistantReviewPreparer) BrowserStorageState(context.Context) (StorageState, error) {
	return StorageState{Cookies: []StateCookie{
		{Name: "SAPISID", Value: "invented-fixture", Domain: ".google.com", Path: "/", Secure: true, Expires: -1},
		{Name: "__Secure-1PAPISID", Value: "invented-fixture", Domain: ".google.com", Path: "/", Secure: true, Expires: -1},
		{Name: "__Secure-3PAPISID", Value: "invented-fixture", Domain: ".google.com", Path: "/", Secure: true, Expires: -1},
	}}, nil
}
func (p *assistantReviewPreparer) SendProtected(context.Context, ProtectedRequest) (*RPCResponse, error) {
	p.sends++
	return nil, errors.New("fixture: response headers unavailable after submit")
}

func TestAssistantProtectedSendBindsProofAndBlocksAmbiguousReplay(t *testing.T) {
	fixture, ctx, lease := assistantCoreFixture(t)
	request, err := NormalizeGenerateRequestModel(assistantCoreRequest(lease))
	if err != nil {
		t.Fatal(err)
	}
	body, err := EncodeAssistantRequest(request, GenerationDefaults{}, "APPLET-1", "FIXTURE-ID")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := NewMakerSuiteHTTPTransport(HTTPTransportOptions{Pool: lease.pool, Headers: ProtocolHeaderProviderFunc(func(context.Context, string) (http.Header, error) {
		t.Fatal("no HTTP is allowed")
		return nil, errors.New("offline")
	})})
	if err != nil {
		t.Fatal(err)
	}
	preparer := &assistantReviewPreparer{}
	transport, err := NewWorkerProtectedTransport(WorkerProtectedTransportOptions{Transport: plain, SetupTimeout: time.Second, Workers: ProtectedPreparerProviderFunc(func(_ context.Context, account, model string) (ProtectedPreparer, error) {
		if account != lease.Account().ID || model != "gemini-4-argon" {
			t.Fatal("incorrect worker binding")
		}
		return preparer, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	_, err = transport.DoProtected(ctx, request, newRPCRequest(assistantInitMethod, request.AccountID, request.ID, body, false))
	if err == nil || preparer.sends != 1 {
		t.Fatal("expected one failed physical send", err)
	}
	if preparer.prepared.ProofField != 2 || preparer.prepared.Prompt != "fixture question" || preparer.prepared.PagePrompt != "fixture question" {
		t.Fatal("incorrect assistant proof binding")
	}
	if !fixture.progress.Load() {
		t.Fatal("ambiguous task submit would be replayable")
	}
	_, _, attempts := fixture.snapshot()
	if len(attempts) != 1 {
		t.Fatal("incorrect physical accounting")
	}
	for _, attempt := range attempts {
		if !attempt.Sent || attempt.ResponseReceived || attempt.Stage != "failed" || attempt.Channel != ChannelAssistant {
			t.Fatalf("wrong send boundary: %+v", attempt)
		}
	}
}
