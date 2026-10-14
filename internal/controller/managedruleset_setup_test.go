/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	cf "github.com/cloudflare/cloudflare-go"
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
		reason, ok := managedRulesetKind.conflict(cloudflareError(400, managedRulesetConflictCode))
		Expect(ok).To(BeTrue())
		Expect(reason).To(Equal("RulesetConflict"))

		_, ok = managedRulesetKind.conflict(cloudflareError(400, 20226))
		Expect(ok).To(BeFalse())
		_, ok = managedRulesetKind.conflict(nil)
		Expect(ok).To(BeFalse())
		_, ok = wafCustomRuleKind.conflict(cloudflareError(400, managedRulesetConflictCode))
		Expect(ok).To(BeFalse(), "only kinds that list a code retry on it")
	})
})

var _ = DescribeTable("the Cloudflare rule a ManagedRuleset asks for",
	func(spec cloudflarev1alpha1.ManagedRulesetSpec, want cf.RulesetRule) {
		spec.Expression = "true"
		spec.RulesetID = freeManagedRulesetID
		want.Action = "execute"
		want.Expression = spec.Expression
		if want.ActionParameters == nil {
			want.ActionParameters = &cf.RulesetRuleActionParameters{}
		}
		want.ActionParameters.ID = freeManagedRulesetID
		Expect(managedRulesetDefinition(&cloudflarev1alpha1.ManagedRuleset{Spec: spec})).To(Equal(want))
	},
	Entry("the ruleset as Cloudflare ships it, enabled by default",
		cloudflarev1alpha1.ManagedRulesetSpec{},
		cf.RulesetRule{Enabled: cf.BoolPtr(true)}),
	Entry("a disabled deployment with a description",
		cloudflarev1alpha1.ManagedRulesetSpec{
			RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{Enabled: cf.BoolPtr(false), Description: "managed rules"},
		},
		cf.RulesetRule{Enabled: cf.BoolPtr(false), Description: "managed rules"}),
	Entry("overrides for the whole ruleset",
		cloudflarev1alpha1.ManagedRulesetSpec{
			Overrides: &cloudflarev1alpha1.ManagedRulesetOverrides{Enabled: cf.BoolPtr(false), Action: "log"},
		},
		cf.RulesetRule{
			Enabled: cf.BoolPtr(true),
			ActionParameters: &cf.RulesetRuleActionParameters{
				Overrides: &cf.RulesetRuleActionParametersOverrides{Enabled: cf.BoolPtr(false), Action: "log"},
			},
		}),
	Entry("category and rule overrides, sorted by category and rule ID",
		cloudflarev1alpha1.ManagedRulesetSpec{
			Overrides: &cloudflarev1alpha1.ManagedRulesetOverrides{
				Categories: []cloudflarev1alpha1.ManagedRulesetCategoryOverride{
					{Category: "wordpress", Enabled: cf.BoolPtr(false)},
					{Category: "log4j", Action: "managed_challenge"},
				},
				Rules: []cloudflarev1alpha1.ManagedRulesetRuleOverride{
					{ID: "9f7e454e4cad45abbcc40696037f70df", Action: "log"},
					{ID: "7dfd111a6bad4b86bf3522cce6c5792f", Enabled: cf.BoolPtr(false), Action: "block"},
				},
			},
		},
		cf.RulesetRule{
			Enabled: cf.BoolPtr(true),
			ActionParameters: &cf.RulesetRuleActionParameters{
				Overrides: &cf.RulesetRuleActionParametersOverrides{
					Categories: []cf.RulesetRuleActionParametersCategories{
						{Category: "log4j", Action: "managed_challenge"},
						{Category: "wordpress", Enabled: cf.BoolPtr(false)},
					},
					Rules: []cf.RulesetRuleActionParametersRules{
						{ID: "7dfd111a6bad4b86bf3522cce6c5792f", Enabled: cf.BoolPtr(false), Action: "block"},
						{ID: "9f7e454e4cad45abbcc40696037f70df", Action: "log"},
					},
				},
			},
		}),
)
