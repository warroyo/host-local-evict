# host-local-evict

[![Build](https://github.com/warroyo/host-local-evict/actions/workflows/build.yml/badge.svg)](https://github.com/warroyo/host-local-evict/actions/workflows/build.yml)
[![Go version](https://img.shields.io/github/go-mod/go-version/warroyo/host-local-evict)](go.mod)
[![Latest release](https://img.shields.io/github/v/release/warroyo/host-local-evict?include_prereleases)](https://github.com/warroyo/host-local-evict/releases)

You need to put an ESXi host into maintenance mode, but the VMs on it use local storage. vMotion won't work. Draining the Kubernetes nodes isn't enough either — the CAPI Machines are still there, still bound to the host. This tool handles the CAPI side of that.

## How it works

1. Finds all CAPI `Machine` objects in a VKS workload cluster that are pinned to the target ESXi host (via label).
2. Labels each Machine with `cluster.x-k8s.io/delete-machine=""` so CAPI selects them for deletion during scale-down.
3. Scales down the matching entries in `Cluster.spec.topology.workers.machineDeployments[].replicas` to trigger deletion.

The replica change goes through the **Cluster object**, not the MachineDeployment directly. On ClusterClass clusters the topology controller owns the MachineDeployment objects — a direct patch to them gets reverted on the next reconcile.

Run this against the **Supervisor cluster** kubeconfig — that's where Cluster API lives, not the workload cluster.

## Prerequisites

- Go 1.22+
- A kubeconfig pointing at the vSphere Supervisor cluster
- RBAC permissions to:
  - `list` / `get` Machines and Clusters in the target namespace
  - `patch` Machines (to add the delete-machine label)
  - `patch` Clusters (to update `spec.topology.workers.machineDeployments[].replicas`)

## Build

```bash
go mod tidy
go build -o host-local-evict ./cmd
```

Or install directly:

```bash
go install github.com/warroyo/host-local-evict/cmd@latest
```

## Flags

| Flag | Default | Description |
|---|---|---|
| `--cluster` | *(required)* | VKS workload cluster name |
| `--esx-host` | *(required)* | ESXi host FQDN or short name to evacuate |
| `--namespace` | all namespaces | vSphere namespace containing the cluster |
| `--kubeconfig` | `$KUBECONFIG` or `~/.kube/config` | Path to Supervisor kubeconfig |
| `--host-label` | `node.cluster.x-k8s.io/esxi-host` | Machine label key that holds the ESXi host name |
| `--api-version` | autodetect | Override CAPI API version (e.g. `v1beta2`) |
| `--dry-run` | `false` | Print the plan; make no mutations |
| `--yes` | `false` | Skip the confirmation prompt |

## Usage

**Dry-run first — always:**

```bash
./host-local-evict \
  --cluster my-cluster \
  --esx-host esxi01.example.com \
  --namespace my-vsphere-ns \
  --dry-run
```

**Run for real:**

```bash
./host-local-evict \
  --cluster my-cluster \
  --esx-host esxi01.example.com \
  --namespace my-vsphere-ns
```

**Skip confirmation (for automation):**

```bash
./host-local-evict \
  --cluster my-cluster \
  --esx-host esxi01.example.com \
  --namespace my-vsphere-ns \
  --yes
```

**Override the CAPI API version:**

```bash
./host-local-evict ... --api-version v1beta1
```

## Before you run

### Confirm the host-label key

The tool identifies Machines by a label that carries the ESXi host name. The default key is `node.cluster.x-k8s.io/esxi-host`. Check what your environment actually uses:

```bash
kubectl get machines -A --show-labels
```

Find the label whose value matches your ESXi host FQDN or short name. If the key differs, pass it with `--host-label`.

Machines are traced back to their topology MachineDeployment entry via the `topology.cluster.x-k8s.io/deployment-name` label, which CAPI stamps on every Machine. If multiple MachineDeployments exist in the cluster, each Machine's label points to the right one automatically — no extra config needed.

### CAPI API version

The tool checks the server's discovery API and prefers `v1beta2` over `v1beta1`, falling back to whatever is advertised first. Use `--api-version` to override if autodetection picks wrong.

## Watch out for mid-rollout MachineDeployments

> **Important:** The `cluster.x-k8s.io/delete-machine` label only guarantees
> deletion priority during **MachineSet scale-down**. A MachineDeployment
> controls which MachineSet to shrink — if a rollout is in progress with multiple
> live MachineSets, CAPI may shrink the new one and completely skip the labeled
> Machines on the old one.
>
> Before running, confirm no rollout is in progress:
>
> ```bash
> kubectl get machinesets -A -l cluster.x-k8s.io/cluster-name=<your-cluster>
> ```
>
> Each MachineDeployment should have exactly one MachineSet with non-zero replicas,
> and `DESIRED == READY` across the board.

This is a known CAPI constraint. The tool warns at plan time but won't block you.

## What's missing

- **No drain/delete wait:** The tool fires and exits. It doesn't wait for Machines to actually drain and delete. With local storage, `nodeDrainTimeout` is the backstop — if a pod won't drain, CAPI force-deletes after that window. A `--wait` flag should eventually poll `Machine.status.phase` until each Machine reaches `Deleted`.

- **No rollback:** To undo manually: remove the `cluster.x-k8s.io/delete-machine` label from each Machine, then restore the original replica counts in `Cluster.spec.topology.workers.machineDeployments[].replicas`.

- **Standalone Machines:** Machines without a `topology.cluster.x-k8s.io/deployment-name` label get the delete label applied, but their topology entry won't be scaled. These shouldn't exist in a normal VKS cluster, but the tool warns if it encounters them.

- **Error handling:** The first labeling failure aborts the whole run. A future version should collect all failures and report them together.
