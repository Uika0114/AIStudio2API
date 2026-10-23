package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

func TestRemoteCurrentContinuationRPCReplayIsIdentical(t *testing.T) {
	refresher, ctx, _, _, workflows := fixtureRemoteRefresher(t)
	// A current-round request already carries the continuation token. Recovery
	// may replay precisely this RPC, not submit the original generation again.
	body := []byte(`["fixture-model",null,"fixture-continuation-token",32768]`)
	sent := 0
	transport := &authRetryProtectedTransport{refresher: refresher, transport: aistudio.ProtectedTransportFunc(func(_ context.Context, request aistudio.GenerateRequest, rpc aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
		sent++
		if !bytes.Equal(body, rpc.Body) || request.Selection.Channel != aistudio.ChannelPlayground || rpc.Method != "GenerateContent" {
			t.Fatal("replay changed current round")
		}
		if sent == 1 {
			return nil, &aistudio.RPCError{StatusCode: 401, Code: 16, Method: rpc.Method}
		}
		return &aistudio.RPCResponse{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(`[]`))}, nil
	})}
	selection, _ := aistudio.ParseModelSelection("playground/fixture-model")
	response, err := transport.DoProtected(ctx, aistudio.GenerateRequest{Selection: selection}, aistudio.RPCRequest{Method: "GenerateContent", Body: body, Streaming: true})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if sent != 2 || *workflows != 1 {
		t.Fatal("unexpected sends/workflows")
	}
}

func TestRemoteWorkerAndRPCUseOneLogicalRecoveryBudget(t *testing.T) {
	refresher, ctx, _, _, workflows := fixtureRemoteRefresher(t)
	expired := &aistudio.RPCError{StatusCode: 401, Code: 16}
	if err := refresher.Recover(ctx, expired); err != nil {
		t.Fatal(err)
	} // Worker recovery.
	sends := 0
	_, err := refresher.do(ctx, "GenerateContent", func() (*aistudio.RPCResponse, error) { sends++; return nil, expired })
	if !aistudio.IsSessionFailureCode(err, "RECOVERY_BUDGET_EXHAUSTED") || sends != 1 || *workflows != 1 {
		t.Fatal("RPC reset worker budget", err)
	}
}

func TestRemoteConfigurationRemainsNativeAndSurvivesAdminSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.env")
	cfg := config.Default()
	cfg.RemoteAuthConfig = filepath.Join(t.TempDir(), "fixture-client.json")
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil || loaded.RemoteAuthConfig != cfg.RemoteAuthConfig {
		t.Fatal(err)
	}
	ctx := t.Context()
	admin := &runtimeAdmin{configPath: path, requests: newRequestRegistry(ctx), pool: aistudio.NewAccountPool(nil, 1)}
	value, err := admin.RuntimeConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("fixture-client.json")) {
		t.Fatal("native config reference exposed through management DTO")
	}
	if _, err := admin.UpdateRuntimeConfig(ctx, value); err != nil {
		t.Fatal(err)
	}
	saved, err := config.Load(path)
	if err != nil || saved.RemoteAuthConfig != cfg.RemoteAuthConfig {
		t.Fatal("management save removed native config", err)
	}
	// Marker priority also applies to nested worker errors with old causes.
	mixed := errors.Join(aistudio.SessionError("HELPER_UNAVAILABLE"), &accountWorkerInitError{err: &aistudio.RPCError{StatusCode: http.StatusUnauthorized}})
	if retryableGenerateAccountError(ctx, mixed) {
		t.Fatal("nested worker error bypassed marker")
	}
}
