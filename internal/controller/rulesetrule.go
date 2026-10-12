/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	cf "github.com/cloudflare/cloudflare-go"

	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
)

// Ruleset rules. WAF custom rules, rate limiting rules, managed ruleset
// deployments and the Rules products (transform, redirect, cache,
// configuration and origin rules) are all rules in a zone's entry point
// ruleset for one Rulesets API phase. A kflare resource of one of these kinds
// manages exactly one such rule, and this file holds what they share:
//
//   - A rule is kflare's through its ref (rulesetRuleRef), which is derived
//     from the resource's kind, namespace and name. kflare finds its rules by
//     ref and never touches a rule whose ref it did not derive.
//   - Rules are written one at a time through the rule endpoints. The entry
//     point is created when the zone has none, but never replaced as a whole:
//     that would replace every rule in it, including rules kflare does not
//     manage.
//   - kflare keeps the rules it manages in a zone and phase in priority
//     order, moving them with position-only updates. Rules created outside
//     kflare keep their place.

// rulesetEntrypointExistsCode is the Cloudflare error code (with HTTP 400)
// for creating a zone entry point ruleset in a phase that already has one.
const rulesetEntrypointExistsCode = 20217

// entrypointRulesetName names the entry point rulesets kflare creates.
// Cloudflare requires a name; the phase, not the name, identifies the entry
// point.
const entrypointRulesetName = "default"

// RulesetRuleAPI is the part of the Cloudflare API that rule controllers use.
// The rule calls are kflare's own (pkg/cloudflare/rulesets.go): cloudflare-go
// cannot create or edit a single rule, and mishandles the response of a rule
// delete.
type RulesetRuleAPI interface {
	GetEntrypointRuleset(ctx context.Context, rc *cf.ResourceContainer, phase string) (cf.Ruleset, error)
	CreateRuleset(ctx context.Context, rc *cf.ResourceContainer, params cf.CreateRulesetParams) (cf.Ruleset, error)
	CreateRulesetRule(
		ctx context.Context, rc *cf.ResourceContainer, rulesetID string,
		rule cf.RulesetRule, position *cfpkg.RulesetRulePosition,
	) (cf.Ruleset, error)
	UpdateRulesetRule(
		ctx context.Context, rc *cf.ResourceContainer, rulesetID, ruleID string, rule cf.RulesetRule,
	) (cf.Ruleset, error)
	MoveRulesetRule(
		ctx context.Context, rc *cf.ResourceContainer, rulesetID, ruleID string, position cfpkg.RulesetRulePosition,
	) (cf.Ruleset, error)
	DeleteRulesetRule(ctx context.Context, rc *cf.ResourceContainer, rulesetID, ruleID string) (cf.Ruleset, error)
}

var _ RulesetRuleAPI = (*cfpkg.Client)(nil)

// rulesetRuleRef returns the ref of the rule a resource manages: "kflare-"
// and the first 32 hex characters of the SHA-256 of kind/namespace/name.
// Deriving it from the resource's identity rather than its UID lets a
// resource that is created again, for example from a backup, find its rule.
// Cloudflare requires refs to be unique within a ruleset and at most 128
// bytes long, and refuses to change an existing rule's ref.
func rulesetRuleRef(kind, namespace, name string) string {
	sum := sha256.Sum256([]byte(kind + "/" + namespace + "/" + name))
	return "kflare-" + hex.EncodeToString(sum[:16])
}

// rulesetRulePeer is a rule kflare manages in a zone and phase, with what
// decides its place among the others: lower priority first, then namespace,
// name and kind.
type rulesetRulePeer struct {
	ref       string
	priority  int32
	namespace string
	name      string
	kind      string
}

// sortedRulesetRulePeers returns peers in the order kflare keeps their rules
// in.
func sortedRulesetRulePeers(peers []rulesetRulePeer) []rulesetRulePeer {
	sorted := slices.Clone(peers)
	slices.SortFunc(sorted, func(a, b rulesetRulePeer) int {
		return cmp.Or(
			cmp.Compare(a.priority, b.priority),
			strings.Compare(a.namespace, b.namespace),
			strings.Compare(a.name, b.name),
			strings.Compare(a.kind, b.kind),
		)
	})
	return sorted
}

// rulesetRuleState is where a sync left the rule.
type rulesetRuleState struct {
	rulesetID string
	ruleID    string
	// version is the rule's version, which Cloudflare bumps on every update.
	version string
	// wrote is true when the sync created or updated the rule.
	wrote bool
	// moved is true when the sync reordered rules, or stopped reordering
	// because a rule disappeared. Another resource may have been reordering
	// the same rules at the same time, so the caller checks again shortly.
	moved bool
}

