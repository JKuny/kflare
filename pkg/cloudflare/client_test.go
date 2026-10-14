/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package cloudflare_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/cloudflare/cloudflare-go/v7/kv"
	"github.com/cloudflare/cloudflare-go/v7/option"

	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
)

func TestNew_EmptyToken(t *testing.T) {
	_, err := cfpkg.New("")
	if err == nil {
		t.Fatal("expected error for empty token, got nil")
	}
}

func TestNew_ValidToken(t *testing.T) {
	// cloudflare-go only validates token format at construction time; a
	// syntactically valid token string (non-empty) is accepted without a
	// live API call.  Credential correctness is verified on the first request.
	client, err := cfpkg.New("test-token-value")
	if err != nil {
		t.Fatalf("unexpected error for non-empty token: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil client")
	}
}

// roundTripFunc stands in for the network.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestNew_V7Options(t *testing.T) {
	// v7's NewClient would apply each of these to every request.
	t.Setenv("CLOUDFLARE_BASE_URL", "https://env.example.invalid/client/v4/")
	t.Setenv("CLOUDFLARE_API_TOKEN", "env-token")
	t.Setenv("CLOUDFLARE_API_KEY", "env-key")
	t.Setenv("CLOUDFLARE_EMAIL", "env@example.invalid")
	t.Setenv("CLOUDFLARE_API_USER_SERVICE_KEY", "env-service-key")
	t.Setenv("CLOUDFLARE_CUSTOM_HEADERS", "X-Kflare-Env: leaked")

	client, err := cfpkg.New("test-token-value")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Only the transport is replaced: the URL and headers come from the
	// client's options.
	var sent *http.Request
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		sent = req
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"result":{"id":"ns-id","title":"ns"},"success":true,"errors":[],"messages":[]}`)),
			Request: req,
		}, nil
	})
	var params kv.NamespaceGetParams
	params.AccountID.Value, params.AccountID.Present = "account-id", true
	ns, err := kv.NewNamespaceService(client.V7Options()...).Get(context.Background(), "ns-id", params,
		option.WithHTTPClient(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ns.Title != "ns" {
		t.Errorf("title = %q, want %q", ns.Title, "ns")
	}

	const wantURL = "https://api.cloudflare.com/client/v4/accounts/account-id/storage/kv/namespaces/ns-id"
	if sent.URL.String() != wantURL {
		t.Errorf("request URL = %s, want %s", sent.URL, wantURL)
	}
	if got := sent.Header.Get("Authorization"); got != "Bearer test-token-value" {
		t.Errorf("Authorization = %q, want the client's token", got)
	}
	for _, header := range []string{"X-Auth-Key", "X-Auth-Email", "X-Auth-User-Service-Key", "X-Kflare-Env"} {
		if got := sent.Header.Get(header); got != "" {
			t.Errorf("request carries %s %q from the environment", header, got)
		}
	}
}
