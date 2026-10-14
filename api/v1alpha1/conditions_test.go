/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestConditionAccessors(t *testing.T) {
	objects := map[string]interface {
		GetConditions() []metav1.Condition
		SetConditions([]metav1.Condition)
	}{
		"CloudflareAccount":   &CloudflareAccount{},
		"Zone":                &Zone{},
		"DNSRecord":           &DNSRecord{},
		"Tunnel":              &Tunnel{},
		"TunnelConfiguration": &TunnelConfiguration{},
		"WorkerScript":        &WorkerScript{},
		"WorkerRoute":         &WorkerRoute{},
		"KVNamespace":         &KVNamespace{},
		"R2Bucket":            &R2Bucket{},
		"WAFCustomRule":       &WAFCustomRule{},
		"RateLimitRule":       &RateLimitRule{},
		"IPList":              &IPList{},
		"ManagedRuleset":      &ManagedRuleset{},
	}
	want := []metav1.Condition{{Type: ConditionReady, Status: metav1.ConditionTrue, Reason: "Synced"}}
	for kind, obj := range objects {
		obj.SetConditions(want)
		got := obj.GetConditions()
		if len(got) != 1 || got[0] != want[0] {
			t.Errorf("%s: GetConditions() = %v, want %v", kind, got, want)
		}
	}
}

func TestRulesetRuleAccessors(t *testing.T) {
	waf, rateLimit, managed := &WAFCustomRule{}, &RateLimitRule{}, &ManagedRuleset{}
	rules := map[string]struct {
		rule interface {
			GetRulesetRuleSpec() *RulesetRuleSpec
			GetRulesetRuleStatus() *RulesetRuleStatus
		}
		spec   *RulesetRuleSpec
		status *RulesetRuleStatus
	}{
		"WAFCustomRule":  {waf, &waf.Spec.RulesetRuleSpec, &waf.Status},
		"RateLimitRule":  {rateLimit, &rateLimit.Spec.RulesetRuleSpec, &rateLimit.Status},
		"ManagedRuleset": {managed, &managed.Spec.RulesetRuleSpec, &managed.Status},
	}
	for kind, r := range rules {
		if r.rule.GetRulesetRuleSpec() != r.spec || r.rule.GetRulesetRuleStatus() != r.status {
			t.Errorf("%s: the accessors do not return the rule's own spec and status", kind)
		}
	}
}
