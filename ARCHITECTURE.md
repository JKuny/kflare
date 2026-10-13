# kflare architecture

This document explains how kflare's controllers behave and why: how they find and own Cloudflare resources, when they
notice drift, how they handle errors, and what happens on deletion. It describes the code as it is today, including
its known limitations. The phased roadmap lives in [CLAUDE.md](./CLAUDE.md).

## Resource model

```
Secret (API token)
  └─ CloudflareAccount (cluster-scoped)
       ├─ Zone (namespaced)
       │    ├─ DNSRecord (same namespace, via zoneRef)
       │    ├─ WorkerRoute (same namespace, via zoneRef; runs a WorkerScript)
       │    ├─ WAFCustomRule (same namespace, via zoneRef)
       │    └─ RateLimitRule (same namespace, via zoneRef)
       ├─ Tunnel (namespaced)
       │    └─ TunnelConfiguration (same namespace, via tunnelRef)
       ├─ WorkerScript (namespaced; binds KVNamespaces via kvNamespaceRef, R2Buckets via r2BucketRef)
       ├─ KVNamespace (namespaced)
       └─ R2Bucket (namespaced)
```

Every namespaced resource resolves its credentials through this chain at reconcile time: a DNSRecord reads its Zone, the
Zone's CloudflareAccount, and the account's token Secret. Nothing caches a token between reconciles, so rotating the
Secret takes effect on the next reconcile.

Each controller declares a narrow interface over the Cloudflare SDK (for example `DNSRecordAPI`) holding only the calls it
makes. The production client (`pkg/cloudflare.Client`) embeds the SDK and satisfies all of them; tests inject fakes.

## The reconcile loop

Every controller follows the same shape:

