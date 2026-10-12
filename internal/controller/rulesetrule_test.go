/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	cf "github.com/cloudflare/cloudflare-go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
)

const (
	rulesTestZone  = "zone-rules-1"
	rulesTestPhase = "http_request_firewall_custom"
)

// fakeRulesetAPI is an in-memory Rulesets API for one zone. It follows the
// behavior verified against the live API on 2026-10-12:
//
//   - A phase without an entry point answers 404 (code 10003). Creating a
//     second zone entry point in a phase is a 400 (code 20217).
//   - A new rule goes last unless placed before or after another rule. A rule
//     without a ref gets its ID as ref. Refs are unique within a ruleset and
//     cannot change (400, code 20023). The plan caps the rules in a phase
//     (400, code 50001).
//   - Cloudflare enables a rule unless told otherwise, and makes a skip rule
//     log its matches.
//   - An update replaces the whole rule but keeps its ID, ref and place, and
//     bumps its version. A move changes neither its definition nor its
//     version.
//   - Every write bumps the ruleset's version and returns the whole ruleset.
//   - An unknown ruleset is a 404 (code 10001), an unknown rule a 404 (code
//     10002). Deleting the last rule leaves the entry point, without rules.
type fakeRulesetAPI struct {
	rulesets map[string]*cf.Ruleset // entry points, by phase
	maxRules int
	nextID   int
	clock    time.Time

	// before runs at the start of every call with the method's name, so that
	// a test can change the ruleset the way a concurrent writer would.
	before func(method string)
	// errs makes a method fail with the error instead of running.
	errs map[string]error

	writes int // successful creates, updates and deletes
	moves  int // successful moves
}

func newFakeRulesetAPI() *fakeRulesetAPI {
	return &fakeRulesetAPI{
		rulesets: map[string]*cf.Ruleset{},
		maxRules: 5,
		clock:    time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC),
		errs:     map[string]error{},
	}
}

func (f *fakeRulesetAPI) start(method string, rc *cf.ResourceContainer) error {
	if rc == nil || rc.Level != cf.ZoneRouteLevel || rc.Identifier != rulesTestZone {
		return fmt.Errorf("fake: %s called for %+v, want zone %s", method, rc, rulesTestZone)
	}
	if f.before != nil {
		f.before(method)
	}
	return f.errs[method]
}

func (f *fakeRulesetAPI) newID() string {
	f.nextID++
	return fmt.Sprintf("id%02d", f.nextID)
}

// stored returns rule as Cloudflare stores it under id, with version.
func (f *fakeRulesetAPI) stored(rule cf.RulesetRule, id string, version int) cf.RulesetRule {
	f.clock = f.clock.Add(time.Minute)
	updated := f.clock
	rule.ID, rule.LastUpdated = id, &updated
	v := strconv.Itoa(version)
	rule.Version = &v
	if rule.Ref == "" {
		rule.Ref = id
	}
	if rule.Enabled == nil {
		rule.Enabled = cf.BoolPtr(true)
	}
	if rule.Action == "skip" && rule.Logging == nil {
		rule.Logging = &cf.RulesetRuleLogging{Enabled: cf.BoolPtr(true)}
	}
	return rule
}

// bump records a change to rs, which Cloudflare answers with the whole
// ruleset.
func (f *fakeRulesetAPI) bump(rs *cf.Ruleset) cf.Ruleset {
	version := 1
	if rs.Version != nil {
		version, _ = strconv.Atoi(*rs.Version)
		version++
	}
	v := strconv.Itoa(version)
	rs.Version = &v
	if len(rs.Rules) == 0 {
		rs.Rules = nil // Cloudflare returns "rules": null for an emptied ruleset
	}
	return copyRuleset(rs)
}

func (f *fakeRulesetAPI) ruleset(id string) (*cf.Ruleset, error) {
	for _, rs := range f.rulesets {
		if rs.ID == id {
			return rs, nil
		}
	}
	return nil, cloudflareError(404, 10001)
}

func (f *fakeRulesetAPI) GetEntrypointRuleset(_ context.Context, rc *cf.ResourceContainer, phase string) (cf.Ruleset, error) {
	if err := f.start("GetEntrypointRuleset", rc); err != nil {
		return cf.Ruleset{}, err
	}
	rs, ok := f.rulesets[phase]
	if !ok {
		return cf.Ruleset{}, cloudflareError(404, 10003)
	}
	return copyRuleset(rs), nil
}

