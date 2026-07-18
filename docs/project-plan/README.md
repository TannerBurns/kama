# Kama Modular Project Plan

This directory is the canonical implementation plan for Kama. Each module owns one
architectural concern and links to its dependencies so that it can be refined or
implemented independently without losing the system-level contract.

## Reading order

| Module | Purpose |
|---|---|
| [01 - Product definition](01-product-definition.md) | Goal, users, v1 boundary, non-goals, and success criteria |
| [02 - System architecture](02-system-architecture.md) | Components, control flow, data flow, and invariants |
| [03 - Resource model and APIs](03-resource-model-and-apis.md) | CRDs, ownership, status, lifecycle, and generated resources |
| [04 - Model artifacts and storage](04-model-artifacts-and-storage.md) | Hugging Face/PVC imports, persistent caching, verification, and retention |
| [05 - Runtime topology and placement](05-runtime-topology-and-placement.md) | Fit estimation, profiling, GPUs, CPU fallback, and scheduling |
| [06 - Serving, routing, and autoscaling](06-serving-routing-and-autoscaling.md) | Reverse proxy, slot-aware routing, KEDA, queues, and scale-to-zero |
| [07 - Operations, security, and observability](07-operations-security-and-observability.md) | Isolation, credentials, metrics, upgrades, and runbooks |
| [08 - Delivery roadmap](08-delivery-roadmap.md) | Dependency-ordered milestones and definitions of done |
| [09 - Verification and acceptance](09-verification-and-acceptance.md) | Test strategy, hardware/storage matrices, and release gates |
| [10 - Decisions and open questions](10-decisions-and-open-questions.md) | Accepted decisions, defaults, risks, and validation items |

## Architecture at a glance

```text
Model sources                 Kama control plane
-------------                 ------------------
Hugging Face ----+            ModelArtifact controller ----> import/verify Job
Manual PVC ------+----------> ModelCache / verified GGUF
                                           |
                                           v
Client ---> Kama Gateway ---> ModelDeployment controller ---> topology planner
               |                       |                           |
               +--> KEDA scaler        +--> serving pools <-------+
                                                |
                                                +--> llama-server Pod: 1 GPU
                                                +--> llama-server Pod: N same-node GPUs
                                                +--> hybrid/CPU fallback pool
```

## System invariants

- A serving replica is one complete logical model server and one routable endpoint.
- Extra Pods are independent replicas. A model spread across GPUs in v1 uses one
  Pod and one `llama-server` process with several same-node GPUs.
- The gateway routes to complete replicas, never to individual GPUs or model shards.
- Model storage outlives serving Pods and scale-to-zero.
- Kubernetes remains the scheduling authority. Kama declares resource requests and
  placement constraints; it never manually assigns GPU IDs or binds Pods.
- Requested context, concurrency, and cache precision are not silently reduced.
- Automatic fallback may move from GPU to hybrid CPU/GPU and finally CPU-only, but
  every fallback is visible through status, Events, and metrics.
- Only verified, immutable artifacts are eligible for profiling or serving.

## Target environment

- Kubernetes 1.34 through 1.36 for the first compatibility matrix.
- Linux worker nodes.
- NVIDIA GPU Operator or NVIDIA device plugin plus GPU Feature Discovery.
- KEDA installed for elastic scaling; Prometheus is optional for correctness.
- A filesystem PVC for durable GGUF storage. RWX is recommended for multi-node
  replicas; RWO is supported with node-placement constraints.

## Definition of v1 success

A user can declare a pinned Hugging Face GGUF or an existing PVC model, wait for a
verified persistent artifact, create a `ModelDeployment`, and send a streaming
llama-server request to a stable gateway. Kama chooses a feasible topology, queues
the first request while KEDA scales from zero, routes across ready replicas, uses
additional compatible GPUs under demand, and retains the model after compute scales
back to zero.
