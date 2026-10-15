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

// RateLimitRuleReconciler reconciles a RateLimitRule object: one rule in its
// zone's rate limiting rules, Cloudflare's http_ratelimit phase. The work is
// shared with every rule kind (rulesetrule_controller.go); this file holds
// what is specific to rate limiting rules.
type RateLimitRuleReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// NewRulesetRuleAPI constructs a RulesetRuleAPI from a raw API token.
	// Defaults to defaultRulesetRuleAPI; overridden in tests to inject a fake.
	NewRulesetRuleAPI func(token string) (RulesetRuleAPI, error)
}

//+kubebuilder:rbac:groups=kflare.dev,resources=ratelimitrules,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kflare.dev,resources=ratelimitrules/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=kflare.dev,resources=ratelimitrules/finalizers,verbs=update
//+kubebuilder:rbac:groups=kflare.dev,resources=zones,verbs=get;list;watch
//+kubebuilder:rbac:groups=kflare.dev,resources=cloudflareaccounts,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// rateLimitColoCharacteristic counts requests per Cloudflare data center.
// Cloudflare counts every rate limiting rule that way and requires the rule
// to say so (code 20155), so kflare adds it to every rule.
const rateLimitColoCharacteristic = "cf.colo.id"

// rateLimitRuleKind describes RateLimitRule to the shared rule reconciler.
var rateLimitRuleKind = rulesetRuleKind{
	name:      "RateLimitRule",
	phases:    []rulesets.Phase{rulesets.PhaseHTTPRatelimit},
	newObject: func() rulesetRuleObject { return &cloudflarev1alpha1.RateLimitRule{} },
	newList:   func() client.ObjectList { return &cloudflarev1alpha1.RateLimitRuleList{} },
	definition: func(obj rulesetRuleObject) rulesets.RulesetNewParamsRule {
		return rateLimitRuleDefinition(obj.(*cloudflarev1alpha1.RateLimitRule))
	},
}

func (r *RateLimitRuleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	return r.rules().reconcile(ctx, req)
}

// SetupWithManager registers RateLimitRuleReconciler with the manager. It
// watches Zones, so a rule waiting for its Zone is written as soon as the
// Zone is ready.
func (r *RateLimitRuleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return r.rules().setup(mgr, r)
}

// rules returns the shared rule reconciler for RateLimitRules.
func (r *RateLimitRuleReconciler) rules() *rulesetRuleReconciler {
	newAPI := r.NewRulesetRuleAPI
	if newAPI == nil {
		newAPI = defaultRulesetRuleAPI
	}
	return &rulesetRuleReconciler{Client: r.Client, kind: rateLimitRuleKind, newAPI: newAPI}
}

// rateLimitRuleDefinition is the Cloudflare rule a RateLimitRule asks for.
//
// Cloudflare keeps the characteristics in the order it is sent them, so
// cf.colo.id goes first and the others are sorted: reordering them in the
// spec changes nothing. The mitigation timeout defaults to the period, never
// to Cloudflare's own default: a rule without one throttles, which only
// Enterprise plans may do. An explicit 0 asks for exactly that, and is left
// out of the request, which Cloudflare reads as 0. An empty counting
// expression and requests to origin when false are left out too, as Cloudflare
// returns them (verified live).
//
// v7 has a rate limit type for each action, all alike; the block rule's
// serves every action.
func rateLimitRuleDefinition(rule *cloudflarev1alpha1.RateLimitRule) rulesets.RulesetNewParamsRule {
	spec := rule.Spec
	characteristics := []string{rateLimitColoCharacteristic}
	for _, characteristic := range spec.Characteristics {
		if characteristic != rateLimitColoCharacteristic {
			characteristics = append(characteristics, string(characteristic))
		}
	}
	slices.Sort(characteristics[1:])
	mitigationTimeout := spec.Period
	if spec.MitigationTimeout != nil {
		mitigationTimeout = *spec.MitigationTimeout
	}
	var ratelimit rulesets.BlockRuleRatelimitParam
	ratelimit.Characteristics.Value, ratelimit.Characteristics.Present = characteristics, true
	ratelimit.Period.Value, ratelimit.Period.Present = int64(spec.Period), true
	ratelimit.RequestsPerPeriod.Value, ratelimit.RequestsPerPeriod.Present = int64(spec.RequestsPerPeriod), true
	if mitigationTimeout != 0 {
		ratelimit.MitigationTimeout.Value, ratelimit.MitigationTimeout.Present = int64(mitigationTimeout), true
	}
	if spec.CountingExpression != "" {
		ratelimit.CountingExpression.Value, ratelimit.CountingExpression.Present = spec.CountingExpression, true
	}
	if spec.RequestsToOrigin {
		ratelimit.RequestsToOrigin.Value, ratelimit.RequestsToOrigin.Present = true, true
	}

	definition := rulesetRuleDefinition(rulesets.RulesetNewParamsRulesAction(spec.Action), spec.RulesetRuleSpec)
	definition.Ratelimit.Value, definition.Ratelimit.Present = ratelimit, true
	if response := spec.BlockResponse; response != nil {
		definition.ActionParameters.Value = blockResponseParameters(response.StatusCode, response.ContentType,
			response.Content)
		definition.ActionParameters.Present = true
	}
	return definition
}
