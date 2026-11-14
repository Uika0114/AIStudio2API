package api

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/http/httpproxy"
)

func TestRemoteMediaAddressPolicy(t *testing.T) {
	for _, value := range []string{
		"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1", "169.254.169.254", "224.0.0.1", "0.0.0.0",
		"::1", "fc00::1", "fe80::1", "ff02::1", "::", "::ffff:127.0.0.1", "::ffff:192.168.1.1",
	} {
		t.Run(value, func(t *testing.T) {
			if err := checkRemoteMediaAddress(netip.MustParseAddr(value)); err == nil {
				t.Fatal("non-public address was accepted")
			}
			if err := checkRemoteMediaHost(context.Background(), value); err == nil {
				t.Fatal("literal non-public host was accepted")
			}
		})
	}
	for _, value := range []string{"8.8.8.8", "2606:4700:4700::1111", "::ffff:8.8.8.8"} {
		if err := checkRemoteMediaAddress(netip.MustParseAddr(value)); err != nil {
			t.Errorf("public address %s: %v", value, err)
		}
	}
}

func TestRemoteMediaDirectDialRejectsActualAddress(t *testing.T) {
	transport := remoteMediaTransport()
	defer transport.CloseIdleConnections()
	for _, address := range []string{"127.0.0.1:1", "[::1]:1", "[::ffff:127.0.0.1]:1"} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		conn, err := transport.direct.DialContext(ctx, "tcp", address)
		cancel()
		if conn != nil {
			conn.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "non-public address") {
			t.Errorf("dial %s = %v; want address-policy rejection before connect", address, err)
		}
	}
}

func TestRemoteMediaDNSRebindingAndProxyCollision(t *testing.T) {
	for _, noProxyCollision := range []bool{false, true} {
		name := "direct"
		if noProxyCollision {
			name = "no_proxy_matches_proxy_address"
		}
		t.Run(name, func(t *testing.T) {
			var received atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received.Add(1)
				_, _ = io.WriteString(w, "must not be downloaded")
			}))
			defer server.Close()
			_, port, err := net.SplitHostPort(server.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			var lookups atomic.Int32
			useRemoteMediaDNS(t, func(question dnsmessage.Question) []netip.Addr {
				if question.Type != dnsmessage.TypeA {
					return nil
				}
				if lookups.Add(1) == 1 {
					return []netip.Addr{netip.MustParseAddr("203.0.113.1")}
				}
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}
			})
			transport := remoteMediaTransport()
			defer transport.CloseIdleConnections()
			config := &httpproxy.Config{}
			if noProxyCollision {
				config.HTTPProxy = "http://rebind.invalid:" + port
				config.NoProxy = "rebind.invalid"
			}
			selectProxy := config.ProxyFunc()
			transport.proxied.Proxy = func(r *http.Request) (*url.URL, error) {
				return selectProxy(r.URL)
			}
			oldClient := remoteMediaClient
			remoteMediaClient = &http.Client{Transport: transport, CheckRedirect: oldClient.CheckRedirect}
			defer func() { remoteMediaClient = oldClient }()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err = downloadRemoteMedia(ctx, "http://rebind.invalid:"+port+"/media", "text/plain")
			if err == nil || !strings.Contains(err.Error(), "non-public address") {
				t.Fatalf("download = %v; want actual-address rejection", err)
			}
			if lookups.Load() < 2 || received.Load() != 0 {
				t.Fatalf("DNS A queries=%d, server requests=%d", lookups.Load(), received.Load())
			}
		})
	}
}

func TestRemoteMediaHostRejectsMixedDNSAnswers(t *testing.T) {
	useRemoteMediaDNS(t, func(question dnsmessage.Question) []netip.Addr {
		if question.Type == dnsmessage.TypeA {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}
		}
		return []netip.Addr{netip.MustParseAddr("::1")}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := checkRemoteMediaHost(ctx, "mixed.invalid"); err == nil || !strings.Contains(err.Error(), "non-public address ::1") {
		t.Fatalf("mixed DNS answers = %v", err)
	}
}

func TestRemoteMediaSelectedLocalProxy(t *testing.T) {
	for _, secureProxy := range []bool{false, true} {
		for _, targetScheme := range []string{"http", "https"} {
			name := targetScheme
			if secureProxy {
				name += "_via_https_proxy"
			} else {
				name += "_via_http_proxy"
			}
			t.Run(name, func(t *testing.T) {
				seen := make(chan string, 1)
				handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					seen <- r.Method + " " + r.Host
					if r.Method == http.MethodConnect {
						// Confirm CONNECT reaches the selected local proxy, without forwarding anywhere.
						http.Error(w, "offline proxy fixture", http.StatusBadGateway)
						return
					}
					_, _ = io.WriteString(w, "proxy media")
				})
				server := httptest.NewUnstartedServer(handler)
				if secureProxy {
					server.StartTLS()
				} else {
					server.Start()
				}
				defer server.Close()
				config := &httpproxy.Config{HTTPProxy: server.URL, HTTPSProxy: server.URL, NoProxy: "remote-media.invalid"}
				selectProxy := config.ProxyFunc()
				transport := remoteMediaTransport()
				defer transport.CloseIdleConnections()
				transport.proxied.Proxy = func(r *http.Request) (*url.URL, error) {
					return selectProxy(r.URL)
				}
				if secureProxy {
					transport.proxied.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
				}
				client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
				response, err := client.Get(targetScheme + "://203.0.113.2/media")
				if targetScheme == "http" {
					if err != nil {
						t.Fatal(err)
					}
					body, readErr := io.ReadAll(response.Body)
					response.Body.Close()
					if readErr != nil || string(body) != "proxy media" {
						t.Fatalf("proxy response: %q, %v", body, readErr)
					}
				} else if err == nil || !strings.Contains(err.Error(), "Bad Gateway") {
					if response != nil {
						response.Body.Close()
					}
					t.Fatalf("CONNECT result = %v; want fixture response", err)
				}
				want := "GET 203.0.113.2"
				if targetScheme == "https" {
					want = "CONNECT 203.0.113.2:443"
				}
				select {
				case got := <-seen:
					if got != want {
						t.Fatalf("proxy received %q, want %q", got, want)
					}
				default:
					t.Fatal("selected local proxy was not reached")
				}
			})
		}
	}
}

