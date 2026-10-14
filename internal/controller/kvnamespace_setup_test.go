/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"

	"github.com/cloudflare/cloudflare-go/v7/kv"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
)

var _ = Describe("defaultKVNamespaceAPI", func() {
	It("returns an error for an empty token", func() {
		_, err := defaultKVNamespaceAPI("")
		Expect(err).To(HaveOccurred())
	})

	It("returns cloudflare-go v7's KV namespace service for a token", func() {
		api, err := defaultKVNamespaceAPI("token")
		Expect(err).NotTo(HaveOccurred())
		Expect(api).To(BeAssignableToTypeOf(&kv.NamespaceService{}))
	})

	It("is used when no factory is injected", func() {
		_, err := (&KVNamespaceReconciler{}).crud().newAPI("")
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("KVNamespaceReconciler SetupWithManager", func() {
	It("registers without error", func() {
		mgr, err := manager.New(cfg, manager.Options{})
		Expect(err).NotTo(HaveOccurred())
		r := &KVNamespaceReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}
		Expect(r.SetupWithManager(mgr)).To(Succeed())
	})

	It("watches the kind its namespaces wait for", func() {
		Expect(kvNamespaceKind.dependents).To(HaveLen(1))
		Expect(kvNamespaceKind.dependents[0].newObject()).To(BeAssignableToTypeOf(&cloudflarev1alpha1.WorkerScript{}))
	})
})

var _ = Describe("KVNamespaceReconciler mappers", func() {
	const ns = "default"
	ctx := context.Background()
	r := &KVNamespaceReconciler{}

	BeforeEach(func() {
		r.Client = k8sClient
		obj := &cloudflarev1alpha1.KVNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: "kvm-namespace", Namespace: ns},
			Spec: cloudflarev1alpha1.KVNamespaceSpec{
				AccountRef: corev1.LocalObjectReference{Name: "kvm-account"}, Title: "kvm-title",
			},
		}
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.KVNamespace{}, types.NamespacedName{Name: obj.Name, Namespace: ns})
	})

	It("maps a CloudflareAccount to the namespaces that use it", func() {
		account := &cloudflarev1alpha1.CloudflareAccount{ObjectMeta: metav1.ObjectMeta{Name: "kvm-account"}}
		Expect(r.crud().resourcesForAccount(ctx, account)).To(ConsistOf(
			reconcile.Request{NamespacedName: types.NamespacedName{Name: "kvm-namespace", Namespace: ns}}))
		other := &cloudflarev1alpha1.CloudflareAccount{ObjectMeta: metav1.ObjectMeta{Name: "kvm-other"}}
		Expect(r.crud().resourcesForAccount(ctx, other)).To(BeEmpty())
	})

	It("returns nil when the list fails", func() {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		account := &cloudflarev1alpha1.CloudflareAccount{ObjectMeta: metav1.ObjectMeta{Name: "kvm-account"}}
		Expect(r.crud().resourcesForAccount(cancelled, account)).To(BeNil())
	})

	It("re-triggers each bound namespace that is being deleted when a WorkerScript goes", func() {
		ws := &cloudflarev1alpha1.WorkerScript{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns},
			Spec: cloudflarev1alpha1.WorkerScriptSpec{Bindings: []cloudflarev1alpha1.WorkerBinding{
				{Name: "A", KVNamespaceRef: &corev1.LocalObjectReference{Name: "kvm-namespace"}},
				{Name: "B", KVNamespaceRef: &corev1.LocalObjectReference{Name: "kvm-missing"}},
			}},
		}
		Expect(r.crud().deletingResourcesOf(ctx, ws)).To(BeEmpty(), "the namespace is not being deleted yet")

		obj := &cloudflarev1alpha1.KVNamespace{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "kvm-namespace", Namespace: ns}, obj)).To(Succeed())
		obj.Finalizers = []string{"test/hold"}
		Expect(k8sClient.Update(ctx, obj)).To(Succeed())
		Expect(k8sClient.Delete(ctx, obj)).To(Succeed())
		Expect(r.crud().deletingResourcesOf(ctx, ws)).To(ConsistOf(
			reconcile.Request{NamespacedName: types.NamespacedName{Name: "kvm-namespace", Namespace: ns}}))
	})
})

var _ = Describe("KVNamespace as a dependent", func() {
	It("counts as a user of its CloudflareAccount", func() {
		ctx := context.Background()
		obj := &cloudflarev1alpha1.KVNamespace{
			ObjectMeta: metav1.ObjectMeta{Name: "kvd-namespace", Namespace: "default"},
			Spec: cloudflarev1alpha1.KVNamespaceSpec{
				AccountRef: corev1.LocalObjectReference{Name: "kvd-account"}, Title: "kvd-title",
			},
		}
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		DeferCleanup(forceDelete, ctx, &cloudflarev1alpha1.KVNamespace{}, types.NamespacedName{Name: obj.Name, Namespace: obj.Namespace})

		deps, err := dependentsOf(ctx, k8sClient, "", "kvd-account", accountDependents)
		Expect(err).NotTo(HaveOccurred())
		Expect(deps).To(ConsistOf("KVNamespace default/kvd-namespace"))
	})
})
