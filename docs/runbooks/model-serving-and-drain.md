# Model Serving, Load Failure, and Drain

## First response

Inspect the `ModelDeployment`, its referenced artifact, generated resources, and
transition Events in the same namespace:

```sh
namespace=my-models
deployment=smollm2-cpu
kubectl -n "$namespace" describe modeldeployment "$deployment"
kubectl -n "$namespace" get modeldeployment "$deployment" -o yaml
kubectl -n "$namespace" get modelartifact,deploy,svc,pod,configmap \
  -l "kama.tannerburns.github.io/model-deployment=$deployment"
kubectl -n "$namespace" get events \
  --field-selector "involvedObject.kind=ModelDeployment,involvedObject.name=$deployment" \
  --sort-by=.lastTimestamp
```

Do not edit controller-owned ConfigMaps or Deployments, bypass readiness, add raw
llama-server arguments, or remove finalizers. Change the `ModelDeployment` intent or
repair its artifact and let the controller produce a new fingerprint.

## Artifact is not ready

`ArtifactReady=False` before the first successful load intentionally leaves the
Service without a serving Deployment or ready endpoint.

1. Inspect the referenced `ModelArtifact` and its conditions.
2. Confirm `status.location` contains the claim, subpath, read-only contract, volume
   identity, and any node affinity.
3. Follow the [artifact recovery runbook](artifact-import-and-recovery.md) for source,
   checksum, capacity, or storage failures.
4. Do not point a deployment at an artifact that merely has files on a PVC; it must
   have the controller-issued ready identity and digest.

If an already-loaded Pod remains ready while the artifact temporarily reports
unready, Kama preserves that matching process but freezes rollout. Repair the
artifact-plane condition before changing runtime intent. A changed UID, digest, or
location requires a replacement and is not treated as a transient outage.

## Runtime is loading

A running Pod can legitimately remain unready while llama.cpp maps and initializes
the model. Check Pod readiness and the sanitized supervisor state:

```sh
pod="$(kubectl -n "$namespace" get pod \
  -l "kama.tannerburns.github.io/model-deployment=$deployment" \
  -o jsonpath='{.items[0].metadata.name}')"
kubectl -n "$namespace" get pod "$pod" -o wide
kubectl -n "$namespace" port-forward "pod/$pod" 18081:8081
curl --fail-with-body http://127.0.0.1:18081/state
```

`/startupz` and `/livez` describe the supervisor, not model readiness. Only
`/readyz` indicates that the expected child configuration is accepting inference. It
remains false until llama-server's health, slots, model path, per-slot context, and
compiled build identity agree with controller intent. Accelerator mode additionally
requires one visible CUDA device and full reported layer offload. Do not lower probe
thresholds to turn a long load into a restart loop.

`status.runtime.loadedFingerprint` is a durable record that an exact fingerprint
loaded successfully. It allows a loaded Pod to survive a temporary artifact/cache
or diagnostics outage, but it is not a health signal: current `RuntimeReady`,
`Serving`, Pod readiness, and EndpointSlice state remain authoritative.

## Terminal load or child failure

`RuntimeReady=False` with a load/exit reason is terminal for that Pod. The supervisor
remains alive and unready, and it deliberately does not restart llama-server.

1. Confirm the Pod container restart count remains zero and the Service has no ready
   EndpointSlice address for the failed Pod.
2. Read the supervisor logs and `/state`. They contain bounded runtime facts but must
   not contain prompts, completions, credentials, or request bodies.
3. Check the expected runtime image, artifact digest, placement mode, memory, context,
   concurrency, and KV-cache settings recorded in status.
4. Correct the resource or create a new artifact when the bytes are invalid. A spec
   change creates a new fingerprint and replacement Pod. If the failure was
   transient and no intent changes, delete only the failed Pod to request one clean
   retry.

For accelerator mode, also confirm a single `nvidia.com/gpu` request/limit, a healthy
device plugin, compatible driver/CUDA versions, and the supervisor's sanitized GPU
and offloaded-layer facts. Kama M2 does not fall back to CPU automatically.

## CPU degradation

CPU mode intentionally reports `Degraded=True, reason=CPUOnlyRequested` even when it
is serving. This is not a load failure. Check `Serving=True`, ready replicas, and
endpoint readiness independently. Move to accelerator mode only after ensuring a
compatible node and one full NVIDIA GPU are available.

## Update or deletion is draining

During update or deletion the controller removes readiness before terminating the
child. Existing requests may continue until all active slots are idle or
`spec.runtime.drainTimeout` expires.

```sh
kubectl -n "$namespace" describe modeldeployment "$deployment"
service="$(kubectl -n "$namespace" get modeldeployment "$deployment" \
  -o jsonpath='{.status.serviceRef.name}')"
kubectl -n "$namespace" get endpointslice \
  -l "kubernetes.io/service-name=$service" -o yaml
kubectl -n "$namespace" get pod "$pod" -w
```

Allow the declared drain interval plus the shutdown margin before treating
termination as stuck. An unbounded stream may be terminated at the deadline. Do not
remove the `ModelDeployment` or `ModelArtifact` finalizer to accelerate deletion;
doing so can expose serving traffic to a deleted workload or release mounted storage
before the Pod exits.

## Direct internal request

M2 has no public gateway. Test the stable ClusterIP from a namespace-authorized
debug Pod or use a local port-forward:

```sh
service="$(kubectl -n "$namespace" get modeldeployment "$deployment" \
  -o jsonpath='{.status.serviceRef.name}')"
kubectl -n "$namespace" port-forward "service/$service" 18080:8080
curl --no-buffer --fail-with-body http://127.0.0.1:18080/v1/chat/completions \
  -H 'content-type: application/json' \
  -d '{"model":"smollm2-cpu","messages":[{"role":"user","content":"hello"}],"stream":true}'
```

Port 8081, slot state, and metrics are internal diagnostics and are not a public
serving contract. M4 owns gateway exposure and route policy.
