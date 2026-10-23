package authhelper

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// Handler requires a verified mutual-TLS peer even on loopback. It never logs bodies.
func Handler(b *Broker) http.Handler {
	slots := make(chan struct{}, 4)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		consumer, ok := peerConsumer(r.TLS)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.RawQuery != "" || r.URL.RawPath != "" || r.URL.IsAbs() || r.URL.EscapedPath() != r.URL.Path {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
			allowed := false
			for _, m := range b.mappings {
				if m.Consumer == consumer {
					allowed = true
				}
			}
			if !allowed {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"protocol": Protocol, "ready": true})
			return
		}
		if r.Method != http.MethodPost || (r.URL.Path != "/v1/refresh" && r.URL.Path != "/v1/ack") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxBody))
		decoder.DisallowUnknownFields()
		result := Result{Code: "OK"}
		var err error
		if r.URL.Path == "/v1/refresh" {
			var req RefreshRequest
			if decoder.Decode(&req) != nil || decoder.Decode(new(any)) != io.EOF {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			result.Session, err = b.Refresh(r.Context(), consumer, req)
		} else {
			var req AckRequest
			if decoder.Decode(&req) != nil || decoder.Decode(new(any)) != io.EOF {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			err = b.Ack(r.Context(), consumer, req)
		}
		if err != nil {
			result.Code = safeCode(err)
			result.Session = nil
			var notStarted *notStartedError
			if errors.As(err, &notStarted) {
				result.NotStarted = &notStarted.receipt
			}
			w.WriteHeader(statusForError(err))
		}
		_ = json.NewEncoder(w).Encode(result)
	})
}

func peerConsumer(state *tls.ConnectionState) (string, bool) {
	if state == nil || len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
		return "", false
	}
	leaf := state.PeerCertificates[0]
	if len(leaf.DNSNames) != 1 || len(leaf.URIs) != 0 || leaf.IsCA {
		return "", false
	}
	return leaf.DNSNames[0], true
}
