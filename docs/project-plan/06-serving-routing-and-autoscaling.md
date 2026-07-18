# Serving, Routing, and Autoscaling

## Outcome

Kama provides one stable, llama-server-compatible inference gateway. It routes each
request to one complete ready replica, holds bounded cold-start traffic, and supplies
demand metrics to KEDA for scale-to-zero and horizontal scaling.

## Gateway contract

### Public surface

- OpenAI-style endpoints select a deployment through the JSON `model` field.
- Supported llama-server inference endpoints that lack a model field require an
  `X-Kama-Model` header or an explicit model-scoped path.
- `/v1/models` is aggregated from ready/declaratively available model aliases.
- SSE streaming, response headers, client cancellation, backpressure, and long
  inference timeouts are preserved without response buffering.
- Kama pins and conformance-tests the supported llama-server route matrix rather than
  promising compatibility with arbitrary upstream builds.

### Internal surface

Each serving pool has a ClusterIP Service exposing the pinned native llama-server API
for health, metrics, slots, and controlled diagnostics. Model load/unload, slot-state
mutation, file/download, and other lifecycle endpoints are not exposed through the
public gateway because Kama owns that state.

External authentication, TLS policy, and organization rate limits live in the
Ingress/Gateway in front of Kama. Kama still enforces model existence, namespace
authorization boundaries, queue bounds, and safe route selection.

## Endpoint lifecycle

- Startup probes allow long GGUF load and warmup periods.
- Readiness becomes true only when llama-server reports loaded and the supervisor has
  validated its resolved runtime.
- Liveness is conservative and does not restart a server merely because all slots are
  busy or a load is slow.
- Termination first removes readiness, stops new routing, drains active streams, and
  exits after the configured grace period.
- A backend becomes routable only as a complete replica; GPUs and worker containers
  are never independently registered.

## Capacity-aware routing

The gateway watches ready endpoints and polls or consumes internal slot/metrics data.
For each request it:

1. Resolves the model alias and compatible serving pools.
2. Excludes unready, loading, draining, saturated, or incompatible endpoints.
3. Prefers faster primary profiles over fallback profiles.
4. Scores replicas by `(active + deferred) / usableSlots`, then recent latency.
5. Proxies to the best endpoint and tracks active work until completion/cancellation.

Prompt/KV state remains process-local. Optional sticky affinity may improve cache reuse
later but cannot be required for standard full-history requests.

Retries are allowed only for a connection failure before a backend can have accepted
the request and before response bytes are emitted. Generation is not assumed
idempotent.

## Cold-start queue

- The gateway keeps queues in memory; prompts are not persisted to Kubernetes or a
  database.
- Default per-model limits are 100 queued requests, a 10-minute cold-start timeout,
  and configurable maximum request-body size.
- A queued request continuously contributes activation demand.
- Queue overflow returns `429` with `Retry-After`.
- Failure to obtain a ready feasible replica before the timeout returns `503` with
  deployment status context and `Retry-After`.
- If a gateway Pod restarts, affected clients retry; HA gateway replicas prevent one
  Pod from being the only activation path.

These defaults are release-tuning inputs and are tracked in the decision register.

## KEDA integration

KEDA is the required v1 autoscaling controller. Kama implements an external scaler
rather than chaining the KEDA HTTP add-on into a second model-aware proxy.

### Scaler behavior

- A `kama-scaler` gRPC service implements KEDA metric and activation streaming.
- It discovers all gateway Pods and aggregates per-model queued, active, arrival-rate,
  and usable-slot signals from authenticated internal endpoints.
- The controller creates one KEDA `ScaledObject` per independently scalable serving
  pool.
- `minReplicas` defaults to zero. `maxReplicas: auto` resolves to theoretical
  compatible capacity, bounded by user limits and namespace policy.
- Demand units are `(queued + active-weighted work) / target usable slots`; profile
  throughput supplies weighting when pools have different performance.
- Activation streaming wakes zero replicas promptly rather than waiting for a long
  polling interval.
- The external metric stays active for queued and in-flight streams, preventing
  scale-down during a response.
- Scale-down begins only after zero queue, zero active work, and the default 10-minute
  idle cooldown.

KEDA changes replica counts on generated Deployments. The Kama reconciler owns
templates and limits but never continually overwrites KEDA's live replica value.

### Ordered serving pools

A deployment may generate a full-GPU primary pool, hybrid fallback pool, and CPU-only
last-resort pool. The scaler allocates demand to the fastest feasible pool first.
Fallback starts after the primary is unschedulable beyond the placement timeout or
cannot satisfy bounded demand. When faster capacity returns, new requests move to it
and slower pools drain to zero.

## Saturation and scarcity

- Requested replica counts may remain pending when the cluster has no schedulable
  accelerator. Kama reports pending capacity rather than overcommitting GPUs.
- The gateway continues bounded queuing and then rejects overload explicitly.
- `maxReplicas: auto` does not reserve every GPU or keep idle replicas. Compatible
  capacity is consumed only while demand exists.
- Namespace ResourceQuota and PriorityClass are honored; Kama does not implement
  hidden preemption or cross-tenant fairness in v1.

## Metrics

Gateway/scaler metrics include requests, active streams, queue depth/age, rejects by
reason, per-backend slot pressure, routing decisions, TTFT, tokens/second, request
latency, cold-start duration, scale demand, desired/ready replicas, and fallback use.

Backend llama-server metrics remain internal and are relabeled with artifact,
deployment, pool, profile, and replica identifiers without exposing prompt content.

## Acceptance criteria

- Chat/completion streams pass through without buffering and cancellation reaches the
  selected backend.
- Requests distribute according to usable slot pressure rather than round robin.
- Administrative endpoints are inaccessible publicly but available through protected
  internal diagnostics.
- A first request at zero replicas activates KEDA, waits, and receives the completed
  response from the newly ready model.
- Active streams prevent scale-down; idle models return to zero after cooldown.
- Queue limits produce deterministic `429`/`503` behavior.
- Demand scales complete replicas and never individual GPUs or shards.
- Gateway/scaler restart and duplicate activation signals converge without duplicate
  persistent request state.
- GPU scarcity activates an allowed fallback or reports bounded unavailability.

## References

- [llama-server API, slots, health, and metrics](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md)
- [KEDA external scalers](https://keda.sh/docs/2.21/concepts/external-scalers/)
- [KEDA ScaledObject specification](https://keda.sh/docs/2.21/reference/scaledobject-spec/)
- [Kubernetes readiness and startup probes](https://kubernetes.io/docs/concepts/configuration/liveness-readiness-startup-probes/)
- [Kubernetes EndpointSlices](https://kubernetes.io/docs/concepts/services-networking/endpoint-slices/)
