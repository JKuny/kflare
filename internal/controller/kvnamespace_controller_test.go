/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"

	"github.com/cloudflare/cloudflare-go/v7/kv"
	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/cloudflare/cloudflare-go/v7/packages/pagination"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// fakeKVNamespaceAPI is an in-memory cloudflare-go v7 KV namespace service
// for one account. It follows the behavior verified against the live API:
// titles are unique in an account (a duplicate is a 400, code 10014), a rename
// keeps the ID and returns the renamed namespace, an unknown namespace ID is a
// 404 (code 10013), and the list pages by number, at most 1,000 namespaces to
// a page (more is a 400, code 10008). Like cloudflare-go, it refuses a call
// without an account ID.
type fakeKVNamespaceAPI struct {
	namespaces map[string]kv.Namespace
	nextID     int

	createErr, getErr, listErr, updateErr, deleteErr error

	accountIDs []string
	listPages  []int64
	writes     int
}

func newFakeKVNamespaceAPI(existing ...kv.Namespace) *fakeKVNamespaceAPI {
	f := &fakeKVNamespaceAPI{namespaces: map[string]kv.Namespace{}}
	for _, ns := range existing {
		f.namespaces[ns.ID] = ns
	}
	return f
}

// account records the account ID a call names, which cloudflare-go requires.
func (f *fakeKVNamespaceAPI) account(id string) error {
	f.accountIDs = append(f.accountIDs, id)
	if id == "" {
		return errors.New("missing required account_id parameter")
	}
	return nil
}

func kvNamespaceNotFound(method, accountID, namespaceID string) error {
	return v7APIError(method, fmt.Sprintf("accounts/%s/storage/kv/namespaces/%s", accountID, namespaceID),
		http.StatusNotFound, 10013, "namespace not found")
}

func kvTitleTaken(method, accountID string) error {
	return v7APIError(method, fmt.Sprintf("accounts/%s/storage/kv/namespaces", accountID),
		http.StatusBadRequest, 10014, "a namespace with this account ID and title already exists")
}

func (f *fakeKVNamespaceAPI) titleTaken(title string) bool {
	for _, ns := range f.namespaces {
		if ns.Title == title {
			return true
		}
	}
	return false
}

func (f *fakeKVNamespaceAPI) New(_ context.Context, params kv.NamespaceNewParams, _ ...option.RequestOption) (*kv.Namespace, error) {
	if err := f.account(params.AccountID.Value); err != nil {
		return nil, err
	}
	f.writes++
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.titleTaken(params.Title.Value) {
		return nil, kvTitleTaken(http.MethodPost, params.AccountID.Value)
	}
	f.nextID++
	ns := kv.Namespace{ID: fmt.Sprintf("kv-%d", f.nextID), Title: params.Title.Value}
	f.namespaces[ns.ID] = ns
	return &ns, nil
}

func (f *fakeKVNamespaceAPI) Get(_ context.Context, namespaceID string, query kv.NamespaceGetParams,
	_ ...option.RequestOption) (*kv.Namespace, error) {
	if err := f.account(query.AccountID.Value); err != nil {
		return nil, err
	}
	if f.getErr != nil {
		return nil, f.getErr
	}
	ns, ok := f.namespaces[namespaceID]
	if !ok {
		return nil, kvNamespaceNotFound(http.MethodGet, query.AccountID.Value, namespaceID)
	}
	return &ns, nil
}

func (f *fakeKVNamespaceAPI) List(_ context.Context, params kv.NamespaceListParams,
	_ ...option.RequestOption) (*pagination.V4PagePaginationArray[kv.Namespace], error) {
	if err := f.account(params.AccountID.Value); err != nil {
		return nil, err
	}
	page, perPage := int64(params.Page.Value), int64(params.PerPage.Value)
	f.listPages = append(f.listPages, page)
	if f.listErr != nil {
		return nil, f.listErr
	}
	if perPage > kvNamespaceListPageSize {
		return nil, v7APIError(http.MethodGet, fmt.Sprintf("accounts/%s/storage/kv/namespaces", params.AccountID.Value),
			http.StatusBadRequest, 10008, "list namespaces: 'per_page argument must be no greater than 1000'")
	}
	ids := slices.Sorted(maps.Keys(f.namespaces))
	start := min(max(page-1, 0)*perPage, int64(len(ids)))
	end := min(start+perPage, int64(len(ids)))
	res := &pagination.V4PagePaginationArray[kv.Namespace]{
		ResultInfo: pagination.V4PagePaginationArrayResultInfo{Page: page, PerPage: perPage},
	}
	for _, id := range ids[start:end] {
		res.Result = append(res.Result, f.namespaces[id])
	}
	return res, nil
}

