/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package cloudflare_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	cf "github.com/cloudflare/cloudflare-go"

	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
)

// recordedRequest is a request as the test server received it.
type recordedRequest struct {
	method string
	path   string
	body   map[string]any // decoded; nil when the request had no body
}

// rulesetServer stands in for the Rulesets API: it answers every request with
// status and body, and hands each request to the test through requests.
type rulesetServer struct {
	t        *testing.T
	status   int
	body     string
	requests chan recordedRequest
}

func (s *rulesetServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	got := recordedRequest{method: r.Method, path: r.URL.Path}
	if auth := r.Header.Get("Authorization"); auth != "Bearer test-token" {
		s.t.Errorf("Authorization header = %q, want the API token", auth)
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		s.t.Errorf("reading the request body: %v", err)
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &got.body); err != nil {
			s.t.Errorf("request body %q is not a JSON object: %v", raw, err)
		}
	}
	s.requests <- got
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(s.status)
	_, _ = io.WriteString(w, s.body)
}

// request returns the request the server received. Call it after the client
// call returned: the server records a request before it answers.
func (s *rulesetServer) request() recordedRequest {
	s.t.Helper()
	select {
	case r := <-s.requests:
		return r
	default:
		s.t.Fatal("no request reached the server")
		return recordedRequest{}
	}
}

// newRulesetClient returns a Client that sends its requests to a test server
// answering with status and body.
func newRulesetClient(t *testing.T, status int, body string) (*cfpkg.Client, *rulesetServer) {
	t.Helper()
	server := &rulesetServer{t: t, status: status, body: body, requests: make(chan recordedRequest, 8)}
	ts := httptest.NewServer(server)
	t.Cleanup(ts.Close)
	api, err := cf.NewWithAPIToken("test-token", cf.BaseURL(ts.URL), cf.UsingRateLimit(1000))
	if err != nil {
		t.Fatalf("creating the API client: %v", err)
	}
	return &cfpkg.Client{API: api}, server
}

// rulesetResponse is how the API answers every rule call: the whole ruleset
// after the change.
const rulesetResponse = `{
  "success": true, "errors": [], "messages": [],
  "result": {
    "id": "rs1", "name": "default", "kind": "zone", "version": "7",
    "phase": "http_request_firewall_custom",
    "rules": [
      {"id": "r1", "ref": "kflare-a", "version": "2", "action": "block",
       "expression": "http.request.uri.path eq \"/a\"", "enabled": true,
       "last_updated": "2026-10-12T19:00:00Z"}
    ]
  }
}`

var zone = cf.ZoneIdentifier("zone1")

// probeRule is a rule as kflare would send it, with the server-set fields
// filled in as if it had been read back from Cloudflare.
func probeRule() cf.RulesetRule {
	version := "3"
	return cf.RulesetRule{
		ID:          "r1",
		Version:     &version,
		Ref:         "kflare-a",
		Action:      "block",
		Expression:  `http.request.uri.path eq "/a"`,
		Description: "block /a",
		Enabled:     cf.BoolPtr(true),
	}
}

func checkRuleset(t *testing.T, got cf.Ruleset) {
	t.Helper()
	if got.ID != "rs1" || got.Version == nil || *got.Version != "7" {
		t.Errorf("ruleset = %s version %v, want rs1 version 7", got.ID, got.Version)
	}
	if len(got.Rules) != 1 || got.Rules[0].ID != "r1" || got.Rules[0].Ref != "kflare-a" {
		t.Errorf("rules = %+v, want the one rule r1 (kflare-a)", got.Rules)
	}
}

// checkDefinitionOnly fails unless body holds the rule's definition and none
// of the fields Cloudflare sets itself.
func checkDefinitionOnly(t *testing.T, body map[string]any) {
	t.Helper()
	for _, field := range []string{"id", "version", "last_updated"} {
		if _, ok := body[field]; ok {
			t.Errorf("request body sets %q, which Cloudflare manages: %v", field, body)
		}
	}
	want := map[string]any{
		"ref": "kflare-a", "action": "block", "expression": `http.request.uri.path eq "/a"`,
		"description": "block /a", "enabled": true,
	}
	for field, value := range want {
		if body[field] != value {
			t.Errorf("request body %q = %v, want %v", field, body[field], value)
		}
	}
}

func TestCreateRulesetRule(t *testing.T) {
	client, server := newRulesetClient(t, http.StatusOK, rulesetResponse)

	got, err := client.CreateRulesetRule(context.Background(), zone, "rs1", probeRule(),
		&cfpkg.RulesetRulePosition{After: "r0"})
	if err != nil {
		t.Fatalf("CreateRulesetRule: %v", err)
	}
	checkRuleset(t, got)
	req := server.request()
	if req.method != http.MethodPost || req.path != "/zones/zone1/rulesets/rs1/rules" {
		t.Errorf("request = %s %s, want POST /zones/zone1/rulesets/rs1/rules", req.method, req.path)
	}
	checkDefinitionOnly(t, req.body)
	position, _ := req.body["position"].(map[string]any)
	if len(position) != 1 || position["after"] != "r0" {
		t.Errorf("position = %v, want only after r0", req.body["position"])
	}
}

func TestCreateRulesetRuleWithoutPosition(t *testing.T) {
	client, server := newRulesetClient(t, http.StatusOK, rulesetResponse)

	if _, err := client.CreateRulesetRule(context.Background(), zone, "rs1", probeRule(), nil); err != nil {
		t.Fatalf("CreateRulesetRule: %v", err)
	}
	if req := server.request(); req.body["position"] != nil {
		t.Errorf("request body sets a position, want none so the rule is appended: %v", req.body)
	}
}

