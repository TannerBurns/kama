# Artifact and Baseline Serving Examples

All Kama references are names in the object's namespace. Create source PVCs, token
Secrets, caches, and artifacts together in the same namespace.

1. Install Kama and choose a namespace for model resources.
2. Edit storage class, PVC, and private-repository/Secret placeholders. The public
   example is the Apache-2.0 SmolLM2 360M Q8_0 GGUF used by Kama's Hugging Face E2E
   suite. It is pinned to an immutable revision, exact size, and SHA-256 digest.
   Review its model card and license before use. Hugging Face revisions should be
   full immutable commit hashes for reproducible imports, even though Kama resolves
   a tag or branch
   once before publication.
3. Apply one cache and one matching artifact:

   ```sh
   kubectl create namespace my-models
   kubectl -n my-models apply -f examples/modelcache-managed.yaml
   kubectl -n my-models apply -f examples/modelartifact-huggingface-public.yaml
   kubectl -n my-models get modelcache,modelartifact
   ```

For a private repository, create the token Secret without committing its value:

```sh
kubectl -n my-models create secret generic huggingface-token \
  --from-literal=token="$HF_TOKEN"
```

Use `Copy` for a manually populated PVC unless avoiding the second copy is essential.
`Direct` keeps serving the source claim and is a `ValidatedOnce` contract: do not
mutate the validated path after readiness. See the
[artifact recovery runbook](../docs/runbooks/artifact-import-and-recovery.md).

After `smollm2-360m-instruct` reports `Ready=True`, create exactly one baseline
serving workload:

```sh
kubectl -n my-models apply -f examples/modeldeployment-cpu.yaml
kubectl -n my-models get modeldeployment,deploy,svc,pod
```

The CPU example uses model-native context and intentionally reports
`Degraded=True` with reason `CPUOnlyRequested` while it is otherwise healthy. The
[accelerator example](modeldeployment-accelerator.yaml) requests exactly one full
`nvidia.com/gpu`; use it only on a cluster with the NVIDIA device plugin and a
compatible driver. Kama does not automatically move between CPU and GPU in M2.

Both examples declare CPU and memory requests and a memory limit. Users cannot
override runtime images, model paths, raw llama.cpp arguments, GPU quantity, probes,
ports, topology, or replica count. See the
[serving runbook](../docs/runbooks/model-serving-and-drain.md) for direct internal
requests, load failures, and drain behavior.