func (f *fakeKVNamespaceAPI) Update(_ context.Context, namespaceID string, params kv.NamespaceUpdateParams,
	_ ...option.RequestOption) (*kv.Namespace, error) {
	if err := f.account(params.AccountID.Value); err != nil {
		return nil, err
	}
	f.writes++
	if f.updateErr != nil {
		return nil, f.updateErr
	}
	ns, ok := f.namespaces[namespaceID]
	if !ok {
		return nil, kvNamespaceNotFound(http.MethodPut, params.AccountID.Value, namespaceID)
	}
	if ns.Title != params.Title.Value && f.titleTaken(params.Title.Value) {
		return nil, kvTitleTaken(http.MethodPut, params.AccountID.Value)
	}
	ns.Title = params.Title.Value
	f.namespaces[ns.ID] = ns
	return &ns, nil
}

func (f *fakeKVNamespaceAPI) Delete(_ context.Context, namespaceID string, body kv.NamespaceDeleteParams,
	_ ...option.RequestOption) (*kv.NamespaceDeleteResponse, error) {
	if err := f.account(body.AccountID.Value); err != nil {
		return nil, err
	}
	f.writes++
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	if _, ok := f.namespaces[namespaceID]; !ok {
		return nil, kvNamespaceNotFound(http.MethodDelete, body.AccountID.Value, namespaceID)
	}
	delete(f.namespaces, namespaceID)
	return &kv.NamespaceDeleteResponse{}, nil
}

