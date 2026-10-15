/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"slices"
	"strings"

	"github.com/cloudflare/cloudflare-go/v7/rulesets"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
)

// ManagedRulesetReconciler reconciles a ManagedRuleset object: the execute
// rule that deploys a Cloudflare managed ruleset in its zone's
// http_request_firewall_managed phase. The work is shared with every rule
// kind (rulesetrule_controller.go); this file holds what is specific to
// managed ruleset deployments.
//
// Cloudflare deploys its managed rulesets by default only while a zone has no
// entry point in the phase (verified live): the first rule written there,
// whatever it is, replaces that default with the rules' own deployments. The
// foundation therefore deletes the phase's entry point once deleting a rule
// has emptied it (emptiedEntrypointPhases), which brings the default back.
type ManagedRulesetReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// NewRulesetRuleAPI constructs a RulesetRuleAPI from a raw API token.
	// Defaults to defaultRulesetRuleAPI; overridden in tests to inject a fake.
	NewRulesetRuleAPI func(token string) (RulesetRuleAPI, error)
}

//+kubebuilder:rbac:groups=kflare.dev,resources=managedrulesets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kflare.dev,resources=managedrulesets/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=kflare.dev,resources=managedrulesets/finalizers,verbs=update
//+kubebuilder:rbac:groups=kflare.dev,resources=zones,verbs=get;list;watch
//+kubebuilder:rbac:groups=kflare.dev,resources=cloudflareaccounts,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// managedRulesetConflictCode is the Cloudflare error code (with HTTP 400) for
// a second execute rule for a managed ruleset the zone already deploys.
const managedRulesetConflictCode = 20014

// managedRulesetKind describes ManagedRuleset to the shared rule reconciler.
// A zone deploys a managed ruleset at most once, so a deployment whose ruleset
// another rule already deploys reports RulesetConflict and checks again every
// minute: the other rule may be another ManagedRuleset's that is going, for
// example when a ManagedRuleset is replaced by one with another name.
var managedRulesetKind = rulesetRuleKind{
	name:      "ManagedRuleset",
	phase:     rulesets.PhaseHTTPRequestFirewallManaged,
	newObject: func() rulesetRuleObject { return &cloudflarev1alpha1.ManagedRuleset{} },
	newList:   func() client.ObjectList { return &cloudflarev1alpha1.ManagedRulesetList{} },
	definition: func(obj rulesetRuleObject) rulesets.RulesetNewParamsRule {
		return managedRulesetDefinition(obj.(*cloudflarev1alpha1.ManagedRuleset))
	},
	conflicts: map[int]string{managedRulesetConflictCode: "RulesetConflict"},
}

func (r *ManagedRulesetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return r.rules().reconcile(ctx, req)
}

// SetupWithManager registers ManagedRulesetReconciler with the manager. It
// watches Zones, so a deployment waiting for its Zone is written as soon as
// the Zone is ready, and the deletion of ManagedRulesets, so a deployment
// that lost its rule when another emptied the phase is written again.
func (r *ManagedRulesetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return r.rules().setup(mgr, r)
}

// rules returns the shared rule reconciler for ManagedRulesets.
func (r *ManagedRulesetReconciler) rules() *rulesetRuleReconciler {
	newAPI := r.NewRulesetRuleAPI
	if newAPI == nil {
		newAPI = defaultRulesetRuleAPI
	}
	return &rulesetRuleReconciler{Client: r.Client, kind: managedRulesetKind, newAPI: newAPI}
}

// managedRulesetDefinition is the Cloudflare rule a ManagedRuleset asks for:
// an execute rule for its managed ruleset, with its overrides. The version is
// left out, which Cloudflare records as the latest; it refuses any other.
func managedRulesetDefinition(deployment *cloudflarev1alpha1.ManagedRuleset) rulesets.RulesetNewParamsRule {
	spec := deployment.Spec
	var params rulesets.ExecuteRuleActionParametersParam
	params.ID.Value, params.ID.Present = spec.RulesetID, true
	if spec.Overrides != nil {
		params.Overrides.Value, params.Overrides.Present = managedRulesetOverrides(*spec.Overrides), true
	}
	definition := rulesetRuleDefinition(rulesets.RulesetNewParamsRulesActionExecute, spec.RulesetRuleSpec)
	definition.ActionParameters.Value, definition.ActionParameters.Present = params, true
	return definition
}

// managedRulesetOverrides are a deployment's overrides as Cloudflare takes
// them. Cloudflare keeps the category and rule overrides in the order it is
// sent them, so they are sorted: reordering them in the spec changes nothing.
// An override's enabled and action are sent only when set.
func managedRulesetOverrides(
	overrides cloudflarev1alpha1.ManagedRulesetOverrides,
) rulesets.ExecuteRuleActionParametersOverridesParam {
	var params rulesets.ExecuteRuleActionParametersOverridesParam
	if overrides.Enabled != nil {
		params.Enabled.Value, params.Enabled.Present = *overrides.Enabled, true
	}
	if overrides.Action != "" {
		params.Action.Value, params.Action.Present = string(overrides.Action), true
	}

	sortedCategories := slices.SortedFunc(slices.Values(overrides.Categories),
		func(a, b cloudflarev1alpha1.ManagedRulesetCategoryOverride) int {
			return strings.Compare(a.Category, b.Category)
		})
	categories := make([]rulesets.ExecuteRuleActionParametersOverridesCategoryParam, 0, len(sortedCategories))
	for _, override := range sortedCategories {
		var category rulesets.ExecuteRuleActionParametersOverridesCategoryParam
		category.Category.Value, category.Category.Present = override.Category, true
		if override.Enabled != nil {
			category.Enabled.Value, category.Enabled.Present = *override.Enabled, true
		}
		if override.Action != "" {
			category.Action.Value, category.Action.Present = string(override.Action), true
		}
		categories = append(categories, category)
	}
	if len(categories) > 0 {
		params.Categories.Value, params.Categories.Present = categories, true
	}

	sortedRules := slices.SortedFunc(slices.Values(overrides.Rules),
		func(a, b cloudflarev1alpha1.ManagedRulesetRuleOverride) int { return strings.Compare(a.ID, b.ID) })
	rules := make([]rulesets.ExecuteRuleActionParametersOverridesRuleParam, 0, len(sortedRules))
	for _, override := range sortedRules {
		var rule rulesets.ExecuteRuleActionParametersOverridesRuleParam
		rule.ID.Value, rule.ID.Present = override.ID, true
		if override.Enabled != nil {
			rule.Enabled.Value, rule.Enabled.Present = *override.Enabled, true
		}
		if override.Action != "" {
			rule.Action.Value, rule.Action.Present = string(override.Action), true
		}
		rules = append(rules, rule)
	}
	if len(rules) > 0 {
		params.Rules.Value, params.Rules.Present = rules, true
	}
	return params
}