// syncRulesetRule makes the zone's entry point ruleset for phase hold rule,
// which rule.Ref identifies, and keeps the rules kflare manages there in
// order. peers lists those rules, this one included; without peers nothing
// is reordered and a new rule goes last. The rule is written whole, so it
// must set every field it wants: Cloudflare resets a field an update leaves
// out.
func syncRulesetRule(
	ctx context.Context,
	api RulesetRuleAPI,
	zoneID, phase string,
	rule cf.RulesetRule,
	peers []rulesetRulePeer,
) (rulesetRuleState, error) {
	rc := cf.ZoneIdentifier(zoneID)
	ruleset, wrote, err := writeRulesetRule(ctx, api, rc, phase, rule, peers)
	if cfpkg.IsNotFound(err) {
		// The rule or its ruleset was deleted between the read and the
		// write. Read the entry point again, and try once more.
		ruleset, wrote, err = writeRulesetRule(ctx, api, rc, phase, rule, peers)
	}
	if err != nil {
		return rulesetRuleState{}, err
	}
	ruleset, moved, err := orderRulesetRules(ctx, api, rc, ruleset, peers)
	if err != nil {
		return rulesetRuleState{}, err
	}
	current, found := ruleWithRef(ruleset.Rules, rule.Ref)
	if !found {
		// Deleted by someone else right after kflare wrote it; the error
		// makes the next reconcile write it again.
		return rulesetRuleState{}, fmt.Errorf("rule %s disappeared from ruleset %s during the sync", rule.Ref, ruleset.ID)
	}
	state := rulesetRuleState{rulesetID: ruleset.ID, ruleID: current.ID, wrote: wrote, moved: moved}
	if current.Version != nil {
		state.version = *current.Version
	}
	return state, nil
}

// writeRulesetRule creates or updates rule in the phase's entry point,
// creating the entry point when the zone has none, and writes nothing when
// the rule is already as desired. It returns the ruleset as Cloudflare last
// reported it, and whether it wrote.
func writeRulesetRule(
	ctx context.Context,
	api RulesetRuleAPI,
	rc *cf.ResourceContainer,
	phase string,
	rule cf.RulesetRule,
	peers []rulesetRulePeer,
) (cf.Ruleset, bool, error) {
	ruleset, err := api.GetEntrypointRuleset(ctx, rc, phase)
	if cfpkg.IsNotFound(err) {
		created, createErr := api.CreateRuleset(ctx, rc, cf.CreateRulesetParams{
			Name:  entrypointRulesetName,
			Kind:  string(cf.RulesetKindZone),
			Phase: phase,
			Rules: []cf.RulesetRule{rule},
		})
		if createErr == nil {
			return created, true, nil
		}
		if !cfpkg.HasErrorCode(createErr, rulesetEntrypointExistsCode) {
			return cf.Ruleset{}, false, createErr
		}
		// Another resource created the entry point first: add the rule to it.
		ruleset, err = api.GetEntrypointRuleset(ctx, rc, phase)
	}
	if err != nil {
		return cf.Ruleset{}, false, err
	}

	current, found := ruleWithRef(ruleset.Rules, rule.Ref)
	switch {
	case !found:
		position := insertPosition(ruleset.Rules, rule.Ref, peers)
		ruleset, err = api.CreateRulesetRule(ctx, rc, ruleset.ID, rule, position)
	case !rulesetRuleMatches(rule, current):
		ruleset, err = api.UpdateRulesetRule(ctx, rc, ruleset.ID, current.ID, rule)
	default:
		return ruleset, false, nil
	}
	if err != nil {
		return cf.Ruleset{}, false, err
	}
	return ruleset, true, nil
}

// insertPosition is where a new rule goes so that the rules kflare manages
// stay in order: right after the closest peer that sorts before it, or else
// right before the closest that sorts after it. It is nil, which appends the
// rule, when no other peer's rule is in the ruleset or ref is not a peer.
func insertPosition(rules []cf.RulesetRule, ref string, peers []rulesetRulePeer) *cfpkg.RulesetRulePosition {
	sorted := sortedRulesetRulePeers(peers)
	self := slices.IndexFunc(sorted, func(p rulesetRulePeer) bool { return p.ref == ref })
	if self < 0 {
		return nil
	}
	for i := self - 1; i >= 0; i-- {
		if before, ok := ruleWithRef(rules, sorted[i].ref); ok {
			return &cfpkg.RulesetRulePosition{After: before.ID}
		}
	}
	for _, peer := range sorted[self+1:] {
		if after, ok := ruleWithRef(rules, peer.ref); ok {
			return &cfpkg.RulesetRulePosition{Before: after.ID}
		}
	}
	return nil
}

