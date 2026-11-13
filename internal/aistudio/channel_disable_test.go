package aistudio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

type disabledChannelTransport struct {
	rpcCalls       atomic.Int64
	protectedCalls atomic.Int64
	bidiCalls      atomic.Int64
}

func (f *disabledChannelTransport) Do(_ context.Context, request RPCRequest) (*RPCResponse, error) {
	f.rpcCalls.Add(1)
	var body any
	switch request.Method {
	case "GetAiStudioBenefitTier":
		body = []any{0}
	case "ListModels":
		row := make([]any, 65)
		row[0], row[3], row[5], row[6] = "models/gemini-4-argon", "fixture", 1048576, 32768
		row[7], row[64] = []string{"generateContent", "countTokens"}, []int{1, 10, 48}
		body = []any{[]any{row}}
	default:
		return nil, fmt.Errorf("unexpected offline RPC %s", request.Method)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return &RPCResponse{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{JSONProtobufContentType}}, Body: io.NopCloser(bytes.NewReader(raw))}, nil
}

func (f *disabledChannelTransport) DoProtected(context.Context, GenerateRequest, RPCRequest) (*RPCResponse, error) {
	f.protectedCalls.Add(1)
	return nil, errors.New("fixture forbids protected sends")
}

func (f *disabledChannelTransport) OpenBidiProtected(context.Context, BidiRequest, RequestContext, *AccountLease, func() error) (*BidiSession, error) {
	f.bidiCalls.Add(1)
	return nil, errors.New("fixture forbids WebChannel sends")
}

func TestDisabledChannelsPoolSelectionAndCatalog(t *testing.T) {
	pool, account := assistantPolicyFixture(t)
	buildOnly := []Model{
		{ID: "fixture-build-only", Methods: []string{"generateContent"}},
		{ID: "fixture-embedding", Methods: []string{"embedContent"}},
		{ID: "fixture-live", Methods: []string{"bidiGenerateContent"}},
		{ID: "fixture-music", Methods: []string{"bidiGenerateMusic"}},
	}
	if err := pool.SetBuildCatalog(account.ID, append([]Model{account.Models[0]}, buildOnly...)); err != nil {
		t.Fatal(err)
	}
	if got := pool.PublicModels(append(append([]Model(nil), account.Models...), buildOnly...)); len(got) != 5 {
		t.Fatalf("fixture did not establish enabled Build catalog: %+v", got)
	}
	pool.SetUpstreamChannels([]Channel{ChannelPlayground})
	if pool.BuildEnabled() || !pool.PlaygroundEnabled() {
		t.Fatal("wrong enabled channels")
	}
	for _, channel := range []Channel{ChannelBuild, ChannelAssistant} {
		selection := AccountSelection{ModelID: account.Models[0].ID, Method: "generateContent", Channel: channel, StrictChannel: true}
		lease, _, err := pool.TryAcquireFor(t.Context(), selection)
		if lease != nil {
			_ = lease.Release()
			t.Fatalf("disabled %s acquired a lease", channel)
		}
		if err != nil && !errors.Is(err, ErrNoEligibleAccount) && !errors.Is(err, ErrModelNotFound) {
			t.Fatalf("disabled %s: %v", channel, err)
		}
	}
	selection := AccountSelection{ModelID: account.Models[0].ID, Method: "generateContent", PreferredChannel: ChannelBuild}
	lease, _, err := pool.TryAcquireFor(t.Context(), selection)
	if err != nil || lease == nil || lease.Channel() != ChannelPlayground {
		t.Fatalf("auto did not stay on PG: %v %v", lease, err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	models := pool.PublicModels(append(append([]Model(nil), account.Models...), buildOnly...))
	if len(models) != 1 || !reflect.DeepEqual(models[0].Channels, []string{"playground"}) {
		t.Fatalf("disabled channel/model leaked into catalog: %+v", models)
	}
	now := time.Now()
	if _, err := pool.MarkQuotaCooldownIfGeneration(account.ID, account.Models[0].ID, 1, now, QuotaCooldown{Until: now.Add(time.Hour), Reason: "fixture PG quota"}); err != nil {
		t.Fatal(err)
	}
	if lease, _, _ := pool.TryAcquireFor(t.Context(), selection); lease != nil {
		_ = lease.Release()
		t.Fatal("auto borrowed a disabled channel after PG quota exhaustion")
	}
}

func TestDisabledBuildAuxiliaryMethodsAndCatalogRefresh(t *testing.T) {
	pool, account := assistantPolicyFixture(t)
	pool.SetUpstreamChannels([]Channel{ChannelPlayground})
	if err := pool.SetBuildCatalog(account.ID, []Model{
		{ID: "fixture-embedding", Methods: []string{"embedContent"}},
		{ID: "fixture-live", Methods: []string{"bidiGenerateContent"}},
		{ID: "fixture-music", Methods: []string{"bidiGenerateMusic"}},
	}); err != nil {
		t.Fatal(err)
	}
	transport := &disabledChannelTransport{}
	client, err := NewClient(ClientOptions{Transport: transport, Protected: transport})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewPooledService(pool, client)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Embed(t.Context(), EmbeddingRequest{Model: "fixture-embedding"}); err == nil {
		t.Fatal("disabled Build embedding succeeded")
	}
	for _, model := range []string{"fixture-live", "fixture-music"} {
		if _, err := service.OpenBidi(t.Context(), BidiRequest{Model: model}); err == nil {
			t.Fatalf("disabled Build %s succeeded", model)
		}
	}
	if transport.rpcCalls.Load() != 0 || transport.protectedCalls.Load() != 0 || transport.bidiCalls.Load() != 0 {
		t.Fatal("disabled Build auxiliary method reached transport")
	}
	// PG catalog maintenance remains allowed, but it must not refresh Build.
	models, err := service.RefreshAccountModels(t.Context(), account.ID)
	if err != nil || len(models) != 1 {
		t.Fatalf("PG catalog refresh: %+v %v", models, err)
	}
	if transport.rpcCalls.Load() != 2 || transport.protectedCalls.Load() != 0 || transport.bidiCalls.Load() != 0 {
		t.Fatal("catalog refresh invoked a disabled upstream path")
	}
}
