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
	phase:     string(cf.RulesetPhaseHTTPRatelimit),
	newObject: func() rulesetRuleObject { return &cloudflarev1alpha1.RateLimitRule{} },
	newList:   func() client.ObjectList { return &cloudflarev1alpha1.RateLimitRuleList{} },
	definition: func(obj rulesetRuleObject) cf.RulesetRule {
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
// Enterprise plans may do. An explicit 0 asks for exactly that, and
// cloudflare-go leaves a 0 out of the request.
func rateLimitRuleDefinition(rule *cloudflarev1alpha1.RateLimitRule) cf.RulesetRule {
	spec := rule.Spec
	var characteristics []string
	for _, characteristic := range spec.Characteristics {
		if characteristic != rateLimitColoCharacteristic {
			characteristics = append(characteristics, string(characteristic))
		}
	}
	slices.Sort(characteristics)
	mitigationTimeout := spec.Period
	if spec.MitigationTimeout != nil {
		mitigationTimeout = *spec.MitigationTimeout
	}
	definition := cf.RulesetRule{
		Action:      spec.Action,
		Expression:  spec.Expression,
		Description: spec.Description,
		Enabled:     cf.BoolPtr(spec.Enabled == nil || *spec.Enabled),
		RateLimit: &cf.RulesetRuleRateLimit{
			Characteristics:    append([]string{rateLimitColoCharacteristic}, characteristics...),
			Period:             int(spec.Period),
			RequestsPerPeriod:  int(spec.RequestsPerPeriod),
			MitigationTimeout:  int(mitigationTimeout),
			CountingExpression: spec.CountingExpression,
			RequestsToOrigin:   spec.RequestsToOrigin,
		},
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