// orderRulesetRules puts the peers' rules in ruleset into peer order. It
// walks them in that order and moves any rule that sits before the previous
// one to right after it, so the rules walked so far are always in order and
// one pass suffices. A move changes neither the rule's definition nor its
// version. Rules that are not kflare's keep their place. It returns the
// ruleset as Cloudflare last reported it, and whether it moved anything.
func orderRulesetRules(
	ctx context.Context,
	api RulesetRuleAPI,
	rc *cf.ResourceContainer,
	ruleset cf.Ruleset,
	peers []rulesetRulePeer,
) (cf.Ruleset, bool, error) {
	moved := false
	previousID := ""
	for _, peer := range sortedRulesetRulePeers(peers) {
		rule, found := ruleWithRef(ruleset.Rules, peer.ref)
		if !found {
			continue
		}
		if previousID != "" && ruleIndex(ruleset.Rules, rule.ID) < ruleIndex(ruleset.Rules, previousID) {
			updated, err := api.MoveRulesetRule(ctx, rc, ruleset.ID, rule.ID,
				cfpkg.RulesetRulePosition{After: previousID})
			if cfpkg.IsNotFound(err) {
				// One of the two rules was deleted meanwhile. Stop here; the
				// caller checks the order again shortly.
				return ruleset, true, nil
			}
			if err != nil {
				return ruleset, moved, err
			}
			ruleset, moved = updated, true
		}
		previousID = rule.ID
	}
	return ruleset, moved, nil
}

// deleteRulesetRule removes the rule with ref from the zone's entry point
// ruleset for phase. A missing entry point or rule counts as removed. The
// entry point stays even when the rule was its last: Cloudflare keeps an
// emptied entry point, and kflare may not be its only user.
func deleteRulesetRule(ctx context.Context, api RulesetRuleAPI, zoneID, phase, ref string) error {
	rc := cf.ZoneIdentifier(zoneID)
	ruleset, err := api.GetEntrypointRuleset(ctx, rc, phase)
	if cfpkg.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	rule, found := ruleWithRef(ruleset.Rules, ref)
	if !found {
		return nil
	}
	if _, err := api.DeleteRulesetRule(ctx, rc, ruleset.ID, rule.ID); err != nil && !cfpkg.IsNotFound(err) {
		return err
	}
	return nil
}

// ruleWithRef returns the rule in rules with ref.
func ruleWithRef(rules []cf.RulesetRule, ref string) (cf.RulesetRule, bool) {
	for _, rule := range rules {
		if rule.Ref == ref {
			return rule, true
		}
	}
	return cf.RulesetRule{}, false
}

// ruleIndex returns the position of the rule with id in rules, or -1.
func ruleIndex(rules []cf.RulesetRule, id string) int {
	return slices.IndexFunc(rules, func(rule cf.RulesetRule) bool { return rule.ID == id })
}

// rulesetRuleMatches reports whether actual, a rule as Cloudflare returned
// it, already is desired. It compares the definitions only, not the fields
// Cloudflare sets (the ID, version and time of the last update), and counts
// what Cloudflare fills in by itself as set (verified live): a rule is
// enabled unless told otherwise, and a skip rule logs its matches.
func rulesetRuleMatches(desired, actual cf.RulesetRule) bool {
	want, err := comparableRule(desired)
	if err != nil {
		return false
	}
	got, err := comparableRule(actual)
	if err != nil {
		return false
	}
	return reflect.DeepEqual(want, got)
}

// comparableRule turns a rule's definition into plain JSON values, with
// Cloudflare's defaults filled in and empty objects, arrays and nulls left
// out, so that two definitions compare equal when Cloudflare treats them the
// same.
func comparableRule(rule cf.RulesetRule) (any, error) {
	rule.ID, rule.Version, rule.LastUpdated = "", nil, nil
	if rule.Enabled == nil {
		rule.Enabled = cf.BoolPtr(true)
	}
	if rule.Action == string(cf.RulesetRuleActionSkip) && rule.Logging == nil {
		rule.Logging = &cf.RulesetRuleLogging{Enabled: cf.BoolPtr(true)}
	}
	raw, err := json.Marshal(rule)
	if err != nil {
		return nil, err
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return withoutEmpty(value), nil
}

// withoutEmpty drops the nulls, and the objects and arrays that are or end
// up empty, from the objects in a decoded JSON value. Array elements stay:
// their positions matter.
func withoutEmpty(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if item = withoutEmpty(item); !isEmptyJSON(item) {
				out[key] = item
			}
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = withoutEmpty(item)
		}
		return out
	default:
		return v
	}
}

// isEmptyJSON reports whether a decoded JSON value is null, {} or [].
func isEmptyJSON(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case map[string]any:
		return len(v) == 0
	case []any:
		return len(v) == 0
	default:
		return false
	}
}