func (f *fakeRulesetAPI) CreateRuleset(
	_ context.Context, rc *cf.ResourceContainer, params cf.CreateRulesetParams,
) (cf.Ruleset, error) {
	if err := f.start("CreateRuleset", rc); err != nil {
		return cf.Ruleset{}, err
	}
	if params.Kind != "zone" || params.Name == "" {
		return cf.Ruleset{}, fmt.Errorf("fake: want a named zone entry point, got %+v", params)
	}
	if _, ok := f.rulesets[params.Phase]; ok {
		return cf.Ruleset{}, cloudflareError(400, rulesetEntrypointExistsCode)
	}
	if len(params.Rules) > f.maxRules {
		return cf.Ruleset{}, cloudflareError(400, 50001)
	}
	rs := &cf.Ruleset{ID: f.newID(), Name: params.Name, Kind: params.Kind, Phase: params.Phase}
	for _, rule := range params.Rules {
		if _, taken := ruleWithRef(rs.Rules, rule.Ref); taken {
			return cf.Ruleset{}, cloudflareError(400, 20023)
		}
		rs.Rules = append(rs.Rules, f.stored(rule, f.newID(), 1))
	}
	f.rulesets[params.Phase] = rs
	f.writes++
	return f.bump(rs), nil
}

func (f *fakeRulesetAPI) CreateRulesetRule(
	_ context.Context, rc *cf.ResourceContainer, rulesetID string,
	rule cf.RulesetRule, position *cfpkg.RulesetRulePosition,
) (cf.Ruleset, error) {
	if err := f.start("CreateRulesetRule", rc); err != nil {
		return cf.Ruleset{}, err
	}
	rs, err := f.ruleset(rulesetID)
	if err != nil {
		return cf.Ruleset{}, err
	}
	if _, taken := ruleWithRef(rs.Rules, rule.Ref); taken && rule.Ref != "" {
		return cf.Ruleset{}, cloudflareError(400, 20023)
	}
	if len(rs.Rules) >= f.maxRules {
		return cf.Ruleset{}, cloudflareError(400, 50001)
	}
	index, err := insertionIndex(rs.Rules, position)
	if err != nil {
		return cf.Ruleset{}, err
	}
	rs.Rules = slices.Insert(rs.Rules, index, f.stored(rule, f.newID(), 1))
	f.writes++
	return f.bump(rs), nil
}

func (f *fakeRulesetAPI) UpdateRulesetRule(
	_ context.Context, rc *cf.ResourceContainer, rulesetID, ruleID string, rule cf.RulesetRule,
) (cf.Ruleset, error) {
	if err := f.start("UpdateRulesetRule", rc); err != nil {
		return cf.Ruleset{}, err
	}
	rs, err := f.ruleset(rulesetID)
	if err != nil {
		return cf.Ruleset{}, err
	}
	i := ruleIndex(rs.Rules, ruleID)
	if i < 0 {
		return cf.Ruleset{}, cloudflareError(404, 10002)
	}
	old := rs.Rules[i]
	if rule.Ref != "" && rule.Ref != old.Ref {
		return cf.Ruleset{}, cloudflareError(400, 20023)
	}
	rule.Ref = old.Ref
	version, _ := strconv.Atoi(*old.Version)
	rs.Rules[i] = f.stored(rule, old.ID, version+1)
	f.writes++
	return f.bump(rs), nil
}

func (f *fakeRulesetAPI) MoveRulesetRule(
	_ context.Context, rc *cf.ResourceContainer, rulesetID, ruleID string, position cfpkg.RulesetRulePosition,
) (cf.Ruleset, error) {
	if err := f.start("MoveRulesetRule", rc); err != nil {
		return cf.Ruleset{}, err
	}
	rs, err := f.ruleset(rulesetID)
	if err != nil {
		return cf.Ruleset{}, err
	}
	i := ruleIndex(rs.Rules, ruleID)
	if i < 0 {
		return cf.Ruleset{}, cloudflareError(404, 10002)
	}
	rule := rs.Rules[i]
	rest := slices.Delete(slices.Clone(rs.Rules), i, i+1)
	index, err := insertionIndex(rest, &position)
	if err != nil {
		return cf.Ruleset{}, err
	}
	rs.Rules = slices.Insert(rest, index, rule)
	f.moves++
	return f.bump(rs), nil
}

