# M1 Artifact Examples

All Kama references are names in the object's namespace. Create source PVCs, token
Secrets, caches, and artifacts together in the same namespace.

1. Install Kama and choose a namespace for model resources.
2. Edit storage class, PVC, and private-repository/Secret placeholders. The public
   example is pinned to an immutable model revision; review its model card, size,
   and license before use. Hugging Face revisions should be full immutable
   commit hashes for reproducible imports, even though Kama resolves a tag or branch
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
