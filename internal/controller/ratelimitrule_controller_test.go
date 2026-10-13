/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"strings"

	cf "github.com/cloudflare/cloudflare-go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// The RateLimitRule specs cover what is specific to rate limiting rules: their
// phase, the rule they ask for and their validation. The reconcile loop they
// share with every rule kind is covered by the WAFCustomRule specs. Both run
// against fakeRulesetAPI (rulesetrule_test.go) in envtest.
var _ = Describe("RateLimitRule Controller", func() {
	const (
		ns          = "default"
		zoneCRName  = "rl-test-zone"
		accountName = "rl-test-account"
		secretName  = "rl-test-api-token"
		phase       = "http_ratelimit"
	)

	ctx := context.Background()
	var fake *fakeRulesetAPI
	var r *RateLimitRuleReconciler

	key := func(name string) types.NamespacedName { return types.NamespacedName{Name: name, Namespace: ns} }

	// create creates obj and deletes it again after the spec.
	create := func(obj client.Object) {
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		DeferCleanup(forceDelete, ctx, obj.DeepCopyObject().(client.Object), client.ObjectKeyFromObject(obj))
	}

	// createRule creates a rule allowing 5 requests to /<name> per period that
	// already carries the finalizer; change adjusts its spec first.
	createRule := func(name string, change func(*cloudflarev1alpha1.RateLimitRuleSpec)) {
		rule := &cloudflarev1alpha1.RateLimitRule{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Finalizers: []string{reconciler.Finalizer}},
			Spec: cloudflarev1alpha1.RateLimitRuleSpec{
				RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{
					ZoneRef:    corev1.LocalObjectReference{Name: zoneCRName},
					Expression: `http.request.uri.path eq "/` + name + `"`,
				},
				RequestsPerPeriod: 5,
			},
		}
		if change != nil {
			change(&rule.Spec)
		}
		create(rule)
	}
	getRule := func(k types.NamespacedName) *cloudflarev1alpha1.RateLimitRule {
		rule := &cloudflarev1alpha1.RateLimitRule{}
		Expect(k8sClient.Get(ctx, k, rule)).To(Succeed())
		return rule
	}
	reconcileRule := func(k types.NamespacedName) {
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: k})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.IsZero()).To(BeTrue())
	}
	// cloudflareRule is the rule fake Cloudflare holds for the resource at k.
	cloudflareRule := func(k types.NamespacedName) cf.RulesetRule {
		rule, ok := ruleWithRef(fake.entrypointIn(phase).Rules, rulesetRuleRef("RateLimitRule", k.Namespace, k.Name))
		Expect(ok).To(BeTrue(), "Cloudflare has no rule for %s", k)
		return rule
	}

	BeforeEach(func() {
		fake = newFakeRulesetAPI()
		r = &RateLimitRuleReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			NewRulesetRuleAPI: func(string) (RulesetRuleAPI, error) {
				return fake, nil
			},
		}
	})

	// readyEnv creates a ready account and a Zone synced with the fake's zone.
	readyEnv := func() {
		create(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
			Data:       map[string][]byte{"CF_API_TOKEN": []byte("token")},
		})
		acct := &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: accountName},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID:      "acct-rl",
				TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: secretName, Namespace: ns, Key: "CF_API_TOKEN"},
			},
		}
		create(acct)
		acct.Status.Conditions = []metav1.Condition{{
			Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
			Reason: "Validated", LastTransitionTime: metav1.Now(),
		}}
		Expect(k8sClient.Status().Update(ctx, acct)).To(Succeed())
		zone := &cloudflarev1alpha1.Zone{
			ObjectMeta: metav1.ObjectMeta{Name: zoneCRName, Namespace: ns},
			Spec: cloudflarev1alpha1.ZoneSpec{
				Name: "rl-example.com", AccountRef: corev1.LocalObjectReference{Name: accountName},
			},
		}
		create(zone)
		zone.Status.CloudflareMetadata.ZoneID = rulesTestZone
		zone.Status.Conditions = []metav1.Condition{{
			Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
			Reason: "Synced", LastTransitionTime: metav1.Now(),
		}}
		Expect(k8sClient.Status().Update(ctx, zone)).To(Succeed())
	}

	Describe("Reconcile", func() {
		BeforeEach(readyEnv)

		It("writes the rule to the zone's rate limiting rules and reports it in status", func() {
			createRule("a", nil)
			reconcileRule(key("a"))

			Expect(fake.rulesets).NotTo(HaveKey(rulesTestPhase), "the WAF custom rules are left alone")
			rule := cloudflareRule(key("a"))
			Expect(rule.Action).To(Equal("block"))
			Expect(rule.Expression).To(Equal(`http.request.uri.path eq "/a"`))
			Expect(*rule.Enabled).To(BeTrue())
			Expect(rule.RateLimit).To(Equal(&cf.RulesetRuleRateLimit{
				Characteristics:   []string{"cf.colo.id", "ip.src"},
				Period:            10,
				RequestsPerPeriod: 5,
				MitigationTimeout: 10,
			}))
			Expect(rule.ActionParameters).To(BeNil())

			Expect(getRule(key("a")).Status.CloudflareMetadata).To(Equal(cloudflarev1alpha1.RulesetRuleCloudflareMetadata{
				ZoneID:    rulesTestZone,
				RulesetID: fake.entrypointIn(phase).ID,
				RuleID:    rule.ID,
				Ref:       rulesetRuleRef("RateLimitRule", ns, "a"),
				Version:   "1",
			}))
			ready := meta.FindStatusCondition(getRule(key("a")).Status.Conditions, cloudflarev1alpha1.ConditionReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		})

		It("writes nothing to Cloudflare once a rule using every option is in sync", func() {
			createRule("a", func(spec *cloudflarev1alpha1.RateLimitRuleSpec) {
				spec.Description = "limit /a"
				spec.Characteristics = []cloudflarev1alpha1.RateLimitRuleCharacteristic{
					`http.request.headers["x-api-key"]`, "cf.colo.id", "ip.src",
				}
				spec.Period = 60
				spec.MitigationTimeout = new(int32(600))
				spec.CountingExpression = `http.request.uri.path eq "/a" and http.response.code eq 401`
				spec.RequestsToOrigin = true
				spec.BlockResponse = &cloudflarev1alpha1.RateLimitRuleBlockResponse{
					ContentType: "application/json", Content: `{"error":"slow down"}`,
				}
			})
			reconcileRule(key("a"))
			rule := cloudflareRule(key("a"))
			Expect(rule.RateLimit.Characteristics).To(Equal([]string{
				"cf.colo.id", `http.request.headers["x-api-key"]`, "ip.src",
			}))
			Expect(rule.ActionParameters.Response.StatusCode).To(BeEquivalentTo(429))

			writes := fake.writes
			reconcileRule(key("a"))
			Expect(fake.writes).To(Equal(writes))
		})

		It("updates the rule in place when the spec changes", func() {
			createRule("a", nil)
			reconcileRule(key("a"))
			ruleID := cloudflareRule(key("a")).ID

			rule := getRule(key("a"))
			rule.Spec.RequestsPerPeriod = 50
			Expect(k8sClient.Update(ctx, rule)).To(Succeed())
			reconcileRule(key("a"))
			Expect(cloudflareRule(key("a")).ID).To(Equal(ruleID))
			Expect(cloudflareRule(key("a")).RateLimit.RequestsPerPeriod).To(Equal(50))
			Expect(getRule(key("a")).Status.CloudflareMetadata.Version).To(Equal("2"))
		})

		It("keeps its rules apart from the zone's WAF custom rules", func() {
			createRule("a", func(spec *cloudflarev1alpha1.RateLimitRuleSpec) { spec.Priority = 10 })
			create(&cloudflarev1alpha1.WAFCustomRule{
				ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: ns, Finalizers: []string{reconciler.Finalizer}},
				Spec: cloudflarev1alpha1.WAFCustomRuleSpec{
					RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{
						ZoneRef: corev1.LocalObjectReference{Name: zoneCRName}, Expression: "true", Priority: 1,
					},
					Action: "block",
				},
			})
			waf := &WAFCustomRuleReconciler{
				Client:            k8sClient,
				NewRulesetRuleAPI: func(string) (RulesetRuleAPI, error) { return fake, nil },
			}
			_, err := waf.Reconcile(ctx, reconcile.Request{NamespacedName: key("a")})
			Expect(err).NotTo(HaveOccurred())
			reconcileRule(key("a"))

			Expect(fake.entrypointIn(phase).Rules).To(HaveLen(1))
			Expect(fake.entrypoint().Rules).To(HaveLen(1))
			Expect(fake.entrypoint().Rules[0].Ref).To(Equal(rulesetRuleRef("WAFCustomRule", ns, "a")))
			peers, err := r.rules().peers(ctx, rulesTestZone)
			Expect(err).NotTo(HaveOccurred())
			Expect(peers).To(ConsistOf(rulesetRulePeer{
				ref: rulesetRuleRef("RateLimitRule", ns, "a"), priority: 10,
				namespace: ns, name: "a", kind: "RateLimitRule",
			}))
		})

		It("deletes the rule from Cloudflare with the resource", func() {
			createRule("a", nil)
			reconcileRule(key("a"))
			Expect(k8sClient.Delete(ctx, getRule(key("a")))).To(Succeed())
			reconcileRule(key("a"))
			Expect(fake.entrypointIn(phase).Rules).To(BeEmpty())
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key("a"), &cloudflarev1alpha1.RateLimitRule{}))).To(BeTrue())
		})
	})

	Describe("Validation", func() {
		// validate creates a rule changed by change, and deletes it again.
		validate := func(change func(*cloudflarev1alpha1.RateLimitRuleSpec)) error {
			rule := &cloudflarev1alpha1.RateLimitRule{
				ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: ns},
				Spec: cloudflarev1alpha1.RateLimitRuleSpec{
					RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{
						ZoneRef: corev1.LocalObjectReference{Name: zoneCRName}, Expression: "true",
					},
					RequestsPerPeriod: 5,
				},
			}
			change(&rule.Spec)
			err := k8sClient.Create(ctx, rule)
			forceDelete(ctx, &cloudflarev1alpha1.RateLimitRule{}, key("a"))
			return err
		}

		It("applies the defaults", func() {
			createRule("a", func(spec *cloudflarev1alpha1.RateLimitRuleSpec) {
				spec.BlockResponse = &cloudflarev1alpha1.RateLimitRuleBlockResponse{
					ContentType: "text/plain", Content: "slow down",
				}
			})
			spec := getRule(key("a")).Spec
			Expect(spec.Action).To(Equal("block"))
			Expect(spec.Characteristics).To(Equal([]cloudflarev1alpha1.RateLimitRuleCharacteristic{"ip.src"}))
			Expect(spec.Period).To(BeEquivalentTo(10))
			Expect(spec.MitigationTimeout).To(BeNil(), "an unset timeout follows the period")
			Expect(*spec.Enabled).To(BeTrue())
			Expect(spec.BlockResponse.StatusCode).To(BeEquivalentTo(429))
		})

		DescribeTable("accepts a mitigation timeout of 0 or at least the period",
			func(timeout int32) {
				Expect(validate(func(spec *cloudflarev1alpha1.RateLimitRuleSpec) {
					spec.Period = 60
					spec.MitigationTimeout = &timeout
				})).To(Succeed())
			},
			Entry("0, which throttles", int32(0)),
			Entry("the period", int32(60)),
			Entry("a day", int32(86400)),
		)

		DescribeTable("rejects an invalid spec",
			func(change func(*cloudflarev1alpha1.RateLimitRuleSpec), message string) {
				Expect(validate(change)).To(MatchError(ContainSubstring(message)))
			},
			Entry("skip, which rate limiting rules cannot use", func(spec *cloudflarev1alpha1.RateLimitRuleSpec) {
				spec.Action = "skip"
			}, "spec.action"),
			Entry("no requests per period", func(spec *cloudflarev1alpha1.RateLimitRuleSpec) {
				spec.RequestsPerPeriod = 0
			}, "spec.requestsPerPeriod"),
			Entry("more than a million requests per period", func(spec *cloudflarev1alpha1.RateLimitRuleSpec) {
				spec.RequestsPerPeriod = 1000001
			}, "spec.requestsPerPeriod"),
			Entry("a negative period", func(spec *cloudflarev1alpha1.RateLimitRuleSpec) {
				spec.Period = -10
			}, "spec.period"),
			Entry("a mitigation timeout shorter than the period", func(spec *cloudflarev1alpha1.RateLimitRuleSpec) {
				spec.Period = 60
				spec.MitigationTimeout = new(int32(10))
			}, "mitigationTimeout must be 0 or at least the period"),
			Entry("a negative mitigation timeout", func(spec *cloudflarev1alpha1.RateLimitRuleSpec) {
				spec.MitigationTimeout = new(int32(-1))
			}, "spec.mitigationTimeout"),
			Entry("an empty characteristic", func(spec *cloudflarev1alpha1.RateLimitRuleSpec) {
				spec.Characteristics = []cloudflarev1alpha1.RateLimitRuleCharacteristic{""}
			}, "spec.characteristics[0]"),
			Entry("a characteristic listed twice", func(spec *cloudflarev1alpha1.RateLimitRuleSpec) {
				spec.Characteristics = []cloudflarev1alpha1.RateLimitRuleCharacteristic{"ip.src", "ip.src"}
			}, "Duplicate value"),
			Entry("a counting expression over 4096 characters", func(spec *cloudflarev1alpha1.RateLimitRuleSpec) {
				spec.CountingExpression = strings.Repeat("a", 4097)
			}, "spec.countingExpression"),
			Entry("a block response on a challenge rule", func(spec *cloudflarev1alpha1.RateLimitRuleSpec) {
				spec.Action = "managed_challenge"
				spec.BlockResponse = &cloudflarev1alpha1.RateLimitRuleBlockResponse{
					ContentType: "text/plain", Content: "slow down",
				}
			}, "blockResponse is allowed only with action block"),
			Entry("a block response status outside 400-499", func(spec *cloudflarev1alpha1.RateLimitRuleSpec) {
				spec.BlockResponse = &cloudflarev1alpha1.RateLimitRuleBlockResponse{
					StatusCode: 503, ContentType: "text/plain", Content: "slow down",
				}
			}, "spec.blockResponse.statusCode"),
			Entry("a block response with an unknown content type", func(spec *cloudflarev1alpha1.RateLimitRuleSpec) {
				spec.BlockResponse = &cloudflarev1alpha1.RateLimitRuleBlockResponse{
					ContentType: "application/xml", Content: "<slow-down/>",
				}
			}, "spec.blockResponse.contentType"),
			Entry("a block response body over 30,720 characters", func(spec *cloudflarev1alpha1.RateLimitRuleSpec) {
				spec.BlockResponse = &cloudflarev1alpha1.RateLimitRuleBlockResponse{
					ContentType: "text/plain", Content: strings.Repeat("a", 30721),
				}
			}, "spec.blockResponse.content"),
		)
	})
})
