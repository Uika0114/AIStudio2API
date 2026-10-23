package authhelper

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

func fixtureTLS(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture-only-ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	raw, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	issue := func(name string, usage x509.ExtKeyUsage, serial int64) tls.Certificate {
		k, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if e != nil {
			t.Fatal(e)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(serial), DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
		der, e := x509.CreateCertificate(rand.Reader, template, ca, &k.PublicKey, key)
		if e != nil {
			t.Fatal(e)
		}
		leaf, e := x509.ParseCertificate(der)
		if e != nil {
			t.Fatal(e)
		}
		return tls.Certificate{Certificate: [][]byte{der, raw}, PrivateKey: k, Leaf: leaf}
	}
	server := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{issue("helper.fixture.invalid", x509.ExtKeyUsageServerAuth, 2)}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}
	client := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{issue("consumer.fixture.invalid", x509.ExtKeyUsageClientAuth, 3)}, RootCAs: pool, ServerName: "helper.fixture.invalid"}
	return server, client
}
func fixtureHTTP(t *testing.T, b *Broker, m Mapping) (*Client, *httptest.Server) {
	t.Helper()
	st, ct := fixtureTLS(t)
	server := httptest.NewUnstartedServer(Handler(b))
	server.TLS = st
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)
	c, err := NewClient(server.URL, m.Consumer, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c, server
}
func fixtureRemoteState(t *testing.T, m Mapping) (aistudio.StorageState, string) {
	t.Helper()
	state := aistudio.StorageState{}
	meta := &aistudio.SessionState{Version: Protocol, Remote: &aistudio.RemoteSession{Handle: m.Handle, Consumer: m.Consumer, GaiaID: m.GaiaID, Email: m.Email, Epoch: 1, MaterialVersion: 1}}
	if err := state.SetSession(meta); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "linux-consumer", "storage-state.json")
	if err := aistudio.WriteManagedSessionState(context.Background(), path, &state); err != nil {
		t.Fatal(err)
	}
	return state, path
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestClientLostResponseReusesDurableOperation(t *testing.T) {
	b, m, count := fixtureBroker(t, nil)
	c, _ := fixtureHTTP(t, b, m)
	state, path := fixtureRemoteState(t, m)
	original := c.http.Transport
	lose := true
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		response, err := original.RoundTrip(r)
		if err == nil && lose {
			lose = false
			response.Body.Close()
			return nil, errors.New("fixture lost response")
		}
		return response, err
	})
	ctx := aistudio.WithRemoteAuthBudget(context.Background())
	budget := func(id string) error { return aistudio.TakeRemoteAuthBudget(ctx, id) }
	if err := c.PrepareRecovery(ctx, &state, path, "reauth", budget); safeCode(err) != "HELPER_UNAVAILABLE" {
		t.Fatal(err)
	}
	state, err := aistudio.LoadStorageState(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Cookies) != 0 {
		t.Fatal("uncommitted cookies on disk")
	}
	if err := c.PrepareRecovery(ctx, &state, path, "reauth", budget); err != nil {
		t.Fatal(err)
	}
	if count.Load() != 1 {
		t.Fatal("lost response authenticated again")
	}
	persisted, _ := aistudio.LoadStorageState(path)
	if len(persisted.Cookies) != 0 {
		t.Fatal("prepare committed before runtime checks")
	}
	if err := aistudio.WriteManagedSessionState(ctx, path, &state); err != nil {
		t.Fatal(err)
	}
	meta, _ := state.Session()
	if meta.Remote.Pending != nil || meta.Remote.Ack == nil {
		t.Fatal("missing durable ack")
	}
	if err := c.AckSession(ctx, meta.Remote); err != nil {
		t.Fatal(err)
	}
	if err := c.AckSession(ctx, meta.Remote); err != nil {
		t.Fatal(err)
	}
}
func TestClientBudgetFailureDoesNotCreatePending(t *testing.T) {
	b, m, count := fixtureBroker(t, nil)
	c, _ := fixtureHTTP(t, b, m)
	state, path := fixtureRemoteState(t, m)
	err := c.PrepareRecovery(context.Background(), &state, path, "reauth", func(string) error { return failure("RECOVERY_BUDGET_EXHAUSTED") })
	if err == nil {
		t.Fatal("missing budget failure")
	}
	actual, _ := aistudio.LoadStorageState(path)
	meta, _ := actual.Session()
	if meta.Remote.Pending != nil || count.Load() != 0 {
		t.Fatal("denied budget changed persistent operation")
	}
}
func TestHandlerRejectsUnauthenticatedAndUnknownFields(t *testing.T) {
	b, m, count := fixtureBroker(t, nil)
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()
	Handler(b).ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatal("missing mTLS accepted")
	}
	c, server := fixtureHTTP(t, b, m)
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/refresh", strings.NewReader(`{"version":1,"root":"D:/must-not-read"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	r, err := c.http.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusBadRequest || count.Load() != 0 {
		t.Fatal("unknown selector was accepted")
	}
	request, err = http.NewRequest(http.MethodPost, server.URL+"/v1/refresh?force=1", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	r, err = c.http.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusBadRequest {
		t.Fatal("query bypass accepted")
	}
}
func TestClientRequiresLoopbackAndVerifyingTLS(t *testing.T) {
	_, ct := fixtureTLS(t)
	for _, endpoint := range []string{"http://127.0.0.1:1234", "https://example.invalid:1234", "https://127.0.0.1:1234/admin", "https://user@127.0.0.1:1234", "https://127.0.0.1:1234?force=1"} {
		if _, err := NewClient(endpoint, "consumer.fixture.invalid", ct, nil); err == nil {
			t.Fatal("bad endpoint accepted")
		}
	}
	ct.InsecureSkipVerify = true
	if _, err := NewClient("https://127.0.0.1:1234", "consumer.fixture.invalid", ct, nil); err == nil {
		t.Fatal("insecure TLS accepted")
	}
}
