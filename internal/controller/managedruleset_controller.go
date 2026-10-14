/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"slices"
	"strings"

	cf "github.com/cloudflare/cloudflare-go"
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
	phase:     string(cf.RulesetPhaseHTTPRequestFirewallManaged),
	newObject: func() rulesetRuleObject { return &cloudflarev1alpha1.ManagedRuleset{} },
	newList:   func() client.ObjectList { return &cloudflarev1alpha1.ManagedRulesetList{} },
	definition: func(obj rulesetRuleObject) cf.RulesetRule {
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
// an execute rule for its managed ruleset, with its overrides. Cloudflare
// keeps the category and rule overrides in the order it is sent them, so they
// are sorted: reordering them in the spec changes nothing. The version is
// left out, which Cloudflare records as the latest; it refuses any other.
func managedRulesetDefinition(deployment *cloudflarev1alpha1.ManagedRuleset) cf.RulesetRule {
	spec := deployment.Spec
	params := &cf.RulesetRuleActionParameters{ID: spec.RulesetID}
	if overrides := spec.Overrides; overrides != nil {
		params.Overrides = &cf.RulesetRuleActionParametersOverrides{
			Enabled: overrides.Enabled,
			Action:  string(overrides.Action),
		}
		for _, category := range overrides.Categories {
			params.Overrides.Categories = append(params.Overrides.Categories, cf.RulesetRuleActionParametersCategories{
				Category: category.Category,
				Action:   string(category.Action),
				Enabled:  category.Enabled,
			})
		}
		slices.SortFunc(params.Overrides.Categories, func(a, b cf.RulesetRuleActionParametersCategories) int {
			return strings.Compare(a.Category, b.Category)
		})
		for _, rule := range overrides.Rules {
			params.Overrides.Rules = append(params.Overrides.Rules, cf.RulesetRuleActionParametersRules{
				ID:      rule.ID,
				Action:  string(rule.Action),
				Enabled: rule.Enabled,
			})
		}
		slices.SortFunc(params.Overrides.Rules, func(a, b cf.RulesetRuleActionParametersRules) int {
			return strings.Compare(a.ID, b.ID)
		})
	}
	return cf.RulesetRule{
		Action:           string(cf.RulesetRuleActionExecute),
		ActionParameters: params,
		Expression:       spec.Expression,
		Description:      spec.Description,
		Enabled:          cf.BoolPtr(spec.Enabled == nil || *spec.Enabled),
	}
}
