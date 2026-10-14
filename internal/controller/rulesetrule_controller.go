/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"fmt"
	"slices"
	"time"

	cf "github.com/cloudflare/cloudflare-go"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
	cfpkg "github.com/JonathanGraniero/kflare/pkg/cloudflare"
	"github.com/JonathanGraniero/kflare/pkg/reconciler"
)

// Every rule kind (WAFCustomRule, RateLimitRule, and the kinds to come)
// reconciles the same way: wait for the Zone, resolve the account's token,
// write the one rule the resource manages with syncRulesetRule
// (rulesetrule.go), and delete it with the resource. The kind's own
// controller supplies only what differs: its phase and how its spec becomes a
// Cloudflare rule.

// rulesetRuleRecheckInterval is how soon a resource that reordered rules
// checks the order again: another resource may have been reordering the same
// rules at the same time, from an older view of the cluster.
const rulesetRuleRecheckInterval = 10 * time.Second

// rulesetRuleObject is a kflare resource that manages one rule in a zone's
// Rulesets phase. Every rule kind's API type implements it.
type rulesetRuleObject interface {
	conditionedObject
	GetRulesetRuleSpec() *cloudflarev1alpha1.RulesetRuleSpec
	GetRulesetRuleStatus() *cloudflarev1alpha1.RulesetRuleStatus
}

// rulesetRuleKind is what the shared reconciler needs to know about a rule
// kind.
type rulesetRuleKind struct {
	// name is the kind's name. It is part of the ref of every rule of the kind.
	name string
	// phase is the Rulesets phase the kind's rules are in.
	phase     string
	newObject func() rulesetRuleObject
	newList   func() client.ObjectList
	// definition returns the rule a resource of the kind asks for, without
	// its ref. It sets every field the rule should have: an update replaces
	// the whole rule.
	definition func(rulesetRuleObject) cf.RulesetRule
}

// rulesetRuleKinds lists every rule kind. The rules of all the kinds that
// share a phase are kept in one order (see syncRulesetRule).
var rulesetRuleKinds = []rulesetRuleKind{wafCustomRuleKind, rateLimitRuleKind}

// rulesetRuleReconciler reconciles the resources of one rule kind. Each kind's
// exported reconciler delegates to one.
type rulesetRuleReconciler struct {
	client.Client
	kind   rulesetRuleKind
	newAPI func(token string) (RulesetRuleAPI, error)
}

func (r *rulesetRuleReconciler) reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	obj := r.kind.newObject()
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion before anything else.
	if !obj.GetDeletionTimestamp().IsZero() {
		return r.reconcileDelete(ctx, obj)
	}

	// Ensure the finalizer is present before doing any work. On the first
	// reconcile it is absent: add it and return; the update re-triggers this
	// reconcile.
	added, err := reconciler.EnsureFinalizer(ctx, r.Client, obj)
	if err != nil {
		return ctrl.Result{}, err
	}
	if added {
		logger.Info("Added finalizer", "rule", req.NamespacedName)
		return ctrl.Result{}, nil
	}

	// The Zone watch re-triggers this reconcile when the Zone appears or
	// becomes ready.
	zone := &cloudflarev1alpha1.Zone{}
	zoneKey := types.NamespacedName{Name: obj.GetRulesetRuleSpec().ZoneRef.Name, Namespace: obj.GetNamespace()}
	if err := r.Get(ctx, zoneKey, zone); err != nil {
		return ctrl.Result{}, updateNotReady(ctx, r.Client, obj, "ZoneNotFound",
			fmt.Sprintf("Zone %q not found: %v", zoneKey.Name, err))
	}
	if !isZoneReady(zone) {
		return ctrl.Result{}, updateNotReady(ctx, r.Client, obj, "ZoneNotReady",
			fmt.Sprintf("Zone %q is not ready", zoneKey.Name))
	}

	_, token, credErr := resolveAccountToken(ctx, r.Client, zone.Spec.AccountRef.Name, true)
	if credErr != nil {
		return notReadyRetryAfter(ctx, r.Client, obj, credErr)
	}
	cfAPI, err := r.newAPI(token)
	if err != nil {
		return invalidToken(ctx, r.Client, obj, err)
	}

	zoneID := zone.Status.CloudflareMetadata.ZoneID
	peers, err := r.peers(ctx, zoneID)
	if err != nil {
		return ctrl.Result{}, err
	}
	rule := r.kind.definition(obj)
	rule.Ref = rulesetRuleRef(r.kind.name, obj.GetNamespace(), obj.GetName())
	state, err := syncRulesetRule(ctx, cfAPI, zoneID, r.kind.phase, rule, peers)
	if err != nil {
		return handleCloudflareError(ctx, r.Client, obj, err)
	}
	if state.wrote {
		logger.Info("Wrote rule", "ruleID", state.ruleID, "version", state.version)
	}
	if state.moved {
		logger.Info("Reordered rules", "zoneID", zoneID, "phase", r.kind.phase)
	}

	obj.GetRulesetRuleStatus().CloudflareMetadata = cloudflarev1alpha1.RulesetRuleCloudflareMetadata{
		ZoneID:    zoneID,
		RulesetID: state.rulesetID,
		RuleID:    state.ruleID,
		Ref:       rule.Ref,
		Version:   state.version,
	}
	if err := updateReady(ctx, r.Client, obj, "Synced", "Rule is synced with Cloudflare"); err != nil {
		return ctrl.Result{}, err
	}
	if state.moved {
		return ctrl.Result{RequeueAfter: rulesetRuleRecheckInterval}, nil
	}
	return ctrl.Result{}, nil
}

