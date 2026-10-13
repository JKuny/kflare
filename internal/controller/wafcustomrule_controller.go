/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"slices"

	cf "github.com/cloudflare/cloudflare-go"
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
	phase:     string(cf.RulesetPhaseHTTPRequestFirewallCustom),
	newObject: func() rulesetRuleObject { return &cloudflarev1alpha1.WAFCustomRule{} },
	newList:   func() client.ObjectList { return &cloudflarev1alpha1.WAFCustomRuleList{} },
	definition: func(obj rulesetRuleObject) cf.RulesetRule {
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
func wafCustomRuleDefinition(rule *cloudflarev1alpha1.WAFCustomRule) cf.RulesetRule {
	spec := rule.Spec
	definition := cf.RulesetRule{
		Action:      spec.Action,
		Expression:  spec.Expression,
		Description: spec.Description,
		Enabled:     cf.BoolPtr(spec.Enabled == nil || *spec.Enabled),
	}
	if skip := spec.Skip; skip != nil {
		params := &cf.RulesetRuleActionParameters{}
		if skip.RemainingRules {
			params.Ruleset = "current"
		}
		for _, phase := range skip.Phases {
			params.Phases = append(params.Phases, string(phase))
		}
		for _, product := range skip.Products {
			params.Products = append(params.Products, string(product))
		}
		slices.Sort(params.Phases)
		slices.Sort(params.Products)
		definition.ActionParameters = params
		// Only a skip rule may set logging (Cloudflare refuses it on other
		// actions, code 20018).
		definition.Logging = &cf.RulesetRuleLogging{Enabled: cf.BoolPtr(skip.Log == nil || *skip.Log)}
	}
	if response := spec.BlockResponse; response != nil {
		definition.ActionParameters = &cf.RulesetRuleActionParameters{
			Response: &cf.RulesetRuleActionParametersBlockResponse{
				StatusCode:  uint16(response.StatusCode),
				ContentType: response.ContentType,
				Content:     response.Content,
			},
		}
	}
	return definition
}
