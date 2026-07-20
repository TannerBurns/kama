# Artifact Storage Qualification

Kama's artifact-plane functional storage floor requires a filesystem volume to pass:

- regular-file `mmap`;
- durable file and directory `fsync`;
- same-filesystem atomic rename;
- a read-only remount and read check;
- filesystem capacity/free-space reporting; and
- recovery after importer and manager restart.

The `ModelCache` probe checks write, file and directory `fsync`, file and directory
atomic rename, advisory-lock exclusion, `mmap`, and capacity reporting before
declaring writable storage ready. The qualification harness must separately prove
read-only remount and restart recovery. RWX is recommended for cross-node reuse;
direct RWO is supported with the PV's node constraints, and direct ROX is read-only
multi-node storage.

The probe implementation and repository tests establish this contract, not a storage
qualification result. The self-contained artifact-plane E2E suite may establish the
functional floor with ephemeral test storage inside its runner. Such a result closes
the M1 storage scenarios but must not be entered as evidence for a named production
CSI driver/backend.

## End-to-end storage results

Record a successful self-contained workflow storage job here after it passes every
artifact-plane storage scenario. The storage result can be recorded while the
independent private Hugging Face lane remains open; this table records functional
semantics only, not overall milestone qualification or a production support matrix.

| Date | Commit | Workflow run | Kubernetes | Test driver/service | RWX cross-node | RWO topology | ENOSPC and publication safety | Evidence |
|---|---|---|---|---|---|---|---|---|
| 2026-07-20 | [`5a73901`](https://github.com/TannerBurns/kama/commit/5a7390176fd6eecf37a5f6803197a87abe92fa40) | [run 29763107454 / job 88422393596](https://github.com/TannerBurns/kama/actions/runs/29763107454/job/88422393596) | 1.36.1 | NFS CSI 4.13.4 with in-runner NFS service | Pass: two-node concurrent read-only checksum and `mmap` | Pass: matching node scheduled; wrong node rejected by PV affinity | Pass: real tmpfs ENOSPC; ready bytes/status unchanged | `m1-functional-29763107454-1` |

## Named production compatibility results

No production CSI implementation has completed a named compatibility qualification
in this repository yet. This does not block M1 functional acceptance. It means Kama
currently publishes a functional contract, not a universal throughput SLA or a named
production StorageClass support claim.

Record every completed environment below; do not generalize results to another CSI,
backend, mount option, or Kubernetes version.

| Date | Kubernetes | CSI driver/version | Backend and mount options | Access mode | Probe | Import throughput | Cold/warm load | Evidence |
|---|---|---|---|---|---|---|---|---|
| Pending | - | - | - | - | - | - | - | Named compatibility gate not yet run |