// peers lists the rules kflare manages in zoneID and the kind's phase: those
// of every kind writing to the phase, in every namespace, since Zones in
// different namespaces can point at the same Cloudflare zone. Resources being
// deleted are left out: their rules are about to go.
func (r *rulesetRuleReconciler) peers(ctx context.Context, zoneID string) ([]rulesetRulePeer, error) {
	var peers []rulesetRulePeer
	zoneIDs := map[types.NamespacedName]string{}
	for _, kind := range rulesetRuleKinds {
		if kind.phase != r.kind.phase {
			continue
		}
		list := kind.newList()
		if err := r.List(ctx, list); err != nil {
			return nil, err
		}
		items, err := meta.ExtractList(list)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			obj, ok := item.(rulesetRuleObject)
			if !ok {
				return nil, fmt.Errorf("unexpected %T in a %s list", item, kind.name)
			}
			if !obj.GetDeletionTimestamp().IsZero() {
				continue
			}
			spec := obj.GetRulesetRuleSpec()
			key := types.NamespacedName{Name: spec.ZoneRef.Name, Namespace: obj.GetNamespace()}
			id, seen := zoneIDs[key]
			if !seen {
				zone := &cloudflarev1alpha1.Zone{}
				if err := r.Get(ctx, key, zone); err != nil && !apierrors.IsNotFound(err) {
					return nil, err
				}
				id = zone.Status.CloudflareMetadata.ZoneID
				zoneIDs[key] = id
			}
			if id != zoneID {
				continue
			}
			peers = append(peers, rulesetRulePeer{
				ref:       rulesetRuleRef(kind.name, obj.GetNamespace(), obj.GetName()),
				priority:  spec.Priority,
				namespace: obj.GetNamespace(),
				name:      obj.GetName(),
				kind:      kind.name,
			})
		}
	}
	return peers, nil
}

