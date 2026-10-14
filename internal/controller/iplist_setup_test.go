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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

var _ = Describe("defaultIPListAPI", func() {
	It("returns an error for an empty token", func() {
		_, err := defaultIPListAPI("")
		Expect(err).To(HaveOccurred())
	})

	It("returns the production client for a token", func() {
		api, err := defaultIPListAPI("token")
		Expect(err).NotTo(HaveOccurred())
		Expect(api).To(BeAssignableToTypeOf(&cfpkg.Client{}))
	})
})

var _ = Describe("IPListReconciler SetupWithManager", func() {
	It("registers without error", func() {
		mgr, err := manager.New(cfg, manager.Options{})
		Expect(err).NotTo(HaveOccurred())
		r := &IPListReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}
		Expect(r.SetupWithManager(mgr)).To(Succeed())
	})

	It("falls back to the production client when no factory is injected", func() {
		_, err := (&IPListReconciler{}).newIPListAPI("")
		Expect(err).To(HaveOccurred(), "the production client rejects an empty token")
	})
})

// These specs cover how IPLists and the rules that use them trigger each
// other: Cloudflare refuses a rule that uses a list the account does not
// have, and a list that a rule uses.
var _ = Describe("IPLists and the rules that use them", func() {
	ctx := context.Background()

	// create creates obj and deletes it again after the spec.
	create := func(obj client.Object) {
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		DeferCleanup(forceDelete, ctx, obj.DeepCopyObject().(client.Object), client.ObjectKeyFromObject(obj))
	}
	// createList creates IPList <namespace>/<name> for the list listName,
	// optionally already synced with Cloudflare.
	createList := func(namespace, name, listName string, synced bool) *cloudflarev1alpha1.IPList {
		list := &cloudflarev1alpha1.IPList{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Finalizers: []string{reconciler.Finalizer}},
			Spec: cloudflarev1alpha1.IPListSpec{
				AccountRef: corev1.LocalObjectReference{Name: "uses-account"}, Name: listName,
			},
		}
		create(list)
		if synced {
			list.Status.CloudflareMetadata.ListID = "id-" + listName
			Expect(k8sClient.Status().Update(ctx, list)).To(Succeed())
		}
		return list
	}
	// wafRule is a WAFCustomRule in namespace with expression.
	wafRule := func(namespace, name, expression string) *cloudflarev1alpha1.WAFCustomRule {
		return &cloudflarev1alpha1.WAFCustomRule{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: cloudflarev1alpha1.WAFCustomRuleSpec{
				RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{
					ZoneRef: corev1.LocalObjectReference{Name: "uses-zone"}, Expression: expression,
				},
				Action: "block",
			},
		}
	}
	request := func(namespace, name string) reconcile.Request {
		return reconcile.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: namespace}}
	}

	Describe("a rule that uses a list", func() {
		It("is reconciled once its list exists in Cloudflare, in any namespace", func() {
			create(wafRule("default", "uses-a", `ip.src in $office_ips and http.request.uri.path eq "/admin"`))
			create(wafRule("kube-public", "uses-b", `not ip.src in $office_ips`))
			create(wafRule("default", "uses-other", `ip.src in $other_ips`))
			rules := (&WAFCustomRuleReconciler{Client: k8sClient}).rules()

			list := createList("default", "office", "office_ips", false)
			Expect(rules.rulesUsingList(ctx, list)).To(BeEmpty(), "the list is not in Cloudflare yet")

			list = createList("default", "office-synced", "office_ips", true)
			Expect(rules.rulesUsingList(ctx, list)).To(ConsistOf(
				request("default", "uses-a"), request("kube-public", "uses-b"),
			))
			Expect(rules.rulesUsingList(ctx, &corev1.Secret{})).To(BeEmpty())
		})

		It("is not reconciled when the rules cannot be listed", func() {
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			list := &cloudflarev1alpha1.IPList{Spec: cloudflarev1alpha1.IPListSpec{Name: "office_ips"}}
			list.Status.CloudflareMetadata.ListID = "id"
			Expect((&WAFCustomRuleReconciler{Client: k8sClient}).rules().rulesUsingList(cancelled, list)).To(BeNil())
		})
	})

	Describe("a list being deleted", func() {
		It("is reconciled when a rule that uses it is deleted", func() {
			r := &IPListReconciler{Client: k8sClient}
			deleting := createList("default", "deleting", "office_ips", true)
			Expect(k8sClient.Delete(ctx, deleting)).To(Succeed())
			deletingElsewhere := createList("kube-public", "deleting", "office_ips", true)
			Expect(k8sClient.Delete(ctx, deletingElsewhere)).To(Succeed())
			createList("default", "kept", "office_ips", true)
			unused := createList("default", "unused", "other_ips", true)
			Expect(k8sClient.Delete(ctx, unused)).To(Succeed())

			Expect(r.deletingListsUsedBy(ctx, wafRule("default", "rule", `ip.src in $office_ips`))).To(ConsistOf(
				request("default", "deleting"), request("kube-public", "deleting"),
			))
			Expect(r.deletingListsUsedBy(ctx, &cloudflarev1alpha1.RateLimitRule{
				Spec: cloudflarev1alpha1.RateLimitRuleSpec{
					RulesetRuleSpec: cloudflarev1alpha1.RulesetRuleSpec{Expression: `ip.src in $office_ips`},
				},
			})).To(HaveLen(2), "every rule kind")
			Expect(r.deletingListsUsedBy(ctx, wafRule("default", "rule", `http.host eq "example.com"`))).To(BeEmpty())
			Expect(r.deletingListsUsedBy(ctx, &corev1.Secret{})).To(BeEmpty())

			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			Expect(r.deletingListsUsedBy(cancelled, wafRule("default", "rule", `ip.src in $office_ips`))).To(BeNil())
		})
	})

	It("maps an account to the IPLists that use it", func() {
		r := &IPListReconciler{Client: k8sClient}
		createList("default", "on-account", "office_ips", false)
		account := &cloudflarev1alpha1.CloudflareAccount{ObjectMeta: metav1.ObjectMeta{Name: "uses-account"}}
		Expect(r.listsForAccount(ctx, account)).To(ConsistOf(request("default", "on-account")))
		Expect(r.listsForAccount(ctx, &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: "other-account"},
		})).To(BeEmpty())

		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		Expect(r.listsForAccount(cancelled, account)).To(BeNil())
	})
})

