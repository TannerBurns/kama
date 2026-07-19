# M1 Storage Qualification

Kama's M1 functional storage floor requires a filesystem volume to pass:

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
qualification result. Fixture-backed Kind uses static local volumes and must not be
entered below as production CSI evidence.

## Qualification results

No production CSI implementation has completed the strict live M1 qualification in
this repository yet. Therefore Kama currently publishes a functional contract, not a
universal throughput SLA or a named production StorageClass support claim.

Record every completed environment below; do not generalize results to another CSI,
backend, mount option, or Kubernetes version.

| Date | Kubernetes | CSI driver/version | Backend and mount options | Access mode | Probe | Import throughput | Cold/warm load | Evidence |
|---|---|---|---|---|---|---|---|---|
| Pending | - | - | - | - | - | - | - | Strict live gate not yet run |
