/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"encoding/json"

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
	func(spec cloudflarev1alpha1.RateLimitRuleSpec, want string) {
		spec.Expression = "true"
		Expect(json.Marshal(rateLimitRuleDefinition(&cloudflarev1alpha1.RateLimitRule{Spec: spec}))).To(MatchJSON(want))
	},
	Entry("the defaults: counted per IP address and data center, blocked for the period",
		cloudflarev1alpha1.RateLimitRuleSpec{
			Action:            "block",
			Characteristics:   []cloudflarev1alpha1.RateLimitRuleCharacteristic{"ip.src"},
			Period:            10,
			RequestsPerPeriod: 5,
		},
		`{"action":"block","expression":"true","enabled":true,
		  "ratelimit":{"characteristics":["cf.colo.id","ip.src"],"period":10,"requests_per_period":5,
		               "mitigation_timeout":10}}`),
	Entry("characteristics sorted after cf.colo.id, which is sent once",
		cloudflarev1alpha1.RateLimitRuleSpec{
			Action: "block",
			Characteristics: []cloudflarev1alpha1.RateLimitRuleCharacteristic{
				"ip.src", "cf.colo.id", `http.request.headers["x-api-key"]`,
			},
			Period:            10,
			RequestsPerPeriod: 5,
		},
		`{"action":"block","expression":"true","enabled":true,
		  "ratelimit":{"characteristics":["cf.colo.id","http.request.headers[\"x-api-key\"]","ip.src"],
		               "period":10,"requests_per_period":5,"mitigation_timeout":10}}`),
	Entry("a disabled challenge with a longer timeout, its own counting expression, counting origin requests",
		cloudflarev1alpha1.RateLimitRuleSpec{
			RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{
				Description: "limit /a", Enabled: new(false),
			},
			Action:             "managed_challenge",
			Characteristics:    []cloudflarev1alpha1.RateLimitRuleCharacteristic{"ip.src"},
			Period:             60,
			RequestsPerPeriod:  100,
			MitigationTimeout:  new(int32(3600)),
			CountingExpression: `http.response.code eq 401`,
			RequestsToOrigin:   true,
		},
		`{"action":"managed_challenge","expression":"true","description":"limit /a","enabled":false,
		  "ratelimit":{"characteristics":["cf.colo.id","ip.src"],"period":60,"requests_per_period":100,
		               "mitigation_timeout":3600,"counting_expression":"http.response.code eq 401",
		               "requests_to_origin":true}}`),
	Entry("throttling, with a mitigation timeout of 0, which is left out",
		cloudflarev1alpha1.RateLimitRuleSpec{
			Action:            "block",
			Characteristics:   []cloudflarev1alpha1.RateLimitRuleCharacteristic{"ip.src"},
			Period:            10,
			RequestsPerPeriod: 5,
			MitigationTimeout: new(int32(0)),
		},
		`{"action":"block","expression":"true","enabled":true,
		  "ratelimit":{"characteristics":["cf.colo.id","ip.src"],"period":10,"requests_per_period":5}}`),
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
		`{"action":"block","expression":"true","enabled":true,
		  "action_parameters":{"response":{"status_code":429,"content_type":"application/json",
		                                   "content":"{\"error\":\"slow down\"}"}},
		  "ratelimit":{"characteristics":["cf.colo.id","ip.src"],"period":10,"requests_per_period":5,
		               "mitigation_timeout":10}}`),
)