1. Handle deletion first if the resource is being deleted (see [Deletion](#deletion)).
2. Add the `kflare.dev/finalizer` finalizer and return; the update re-triggers the reconcile.
3. Resolve the parent resource and credentials. If the parent is missing or not ready, report `Ready=False` and wait
   for the parent's watch event.
4. Observe: fetch the Cloudflare resource by the ID stored in status (a rule: by its ref, see [Rules](#rules)). If there
   is no ID, or Cloudflare says it no longer exists, find an existing resource to adopt or create a new one (see
   [Adoption and ownership](#adoption-and-ownership)).
5. Compare the spec with what Cloudflare reported, and write to Cloudflare only when they differ.
6. Record IDs and metadata in status and set `Ready=True`.

Step 5 is deliberate: an idle reconcile makes read calls only. The live e2e suite checks this for DNSRecord,
WAFCustomRule and RateLimitRule, and TunnelConfiguration's comparison ignores fields Cloudflare adds on its own so an
unchanged config does not bump its version.

## When drift is detected

kflare corrects drift whenever a resource is reconciled, but **a change made in the Cloudflare dashboard or API does not
by itself trigger a reconcile**. Reconciles are triggered by:

- a change to the resource itself, including a metadata-only change such as an annotation;
- a change to a watched dependency (for example a Zone becoming ready re-triggers its DNSRecords; a ConfigMap or Secret
  change re-triggers WorkerScripts that use it);
- a scheduled retry after an error (see below);
- the informer resync, which re-reconciles every resource. kflare does not set a sync period, so it uses
  controller-runtime's default of **10 hours**.

A successful reconcile does not schedule another one. In practice, a record edited in the dashboard can stay changed for
up to about 10 hours before kflare reverts it.

To force a reconcile, change the resource's metadata:

```sh
kubectl annotate dnsrecord my-record kflare.dev/reconcile-at="$(date +%s)" --overwrite
```

What each controller compares when it does reconcile:

| Resource | Drift detected on |
|---|---|
| `CloudflareAccount` | Token validity (re-validated against the account on every reconcile) |
| `Zone` | Zone type and `spec.plan`; recreated if deleted externally |
| `DNSRecord` | Content, TTL, proxied, priority (when set), comment, tags, structured data; recreated if deleted externally |
| `Tunnel` | Existence (recreated if deleted externally); token Secret contents |
| `TunnelConfiguration` | The full ingress configuration |
| `WorkerScript` | Cloudflare's `modified_on` timestamp, because its etag does not change for binding-only edits; the IDs of bound KVNamespaces, so a recreated namespace re-uploads the Worker |
| `KVNamespace` | Title (renamed in place); recreated, empty, if deleted externally |
| `R2Bucket` | Existence only: recreated, empty, if deleted externally. Its name cannot change and its location is fixed at creation. Workers bind a bucket by name, so they keep working with the recreated bucket without a re-upload (verified live) |
| `WorkerRoute` | Pattern and the Worker it runs; recreated if deleted externally, including when Cloudflare deletes it together with its Worker |
| `WAFCustomRule`, `RateLimitRule` | The rule's whole definition: expression, action and its parameters, enabled, description, and for a rate limiting rule how it counts (characteristics, period, requests per period, mitigation timeout, counting expression, origin requests only). Also its place among kflare's rules in the zone and phase (see [Rules](#rules)). Recreated if deleted externally |

## Adoption and ownership

When a resource has no Cloudflare ID yet, kflare looks for an existing Cloudflare object before creating one. This lets
you bring existing infrastructure under management, and it lets a reconcile recover if it created an object but failed
to record its ID in status.

Adoption means kflare takes full ownership: it will overwrite the object to match the spec, and **by default it will
delete the object when the Kubernetes resource is deleted**. If you adopt something kflare did not create, set the
retain policy first (see [Deletion](#deletion)).

| Resource | What gets adopted |
|---|---|
| `Zone` | A zone with `spec.name` in the referenced account. Zones in other accounts the token can reach are never adopted. |
| `DNSRecord` | A record with the same name and type that no other DNSRecord manages. A record whose content (or data) already matches is preferred. If none matches, an existing record is adopted and corrected only when it is the only record with that name and type; otherwise a new record is created alongside the others, so DNSRecords for record sets (several MX, TXT or A records) each find their own. |
| `Tunnel` | The first non-deleted tunnel named `spec.name` in the account. |
| `TunnelConfiguration` | The tunnel's entire ingress configuration. kflare replaces whatever is there. When several TunnelConfigurations point at one Tunnel, the oldest owns it and the others report `TunnelAlreadyConfigured`. |
| `WorkerScript` | No lookup. The first reconcile uploads the script, replacing any existing Worker with the same name. |
| `KVNamespace` | The namespace with the same title in the account (titles are unique per account) unless another KVNamespace manages it, in which case it reports `TitleConflict` and retries every minute. |
| `R2Bucket` | The bucket named `spec.name` in the account's default jurisdiction (names are unique per account) unless another R2Bucket for the same Cloudflare account manages it, in which case it reports `NameConflict` and retries every minute. An adopted bucket keeps its location; `spec.locationHint` only applies when kflare creates the bucket. |
| `WorkerRoute` | The route with the same pattern in the zone (patterns are unique per zone) unless another WorkerRoute manages it, in which case the WorkerRoute reports `PatternConflict` and retries every minute. |
| `WAFCustomRule`, `RateLimitRule` | Nothing. A rule is kflare's only if it carries the ref kflare derives for the resource (see [Rules](#rules)); rules created outside kflare are never touched. |

### DNSRecord, WorkerRoute, KVNamespace and R2Bucket ownership

DNSRecord stores the Cloudflare record ID it manages in the `kflare.dev/record-id` label, WorkerRoute its route ID in
`kflare.dev/route-id` and KVNamespace its namespace ID in `kflare.dev/kv-namespace-id`; each claims the object before doing anything else with it (`internal/controller/ownership.go`).
Adoption skips any object another resource of the same kind already carries in its label, across all namespaces,
because two Zone resources in different namespaces can point at the same Cloudflare zone. Cloudflare record tags would be
a Cloudflare-side alternative, but they are only available on paid plans.

An R2 bucket has no ID, so R2Bucket claims the bucket name in `kflare.dev/r2-bucket-name`. Bucket names are only unique
within an account, so a claim blocks another R2Bucket only when both point at the same Cloudflare account, through the
same CloudflareAccount or two that share an account ID. An R2Bucket that never claimed its bucket, for example one
reporting `NameConflict`, leaves the bucket alone when it is deleted.

This prevents two DNSRecords from fighting over one record. It does **not** protect records that were created outside
kflare: a lone record with a matching name and type is adopted and overwritten. Tools such as external-dns avoid this
with a TXT ownership registry; kflare currently does not.

### Rules

Each rule kind manages one rule per resource in its zone's entry point ruleset for one Rulesets API phase:
`WAFCustomRule` in `http_request_firewall_custom`, the zone's WAF custom rules, and `RateLimitRule` in
`http_ratelimit`, its rate limiting rules. The managed ruleset and Rules kinds planned next work the same way
(`internal/controller/rulesetrule.go`, `rulesetrule_controller.go`). A zone has one entry point per phase, shared by
every rule in that phase, kflare's or not, so kflare:

- Marks each rule it manages with a ref, `kflare-` and the first 32 hex characters of the SHA-256 of
  `<kind>/<namespace>/<name>`, and finds its rule by that ref. The marker is on the Cloudflare side, and because it is
  derived from the resource's identity rather than its UID, a resource restored from a backup finds its rule again.
  A rule without such a ref is never adopted, changed or deleted.
- Writes one rule at a time, and creates the entry point only when the zone has none. It never replaces an entry point
  as a whole: that would replace the rules it does not manage.
- Keeps its own rules in a zone and phase in order of `spec.priority`, lower first, with ties broken by namespace and
  name, across namespaces (two Zones in different namespaces can point at the same Cloudflare zone). Each phase is
  its own list: a WAF custom rule's priority does not order rate limiting rules. Rules created outside kflare keep
  their place. Reordering moves rules without changing them: Cloudflare keeps a moved rule's definition and version
  (verified live). A reconcile that moved rules checks the order again 10 seconds later, in case another resource was
  reordering the same rules at the same time.
- Leaves the entry point in place when its last rule is deleted, as Cloudflare does.

## Error handling

`pkg/cloudflare.IsTerminalError` sorts Cloudflare errors into two groups, and `handleCloudflareError` acts on them:

| Error | Classification | Behavior |
|---|---|---|
| 401, 403, other 4xx | Terminal | `Ready=False` with reason `TerminalError`; no retry. A spec or credential change re-triggers the reconcile. |
| 404 on a known resource ID | Handled first | The resource was deleted externally; kflare adopts or recreates it. |
| 404 elsewhere | Terminal | As above. |
| 429, 5xx, network errors | Retryable | `Ready=False` with reason `APIError`; the error is returned so controller-runtime retries with exponential back-off (5 ms doubling up to about 16 minutes per resource). |

cloudflare-go names its 401 error `AuthorizationError` and its 403 error `AuthenticationError`, the reverse of what the
names suggest; `pkg/cloudflare/errors.go` documents the mapping.

The Rulesets API reports what the zone's plan does not allow (a regular expression, the Log action, a custom block
response in a WAF custom rule, a rate limiting period other than 10 seconds, more rules than the plan allows) as a
400, often without an error code. These are terminal like any other 4xx, and the condition carries Cloudflare's
message.

Failures caused by something a resource does not watch, such as a missing account or token Secret, retry every minute
with reason `AccountNotFound`, `AccountNotReady`, `SecretNotFound` or `TokenKeyMissing`. Terminal errors are also
retried by the 10-hour resync.

## Deletion

Each resource carries `kflare.dev/finalizer`. On deletion, kflare deletes the Cloudflare object and then removes the
finalizer. Set the `kflare.dev/deletion-policy: retain` annotation to keep the Cloudflare object instead:

```yaml
metadata:
  annotations:
    kflare.dev/deletion-policy: retain
```

Per-resource details:

- **Tunnel** removes the tunnel's connections first, which immediately disconnects any running `cloudflared`, then deletes
  the tunnel.
- **TunnelConfiguration** resets the tunnel to its catch-all rule rather than deleting anything.
- **WorkerScript** deletes the Worker only if kflare uploaded it.
- **WorkerScript** deletion also removes the Worker's routes: Cloudflare deletes them with the Worker. A WorkerRoute that
  still references it reports `WorkerScriptNotFound` and recreates its route once the Worker is uploaded again.
- **KVNamespace** deletion deletes the namespace **and all of its data**. Set the retain policy on namespaces holding data
  you need to keep.
- **WAFCustomRule** and **RateLimitRule** deletion deletes the resource's rule, found by its ref. The zone's other
  rules and its entry point stay.
- **R2Bucket** deletion deletes only an empty bucket: kflare never deletes objects. Cloudflare refuses to delete a bucket
  that holds objects (409, code 10008), so the R2Bucket keeps its finalizer and reports `BucketNotEmpty`, checking again
  every minute until the bucket is emptied or the retain policy is set.

Deleting resources in any order, for example a namespace or `kubectl delete -f` on a directory, must not strand a
resource that still needs its parent for its own cleanup. Parents therefore keep their finalizer, reporting
`Ready=False` with reason `InUse` and the resources they wait for, until their dependents are gone
(`internal/controller/protection.go`):

- A `CloudflareAccount` waits for the Zones, Tunnels, WorkerScripts, KVNamespaces and R2Buckets that reference it.
- A `Zone` waits for its DNSRecords, WorkerRoutes, WAFCustomRules and RateLimitRules.
- A `Tunnel` waits for its TunnelConfigurations.
- A `KVNamespace` waits for the WorkerScripts bound to it through `kvNamespaceRef`. Cloudflare deletes a namespace a
  Worker is bound to, but every later upload of that Worker then fails.
- An `R2Bucket` waits for the WorkerScripts bound to it through `r2BucketRef`, for the same reason: Cloudflare deletes a
  bucket a Worker is bound to, then refuses every upload of that Worker (code 10085, verified live).
- The account's token Secret carries `kflare.dev/token-protection` until no `CloudflareAccount` references it.

Each child therefore cleans up with its own deletion policy even when its parent is retained. The live e2e suite deletes
everything in one pass and checks Cloudflare afterwards.

A DNSRecord, WorkerRoute, WAFCustomRule, RateLimitRule or TunnelConfiguration whose parent is nevertheless gone (its
finalizer was removed by hand) cannot resolve credentials. It skips the Cloudflare call so its own deletion does not
hang, and leaves its Cloudflare object in place.

## Known limitations

- **Drift is not corrected promptly.** Out-of-band changes wait for the next trigger, at worst the 10-hour resync. A
  configurable periodic requeue on successful reconciles would bound this; it needs to stay within Cloudflare's API
  rate limits when many resources are managed.
- **Adoption is implicit.** Zone, DNSRecord, Tunnel, WorkerRoute, KVNamespace and R2Bucket adopt matching objects
  automatically, WorkerScript overwrites a same-named Worker, and the default deletion policy applies to adopted objects.
  An explicit opt-in for adopting objects kflare did not create would make this safer.
- **No ownership marker on the Cloudflare side, except for rules.** kflare tracks what it owns only in Kubernetes, so if
  the cluster's resources are lost, it cannot tell its objects apart from anyone else's. Rules are the exception: their
  ref marks them (see [Rules](#rules)).
- **Rule order covers kflare's rules only.** A rule created outside kflare keeps its place, so it cannot be ordered
  relative to kflare's rules.
- **Pinned to the legacy SDK.** kflare uses cloudflare-go v0.119, the newest of the v0 line. Some limitations come
  from it: Tunnel renames are impossible because `UpdateTunnel` omits the tunnel ID from the request path,
  `originRequest` timeouts are not exposed because `TunnelDuration` does not round-trip, and R2Bucket manages buckets
  in the default jurisdiction only: the SDK cannot send the `cf-r2-jurisdiction` header, and its Worker R2 binding has
  no jurisdiction field.