func (f *fakeRulesetAPI) DeleteRulesetRule(
	_ context.Context, rc *cf.ResourceContainer, rulesetID, ruleID string,
) (cf.Ruleset, error) {
	if err := f.start("DeleteRulesetRule", rc); err != nil {
		return cf.Ruleset{}, err
	}
	rs, err := f.ruleset(rulesetID)
	if err != nil {
		return cf.Ruleset{}, err
	}
	i := ruleIndex(rs.Rules, ruleID)
	if i < 0 {
		return cf.Ruleset{}, cloudflareError(404, 10002)
	}
	rs.Rules = slices.Delete(rs.Rules, i, i+1)
	f.writes++
	return f.bump(rs), nil
}

// insertionIndex is where a rule placed at position goes in rules: the end
// without a position, or next to the one rule the position names.
func insertionIndex(rules []cf.RulesetRule, position *cfpkg.RulesetRulePosition) (int, error) {
	switch {
	case position == nil:
		return len(rules), nil
	case position.Before != "" && position.After == "":
		if i := ruleIndex(rules, position.Before); i >= 0 {
			return i, nil
		}
	case position.After != "" && position.Before == "":
		if i := ruleIndex(rules, position.After); i >= 0 {
			return i + 1, nil
		}
	}
	return 0, fmt.Errorf("fake: position %+v does not name exactly one rule of the ruleset", *position)
}

// copyRuleset returns rs the way the API would: serialized, so the caller
// cannot change the fake's state.
func copyRuleset(rs *cf.Ruleset) cf.Ruleset {
	raw, err := json.Marshal(rs)
	Expect(err).NotTo(HaveOccurred())
	var out cf.Ruleset
	Expect(json.Unmarshal(raw, &out)).To(Succeed())
	return out
}

// entrypoint returns the fake's entry point for the test phase.
func (f *fakeRulesetAPI) entrypoint() *cf.Ruleset {
	rs, ok := f.rulesets[rulesTestPhase]
	Expect(ok).To(BeTrue(), "the zone has no entry point in the phase")
	return rs
}

// seed gives the zone an entry point in the test phase holding rules, as if
// they had been created through the API.
func (f *fakeRulesetAPI) seed(rules ...cf.RulesetRule) {
	rs := &cf.Ruleset{ID: f.newID(), Name: "default", Kind: "zone", Phase: rulesTestPhase}
	for _, rule := range rules {
		rs.Rules = append(rs.Rules, f.stored(rule, f.newID(), 1))
	}
	f.rulesets[rulesTestPhase] = rs
	f.bump(rs)
}

// blockRule is a rule as a kflare controller would ask for it: every field it
// wants set, Cloudflare's defaults left out.
func blockRule(ref, path string) cf.RulesetRule {
	return cf.RulesetRule{
		Ref:         ref,
		Action:      "block",
		Expression:  fmt.Sprintf("http.request.uri.path eq %q", path),
		Description: "block " + path,
	}
}

func refs(rules []cf.RulesetRule) []string {
	out := make([]string, len(rules))
	for i, rule := range rules {
		out[i] = rule.Ref
	}
	return out
}

func peer(ref string, priority int32, name string) rulesetRulePeer {
	return rulesetRulePeer{ref: ref, priority: priority, namespace: "default", name: name, kind: "WAFCustomRule"}
}