var _ = Describe("KVNamespace Controller", func() {
	const (
		ns          = "default"
		kvName      = "kv-test-namespace"
		otherKV     = "kv-test-namespace-other"
		accountName = "kv-test-account"
		secretName  = "kv-test-api-token"
		fakeAcctID  = "acct-kv-123"
		title       = "kv-test-title"
	)

	ctx := context.Background()
	kvKey := types.NamespacedName{Name: kvName, Namespace: ns}

	var fake *fakeKVNamespaceAPI
	var r *KVNamespaceReconciler

	createAccount := func(ready bool) {
		acct := &cloudflarev1alpha1.CloudflareAccount{
			ObjectMeta: metav1.ObjectMeta{Name: accountName},
			Spec: cloudflarev1alpha1.CloudflareAccountSpec{
				AccountID:      fakeAcctID,
				TokenSecretRef: cloudflarev1alpha1.SecretReference{Name: secretName, Namespace: ns, Key: "CF_API_TOKEN"},
			},
		}
		Expect(k8sClient.Create(ctx, acct)).To(Succeed())
		if ready {
			acct.Status.Conditions = []metav1.Condition{{
				Type: cloudflarev1alpha1.ConditionReady, Status: metav1.ConditionTrue,
				Reason: "Validated", LastTransitionTime: metav1.Now(),
			}}
			Expect(k8sClient.Status().Update(ctx, acct)).To(Succeed())
		}
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: ns},
			Data:       map[string][]byte{"CF_API_TOKEN": []byte("token")},
		})).To(Succeed())
	}

	// createKV creates a KVNamespace that already carries the finalizer.
	createKV := func(name, kvTitle string, annotations map[string]string) {
		Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.KVNamespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: ns, Annotations: annotations,
				Finalizers: []string{reconciler.Finalizer},
			},
			Spec: cloudflarev1alpha1.KVNamespaceSpec{
				AccountRef: corev1.LocalObjectReference{Name: accountName}, Title: kvTitle,
			},
		})).To(Succeed())
	}

	getKV := func() *cloudflarev1alpha1.KVNamespace {
		obj := &cloudflarev1alpha1.KVNamespace{}
		Expect(k8sClient.Get(ctx, kvKey, obj)).To(Succeed())
		return obj
	}

	readyCondition := func() metav1.Condition {
		cond := meta.FindStatusCondition(getKV().Status.Conditions, cloudflarev1alpha1.ConditionReady)
		Expect(cond).NotTo(BeNil())
		return *cond
	}

	reconcileKV := func() (ctrl.Result, error) {
		return r.Reconcile(ctx, reconcile.Request{NamespacedName: kvKey})
	}

	BeforeEach(func() {
		fake = newFakeKVNamespaceAPI()
		r = &KVNamespaceReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			NewKVNamespaceAPI: func(string) (KVNamespaceAPI, error) {
				return fake, nil
			},
		}
	})

	AfterEach(func() {
		forceDelete(ctx, &cloudflarev1alpha1.KVNamespace{}, kvKey)
		forceDelete(ctx, &cloudflarev1alpha1.KVNamespace{}, types.NamespacedName{Name: otherKV, Namespace: ns})
		forceDelete(ctx, &cloudflarev1alpha1.WorkerScript{}, types.NamespacedName{Name: "kv-test-worker", Namespace: ns})
		forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: accountName})
		forceDelete(ctx, &corev1.Secret{}, types.NamespacedName{Name: secretName, Namespace: ns})
	})

	Describe("Reconcile", func() {
		It("returns no error when the KVNamespace does not exist", func() {
			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
		})

		It("adds the finalizer on the first reconcile without calling Cloudflare", func() {
			Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.KVNamespace{
				ObjectMeta: metav1.ObjectMeta{Name: kvName, Namespace: ns},
				Spec: cloudflarev1alpha1.KVNamespaceSpec{
					AccountRef: corev1.LocalObjectReference{Name: accountName}, Title: title,
				},
			})).To(Succeed())
			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			Expect(getKV().Finalizers).To(ContainElement(reconciler.Finalizer))
			Expect(fake.accountIDs).To(BeEmpty())
		})

		It("retries after a minute when the account is not ready", func() {
			createAccount(false)
			createKV(kvName, title, nil)
			result, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
			Expect(readyCondition().Reason).To(Equal("AccountNotReady"))
		})

		It("reports InvalidToken when no Cloudflare client can be built", func() {
			createAccount(true)
			createKV(kvName, title, nil)
			r.NewKVNamespaceAPI = func(string) (KVNamespaceAPI, error) { return nil, errors.New("bad token") }
			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			Expect(readyCondition().Reason).To(Equal("InvalidToken"))
		})

		It("creates the namespace, claims it and reports its ID", func() {
			createAccount(true)
			createKV(kvName, title, nil)
			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())

			Expect(fake.namespaces).To(HaveKeyWithValue("kv-1", kv.Namespace{ID: "kv-1", Title: title}))
			Expect(fake.accountIDs).To(HaveEach(fakeAcctID))
			Expect(fake.listPages).To(Equal([]int64{1}), "one page holds every namespace of the account")
			obj := getKV()
			Expect(obj.Status.CloudflareMetadata.NamespaceID).To(Equal("kv-1"))
			Expect(obj.Labels).To(HaveKeyWithValue(cloudflarev1alpha1.KVNamespaceIDLabel, "kv-1"))
			Expect(readyCondition().Status).To(Equal(metav1.ConditionTrue))
		})

		It("writes nothing to Cloudflare once the namespace is in sync", func() {
			createAccount(true)
			createKV(kvName, title, nil)
			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			writes := fake.writes

			pages := len(fake.listPages)

			_, err = reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.writes).To(Equal(writes))
			Expect(fake.listPages).To(HaveLen(pages), "the namespace is read by its ID, not found in the list")
		})

		It("adopts a namespace from a later page of the list", func() {
			createAccount(true)
			// A full first page of other namespaces; the one to adopt sorts last.
			existing := make([]kv.Namespace, 0, kvNamespaceListPageSize+1)
			for i := range kvNamespaceListPageSize {
				existing = append(existing, kv.Namespace{ID: fmt.Sprintf("a-%04d", i), Title: fmt.Sprintf("other-%04d", i)})
			}
			fake = newFakeKVNamespaceAPI(append(existing, kv.Namespace{ID: "z-last", Title: title})...)
			createKV(kvName, title, nil)
			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.listPages).To(Equal([]int64{1, 2}))
			Expect(getKV().Status.CloudflareMetadata.NamespaceID).To(Equal("z-last"))
			Expect(fake.writes).To(BeZero())
		})

		It("adopts the existing namespace with its title", func() {
			createAccount(true)
			fake = newFakeKVNamespaceAPI(kv.Namespace{ID: "existing", Title: title})
			createKV(kvName, title, nil)
			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.namespaces).To(HaveLen(1))
			Expect(getKV().Status.CloudflareMetadata.NamespaceID).To(Equal("existing"))
		})

		It("reports TitleConflict instead of taking over a namespace another KVNamespace manages", func() {
			createAccount(true)
			fake = newFakeKVNamespaceAPI(kv.Namespace{ID: "taken", Title: title})
			createKV(otherKV, title, nil)
			other := &cloudflarev1alpha1.KVNamespace{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: otherKV, Namespace: ns}, other)).To(Succeed())
			other.Labels = map[string]string{cloudflarev1alpha1.KVNamespaceIDLabel: "taken"}
			Expect(k8sClient.Update(ctx, other)).To(Succeed())

			createKV(kvName, title, nil)
			result, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.RequeueAfter).To(Equal(credentialsRetryInterval))
			Expect(readyCondition().Reason).To(Equal("TitleConflict"))
			Expect(readyCondition().Message).To(ContainSubstring(ns + "/" + otherKV))
			Expect(fake.writes).To(BeZero())
		})

		It("recreates the namespace when it was deleted outside kflare", func() {
			createAccount(true)
			createKV(kvName, title, nil)
			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			delete(fake.namespaces, "kv-1")

			_, err = reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			obj := getKV()
			Expect(obj.Status.CloudflareMetadata.NamespaceID).To(Equal("kv-2"))
			Expect(obj.Labels).To(HaveKeyWithValue(cloudflarev1alpha1.KVNamespaceIDLabel, "kv-2"))
		})

		It("renames the namespace in place when spec.title changes", func() {
			createAccount(true)
			createKV(kvName, title, nil)
			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())

			obj := getKV()
			obj.Spec.Title = "kv-test-renamed"
			Expect(k8sClient.Update(ctx, obj)).To(Succeed())
			_, err = reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.namespaces).To(HaveKeyWithValue("kv-1", kv.Namespace{ID: "kv-1", Title: "kv-test-renamed"}))
			Expect(getKV().Status.CloudflareMetadata.NamespaceID).To(Equal("kv-1"))
		})

		It("sets TerminalError when the new title is taken by another namespace", func() {
			createAccount(true)
			createKV(kvName, title, nil)
			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			fake.namespaces["other"] = kv.Namespace{ID: "other", Title: "kv-taken-title"}

			obj := getKV()
			obj.Spec.Title = "kv-taken-title"
			Expect(k8sClient.Update(ctx, obj)).To(Succeed())
			result, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
			Expect(readyCondition().Reason).To(Equal("TerminalError"))
			Expect(readyCondition().Message).To(Equal("Terminal Cloudflare API error: " +
				"a namespace with this account ID and title already exists (10014)"))
		})

		It("returns retryable errors for back-off", func() {
			createAccount(true)
			createKV(kvName, title, nil)
			fake.listErr = errors.New("connection reset")
			_, err := reconcileKV()
			Expect(err).To(MatchError("connection reset"))
			Expect(readyCondition().Reason).To(Equal("APIError"))

			fake.listErr = nil
			fake.createErr = errors.New("unavailable")
			_, err = reconcileKV()
			Expect(err).To(MatchError("unavailable"))
		})

		It("returns a failed read of the namespace for back-off", func() {
			createAccount(true)
			createKV(kvName, title, nil)
			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())

			fake.getErr = v7APIError(http.MethodGet, "accounts/acct-kv-123/storage/kv/namespaces/kv-1",
				http.StatusServiceUnavailable, 0, "")
			_, err = reconcileKV()
			Expect(err).To(MatchError(fake.getErr))
			Expect(readyCondition().Reason).To(Equal("APIError"))
			Expect(readyCondition().Message).To(Equal("Cloudflare API error: HTTP 503 Service Unavailable"))
		})

		It("returns a failed update for back-off", func() {
			createAccount(true)
			createKV(kvName, title, nil)
			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())

			obj := getKV()
			obj.Spec.Title = "kv-test-renamed"
			Expect(k8sClient.Update(ctx, obj)).To(Succeed())
			fake.updateErr = errors.New("unavailable")
			_, err = reconcileKV()
			Expect(err).To(MatchError("unavailable"))
			Expect(fake.namespaces["kv-1"].Title).To(Equal(title))
		})
	})

	Describe("deletion", func() {
		syncedKV := func(annotations map[string]string) {
			createAccount(true)
			createKV(kvName, title, annotations)
			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.namespaces).To(HaveLen(1))
		}
		startDeletion := func() {
			Expect(k8sClient.Delete(ctx, getKV())).To(Succeed())
		}
		expectGone := func() {
			Expect(apierrors.IsNotFound(k8sClient.Get(ctx, kvKey, &cloudflarev1alpha1.KVNamespace{}))).To(BeTrue())
		}

		It("deletes the namespace from Cloudflare and removes the finalizer", func() {
			syncedKV(nil)
			startDeletion()
			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.namespaces).To(BeEmpty())
			expectGone()
		})

		It("keeps the namespace and its data with the retain policy", func() {
			syncedKV(map[string]string{reconciler.DeletionPolicyAnnotation: reconciler.DeletionPolicyRetain})
			startDeletion()
			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.namespaces).To(HaveLen(1))
			expectGone()
		})

		It("finishes when the namespace is already gone from Cloudflare", func() {
			syncedKV(nil)
			delete(fake.namespaces, "kv-1")
			startDeletion()
			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			expectGone()
		})

		It("waits for the WorkerScripts bound to it, then deletes", func() {
			syncedKV(nil)
			script := "export default {}"
			Expect(k8sClient.Create(ctx, &cloudflarev1alpha1.WorkerScript{
				ObjectMeta: metav1.ObjectMeta{Name: "kv-test-worker", Namespace: ns, Finalizers: []string{reconciler.Finalizer}},
				Spec: cloudflarev1alpha1.WorkerScriptSpec{
					Name: "kv-test-script", AccountRef: corev1.LocalObjectReference{Name: accountName}, Script: &script,
					Bindings: []cloudflarev1alpha1.WorkerBinding{
						{Name: "KV", KVNamespaceRef: &corev1.LocalObjectReference{Name: kvName}},
					},
				},
			})).To(Succeed())
			startDeletion()

			_, err := reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			Expect(readyCondition().Reason).To(Equal("InUse"))
			Expect(readyCondition().Message).To(ContainSubstring("WorkerScript default/kv-test-worker"))
			Expect(fake.namespaces).To(HaveLen(1))

			forceDelete(ctx, &cloudflarev1alpha1.WorkerScript{}, types.NamespacedName{Name: "kv-test-worker", Namespace: ns})
			_, err = reconcileKV()
			Expect(err).NotTo(HaveOccurred())
			Expect(fake.namespaces).To(BeEmpty())
			expectGone()
		})

		It("keeps the finalizer when Cloudflare fails, so deletion is retried", func() {
			syncedKV(nil)
			fake.deleteErr = errors.New("unavailable")
			startDeletion()
			_, err := reconcileKV()
			Expect(err).To(MatchError("unavailable"))
			Expect(getKV().Finalizers).To(ContainElement(reconciler.Finalizer))
		})

		It("keeps the finalizer when the credentials cannot be resolved", func() {
			syncedKV(nil)
			forceDelete(ctx, &cloudflarev1alpha1.CloudflareAccount{}, types.NamespacedName{Name: accountName})
			startDeletion()
			_, err := reconcileKV()
			Expect(err).To(HaveOccurred())
			Expect(getKV().Finalizers).To(ContainElement(reconciler.Finalizer))
		})

		It("returns the error when no Cloudflare client can be built", func() {
			syncedKV(nil)
			r.NewKVNamespaceAPI = func(string) (KVNamespaceAPI, error) { return nil, errors.New("bad token") }
			startDeletion()
			_, err := reconcileKV()
			Expect(err).To(MatchError("bad token"))
		})

		It("does nothing for a namespace without the finalizer", func() {
			result, err := r.crud().reconcileDelete(ctx, &cloudflarev1alpha1.KVNamespace{})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.IsZero()).To(BeTrue())
		})
	})

	Describe("Validation", func() {
		BeforeEach(func() { createKV(kvName, title, nil) })

		It("rejects changing spec.accountRef", func() {
			obj := getKV()
			obj.Spec.AccountRef.Name = "other-account"
			Expect(k8sClient.Update(ctx, obj)).To(MatchError(ContainSubstring("accountRef is immutable")))
		})

		It("rejects an empty title", func() {
			obj := getKV()
			obj.Spec.Title = ""
			Expect(k8sClient.Update(ctx, obj)).To(MatchError(ContainSubstring("spec.title")))
		})
	})
})