var _ = DescribeTable("the lists a rule expression uses",
	func(expression string, want []string) {
		Expect(listsInExpression(expression)).To(Equal(want))
	},
	Entry("none", `http.request.uri.path eq "/admin"`, []string{}),
	Entry("one", `ip.src in $office_ips`, []string{"office_ips"}),
	Entry("several", `(ip.src in $a and not ip.src in $b_2) or ip.src in $3c`, []string{"a", "b_2", "3c"}),
	Entry("a managed list, by the part before its dot", `ip.src in $cf.open_proxies`, []string{"cf"}),
)

var _ = DescribeTable("the canonical form of an address or range",
	func(value, want string) {
		Expect(canonicalIP(value)).To(Equal(want))
	},
	// The forms Cloudflare stored when sent the value (verified live).
	Entry("an IPv4 address", "192.0.2.1", "192.0.2.1"),
	Entry("an IPv4 range", "198.51.100.0/24", "198.51.100.0/24"),
	Entry("an IPv4 /32", "203.0.113.7/32", "203.0.113.7"),
	Entry("an uppercase IPv6 address", "2001:DB8::1", "2001:db8::1"),
	Entry("an uncompressed IPv6 range", "2001:db8:0:0::/64", "2001:db8::/64"),
	Entry("an IPv6 /128", "2001:db8::1/128", "2001:db8::1"),
	Entry("something else, unchanged", "192.0.2.01", "192.0.2.01"),
)

var _ = DescribeTable("whether a list's items in Cloudflare are the spec's",
	func(want []cloudflarev1alpha1.IPListItem, got []cf.ListItem, match bool) {
		Expect(ipListItemsMatch(want, got)).To(Equal(match))
	},
	Entry("the same items, in another order and form",
		[]cloudflarev1alpha1.IPListItem{{IP: "203.0.113.7/32", Comment: "a"}, {IP: "2001:DB8::/32"}},
		[]cf.ListItem{{IP: new("2001:db8::/32")}, {IP: new("203.0.113.7"), Comment: "a"}},
		true),
	Entry("no items", nil, nil, true),
	Entry("another comment",
		[]cloudflarev1alpha1.IPListItem{{IP: "192.0.2.1", Comment: "a"}},
		[]cf.ListItem{{IP: new("192.0.2.1"), Comment: "b"}},
		false),
	Entry("another address",
		[]cloudflarev1alpha1.IPListItem{{IP: "192.0.2.1"}},
		[]cf.ListItem{{IP: new("192.0.2.2")}},
		false),
	Entry("an item missing",
		[]cloudflarev1alpha1.IPListItem{{IP: "192.0.2.1"}, {IP: "192.0.2.2"}},
		[]cf.ListItem{{IP: new("192.0.2.1")}},
		false),
	Entry("an item too many",
		[]cloudflarev1alpha1.IPListItem{{IP: "192.0.2.1"}},
		[]cf.ListItem{{IP: new("192.0.2.1")}, {IP: new("192.0.2.2")}},
		false),
	Entry("one address twice in the spec, which Cloudflare refuses",
		[]cloudflarev1alpha1.IPListItem{{IP: "192.0.2.1"}, {IP: "192.0.2.1/32"}},
		[]cf.ListItem{{IP: new("192.0.2.1")}, {IP: new("192.0.2.2")}},
		false),
	Entry("an item that is not an address",
		[]cloudflarev1alpha1.IPListItem{{IP: "192.0.2.1"}},
		[]cf.ListItem{{Hostname: &cf.Hostname{UrlHostname: "example.com"}}},
		false),
)