var _ = Describe("Ruleset rules", func() {
	var (
		ctx context.Context
		api *fakeRulesetAPI
	)

	BeforeEach(func() {
		ctx = context.Background()
		api = newFakeRulesetAPI()
	})

	sync := func(rule cf.RulesetRule, peers ...rulesetRulePeer) (rulesetRuleState, error) {
		return syncRulesetRule(ctx, api, rulesTestZone, rulesTestPhase, rule, peers)
	}

	Describe("refs", func() {
		It("derives a stable ref of at most 128 bytes from the kind, namespace and name", func() {
			ref := rulesetRuleRef("WAFCustomRule", "web", "block-admin")
			Expect(ref).To(MatchRegexp(`^kflare-[0-9a-f]{32}$`))
			Expect(len(ref)).To(BeNumerically("<=", 128))
			Expect(rulesetRuleRef("WAFCustomRule", "web", "block-admin")).To(Equal(ref))
		})

		It("gives every resource its own ref", func() {
			all := []string{
				rulesetRuleRef("WAFCustomRule", "web", "a"),
				rulesetRuleRef("WAFCustomRule", "api", "a"),
				rulesetRuleRef("WAFCustomRule", "web", "b"),
				rulesetRuleRef("RateLimitRule", "web", "a"),
			}
			Expect(slices.Compact(slices.Sorted(slices.Values(all)))).To(HaveLen(len(all)))
		})
	})

	Describe("syncing a rule", func() {
		It("creates the zone's entry point, holding the rule, when there is none", func() {
			state, err := sync(blockRule("kflare-a", "/a"))
			Expect(err).NotTo(HaveOccurred())

			rs := api.entrypoint()
			Expect(rs.Name).To(Equal("default"))
			Expect(rs.Kind).To(Equal("zone"))
			Expect(refs(rs.Rules)).To(Equal([]string{"kflare-a"}))
			Expect(state).To(Equal(rulesetRuleState{
				rulesetID: rs.ID, ruleID: rs.Rules[0].ID, version: "1", wrote: true,
			}))
		})

		It("adds the rule after the entry point's other rules and leaves them alone", func() {
			api.seed(blockRule("someone-else", "/x"))
			untouched := api.entrypoint().Rules[0]

			state, err := sync(blockRule("kflare-a", "/a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(state.wrote).To(BeTrue())

			rs := api.entrypoint()
			Expect(refs(rs.Rules)).To(Equal([]string{"someone-else", "kflare-a"}))
			Expect(rs.Rules[0]).To(Equal(untouched))
			Expect(state.ruleID).To(Equal(rs.Rules[1].ID))
		})

		It("writes nothing when the rule is already as desired, counting Cloudflare's defaults as set", func() {
			skip := cf.RulesetRule{
				Ref:              "kflare-skip",
				Action:           "skip",
				ActionParameters: &cf.RulesetRuleActionParameters{Ruleset: "current"},
				Expression:       `http.request.uri.path eq "/health"`,
			}
			first, err := sync(skip)
			Expect(err).NotTo(HaveOccurred())
			Expect(*api.entrypoint().Rules[0].Logging.Enabled).To(BeTrue(), "Cloudflare logs skip rules by default")
			writes, version := api.writes, *api.entrypoint().Version

			second, err := sync(skip)
			Expect(err).NotTo(HaveOccurred())
			Expect(second).To(Equal(rulesetRuleState{rulesetID: first.rulesetID, ruleID: first.ruleID, version: "1"}))
			Expect(api.writes).To(Equal(writes))
			Expect(*api.entrypoint().Version).To(Equal(version))
		})

		It("rewrites a rule changed outside kflare, keeping its ID and place", func() {
			api.seed(blockRule("someone-else", "/x"), blockRule("kflare-a", "/a"), blockRule("someone-else-2", "/y"))
			rs := api.entrypoint()
			ruleID := rs.Rules[1].ID
			rs.Rules[1].Expression = `http.request.uri.path eq "/changed"`
			rs.Rules[1].Enabled = cf.BoolPtr(false)

			state, err := sync(blockRule("kflare-a", "/a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(state.wrote).To(BeTrue())
			Expect(state.ruleID).To(Equal(ruleID))
			Expect(state.version).To(Equal("2"))

			rule := api.entrypoint().Rules[1]
			Expect(rule.ID).To(Equal(ruleID))
			Expect(rule.Expression).To(Equal(`http.request.uri.path eq "/a"`))
			Expect(*rule.Enabled).To(BeTrue())
		})

		It("adds its rule when another resource created the entry point first", func() {
			api.before = func(method string) {
				if method == "CreateRuleset" && api.rulesets[rulesTestPhase] == nil {
					api.seed(blockRule("kflare-other", "/other"))
				}
			}

			state, err := sync(blockRule("kflare-a", "/a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(state.wrote).To(BeTrue())
			Expect(refs(api.entrypoint().Rules)).To(Equal([]string{"kflare-other", "kflare-a"}))
		})

		It("tries once more when its rule disappears between the read and the write", func() {
			api.seed(blockRule("kflare-a", "/old"))
			api.before = func(method string) {
				if method == "UpdateRulesetRule" {
					rs := api.entrypoint()
					rs.Rules = nil
				}
			}

			state, err := sync(blockRule("kflare-a", "/a"))
			Expect(err).NotTo(HaveOccurred())
			Expect(state.wrote).To(BeTrue())
			Expect(api.entrypoint().Rules).To(HaveLen(1))
			Expect(api.entrypoint().Rules[0].Expression).To(Equal(`http.request.uri.path eq "/a"`))
		})

		It("reports Cloudflare's rejections", func() {
			api.maxRules = 1
			api.seed(blockRule("someone-else", "/x"))
			_, err := sync(blockRule("kflare-a", "/a"))
			Expect(cfpkg.HasErrorCode(err, 50001)).To(BeTrue(), "over the plan's rule quota")

			api.errs["GetEntrypointRuleset"] = forbidden()
			_, err = sync(blockRule("kflare-a", "/a"))
			Expect(cfpkg.IsTerminalError(err)).To(BeTrue())
		})

		It("reports a failure to create the entry point", func() {
			api.errs["CreateRuleset"] = cloudflareError(400, 20166)
			_, err := sync(blockRule("kflare-a", "/a"))
			Expect(cfpkg.HasErrorCode(err, 20166)).To(BeTrue())
		})

		It("reports a failed update", func() {
			api.seed(blockRule("kflare-a", "/old"))
			api.errs["UpdateRulesetRule"] = cloudflareError(400, 20023)
			_, err := sync(blockRule("kflare-a", "/a"))
			Expect(cfpkg.HasErrorCode(err, 20023)).To(BeTrue())
		})

		It("reports a rule that disappears after it was written", func() {
			api.seed(blockRule("kflare-b", "/b"), blockRule("kflare-a", "/a"), blockRule("kflare-c", "/c"))
			api.before = func(method string) {
				if method == "MoveRulesetRule" {
					// Someone deletes kflare's rule while it reorders the others.
					rs := api.entrypoint()
					rs.Rules = slices.DeleteFunc(rs.Rules, func(r cf.RulesetRule) bool { return r.Ref == "kflare-c" })
				}
			}

			_, err := sync(blockRule("kflare-c", "/c"),
				peer("kflare-a", 1, "a"), peer("kflare-b", 2, "b"), peer("kflare-c", 3, "c"))
			Expect(err).To(MatchError(ContainSubstring("disappeared")))
			Expect(api.moves).To(Equal(1))
		})
	})

	Describe("ordering", func() {
		It("places a new rule right after the closest peer that sorts before it", func() {
			api.seed(blockRule("kflare-a", "/a"), blockRule("someone-else", "/x"), blockRule("kflare-c", "/c"))

			state, err := sync(blockRule("kflare-b", "/b"),
				peer("kflare-a", 1, "a"), peer("kflare-b", 2, "b"), peer("kflare-c", 3, "c"))
			Expect(err).NotTo(HaveOccurred())
			Expect(state.moved).To(BeFalse())
			Expect(refs(api.entrypoint().Rules)).To(Equal([]string{"kflare-a", "kflare-b", "someone-else", "kflare-c"}))
		})

		It("places a new rule right before the closest peer that sorts after it when none sorts before", func() {
			api.seed(blockRule("someone-else", "/x"), blockRule("kflare-b", "/b"), blockRule("kflare-c", "/c"))

			_, err := sync(blockRule("kflare-a", "/a"),
				peer("kflare-a", 1, "a"), peer("kflare-b", 2, "b"), peer("kflare-c", 3, "c"))
			Expect(err).NotTo(HaveOccurred())
			Expect(refs(api.entrypoint().Rules)).To(Equal([]string{"someone-else", "kflare-a", "kflare-b", "kflare-c"}))
		})

		It("appends a rule when none of its peers' rules exists yet", func() {
			api.seed(blockRule("someone-else", "/x"))

			_, err := sync(blockRule("kflare-b", "/b"), peer("kflare-a", 1, "a"), peer("kflare-b", 2, "b"))
			Expect(err).NotTo(HaveOccurred())
			Expect(refs(api.entrypoint().Rules)).To(Equal([]string{"someone-else", "kflare-b"}))
		})

		It("moves peers into priority order without changing them, and leaves other rules in place", func() {
			api.seed(
				blockRule("someone-else", "/x"),
				blockRule("kflare-c", "/c"),
				blockRule("someone-else-2", "/y"),
				blockRule("kflare-a", "/a"),
				blockRule("kflare-b", "/b"),
			)
			before := map[string]cf.RulesetRule{}
			for _, rule := range api.entrypoint().Rules {
				before[rule.Ref] = rule
			}

			state, err := sync(blockRule("kflare-a", "/a"),
				peer("kflare-c", 3, "c"), peer("kflare-a", 1, "a"), peer("kflare-b", 2, "b"))
			Expect(err).NotTo(HaveOccurred())
			Expect(state.wrote).To(BeFalse())
			Expect(state.moved).To(BeTrue())
			Expect(api.moves).To(Equal(1))
			Expect(refs(api.entrypoint().Rules)).To(Equal(
				[]string{"someone-else", "someone-else-2", "kflare-a", "kflare-b", "kflare-c"}))
			for _, rule := range api.entrypoint().Rules {
				Expect(rule).To(Equal(before[rule.Ref]), "a move must not change rule %s", rule.Ref)
			}

			By("leaving rules that are already in order alone")
			again, err := sync(blockRule("kflare-a", "/a"),
				peer("kflare-c", 3, "c"), peer("kflare-a", 1, "a"), peer("kflare-b", 2, "b"))
			Expect(err).NotTo(HaveOccurred())
			Expect(again.moved).To(BeFalse())
			Expect(api.moves).To(Equal(1))
		})

		It("breaks priority ties by namespace, name and kind", func() {
			api.seed(
				blockRule("kflare-4", "/4"), blockRule("kflare-3", "/3"),
				blockRule("kflare-2", "/2"), blockRule("kflare-1", "/1"),
			)
			peers := []rulesetRulePeer{
				{ref: "kflare-4", priority: 5, namespace: "b", name: "a", kind: "WAFCustomRule"},
				{ref: "kflare-3", priority: 5, namespace: "a", name: "b", kind: "WAFCustomRule"},
				{ref: "kflare-2", priority: 5, namespace: "a", name: "a", kind: "WAFCustomRule"},
				{ref: "kflare-1", priority: 5, namespace: "a", name: "a", kind: "RateLimitRule"},
			}

			_, err := sync(blockRule("kflare-1", "/1"), peers...)
			Expect(err).NotTo(HaveOccurred())
			Expect(refs(api.entrypoint().Rules)).To(Equal([]string{"kflare-1", "kflare-2", "kflare-3", "kflare-4"}))
		})

		It("stops, and says so, when a rule disappears while it reorders", func() {
			api.seed(blockRule("kflare-b", "/b"), blockRule("kflare-a", "/a"))
			api.before = func(method string) {
				if method == "MoveRulesetRule" {
					rs := api.entrypoint()
					rs.Rules = slices.DeleteFunc(rs.Rules, func(r cf.RulesetRule) bool { return r.Ref == "kflare-b" })
				}
			}

			state, err := sync(blockRule("kflare-a", "/a"), peer("kflare-a", 1, "a"), peer("kflare-b", 2, "b"))
			Expect(err).NotTo(HaveOccurred())
			Expect(state.moved).To(BeTrue(), "the caller must check the order again")
			Expect(refs(api.entrypoint().Rules)).To(Equal([]string{"kflare-a"}))
		})

		It("reports a failed move", func() {
			api.seed(blockRule("kflare-b", "/b"), blockRule("kflare-a", "/a"))
			api.errs["MoveRulesetRule"] = cloudflareError(400, 0)

			_, err := sync(blockRule("kflare-a", "/a"), peer("kflare-a", 1, "a"), peer("kflare-b", 2, "b"))
			Expect(cfpkg.IsTerminalError(err)).To(BeTrue())
		})
	})

	Describe("deleting a rule", func() {
		del := func(ref string) error {
			return deleteRulesetRule(ctx, api, rulesTestZone, rulesTestPhase, ref)
		}

		It("deletes the rule with its ref and leaves the others", func() {
			api.seed(blockRule("someone-else", "/x"), blockRule("kflare-a", "/a"))
			Expect(del("kflare-a")).To(Succeed())
			Expect(refs(api.entrypoint().Rules)).To(Equal([]string{"someone-else"}))
		})

		It("leaves the entry point in place when it deletes the last rule", func() {
			api.seed(blockRule("kflare-a", "/a"))
			Expect(del("kflare-a")).To(Succeed())
			Expect(api.entrypoint().Rules).To(BeEmpty())
		})

		It("counts a missing entry point, rule or concurrently deleted rule as deleted", func() {
			Expect(del("kflare-a")).To(Succeed())

			api.seed(blockRule("someone-else", "/x"))
			Expect(del("kflare-a")).To(Succeed())
			Expect(api.writes).To(BeZero())

			api.seed(blockRule("kflare-a", "/a"))
			api.before = func(method string) {
				if method == "DeleteRulesetRule" {
					api.entrypoint().Rules = nil
				}
			}
			Expect(del("kflare-a")).To(Succeed())
		})

		It("reports other errors", func() {
			api.errs["GetEntrypointRuleset"] = forbidden()
			Expect(cfpkg.IsTerminalError(del("kflare-a"))).To(BeTrue())

			api.errs["GetEntrypointRuleset"] = nil
			api.seed(blockRule("kflare-a", "/a"))
			api.errs["DeleteRulesetRule"] = cloudflareError(400, 0)
			Expect(cfpkg.IsTerminalError(del("kflare-a"))).To(BeTrue())
		})
	})

	DescribeTable("comparing a desired rule with Cloudflare's",
		func(desired, actual cf.RulesetRule, same bool) {
			Expect(rulesetRuleMatches(desired, actual)).To(Equal(same))
		},
		Entry("ignores the fields Cloudflare sets",
			blockRule("kflare-a", "/a"),
			withServerFields(enabled(blockRule("kflare-a", "/a"), true)), true),
		Entry("counts an unset enabled as enabled",
			blockRule("kflare-a", "/a"), enabled(blockRule("kflare-a", "/a"), true), true),
		Entry("tells a disabled rule apart",
			enabled(blockRule("kflare-a", "/a"), true), enabled(blockRule("kflare-a", "/a"), false), false),
		Entry("counts an unset skip logging as logged",
			skipRule(nil), enabled(skipRule(&cf.RulesetRuleLogging{Enabled: cf.BoolPtr(true)}), true), true),
		Entry("tells a skip rule that does not log apart",
			skipRule(nil), skipRule(&cf.RulesetRuleLogging{Enabled: cf.BoolPtr(false)}), false),
		Entry("tells a different expression apart",
			blockRule("kflare-a", "/a"), blockRule("kflare-a", "/b"), false),
		Entry("tells a different description apart",
			blockRule("kflare-a", "/a"), withDescription(blockRule("kflare-a", "/a"), ""), false),
		Entry("counts empty action parameters as none",
			withParameters(blockRule("kflare-a", "/a"), &cf.RulesetRuleActionParameters{}),
			blockRule("kflare-a", "/a"), true),
		Entry("tells different action parameters apart",
			withParameters(skipRule(nil), &cf.RulesetRuleActionParameters{Phases: []string{"http_ratelimit"}}),
			skipRule(nil), false),
	)
})

func enabled(rule cf.RulesetRule, on bool) cf.RulesetRule {
	rule.Enabled = cf.BoolPtr(on)
	return rule
}

func withServerFields(rule cf.RulesetRule) cf.RulesetRule {
	version := "4"
	updated := time.Date(2026, 10, 12, 19, 0, 0, 0, time.UTC)
	rule.ID, rule.Version, rule.LastUpdated = "id01", &version, &updated
	return rule
}

func withDescription(rule cf.RulesetRule, description string) cf.RulesetRule {
	rule.Description = description
	return rule
}

func withParameters(rule cf.RulesetRule, parameters *cf.RulesetRuleActionParameters) cf.RulesetRule {
	rule.ActionParameters = parameters
	return rule
}

func skipRule(logging *cf.RulesetRuleLogging) cf.RulesetRule {
	return cf.RulesetRule{
		Ref:              "kflare-skip",
		Action:           "skip",
		ActionParameters: &cf.RulesetRuleActionParameters{Ruleset: "current"},
		Expression:       `http.request.uri.path eq "/health"`,
		Logging:          logging,
	}
}

// forbidden is the error cloudflare-go returns for a 403: a token without the
// permission the call needs.
func forbidden() error {
	v := cf.NewAuthenticationError(&cf.Error{StatusCode: 403, Type: cf.ErrorTypeAuthentication})
	return &v
}
