/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"slices"

	"github.com/cloudflare/cloudflare-go/v7/rulesets"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
)

// WAFCustomRuleReconciler reconciles a WAFCustomRule object: one rule in its
// zone's WAF custom rules, Cloudflare's http_request_firewall_custom phase.
// The work is shared with every rule kind (rulesetrule_controller.go); this
// file holds what is specific to WAF custom rules.
type WAFCustomRuleReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// NewRulesetRuleAPI constructs a RulesetRuleAPI from a raw API token.
	// Defaults to defaultRulesetRuleAPI; overridden in tests to inject a fake.
	NewRulesetRuleAPI func(token string) (RulesetRuleAPI, error)
}

//+kubebuilder:rbac:groups=kflare.dev,resources=wafcustomrules,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kflare.dev,resources=wafcustomrules/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=kflare.dev,resources=wafcustomrules/finalizers,verbs=update
//+kubebuilder:rbac:groups=kflare.dev,resources=zones,verbs=get;list;watch
//+kubebuilder:rbac:groups=kflare.dev,resources=cloudflareaccounts,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// wafCustomRuleKind describes WAFCustomRule to the shared rule reconciler.
var wafCustomRuleKind = rulesetRuleKind{
	name:      "WAFCustomRule",
	phase:     rulesets.PhaseHTTPRequestFirewallCustom,
	newObject: func() rulesetRuleObject { return &cloudflarev1alpha1.WAFCustomRule{} },
	newList:   func() client.ObjectList { return &cloudflarev1alpha1.WAFCustomRuleList{} },
	definition: func(obj rulesetRuleObject) rulesets.RulesetNewParamsRule {
		return wafCustomRuleDefinition(obj.(*cloudflarev1alpha1.WAFCustomRule))
	},
}

func (r *WAFCustomRuleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return r.rules().reconcile(ctx, req)
}

// SetupWithManager registers WAFCustomRuleReconciler with the manager. It
// watches Zones, so a rule waiting for its Zone is written as soon as the
// Zone is ready.
func (r *WAFCustomRuleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return r.rules().setup(mgr, r)
}

// rules returns the shared rule reconciler for WAFCustomRules.
func (r *WAFCustomRuleReconciler) rules() *rulesetRuleReconciler {
	newAPI := r.NewRulesetRuleAPI
	if newAPI == nil {
		newAPI = defaultRulesetRuleAPI
	}
	return &rulesetRuleReconciler{Client: r.Client, kind: wafCustomRuleKind, newAPI: newAPI}
}

// wafCustomRuleDefinition is the Cloudflare rule a WAFCustomRule asks for.
// Cloudflare keeps the skipped phases and products in the order it is sent
// them, so they are sorted: reordering them in the spec changes nothing.
func wafCustomRuleDefinition(rule *cloudflarev1alpha1.WAFCustomRule) rulesets.RulesetNewParamsRule {
	spec := rule.Spec
	definition := rulesetRuleDefinition(rulesets.RulesetNewParamsRulesAction(spec.Action), spec.RulesetRuleSpec)
	if skip := spec.Skip; skip != nil {
		var params rulesets.SkipRuleActionParametersParam
		if skip.RemainingRules {
			params.Ruleset.Value, params.Ruleset.Present = rulesets.SkipRuleActionParametersRulesetCurrent, true
		}
		if len(skip.Phases) > 0 {
			phases := make([]rulesets.Phase, 0, len(skip.Phases))
			for _, phase := range skip.Phases {
				phases = append(phases, rulesets.Phase(phase))
			}
			slices.Sort(phases)
			params.Phases.Value, params.Phases.Present = phases, true
		}
		if len(skip.Products) > 0 {
			products := make([]rulesets.SkipRuleActionParametersProduct, 0, len(skip.Products))
			for _, product := range skip.Products {
				products = append(products, rulesets.SkipRuleActionParametersProduct(product))
			}
			slices.Sort(products)
			params.Products.Value, params.Products.Present = products, true
		}
		definition.ActionParameters.Value, definition.ActionParameters.Present = params, true
		// Only a skip rule may set logging (Cloudflare refuses it on other
		// actions, code 20018).
		var logging rulesets.LoggingParam
		logging.Enabled.Value, logging.Enabled.Present = skip.Log == nil || *skip.Log, true
		definition.Logging.Value, definition.Logging.Present = logging, true
	}
	if response := spec.BlockResponse; response != nil {
		definition.ActionParameters.Value = blockResponseParameters(response.StatusCode, response.ContentType,
			response.Content)
		definition.ActionParameters.Present = true
	}
	return definition
}