func TestRemoteMediaProxyRedirectRejectsPrivateTarget(t *testing.T) {
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		http.Redirect(w, r, "http://127.0.0.1/private", http.StatusFound)
	}))
	defer server.Close()
	proxyURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := remoteMediaTransport()
	defer transport.CloseIdleConnections()
	transport.proxied.Proxy = http.ProxyURL(proxyURL)
	client := &http.Client{Transport: transport, CheckRedirect: remoteMediaClient.CheckRedirect, Timeout: 3 * time.Second}
	response, err := client.Get("http://203.0.113.2/media")
	if response != nil {
		response.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "non-public address") || received.Load() != 1 {
		t.Fatalf("redirect result = %v, proxy requests = %d", err, received.Load())
	}
}

func TestRemoteMediaProxySelectionError(t *testing.T) {
	transport := remoteMediaTransport()
	defer transport.CloseIdleConnections()
	want := errors.New("proxy selection failed")
	transport.proxied.Proxy = func(*http.Request) (*url.URL, error) { return nil, want }
	request, err := http.NewRequest(http.MethodGet, "http://203.0.113.1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.RoundTrip(request); !errors.Is(err, want) {
		t.Fatalf("RoundTrip error = %v, want %v", err, want)
	}
}

func TestRemoteMediaRedirectPolicy(t *testing.T) {
	for _, target := range []string{"http://127.0.0.1/media", "https://[::1]/media", "http://[::ffff:127.0.0.1]/media"} {
		request, err := http.NewRequest(http.MethodGet, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := remoteMediaClient.CheckRedirect(request, []*http.Request{request}); err == nil {
			t.Errorf("redirect to %s was accepted", target)
		}
	}
	request, err := http.NewRequest(http.MethodGet, "https://8.8.8.8/media", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := remoteMediaClient.CheckRedirect(request, make([]*http.Request, 9)); err != nil {
		t.Fatalf("ninth redirect: %v", err)
	}
	if err := remoteMediaClient.CheckRedirect(request, make([]*http.Request, 10)); err == nil || err.Error() != "stopped after 10 redirects" {
		t.Fatalf("tenth redirect: %v", err)
	}
}

// DNS exchanges stay entirely inside net.Pipe; no resolver or Internet traffic is sent.
func useRemoteMediaDNS(t *testing.T, answer func(dnsmessage.Question) []netip.Addr) {
	t.Helper()
	previous := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			client, server := net.Pipe()
			go func() {
				defer server.Close()
				var length [2]byte
				if _, err := io.ReadFull(server, length[:]); err != nil {
					return
				}
				packet := make([]byte, binary.BigEndian.Uint16(length[:]))
				if _, err := io.ReadFull(server, packet); err != nil {
					return
				}
				var query dnsmessage.Message
				if err := query.Unpack(packet); err != nil || len(query.Questions) != 1 {
					t.Errorf("invalid fixture DNS query: %v", err)
					return
				}
				question := query.Questions[0]
				response := dnsmessage.Message{
					Header:    dnsmessage.Header{ID: query.Header.ID, Response: true, RecursionDesired: true, RecursionAvailable: true},
					Questions: query.Questions,
				}
				for _, ip := range answer(question) {
					header := dnsmessage.ResourceHeader{Name: question.Name, Type: question.Type, Class: dnsmessage.ClassINET}
					if question.Type == dnsmessage.TypeA && ip.Is4() {
						response.Answers = append(response.Answers, dnsmessage.Resource{Header: header, Body: &dnsmessage.AResource{A: ip.As4()}})
					} else if question.Type == dnsmessage.TypeAAAA && ip.Is6() {
						response.Answers = append(response.Answers, dnsmessage.Resource{Header: header, Body: &dnsmessage.AAAAResource{AAAA: ip.As16()}})
					}
				}
				packet, err := response.Pack()
				if err != nil {
					t.Errorf("packing fixture DNS response: %v", err)
					return
				}
				binary.BigEndian.PutUint16(length[:], uint16(len(packet)))
				_, _ = server.Write(append(length[:], packet...))
			}()
			return client, nil
		},
	}
	t.Cleanup(func() { net.DefaultResolver = previous })
}
