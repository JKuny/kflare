/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package cloudflare

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	cf "github.com/cloudflare/cloudflare-go"
)

// Rules in a ruleset are managed one at a time through the Rulesets API's
// rule endpoints, so that kflare never replaces rules it does not manage.
// cloudflare-go v0.119 cannot create or edit a single rule, and its
// DeleteRulesetRule expects an empty 204 response while the API answers 200
// with the updated ruleset (verified live), so it reports an error after a
// successful delete. Client therefore makes these calls itself through
// API.Raw, which returns the same typed errors as every other SDK call.

// RulesetRulePosition places a rule next to another rule of the same
// ruleset. Set exactly one field.
type RulesetRulePosition struct {
	// Before places the rule immediately before the rule with this ID.
	Before string `json:"before,omitempty"`
	// After places the rule immediately after the rule with this ID.
	After string `json:"after,omitempty"`
}

// createRulesetRuleRequest is the body of a rule create: the rule's own
// fields, plus where to put it.
type createRulesetRuleRequest struct {
	cf.RulesetRule
	Position *RulesetRulePosition `json:"position,omitempty"`
}

// moveRulesetRuleRequest is the body of a move. A PATCH that holds only a
// position leaves the rule's definition and version unchanged (verified
// live).
type moveRulesetRuleRequest struct {
	Position RulesetRulePosition `json:"position"`
}

// CreateRulesetRule adds rule to the ruleset rulesetID: at position, or after
// its last rule when position is nil. It returns the updated ruleset.
func (c *Client) CreateRulesetRule(
	ctx context.Context,
	rc *cf.ResourceContainer,
	rulesetID string,
	rule cf.RulesetRule,
	position *RulesetRulePosition,
) (cf.Ruleset, error) {
	if err := checkRulesetIDs(rc, rulesetID); err != nil {
		return cf.Ruleset{}, err
	}
	body := createRulesetRuleRequest{RulesetRule: definitionOnly(rule), Position: position}
	return c.rulesetRequest(ctx, http.MethodPost, rulesetRulesPath(rc, rulesetID), body)
}

// UpdateRulesetRule replaces the definition of the rule ruleID with rule. The
// API replaces the whole rule, so a field rule leaves unset is reset rather
// than kept; the rule's ID and position stay the same. It returns the
// updated ruleset.
func (c *Client) UpdateRulesetRule(
	ctx context.Context,
	rc *cf.ResourceContainer,
	rulesetID, ruleID string,
	rule cf.RulesetRule,
) (cf.Ruleset, error) {
	if err := checkRulesetIDs(rc, rulesetID, ruleID); err != nil {
		return cf.Ruleset{}, err
	}
	return c.rulesetRequest(ctx, http.MethodPatch, rulesetRulePath(rc, rulesetID, ruleID), definitionOnly(rule))
}

// MoveRulesetRule moves the rule ruleID to position without changing its
// definition. It returns the updated ruleset.
func (c *Client) MoveRulesetRule(
	ctx context.Context,
	rc *cf.ResourceContainer,
	rulesetID, ruleID string,
	position RulesetRulePosition,
) (cf.Ruleset, error) {
	if err := checkRulesetIDs(rc, rulesetID, ruleID); err != nil {
		return cf.Ruleset{}, err
	}
	body := moveRulesetRuleRequest{Position: position}
	return c.rulesetRequest(ctx, http.MethodPatch, rulesetRulePath(rc, rulesetID, ruleID), body)
}

// DeleteRulesetRule removes the rule ruleID from the ruleset rulesetID and
// returns the updated ruleset. It replaces cf.API.DeleteRulesetRule, which
// fails on the ruleset the API returns (see above).
func (c *Client) DeleteRulesetRule(
	ctx context.Context,
	rc *cf.ResourceContainer,
	rulesetID, ruleID string,
) (cf.Ruleset, error) {
	if err := checkRulesetIDs(rc, rulesetID, ruleID); err != nil {
		return cf.Ruleset{}, err
	}
	return c.rulesetRequest(ctx, http.MethodDelete, rulesetRulePath(rc, rulesetID, ruleID), nil)
}

// rulesetRequest sends a rule request and decodes the ruleset the API
// returns.
func (c *Client) rulesetRequest(ctx context.Context, method, path string, body any) (cf.Ruleset, error) {
	res, err := c.Raw(ctx, method, path, body, nil)
	if err != nil {
		return cf.Ruleset{}, err
	}
	var ruleset cf.Ruleset
	if err := json.Unmarshal(res.Result, &ruleset); err != nil {
		return cf.Ruleset{}, fmt.Errorf("decoding the ruleset Cloudflare returned: %w", err)
	}
	return ruleset, nil
}

// checkRulesetIDs rejects a request that would target the wrong path: a
// missing zone or account, or an empty ruleset or rule ID.
func checkRulesetIDs(rc *cf.ResourceContainer, ids ...string) error {
	if rc == nil || rc.Identifier == "" {
		return cf.ErrMissingResourceIdentifier
	}
	for _, id := range ids {
		if id == "" {
			return cf.ErrMissingResourceIdentifier
		}
	}
	return nil
}

func rulesetRulesPath(rc *cf.ResourceContainer, rulesetID string) string {
	return fmt.Sprintf("/%s/%s/rulesets/%s/rules", rc.Level, rc.Identifier, rulesetID)
}

func rulesetRulePath(rc *cf.ResourceContainer, rulesetID, ruleID string) string {
	return rulesetRulesPath(rc, rulesetID) + "/" + ruleID
}

// definitionOnly drops the fields Cloudflare sets on a rule (its ID, version
// and last update), which do not belong in a request body.
func definitionOnly(rule cf.RulesetRule) cf.RulesetRule {
	rule.ID = ""
	rule.Version = nil
	rule.LastUpdated = nil
	return rule
}
