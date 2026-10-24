package authhelper

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// These are generated, fixture-only keys. No real profiles or credentials.
func writeTLSFixture(t *testing.T, files TLSFiles, cert tls.Certificate) {
	t.Helper()
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string][]byte{
		files.CAFile:   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[1]}),
		files.CertFile: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}),
		files.KeyFile:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}),
	} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
func fixtureTLSFiles(t *testing.T, base, role string) TLSFiles {
	t.Helper()
	dir := filepath.Join(base, role)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return TLSFiles{CAFile: filepath.Join(dir, "ca.pem"), CertFile: filepath.Join(dir, "cert.pem"), KeyFile: filepath.Join(dir, "key.pem")}
}

func TestTLSFilesReloadOnNewHandshakeWithoutRecreatingClientOrServer(t *testing.T) {
	st1, ct1 := fixtureTLS(t)
	st2, ct2 := fixtureTLS(t) // Also changes the CA key: proves root pools reload.
	base := t.TempDir()
	sf, cf := fixtureTLSFiles(t, base, "server"), fixtureTLSFiles(t, base, "client")
	writeTLSFixture(t, sf, st1.Certificates[0])
	writeTLSFixture(t, cf, ct1.Certificates[0])
	serverTLS, err := ServerTLS(sf)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var clientRaw []byte
	served := 0
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		clientRaw = append([]byte(nil), r.TLS.PeerCertificates[0].Raw...)
		served++
		w.Write([]byte("fixture-only"))
	}))
	server.TLS = serverTLS
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	defer server.Close()
	settings := ClientSettings{Version: Protocol, Enabled: true, Endpoint: server.URL, ServerName: "helper.fixture.invalid", Consumer: "consumer.fixture.invalid", TLS: cf}
	client, err := LoadClient(settings)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	// A static client provides a control: unchanged in-memory cert/root fails
	// after the fixture files switch, while the long-lived LoadClient works.
	static, err := NewClient(server.URL, settings.Consumer, ct1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer static.Close()
	request := func(wantServer, wantClient []byte) {
		t.Helper()
		client.Close() // Forces a fresh handshake, not a new client instance.
		response, e := client.http.Get(server.URL)
		if e != nil {
			t.Fatal(e)
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if !bytes.Equal(response.TLS.PeerCertificates[0].Raw, wantServer) {
			t.Fatal("server used a stale leaf")
		}
		mu.Lock()
		defer mu.Unlock()
		if !bytes.Equal(clientRaw, wantClient) {
			t.Fatal("client used a stale leaf")
		}
	}
	request(st1.Certificates[0].Certificate[0], ct1.Certificates[0].Certificate[0])
	writeTLSFixture(t, sf, st2.Certificates[0])
	writeTLSFixture(t, cf, ct2.Certificates[0])
	if response, e := static.http.Get(server.URL); e == nil {
		response.Body.Close()
		t.Fatal("static control unexpectedly trusted the replacement CA")
	}
	request(st2.Certificates[0].Certificate[0], ct2.Certificates[0].Certificate[0])
	mu.Lock()
	if served != 2 {
		t.Fatalf("unexpected handled requests %d", served)
	}
	mu.Unlock()

	// A bad/missing new file must fail closed, never fall back to cached TLS.
	if err := os.WriteFile(cf.CAFile, []byte("not a certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	client.Close()
	if response, e := client.http.Get(server.URL); e == nil {
		response.Body.Close()
		t.Fatal("invalid CA fell back to old roots")
	}
	writeTLSFixture(t, cf, ct2.Certificates[0])
	writeTLSFixture(t, cf, st2.Certificates[0]) // Correctly signed but wrong role/name.
	client.Close()
	if response, e := client.http.Get(server.URL); e == nil {
		response.Body.Close()
		t.Fatal("changed consumer identity accepted")
	}
	writeTLSFixture(t, cf, ct2.Certificates[0])
	if err := os.Remove(sf.CertFile); err != nil {
		t.Fatal(err)
	}
	client.Close()
	if response, e := client.http.Get(server.URL); e == nil {
		response.Body.Close()
		t.Fatal("missing server leaf fell back to cached leaf")
	}
	mu.Lock()
	defer mu.Unlock()
	if served != 2 {
		t.Fatal("bad TLS reached the HTTP handler")
	}
}