// reconcileDelete handles the deletion lifecycle: unless the retain policy is
// set, it deletes the rule from Cloudflare, then removes the finalizer.
func (r *rulesetRuleReconciler) reconcileDelete(ctx context.Context, obj rulesetRuleObject) (ctrl.Result, error) {
	// Safety check: if the finalizer is already gone, there is nothing to do.
	if !controllerutil.ContainsFinalizer(obj, reconciler.Finalizer) {
		return ctrl.Result{}, nil
	}

	if !reconciler.RetainOnDelete(obj) {
		if err := r.deleteCloudflareRule(ctx, obj); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Remove the finalizer to allow Kubernetes to complete the deletion.
	_, err := reconciler.RemoveFinalizer(ctx, r.Client, obj)
	return ctrl.Result{}, err
}

// deleteCloudflareRule deletes the resource's rule from Cloudflare, resolving
// Zone → Account → Secret for credentials.
//
// The Zone waits for its rules before it is deleted, so it is normally still
// there. If it is gone anyway (its finalizer was removed by hand), there are
// no credentials to delete with and the rule stays in Cloudflare.
func (r *rulesetRuleReconciler) deleteCloudflareRule(ctx context.Context, obj rulesetRuleObject) error {
	zone := &cloudflarev1alpha1.Zone{}
	zoneKey := types.NamespacedName{Name: obj.GetRulesetRuleSpec().ZoneRef.Name, Namespace: obj.GetNamespace()}
	if err := r.Get(ctx, zoneKey, zone); err != nil {
		if apierrors.IsNotFound(err) {
			log.FromContext(ctx).Info("Zone is gone, skipping Cloudflare deletion", "zone", zoneKey.Name)
			return nil
		}
		return err
	}

	// The rule is in the zone of the last sync. Without one, a sync may still
	// have written the rule before failing to record it, so look in the
	// Zone's zone; a Zone that never had one never reached Cloudflare.
	zoneID := obj.GetRulesetRuleStatus().CloudflareMetadata.ZoneID
	if zoneID == "" {
		zoneID = zone.Status.CloudflareMetadata.ZoneID
	}
	if zoneID == "" {
		return nil
	}

	_, token, credErr := resolveAccountToken(ctx, r.Client, zone.Spec.AccountRef.Name, false)
	if credErr != nil {
		return credErr
	}
	cfAPI, err := r.newAPI(token)
	if err != nil {
		return err
	}
	return deleteRulesetRule(ctx, cfAPI, zoneID, r.kind.phase,
		rulesetRuleRef(r.kind.name, obj.GetNamespace(), obj.GetName()))
}

// setup registers the kind's controller, with rec as its reconciler. It
// watches Zones, so a rule waiting for its Zone is reconciled as soon as the
// Zone is ready, and IPLists, so a rule that uses a list is written once the
// list exists.
func (r *rulesetRuleReconciler) setup(mgr ctrl.Manager, rec reconcile.Reconciler) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(r.kind.newObject()).
		Watches(&cloudflarev1alpha1.Zone{}, handler.EnqueueRequestsFromMapFunc(r.rulesForZone)).
		Watches(&cloudflarev1alpha1.IPList{}, handler.EnqueueRequestsFromMapFunc(r.rulesUsingList)).
		Complete(rec)
}

// The rule kinds' controllers watch IPLists.
//+kubebuilder:rbac:groups=kflare.dev,resources=iplists,verbs=get;list;watch

// rulesUsingList maps an IPList event to the kind's resources, in any
// namespace, whose expression uses the list, once the list exists in
// Cloudflare. Cloudflare refuses a rule that uses a list the account does not
// have (code 20127, a terminal error), so a rule applied together with its
// list may fail first; this writes it once the list is there.
func (r *rulesetRuleReconciler) rulesUsingList(ctx context.Context, obj client.Object) []reconcile.Request {
	ipList, ok := obj.(*cloudflarev1alpha1.IPList)
	if !ok || ipList.Status.CloudflareMetadata.ListID == "" {
		return nil
	}
	list := r.kind.newList()
	if err := r.List(ctx, list); err != nil {
		return nil
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, item := range items {
		rule, ok := item.(rulesetRuleObject)
		if ok && slices.Contains(listsInExpression(rule.GetRulesetRuleSpec().Expression), ipList.Spec.Name) {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: rule.GetName(), Namespace: rule.GetNamespace()},
			})
		}
	}
	return reqs
}

// rulesForZone maps a Zone event to the kind's resources in that zone.
func (r *rulesetRuleReconciler) rulesForZone(ctx context.Context, zone client.Object) []reconcile.Request {
	list := r.kind.newList()
	if err := r.List(ctx, list, client.InNamespace(zone.GetNamespace())); err != nil {
		return nil
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, item := range items {
		obj, ok := item.(rulesetRuleObject)
		if ok && obj.GetRulesetRuleSpec().ZoneRef.Name == zone.GetName() {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()},
			})
		}
	}
	return reqs
}

// defaultRulesetRuleAPI is the production factory: it delegates to cfpkg.New
// so that the returned *cfpkg.Client satisfies RulesetRuleAPI.
func defaultRulesetRuleAPI(token string) (RulesetRuleAPI, error) {
	return cfpkg.New(token)
}
