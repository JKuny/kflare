/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"

	cf "github.com/cloudflare/cloudflare-go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
)

var _ = Describe("defaultRulesetRuleAPI", func() {
	It("returns an error for an empty token", func() {
		_, err := defaultRulesetRuleAPI("")
		Expect(err).To(HaveOccurred())
	})

	It("returns the production client for a token", func() {
		api, err := defaultRulesetRuleAPI("token")
		Expect(err).NotTo(HaveOccurred())
		Expect(api).To(BeAssignableToTypeOf(&cfpkg.Client{}))
	})
})

var _ = Describe("WAFCustomRuleReconciler SetupWithManager", func() {
	It("registers without error", func() {
		mgr, err := manager.New(cfg, manager.Options{})
		Expect(err).NotTo(HaveOccurred())
		r := &WAFCustomRuleReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}
		Expect(r.SetupWithManager(mgr)).To(Succeed())
	})

	It("falls back to the production client when no factory is injected", func() {
		_, err := (&WAFCustomRuleReconciler{}).rules().newAPI("")
		Expect(err).To(HaveOccurred(), "the production client rejects an empty token")
	})
})

var _ = Describe("WAFCustomRule mapper", func() {
	const ns = "default"
	ctx := context.Background()
	r := &WAFCustomRuleReconciler{}

	BeforeEach(func() {
		r.Client = k8sClient
		for name, zone := range map[string]string{"wcm-a": "wcm-zone", "wcm-b": "wcm-zone", "wcm-c": "wcm-other-zone"} {
			Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.WAFCustomRule{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec: cloudflarev1alpha1.WAFCustomRuleSpec{
					RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{
						ZoneRef: corev1.LocalObjectReference{Name: zone}, Expression: "true",
					},
					Action: "block",
				},
			})).To(Succeed())
			DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.WAFCustomRule{}, types.NamespacedName{Name: name, Namespace: ns})
		}
	})

	request := func(name string) reconcile.Request {
		return reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}
	}

	It("maps a Zone to every rule in it", func() {
		zone := &cloudflarev1alpha1.Zone{ObjectMeta: metav1.ObjectMeta{Name: "wcm-zone", Namespace: ns}}
		Expect(r.rules().rulesForZone(ctx, zone)).To(ConsistOf(request("wcm-a"), request("wcm-b")))
	})

	It("ignores Zones in another namespace", func() {
		zone := &cloudflarev1alpha1.Zone{ObjectMeta: metav1.ObjectMeta{Name: "wcm-zone", Namespace: "kube-public"}}
		Expect(r.rules().rulesForZone(ctx, zone)).To(BeEmpty())
	})

	It("returns nil when the list fails", func() {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		zone := &cloudflarev1alpha1.Zone{ObjectMeta: metav1.ObjectMeta{Name: "wcm-zone", Namespace: ns}}
		Expect(r.rules().rulesForZone(cancelled, zone)).To(BeNil())
	})
})

var _ = DescribeTable("the Cloudflare rule a WAFCustomRule asks for",
	func(spec cloudflarev1alpha1.WAFCustomRuleSpec, want cf.RulesetRule) {
		spec.Expression = `http.request.uri.path eq "/a"`
		want.Expression = spec.Expression
		Expect(wafCustomRuleDefinition(&cloudflarev1alpha1.WAFCustomRule{Spec: spec})).To(Equal(want))
	},
	Entry("a block rule, enabled by default",
		cloudflarev1alpha1.WAFCustomRuleSpec{
			RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{Description: "block /a"},
			Action:          "block",
		},
		cf.RulesetRule{Action: "block", Description: "block /a", Enabled: cf.BoolPtr(true)}),
	Entry("a disabled challenge",
		cloudflarev1alpha1.WAFCustomRuleSpec{
			RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{Enabled: cf.BoolPtr(false)},
			Action:          "managed_challenge",
		},
		cf.RulesetRule{Action: "managed_challenge", Enabled: cf.BoolPtr(false)}),
	Entry("a skip of the remaining rules, logged by default",
		cloudflarev1alpha1.WAFCustomRuleSpec{
			Action: "skip",
			Skip:   &cloudflarev1alpha1.WAFCustomRuleSkip{RemainingRules: true},
		},
		cf.RulesetRule{
			Action:           "skip",
			Enabled:          cf.BoolPtr(true),
			ActionParameters: &cf.RulesetRuleActionParameters{Ruleset: "current"},
			Logging:          &cf.RulesetRuleLogging{Enabled: cf.BoolPtr(true)},
		}),
	Entry("a skip of phases and products, sorted, without logging",
		cloudflarev1alpha1.WAFCustomRuleSpec{
			Action: "skip",
			Skip: &cloudflarev1alpha1.WAFCustomRuleSkip{
				Phases:   []cloudflarev1alpha1.WAFCustomRuleSkipPhase{"http_request_sbfm", "http_ratelimit"},
				Products: []cloudflarev1alpha1.WAFCustomRuleSkipProduct{"zoneLockdown", "bic"},
				Log:      cf.BoolPtr(false),
			},
		},
		cf.RulesetRule{
			Action:  "skip",
			Enabled: cf.BoolPtr(true),
			ActionParameters: &cf.RulesetRuleActionParameters{
				Phases:   []string{"http_ratelimit", "http_request_sbfm"},
				Products: []string{"bic", "zoneLockdown"},
			},
			Logging: &cf.RulesetRuleLogging{Enabled: cf.BoolPtr(false)},
		}),
	Entry("a block with a custom response",
		cloudflarev1alpha1.WAFCustomRuleSpec{
			Action: "block",
			BlockResponse: &cloudflarev1alpha1.WAFCustomRuleBlockResponse{
				StatusCode: 429, ContentType: "application/json", Content: `{"error":"blocked"}`,
			},
		},
		cf.RulesetRule{
			Action:  "block",
			Enabled: cf.BoolPtr(true),
			ActionParameters: &cf.RulesetRuleActionParameters{
				Response: &cf.RulesetRuleActionParametersBlockResponse{
					StatusCode: 429, ContentType: "application/json", Content: `{"error":"blocked"}`,
				},
			},
		}),
)
