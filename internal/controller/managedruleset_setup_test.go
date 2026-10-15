/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"encoding/json"
	"net/http"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
)

var _ = Describe("ManagedRulesetReconciler SetupWithManager", func() {
	It("registers without error", func() {
		mgr, err := manager.New(cfg, manager.Options{})
		Expect(err).NotTo(HaveOccurred())
		r := &ManagedRulesetReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}
		Expect(r.SetupWithManager(mgr)).To(Succeed())
	})

	It("falls back to the production client when no factory is injected", func() {
		_, err := (&ManagedRulesetReconciler{}).rules().newAPI("")
		Expect(err).To(HaveOccurred(), "the production client rejects an empty token")
	})

	It("retries a ruleset deployed twice, and fails on other errors", func() {
		reason, ok := managedRulesetKind.conflict(rulesetsError(http.StatusBadRequest, managedRulesetConflictCode))
		Expect(ok).To(BeTrue())
		Expect(reason).To(Equal("RulesetConflict"))

		_, ok = managedRulesetKind.conflict(rulesetsError(http.StatusBadRequest, 20226))
		Expect(ok).To(BeFalse())
		_, ok = managedRulesetKind.conflict(nil)
		Expect(ok).To(BeFalse())
		_, ok = wafCustomRuleKind.conflict(rulesetsError(http.StatusBadRequest, managedRulesetConflictCode))
		Expect(ok).To(BeFalse(), "only kinds that list a code retry on it")
	})
})

var _ = DescribeTable("the Cloudflare rule a ManagedRuleset asks for",
	func(spec cloudflarev1alpha1.ManagedRulesetSpec, want string) {
		spec.Expression = "true"
		spec.RulesetID = freeManagedRulesetID
		Expect(json.Marshal(managedRulesetDefinition(&cloudflarev1alpha1.ManagedRuleset{Spec: spec}))).To(MatchJSON(want))
	},
	Entry("the ruleset as Cloudflare ships it, enabled by default",
		cloudflarev1alpha1.ManagedRulesetSpec{},
		`{"action":"execute","expression":"true","enabled":true,
		  "action_parameters":{"id":"77454fe2d30c4220b5701f6fdfb893ba"}}`),
	Entry("a disabled deployment with a description",
		cloudflarev1alpha1.ManagedRulesetSpec{
			RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{Enabled: new(false), Description: "managed rules"},
		},
		`{"action":"execute","expression":"true","description":"managed rules","enabled":false,
		  "action_parameters":{"id":"77454fe2d30c4220b5701f6fdfb893ba"}}`),
	Entry("overrides for the whole ruleset",
		cloudflarev1alpha1.ManagedRulesetSpec{
			Overrides: &cloudflarev1alpha1.ManagedRulesetOverrides{Enabled: new(false), Action: "log"},
		},
		`{"action":"execute","expression":"true","enabled":true,
		  "action_parameters":{"id":"77454fe2d30c4220b5701f6fdfb893ba","overrides":{"enabled":false,"action":"log"}}}`),
	Entry("category and rule overrides, sorted by category and rule ID",
		cloudflarev1alpha1.ManagedRulesetSpec{
			Overrides: &cloudflarev1alpha1.ManagedRulesetOverrides{
				Categories: []cloudflarev1alpha1.ManagedRulesetCategoryOverride{
					{Category: "wordpress", Enabled: new(false)},
					{Category: "log4j", Action: "managed_challenge"},
				},
				Rules: []cloudflarev1alpha1.ManagedRulesetRuleOverride{
					{ID: "9f7e454e4cad45abbcc40696037f70df", Action: "log"},
					{ID: "7dfd111a6bad4b86bf3522cce6c5792f", Enabled: new(false), Action: "block"},
				},
			},
		},
		`{"action":"execute","expression":"true","enabled":true,"action_parameters":{
		  "id":"77454fe2d30c4220b5701f6fdfb893ba",
		  "overrides":{
		    "categories":[{"category":"log4j","action":"managed_challenge"},{"category":"wordpress","enabled":false}],
		    "rules":[{"id":"7dfd111a6bad4b86bf3522cce6c5792f","enabled":false,"action":"block"},
		             {"id":"9f7e454e4cad45abbcc40696037f70df","action":"log"}]}}}`),
)