func TestUpdateRulesetRule(t *testing.T) {
	client, server := newRulesetClient(t, http.StatusOK, rulesetResponse)

	got, err := client.UpdateRulesetRule(context.Background(), zone, "rs1", "r1", probeRule())
	if err != nil {
		t.Fatalf("UpdateRulesetRule: %v", err)
	}
	checkRuleset(t, got)
	req := server.request()
	if req.method != http.MethodPatch || req.path != "/zones/zone1/rulesets/rs1/rules/r1" {
		t.Errorf("request = %s %s, want PATCH /zones/zone1/rulesets/rs1/rules/r1", req.method, req.path)
	}
	checkDefinitionOnly(t, req.body)
	if req.body["position"] != nil {
		t.Errorf("request body sets a position, want the rule left in place: %v", req.body)
	}
}

func TestMoveRulesetRule(t *testing.T) {
	client, server := newRulesetClient(t, http.StatusOK, rulesetResponse)

	got, err := client.MoveRulesetRule(context.Background(), zone, "rs1", "r1",
		cfpkg.RulesetRulePosition{Before: "r2"})
	if err != nil {
		t.Fatalf("MoveRulesetRule: %v", err)
	}
	checkRuleset(t, got)
	req := server.request()
	if req.method != http.MethodPatch || req.path != "/zones/zone1/rulesets/rs1/rules/r1" {
		t.Errorf("request = %s %s, want PATCH /zones/zone1/rulesets/rs1/rules/r1", req.method, req.path)
	}
	// Only the position: a definition in the body would replace the rule's.
	want := mustJSON(t, map[string]any{"position": map[string]any{"before": "r2"}})
	if body := mustJSON(t, req.body); body != want {
		t.Errorf("request body = %s, want %s", body, want)
	}
}

func TestDeleteRulesetRule(t *testing.T) {
	// The API answers a delete with the updated ruleset and status 200
	// (verified live), not with an empty 204.
	client, server := newRulesetClient(t, http.StatusOK, rulesetResponse)

	got, err := client.DeleteRulesetRule(context.Background(), zone, "rs1", "r9")
	if err != nil {
		t.Fatalf("DeleteRulesetRule: %v", err)
	}
	checkRuleset(t, got)
	req := server.request()
	if req.method != http.MethodDelete || req.path != "/zones/zone1/rulesets/rs1/rules/r9" {
		t.Errorf("request = %s %s, want DELETE /zones/zone1/rulesets/rs1/rules/r9", req.method, req.path)
	}
	if req.body != nil {
		t.Errorf("request body = %v, want none", req.body)
	}
}

func TestRulesetRuleCallsReturnTypedErrors(t *testing.T) {
	notFound := `{"success": false, "messages": [], "result": null,
	  "errors": [{"code": 10002, "message": "could not find rule with public id r9"}]}`
	client, _ := newRulesetClient(t, http.StatusNotFound, notFound)
	_, err := client.DeleteRulesetRule(context.Background(), zone, "rs1", "r9")
	if !cfpkg.IsNotFound(err) {
		t.Errorf("DeleteRulesetRule of an unknown rule: err = %v, want a 404", err)
	}

	duplicateRef := `{"success": false, "messages": [], "result": null,
	  "errors": [{"code": 20023, "message": "'kflare-a' is not a valid value for ref"}]}`
	client, _ = newRulesetClient(t, http.StatusBadRequest, duplicateRef)
	_, err = client.CreateRulesetRule(context.Background(), zone, "rs1", probeRule(), nil)
	if !cfpkg.HasErrorCode(err, 20023) || !cfpkg.IsTerminalError(err) {
		t.Errorf("CreateRulesetRule with a taken ref: err = %v, want a terminal error with code 20023", err)
	}
}

func TestRulesetRuleCallsRejectMissingIDs(t *testing.T) {
	client, server := newRulesetClient(t, http.StatusOK, rulesetResponse)
	ctx := context.Background()
	calls := map[string]func() error{
		"nil container": func() error {
			_, err := client.CreateRulesetRule(ctx, nil, "rs1", probeRule(), nil)
			return err
		},
		"empty zone ID": func() error {
			_, err := client.UpdateRulesetRule(ctx, cf.ZoneIdentifier(""), "rs1", "r1", probeRule())
			return err
		},
		"empty ruleset ID": func() error {
			_, err := client.MoveRulesetRule(ctx, zone, "", "r1", cfpkg.RulesetRulePosition{After: "r0"})
			return err
		},
		"empty rule ID": func() error {
			_, err := client.DeleteRulesetRule(ctx, zone, "rs1", "")
			return err
		},
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, cf.ErrMissingResourceIdentifier) {
			t.Errorf("%s: err = %v, want ErrMissingResourceIdentifier", name, err)
		}
	}
	select {
	case req := <-server.requests:
		t.Errorf("a request was sent (%s %s), want none", req.method, req.path)
	default:
	}
}

func TestRulesetRuleCallsRejectAnUnexpectedResult(t *testing.T) {
	client, _ := newRulesetClient(t, http.StatusOK,
		`{"success": true, "errors": [], "messages": [], "result": "not a ruleset"}`)
	if _, err := client.DeleteRulesetRule(context.Background(), zone, "rs1", "r1"); err == nil {
		t.Error("DeleteRulesetRule with a result that is not a ruleset: err = nil, want a decoding error")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encoding %v: %v", v, err)
	}
	return string(b)
}
