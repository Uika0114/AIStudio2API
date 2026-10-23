package authhelper

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
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
	return value, nil
}
func LoadClient(settings ClientSettings) (*Client, error) {
	if !settings.Enabled {
		return nil, failure("REMOTE_DISABLED")
	}
	value, err := loadTLS(settings.TLS)
	if err != nil {
		return nil, err
	}
	if len(value.Certificates[0].Leaf.DNSNames) != 1 || value.Certificates[0].Leaf.DNSNames[0] != settings.Consumer {
		return nil, failure("IDENTITY_MISMATCH")
	}
	value.ServerName = settings.ServerName
	return NewClient(settings.Endpoint, settings.Consumer, value, settings.CookieDomains)
}
