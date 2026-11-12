package chromeauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"testing"

	http "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
)

type observationClient struct {
	tls_client.HttpClient
	do func(*http.Request) (*http.Response, error)
}

func (c observationClient) Do(r *http.Request) (*http.Response, error) { return c.do(r) }
func (c observationClient) CloseIdleConnections()                      {}

type observationKey struct{ key *ecdsa.PrivateKey }

func (k observationKey) PublicKey() (*ecdsa.PublicKey, []byte, error) {
	encoded, err := x509.MarshalPKIXPublicKey(&k.key.PublicKey)
	return &k.key.PublicKey, encoded, err
}
func (k observationKey) SignSHA256(value []byte) ([]byte, error) {
	digest := sha256.Sum256(value)
	r, s, err := ecdsa.Sign(rand.Reader, k.key, digest[:])
	if err != nil {
		return nil, err
	}
	signature := make([]byte, 64)
	r.FillBytes(signature[:32])
	s.FillBytes(signature[32:])
	return signature, nil
}
func (k observationKey) Close() {}

type observationReadFailure struct{}

func (observationReadFailure) Read([]byte) (int, error) {
	return 0, errors.New("fixture-private-body-token")
}
func (observationReadFailure) Close() error { return nil }

func TestMultiloginTransportDiagnosticsNeverClaimUnstarted(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"dns", &net.DNSError{Name: "fixture-private-host", Err: "fixture-private-dns"}, "DNS_FAILURE"},
		{"proxy", errors.New("proxy https://user:fixture-private-token@fixture.invalid failed"), "TRANSPORT_FAILURE"},
		{"tls", errors.New("TLS fixture-private-key failed"), "TRANSPORT_FAILURE"},
		{"eof-after-partial-write", io.ErrUnexpectedEOF, "TRANSPORT_FAILURE"},
		{"canceled", context.Canceled, "CANCELED"},
		{"timeout", context.DeadlineExceeded, "TIMEOUT"},
		{"final-dial-after-hidden-retry", &url.Error{Op: "Post", URL: "https://fixture.invalid/?token=fixture-private-token", Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("fixture-private-last-error")}}, "TRANSPORT_FAILURE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var events []HTTPObservation
			ctx := WithHTTPObserver(t.Context(), func(v HTTPObservation) { events = append(events, v) })
			client := observationClient{do: func(*http.Request) (*http.Response, error) {
				if len(events) != 0 {
					t.Fatal("Do entry was reported as sent")
				}
				return nil, tc.err
			}}
			_, _, err := requestMultilogin(ctx, client, "fixture-private-gaia", "fixture-private-token", assertionSentinel)
			if len(events) != 1 || events[0].Phase != "challenge" || events[0].Stage != "transport" || events[0].Category != tc.want || events[0].SendEvidence != "unknown" || events[0].ResponseReceived || events[0].Elapsed < 0 {
				t.Fatalf("wrong safe observation: %+v", events)
			}
			if err == nil || oauthCategory(err) != tc.want || strings.Contains(fmt.Sprint(err, events), "fixture-private") {
				t.Fatal("raw transport error or secret escaped")
			}
			if tc.err == context.Canceled && !errors.Is(err, context.Canceled) || tc.err == context.DeadlineExceeded && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("safe cancellation identity lost")
			}
		})
	}
}

func TestMultiloginResponseDiagnosticsRemainSafe(t *testing.T) {
	for _, tc := range []struct {
		name string
		body io.ReadCloser
		want string
	}{
		{"body-read", observationReadFailure{}, "RESPONSE_READ_FAILURE"},
		{"body-parse", io.NopCloser(strings.NewReader("fixture-private-cookie")), "RESPONSE_INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var event HTTPObservation
			ctx := WithHTTPObserver(t.Context(), func(v HTTPObservation) { event = v })
			client := observationClient{do: func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: tc.body}, nil
			}}
			_, _, err := requestMultilogin(ctx, client, "fixture-gaia", "fixture-private-token", "fixture-private-assertion")
			if event.Phase != "assertion" || !event.ResponseReceived || event.SendEvidence != "response_received" || event.Category != tc.want || event.Status != 200 {
				t.Fatalf("response evidence lost: %+v", event)
			}
			if strings.Contains(fmt.Sprint(err, event), "fixture-private") {
				t.Fatal("raw response escaped")
			}
		})
	}
	if err := multiloginStatusError("assertion", 500, "fixture-private-server-status"); oauthCategory(err) != "UPSTREAM_REJECTED" || strings.Contains(err.Error(), "fixture-private") {
		t.Fatal("server status text escaped", err)
	}
	if !errors.Is(multiloginStatusError("challenge", 401, "INVALID_TOKENS"), ErrCredentialsRejected) {
		t.Fatal("material rejection identity lost")
	}
}

func TestMultiloginBothPhasesRetainEarlierResponse(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var events []HTTPObservation
	firstObserverCalls := 0
	ctx := WithHTTPObserver(t.Context(), func(HTTPObservation) { firstObserverCalls++ })
	ctx = WithHTTPObserver(ctx, func(v HTTPObservation) { events = append(events, v) })
	calls := 0
	client := observationClient{do: func(*http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			body := `{"status":"RETRY","failed_accounts":[{"token_binding_retry_response":{"challenge":"fixture-private-challenge"}}]}`
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		}
		// This is exactly what Do exposes after any hidden partial send/retry.
		return nil, errors.New("fixture-private-final-connection-error")
	}}
	_, err = fetchGoogleCookiesWith(ctx, "fixture-private-gaia", "fixture-private-token", nil, "",
		func([]byte) (deviceBindingKey, error) { return observationKey{key}, nil },
		func(string) (tls_client.HttpClient, error) { return client, nil })
	if oauthCategory(err) != "TRANSPORT_FAILURE" || calls != 2 || len(events) != 3 || firstObserverCalls != 3 {
		t.Fatalf("phase/observer loss: calls=%d events=%+v err=%v", calls, events, err)
	}
	if events[0].Phase != "challenge" || !events[0].ResponseReceived || events[1].Phase != "assertion" || events[1].SendEvidence != "unknown" || events[2].Phase != "workflow" || events[2].Stage != "assertion" {
		t.Fatalf("last failure erased earlier response: %+v", events)
	}
	encoded, _ := json.Marshal(events)
	if strings.Contains(string(encoded), "fixture-private") {
		t.Fatal("private material entered diagnostics")
	}
}

func TestMultiloginPreparationFailureIsSafeButNotAReceipt(t *testing.T) {
	var events []HTTPObservation
	ctx := WithHTTPObserver(t.Context(), func(v HTTPObservation) { events = append(events, v) })
	_, err := fetchGoogleCookiesWith(ctx, "fixture", "fixture-private-token", nil, "",
		func([]byte) (deviceBindingKey, error) { return nil, errors.New("fixture-private-device-key") },
		func(string) (tls_client.HttpClient, error) { t.Fatal("client must not be opened"); return nil, nil })
	if !errors.Is(err, ErrDeviceKeyUnavailable) || len(events) != 1 || events[0].Stage != "key_open" || events[0].Category != "DEVICE_KEY_FAILURE" || events[0].SendEvidence != "unknown" {
		t.Fatalf("preparation failure mislabeled: %+v %v", events, err)
	}
}
