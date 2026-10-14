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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// The ManagedRuleset specs cover what is specific to managed ruleset
// deployments: their phase, the rule they ask for, the conflict Cloudflare
// reports for a ruleset deployed twice, the entry point they delete once it
// is empty, and their validation. The reconcile loop they share with every
// rule kind is covered by the WAFCustomRule specs. Both run against
// fakeRulesetAPI (rulesetrule_test.go) in envtest.
var _ = Describe("ManagedRuleset Controller", func() {
	const (
		ns          = "default"
		zoneCRName  = "mr-test-zone"
		accountName = "mr-test-account"
		secretName  = "mr-test-api-token"
		// managedRulesetID is the Cloudflare Managed Ruleset, a second ruleset
		// for the specs that deploy two.
		managedRulesetID = "efb7b8c949ac4650a09736fc376e9aee"
	)

	ctx := context.Background()
	var fake *fakeRulesetAPI
	var r *ManagedRulesetReconciler

	key := func(name string) types.NamespacedName { return types.NamespacedName{Name: name, Namespace: ns} }

	// create creates obj and deletes it again after the spec.
	create := func(obj client.Object) {
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		DeferCleanup(forceDelete, ctx, obj.DeepCopyObject().(client.Object), client.ObjectKeyFromObject(obj))
	}

	// createDeployment creates a deployment of the Free Managed Ruleset for
	// every request that already carries the finalizer; change adjusts its
	// spec first.
	createDeployment := func(name string, change func(*cloudflarev1alpha1.ManagedRulesetSpec)) {
		deployment := &cloudflarev1alpha1.ManagedRuleset{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Finalizers: []string{reconciler.Finalizer}},
			Spec: cloudflarev1alpha1.ManagedRulesetSpec{
				RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{
					ZoneRef:    corev1.LocalObjectReference{Name: zoneCRName},
					Expression: "true",
				},
				RulesetID: freeManagedRulesetID,
			},
		}
		if change != nil {
			change(&deployment.Spec)
		}
		create(deployment)
	}
	getDeployment := func(k types.NamespacedName) *cloudflarev1alpha1.ManagedRuleset {
		deployment := &cloudflarev1alpha1.ManagedRuleset{}
		Expect(k8sClient.Get(ctx, k, deployment)).To(Succeed())
		return deployment
	}
	reconcileDeployment := func(k types.NamespacedName) {
		result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: k})
		Expect(err).NotTo(HaveOccurred())
		Expect(result.IsZero()).To(BeTrue())
	}
	// cloudflareRule is the rule fake Cloudflare holds for the resource at k.
	cloudflareRule := func(k types.NamespacedName) cf.RulesetRule {
		rule, ok := ruleWithRef(fake.entrypointIn(managedPhase).Rules, rulesetRuleRef("ManagedRuleset", k.Namespace, k.Name))
		Expect(ok).To(BeTrue(), "Cloudflare has no rule for %s", k)
		return rule
	}
	hasEntrypoint := func() bool {
		_, ok := fake.rulesets[managedPhase]
		return ok
	}

	BeforeEach(func() {
		fake = newFakeRulesetAPI()
		r = &ManagedRulesetReconciler{
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
				AccountID:      "acct-mr",
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
				Name: "mr-example.com", AccountRef: corev1.LocalObjectReference{Name: accountName},
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

		It("writes the execute rule to the zone's managed phase and reports it in status", func() {
			createDeployment("free", nil)
			reconcileDeployment(key("free"))

			Expect(fake.rulesets).To(HaveLen(1), "the zone's other phases are left alone")
			rule := cloudflareRule(key("free"))
			Expect(rule.Action).To(Equal("execute"))
			Expect(rule.Expression).To(Equal("true"))
			Expect(*rule.Enabled).To(BeTrue())
			Expect(rule.ActionParameters.ID).To(Equal(freeManagedRulesetID))
			Expect(*rule.ActionParameters.Version).To(Equal("latest"))
			Expect(rule.ActionParameters.Overrides).To(BeNil())

			Expect(getDeployment(key("free")).Status.CloudflareMetadata).To(Equal(cloudflarev1alpha1.RulesetRuleCloudflareMetadata{
				ZoneID:    rulesTestZone,
				RulesetID: fake.entrypointIn(managedPhase).ID,
				RuleID:    rule.ID,
				Ref:       rulesetRuleRef("ManagedRuleset", ns, "free"),
				Version:   "1",
			}))
			ready := meta.FindStatusCondition(getDeployment(key("free")).Status.Conditions, cloudflarev1alpha1.ConditionReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		})

		It("writes nothing to Cloudflare once a deployment with every override is in sync", func() {
			createDeployment("free", func(spec *cloudflarev1alpha1.ManagedRulesetSpec) {
				spec.Description = "managed rules"
				spec.Overrides = &cloudflarev1alpha1.ManagedRulesetOverrides{
					Enabled: new(true),
					Action:  "managed_challenge",
					Categories: []cloudflarev1alpha1.ManagedRulesetCategoryOverride{
						{Category: "wordpress", Enabled: new(false)},
						{Category: "log4j", Action: "block"},
					},
					Rules: []cloudflarev1alpha1.ManagedRulesetRuleOverride{
						{ID: "9f7e454e4cad45abbcc40696037f70df", Action: "log"},
						{ID: "7dfd111a6bad4b86bf3522cce6c5792f", Enabled: new(false)},
					},
				}
			})
			reconcileDeployment(key("free"))
			overrides := cloudflareRule(key("free")).ActionParameters.Overrides
			Expect(overrides.Categories[0].Category).To(Equal("log4j"))
			Expect(overrides.Rules[0].ID).To(Equal("7dfd111a6bad4b86bf3522cce6c5792f"))

			writes := fake.writes
			reconcileDeployment(key("free"))
			Expect(fake.writes).To(Equal(writes))
		})

		It("updates the rule in place when the overrides change", func() {
			createDeployment("free", nil)
			reconcileDeployment(key("free"))
			ruleID := cloudflareRule(key("free")).ID

			deployment := getDeployment(key("free"))
			deployment.Spec.Overrides = &cloudflarev1alpha1.ManagedRulesetOverrides{
				Rules: []cloudflarev1alpha1.ManagedRulesetRuleOverride{
					{ID: "b453c8ace3a54e0ab7c791510b51dc4d", Enabled: new(false)},
				},
			}
			Expect(k8sClient.Update(ctx, deployment)).To(Succeed())
			reconcileDeployment(key("free"))
			rule := cloudflareRule(key("free"))
			Expect(rule.ID).To(Equal(ruleID))
			Expect(rule.ActionParameters.Overrides.Rules).To(Equal([]cf.RulesetRuleActionParametersRules{
				{ID: "b453c8ace3a54e0ab7c791510b51dc4d", Enabled: new(false)},
			}))
			Expect(getDeployment(key("free")).Status.CloudflareMetadata.Version).To(Equal("2"))
		})

		It("reports RulesetConflict and checks again every minute while another rule deploys the ruleset", func() {
			fake.seedIn(managedPhase, executeRule("someone-else", freeManagedRulesetID))
			createDeployment("free", nil)

			result, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key("free")})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
			ready := meta.FindStatusCondition(getDeployment(key("free")).Status.Conditions, cloudflarev1alpha1.ConditionReady)
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal("RulesetConflict"))
			Expect(ready.Message).To(ContainSubstring("Cloudflare refused the rule"))
			Expect(refs(fake.entrypointIn(managedPhase).Rules)).To(Equal([]string{"someone-else"}))

			By("writing the rule once the other deployment is gone")
			fake.entrypointIn(managedPhase).Rules = nil
			reconcileDeployment(key("free"))
			Expect(cloudflareRule(key("free")).ActionParameters.ID).To(Equal(freeManagedRulesetID))
		})

		It("deletes its rule and the entry point it empties, which brings back Cloudflare's default", func() {
			createDeployment("free", nil)
			reconcileDeployment(key("free"))

			Expect(k8sClient.Delete(ctx, getDeployment(key("free")))).To(Succeed())
			reconcileDeployment(key("free"))
			Expect(hasEntrypoint()).To(BeFalse())
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, key("free"), &cloudflarev1alpha1.ManagedRuleset{}))).To(BeTrue())
		})

		It("keeps the entry point while another deployment in the zone is about to write to it", func() {
			createDeployment("free", nil)
			reconcileDeployment(key("free"))
			createDeployment("managed", func(spec *cloudflarev1alpha1.ManagedRulesetSpec) {
				spec.RulesetID = managedRulesetID
			})

			Expect(k8sClient.Delete(ctx, getDeployment(key("free")))).To(Succeed())
			reconcileDeployment(key("free"))
			Expect(fake.entrypointIn(managedPhase).Rules).To(BeEmpty())

			reconcileDeployment(key("managed"))
			Expect(cloudflareRule(key("managed")).ActionParameters.ID).To(Equal(managedRulesetID))
		})

		It("re-triggers the deployments that may have lost their rule when one is deleted", func() {
			createDeployment("synced", nil)
			reconcileDeployment(key("synced"))
			createDeployment("pending", func(spec *cloudflarev1alpha1.ManagedRulesetSpec) {
				spec.RulesetID = managedRulesetID
			})
			createDeployment("elsewhere", func(spec *cloudflarev1alpha1.ManagedRulesetSpec) {
				spec.RulesetID = "4814384a9e5d4991b9815dcfc25d2f1f"
			})
			elsewhere := getDeployment(key("elsewhere"))
			elsewhere.Status.CloudflareMetadata.ZoneID = "zone-elsewhere"
			Expect(k8sClient.Status().Update(ctx, elsewhere)).To(Succeed())

			deleted := &cloudflarev1alpha1.ManagedRuleset{ObjectMeta: metav1.ObjectMeta{Name: "gone", Namespace: ns}}
			deleted.Status.CloudflareMetadata.ZoneID = rulesTestZone
			Expect(r.rules().rulesInZoneOfDeleted(ctx, deleted)).To(ConsistOf(
				reconcile.Request{NamespacedName: key("synced")},
				reconcile.Request{NamespacedName: key("pending")},
			))

			By("re-triggering every deployment when the deleted one never synced")
			deleted.Status.CloudflareMetadata.ZoneID = ""
			Expect(r.rules().rulesInZoneOfDeleted(ctx, deleted)).To(HaveLen(3))
			Expect(r.rules().rulesInZoneOfDeleted(ctx, &cloudflarev1alpha1.Zone{})).To(BeEmpty())
		})
	})

	Describe("Validation", func() {
		// validate creates a deployment changed by change, and deletes it again.
		validate := func(change func(*cloudflarev1alpha1.ManagedRulesetSpec)) error {
			deployment := &cloudflarev1alpha1.ManagedRuleset{
				ObjectMeta: metav1.ObjectMeta{Name: "free", Namespace: ns},
				Spec: cloudflarev1alpha1.ManagedRulesetSpec{
					RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{
						ZoneRef: corev1.LocalObjectReference{Name: zoneCRName}, Expression: "true",
					},
					RulesetID: freeManagedRulesetID,
				},
			}
			change(&deployment.Spec)
			err := k8sClient.Create(ctx, deployment)
			forceDelete(ctx, &cloudflarev1alpha1.ManagedRuleset{}, key("free"))
			return err
		}

		It("applies the defaults", func() {
			createDeployment("free", nil)
			spec := getDeployment(key("free")).Spec
			Expect(*spec.Enabled).To(BeTrue())
			Expect(spec.Overrides).To(BeNil())
		})

		It("rejects changing spec.rulesetID", func() {
			createDeployment("free", nil)
			deployment := getDeployment(key("free"))
			deployment.Spec.RulesetID = managedRulesetID
			Expect(k8sClient.Update(ctx, deployment)).To(MatchError(ContainSubstring("rulesetID is immutable")))
		})

		It("accepts overrides at every level", func() {
			Expect(validate(func(spec *cloudflarev1alpha1.ManagedRulesetSpec) {
				spec.Overrides = &cloudflarev1alpha1.ManagedRulesetOverrides{
					Enabled:    new(false),
					Categories: []cloudflarev1alpha1.ManagedRulesetCategoryOverride{{Category: "wordpress", Action: "log"}},
					Rules: []cloudflarev1alpha1.ManagedRulesetRuleOverride{
						{ID: "b453c8ace3a54e0ab7c791510b51dc4d", Enabled: new(true), Action: "js_challenge"},
					},
				}
			})).To(Succeed())
		})

		DescribeTable("rejects an invalid spec",
			func(change func(*cloudflarev1alpha1.ManagedRulesetSpec), message string) {
				Expect(validate(change)).To(MatchError(ContainSubstring(message)))
			},
			Entry("a ruleset ID that is not 32 hex characters", func(spec *cloudflarev1alpha1.ManagedRulesetSpec) {
				spec.RulesetID = "cloudflare-free"
			}, "spec.rulesetID"),
			Entry("overrides that override nothing", func(spec *cloudflarev1alpha1.ManagedRulesetSpec) {
				spec.Overrides = &cloudflarev1alpha1.ManagedRulesetOverrides{}
			}, "overrides must override something"),
			Entry("an unknown action", func(spec *cloudflarev1alpha1.ManagedRulesetSpec) {
				spec.Overrides = &cloudflarev1alpha1.ManagedRulesetOverrides{Action: "skip"}
			}, "spec.overrides.action"),
			Entry("a category override without enabled or action", func(spec *cloudflarev1alpha1.ManagedRulesetSpec) {
				spec.Overrides = &cloudflarev1alpha1.ManagedRulesetOverrides{
					Categories: []cloudflarev1alpha1.ManagedRulesetCategoryOverride{{Category: "wordpress"}},
				}
			}, "a category override needs enabled or action"),
			Entry("a category listed twice", func(spec *cloudflarev1alpha1.ManagedRulesetSpec) {
				spec.Overrides = &cloudflarev1alpha1.ManagedRulesetOverrides{
					Categories: []cloudflarev1alpha1.ManagedRulesetCategoryOverride{
						{Category: "wordpress", Enabled: new(false)}, {Category: "wordpress", Action: "log"},
					},
				}
			}, "Duplicate value"),
			Entry("an empty category", func(spec *cloudflarev1alpha1.ManagedRulesetSpec) {
				spec.Overrides = &cloudflarev1alpha1.ManagedRulesetOverrides{
					Categories: []cloudflarev1alpha1.ManagedRulesetCategoryOverride{{Enabled: new(false)}},
				}
			}, "spec.overrides.categories[0].category"),
			Entry("a rule override without enabled or action", func(spec *cloudflarev1alpha1.ManagedRulesetSpec) {
				spec.Overrides = &cloudflarev1alpha1.ManagedRulesetOverrides{
					Rules: []cloudflarev1alpha1.ManagedRulesetRuleOverride{{ID: "b453c8ace3a54e0ab7c791510b51dc4d"}},
				}
			}, "a rule override needs enabled or action"),
			Entry("a rule ID that is not 32 hex characters", func(spec *cloudflarev1alpha1.ManagedRulesetSpec) {
				spec.Overrides = &cloudflarev1alpha1.ManagedRulesetOverrides{
					Rules: []cloudflarev1alpha1.ManagedRulesetRuleOverride{{ID: "log4j", Enabled: new(false)}},
				}
			}, "spec.overrides.rules[0].id"),
			Entry("a rule listed twice", func(spec *cloudflarev1alpha1.ManagedRulesetSpec) {
				spec.Overrides = &cloudflarev1alpha1.ManagedRulesetOverrides{
					Rules: []cloudflarev1alpha1.ManagedRulesetRuleOverride{
						{ID: "b453c8ace3a54e0ab7c791510b51dc4d", Enabled: new(false)},
						{ID: "b453c8ace3a54e0ab7c791510b51dc4d", Action: "log"},
					},
				}
			}, "Duplicate value"),
		)
	})
})
