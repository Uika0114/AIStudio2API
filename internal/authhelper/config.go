package authhelper

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

type TLSFiles struct {
	CAFile   string `json:"ca_file"`
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
}

type ServerSettings struct {
	Version  int       `json:"version"`
	Enabled  bool      `json:"enabled"`
	Listen   string    `json:"listen"`
	TLS      TLSFiles  `json:"tls"`
	Mappings []Mapping `json:"mappings"`
}

type ClientSettings struct {
	Version       int      `json:"version"`
	Enabled       bool     `json:"enabled"`
	Endpoint      string   `json:"endpoint"`
	ServerName    string   `json:"server_name"`
	Consumer      string   `json:"consumer"`
	TLS           TLSFiles `json:"tls"`
	CookieDomains []string `json:"cookie_domains,omitempty"`
}

func ReadServerSettings(path string) (ServerSettings, error) {
	var value ServerSettings
	if err := readSettings(path, &value); err != nil {
		return value, err
	}
	if value.Version != Protocol {
		return value, failure("INVALID_REQUEST")
	}
	if value.Enabled && !LoopbackAddress(value.Listen) {
		return value, failure("INVALID_REQUEST")
	}
	return value, nil
}
func ReadClientSettings(path string) (ClientSettings, error) {
	var value ClientSettings
	if err := readSettings(path, &value); err != nil {
		return value, err
	}
	if value.Version != Protocol {
		return value, failure("INVALID_REQUEST")
	}
	return value, nil
}
func readSettings(path string, out any) error {
	if !filepath.IsAbs(path) {
		return failure("INVALID_REQUEST")
	}
	file, err := os.Open(path)
	if err != nil {
		return failure("STATE_UNAVAILABLE")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 65537))
	if err != nil || len(raw) > 65536 {
		return failure("INVALID_REQUEST")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(out) != nil || decoder.Decode(new(any)) != io.EOF {
		return failure("INVALID_REQUEST")
	}
	return nil
}
func LoopbackAddress(value string) bool {
	host, port, err := net.SplitHostPort(value)
	if err != nil || port == "" {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func loadTLS(files TLSFiles) (*tls.Config, error) {
	if !filepath.IsAbs(files.CAFile) || !filepath.IsAbs(files.CertFile) || !filepath.IsAbs(files.KeyFile) {
		return nil, failure("INVALID_REQUEST")
	}
	roots, err := os.ReadFile(files.CAFile)
	if err != nil {
		return nil, failure("STATE_UNAVAILABLE")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(roots) {
		return nil, failure("INVALID_REQUEST")
	}
	cert, err := tls.LoadX509KeyPair(files.CertFile, files.KeyFile)
	if err != nil || len(cert.Certificate) == 0 {
		return nil, failure("INVALID_REQUEST")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil || leaf.IsCA {
		return nil, failure("INVALID_REQUEST")
	}
	cert.Leaf = leaf
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, RootCAs: pool, ClientCAs: pool, NextProtos: []string{"http/1.1"}}, nil
}
func ServerTLS(files TLSFiles) (*tls.Config, error) {
	value, err := loadTLS(files)
	if err != nil {
		return nil, err
	}
	value.ClientAuth = tls.RequireAndVerifyClientCert
	// A cert refresh must affect new handshakes without restarting a helper
	// that might own an in-flight device-bound authentication operation.
	value.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		fresh, err := loadTLS(files)
		if err != nil {
			return nil, err
		}
		fresh.ClientAuth = tls.RequireAndVerifyClientCert
		return fresh, nil
	}
	return value, nil
}
func LoadClient(settings ClientSettings) (*Client, error) {
	if !settings.Enabled {
		return nil, failure("REMOTE_DISABLED")
	}
	value, err := clientTLS(settings)
	if err != nil {
		return nil, err
	}
	client, err := NewClient(settings.Endpoint, settings.Consumer, value, settings.CookieDomains)
	if err != nil {
		return nil, err
	}
	transport := client.http.Transport.(*http.Transport)
	// DialTLSContext retains the standard x509 verification. Reloading only
	// GetClientCertificate would leave the root pool stale when the CA cert
	// is renewed. There is deliberately no last-good or unverified fallback.
	transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		fresh, err := clientTLS(settings)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: fresh}
		return dialer.DialContext(ctx, network, addr)
	}
	return client, nil
}

func clientTLS(settings ClientSettings) (*tls.Config, error) {
	value, err := loadTLS(settings.TLS)
	if err != nil {
		return nil, err
	}
	leaf := value.Certificates[0].Leaf
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != settings.Consumer || len(leaf.URIs) != 0 {
		return nil, failure("IDENTITY_MISMATCH")
	}
	value.ServerName = settings.ServerName
	return value, nil
}
