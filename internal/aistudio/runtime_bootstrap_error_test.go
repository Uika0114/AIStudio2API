package aistudio

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestBootstrapTransportErrorsRetainSafeClassification(t *testing.T) {
	for _, stage := range []string{"page", "logging", "waa", "discovery"} {
		for _, cause := range []error{errors.New("synthetic-proxy-secret"), context.Canceled, context.DeadlineExceeded} {
			t.Run(stage+"/"+cause.Error(), func(t *testing.T) {
				fail := bootstrapRoundTrip(func(*http.Request) (*http.Response, error) {
					return nil, fmt.Errorf("synthetic-proxy-secret: %w", cause)
				})
				runtime := bootstrapRuntime(fail)
				var err error
				switch stage {
				case "page":
					_, err = runtime.loadPage(context.Background())
				case "logging":
					_, err = runtime.loggingContextHeader(context.Background(), "synthetic-public-key")
				case "waa":
					_, err = runtime.createChallenge(context.Background(), nil)
				case "discovery":
					runtime = bootstrapRuntime(bootstrapRoundTrip(func(r *http.Request) (*http.Response, error) {
						response := bootstrapResponse(r, http.StatusFound, "")
						response.Header.Set("Location", "https://accounts.google.com/info/servicerestricted")
						return response, nil
					}))
					_, err = runtime.bootstrapAPIKey(context.Background(), func(string) (*http.Client, error) {
						return &http.Client{Transport: fail}, nil
					})
				}
				var transport *url.Error
				if err == nil || !errors.As(err, &transport) {
					t.Fatalf("transport classification lost: %v", err)
				}
				if transport.URL != "" || strings.Contains(fmt.Sprintf("%+v", err), "synthetic-proxy-secret") || strings.Contains(fmt.Sprintf("%+v", transport), "synthetic-proxy-secret") {
					t.Fatalf("unsafe transport error: %v", err)
				}
				if DefinitiveAuthenticationFailure(err) {
					t.Fatalf("transport failure became account authentication failure: %v", err)
				}
				for _, cancellation := range []error{context.Canceled, context.DeadlineExceeded} {
					if errors.Is(err, cancellation) != errors.Is(cause, cancellation) {
						t.Fatalf("cancellation identity changed: %v", err)
					}
				}
			})
		}
	}
}

func TestBootstrapNonTransportErrorsStayNonTransport(t *testing.T) {
	for _, cause := range []error{errors.New("synthetic-parser-secret"), context.Canceled, context.DeadlineExceeded} {
		err := safeBootstrapFailure("parse", cause)
		var transport *url.Error
		if errors.As(err, &transport) || strings.Contains(err.Error(), "synthetic-parser-secret") {
			t.Fatalf("parser failure reclassified or exposed: %v", err)
		}
		for _, cancellation := range []error{context.Canceled, context.DeadlineExceeded} {
			if errors.Is(err, cancellation) != errors.Is(cause, cancellation) {
				t.Fatalf("cancellation identity changed: %v", err)
			}
		}
	}
}
