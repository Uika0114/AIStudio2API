package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

func TestBootstrapProxyFailureStillReachesProxyPool(t *testing.T) {
	// This CONNECT endpoint never forwards traffic or reaches Google.
	var connects atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connects.Add(1)
		if r.Method != http.MethodConnect {
			t.Errorf("unexpected method %s", r.Method)
		}
		connection, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer connection.Close()
		_, _ = fmt.Fprint(buffer, "HTTP/1.1 502 synthetic-proxy-secret\r\nContent-Length: 0\r\n\r\n")
		_ = buffer.Flush()
	}))
	defer proxy.Close()
	path := filepath.Join(t.TempDir(), "synthetic-state.json")
	if err := os.WriteFile(path, []byte(`{"cookies":[],"origins":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	worker, err := newWAAWorker(ctx, "synthetic-account", camoufoxnative.Options{
		StorageStatePath: path, Model: "synthetic-model", Proxy: proxy.URL,
	})
	if worker != nil {
		_ = worker.Close()
		t.Fatal("failing proxy unexpectedly started a worker")
	}
	if err == nil || connects.Load() != 1 || !strings.Contains(err.Error(), "page_navigation") {
		t.Fatalf("did not reach isolated navigation failure: connects=%d error=%v", connects.Load(), err)
	}
	startup := &accountWorkerInitError{err: err, proxy: proxy.URL}
	if !proxyNetworkError(ctx, startup) || aistudio.DefinitiveAuthenticationFailure(startup) || strings.Contains(startup.Error(), "synthetic-proxy-secret") {
		t.Fatalf("startup classification/redaction changed: %v", startup)
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if proxyNetworkError(cancelled, startup) {
		t.Fatal("cancelled request counted as proxy failure")
	}
	alternative := "http://unused-alternative.invalid:9000"
	service := &trackedService{proxies: aistudio.NewProxyPool(&aistudio.AccountPool{}, []string{proxy.URL, alternative})}
	for i := 0; i < 3; i++ {
		if proxyNetworkError(ctx, startup) {
			service.reportProxyFailure(proxy.URL, "synthetic-account", startup)
		}
	}
	if service.proxies.Available(proxy.URL) || !service.proxies.Available(alternative) {
		t.Fatal("startup transport errors no longer reach existing proxy failure threshold")
	}
}
