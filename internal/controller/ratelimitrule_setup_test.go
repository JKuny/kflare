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

var _ = Describe("RateLimitRuleReconciler SetupWithManager", func() {
	It("registers without error", func() {
		mgr, err := manager.New(cfg, manager.Options{})
		Expect(err).NotTo(HaveOccurred())
		r := &RateLimitRuleReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}
		Expect(r.SetupWithManager(mgr)).To(Succeed())
	})

	It("falls back to the production client when no factory is injected", func() {
		_, err := (&RateLimitRuleReconciler{}).rules().newAPI("")
		Expect(err).To(HaveOccurred(), "the production client rejects an empty token")
	})
})

var _ = DescribeTable("the Cloudflare rule a RateLimitRule asks for",
	func(spec cloudflarev1alpha1.RateLimitRuleSpec, want cf.RulesetRule) {
		spec.Expression = `http.request.uri.path eq "/a"`
		want.Expression = spec.Expression
		Expect(rateLimitRuleDefinition(&cloudflarev1alpha1.RateLimitRule{Spec: spec})).To(Equal(want))
	},
	Entry("the defaults: counted per IP address and data center, blocked for the period",
		cloudflarev1alpha1.RateLimitRuleSpec{
			Action:            "block",
			Characteristics:   []cloudflarev1alpha1.RateLimitRuleCharacteristic{"ip.src"},
			Period:            10,
			RequestsPerPeriod: 5,
		},
		cf.RulesetRule{
			Action:  "block",
			Enabled: cf.BoolPtr(true),
			RateLimit: &cf.RulesetRuleRateLimit{
				Characteristics: []string{"cf.colo.id", "ip.src"}, Period: 10, RequestsPerPeriod: 5, MitigationTimeout: 10,
			},
		}),
	Entry("characteristics sorted after cf.colo.id, which is sent once",
		cloudflarev1alpha1.RateLimitRuleSpec{
			Action: "block",
			Characteristics: []cloudflarev1alpha1.RateLimitRuleCharacteristic{
				"ip.src", "cf.colo.id", `http.request.headers["x-api-key"]`,
			},
			Period:            10,
			RequestsPerPeriod: 5,
		},
		cf.RulesetRule{
			Action:  "block",
			Enabled: cf.BoolPtr(true),
			RateLimit: &cf.RulesetRuleRateLimit{
				Characteristics: []string{"cf.colo.id", `http.request.headers["x-api-key"]`, "ip.src"},
				Period:          10, RequestsPerPeriod: 5, MitigationTimeout: 10,
			},
		}),
	Entry("a disabled challenge with a longer timeout, its own counting expression, counting origin requests",
		cloudflarev1alpha1.RateLimitRuleSpec{
			RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{
				Description: "limit /a", Enabled: cf.BoolPtr(false),
			},
			Action:             "managed_challenge",
			Characteristics:    []cloudflarev1alpha1.RateLimitRuleCharacteristic{"ip.src"},
			Period:             60,
			RequestsPerPeriod:  100,
			MitigationTimeout:  new(int32(3600)),
			CountingExpression: `http.response.code eq 401`,
			RequestsToOrigin:   true,
		},
		cf.RulesetRule{
			Action:      "managed_challenge",
			Description: "limit /a",
			Enabled:     cf.BoolPtr(false),
			RateLimit: &cf.RulesetRuleRateLimit{
				Characteristics: []string{"cf.colo.id", "ip.src"}, Period: 60, RequestsPerPeriod: 100,
				MitigationTimeout: 3600, CountingExpression: `http.response.code eq 401`, RequestsToOrigin: true,
			},
		}),
	Entry("throttling, with a mitigation timeout of 0",
		cloudflarev1alpha1.RateLimitRuleSpec{
			Action:            "block",
			Characteristics:   []cloudflarev1alpha1.RateLimitRuleCharacteristic{"ip.src"},
			Period:            10,
			RequestsPerPeriod: 5,
			MitigationTimeout: new(int32(0)),
		},
		cf.RulesetRule{
			Action:  "block",
			Enabled: cf.BoolPtr(true),
			RateLimit: &cf.RulesetRuleRateLimit{
				Characteristics: []string{"cf.colo.id", "ip.src"}, Period: 10, RequestsPerPeriod: 5,
			},
		}),
	Entry("a block with a custom response",
		cloudflarev1alpha1.RateLimitRuleSpec{
			Action:            "block",
			Characteristics:   []cloudflarev1alpha1.RateLimitRuleCharacteristic{"ip.src"},
			Period:            10,
			RequestsPerPeriod: 5,
			BlockResponse: &cloudflarev1alpha1.RateLimitRuleBlockResponse{
				StatusCode: 429, ContentType: "application/json", Content: `{"error":"slow down"}`,
			},
		},
		cf.RulesetRule{
			Action:  "block",
			Enabled: cf.BoolPtr(true),
			ActionParameters: &cf.RulesetRuleActionParameters{
				Response: &cf.RulesetRuleActionParametersBlockResponse{
					StatusCode: 429, ContentType: "application/json", Content: `{"error":"slow down"}`,
				},
			},
			RateLimit: &cf.RulesetRuleRateLimit{
				Characteristics: []string{"cf.colo.id", "ip.src"}, Period: 10, RequestsPerPeriod: 5, MitigationTimeout: 10,
			},
		}),
)
