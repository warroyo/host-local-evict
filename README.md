# host-local-evict

[![Build](https://github.com/warroyo/host-local-evict/actions/workflows/build.yml/badge.svg)](https://github.com/warroyo/host-local-evict/actions/workflows/build.yml)
[![Go version](https://img.shields.io/github/go-mod/go-version/warroyo/host-local-evict)](go.mod)
[![Latest release](https://img.shields.io/github/v/release/warroyo/host-local-evict?include_prereleases)](https://github.com/warroyo/host-local-evict/releases)

host-local-evict evacuates CAPI Machines from a VMware ESXi host that is attempting to go into maintenence mode. VMs backed by local storage can't vMotion, so this tool marks the affected Machines for deletion and scales down the Cluster topology replicas to trigger controlled removal. This provides an automated clean approach to putting hosts into maintenence mode when the local storage is being used by VKS worker nodes.

## How it works

1. Finds all CAPI `Machine` objects in a VKS workload cluster that are pinned to the target ESXi host (via label).
2. Annotates each Machine with `cluster.x-k8s.io/delete-machine=""` so CAPI selects them for deletion during scale-down.
3. Scales down the matching entries in `Cluster.spec.topology.workers.machineDeployments[].replicas` to trigger deletion.

The replica change goes through the **Cluster object**, not the MachineDeployment directly. On ClusterClass clusters the topology controller owns the MachineDeployment objects — a direct patch to them gets reverted on the next reconcile.

Run this against the **Supervisor cluster** kubeconfig — that's where Cluster API lives, not the workload cluster.

## Prerequisites

- Go 1.26+ (only required for building from source)
- A kubeconfig pointing at the vSphere Supervisor cluster
- **Namespace edit permissions** in the target vSphere namespace

Admission policies on Supervisor clusters block direct CAPI writes from regular user credentials. The tool works around this by creating an ephemeral service account with the minimum required permissions, performing all CAPI writes through that account, and deleting it on exit. Your user credentials only need to be able to manage namespace-scoped RBAC resources (ServiceAccount, Role, RoleBinding).

If you'd rather supply your own service account token, pass `--token $(kubectl create token <sa-name> -n <namespace>)` — the tool skips SA creation entirely in that case.

## Install

**Download a pre-built binary** (Linux and macOS, amd64 and arm64):

```bash
curl -sSfL "https://github.com/warroyo/host-local-evict/releases/latest/download/host-local-evict-$(uname -s | tr '[:upper:]' '[:lower:]')-$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')" -o host-local-evict && chmod +x host-local-evict
```

**Build from source:**

```bash
go mod tidy
go build -o host-local-evict ./cmd
```

**Install with Go:**

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
| `--token` | *(none)* | Bearer token for CAPI writes; skips ephemeral SA creation |
| `--remediate` | `false` | Replace Machines on a new host instead of permanently evicting (no replica scale-down) |
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

## Remediate vs evict

The tool has two modes.

**Evict (default):** Annotates Machines with `cluster.x-k8s.io/delete-machine` and scales down the replica counts in `Cluster.spec.topology.workers.machineDeployments`. Use this when your cluster is sized with a fixed number of worker nodes per physical host — removing a host means the worker count should drop to match. The Machines are deleted and the replica count permanently reflects the smaller fleet.

**Remediate (`--remediate`):** Annotates Machines with `cluster.x-k8s.io/remediate-machine` instead. CAPI deletes each Machine and lets the MachineSet create a replacement on a different host. Replica count stays the same. Use this when you want to maintain a consistent total worker count regardless of which hosts are active — nodes move off the host going into maintenance and land elsewhere.

```bash
./host-local-evict \
  --cluster my-cluster \
  --esx-host esxi01.example.com \
  --namespace my-vsphere-ns \
  --remediate
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

> **Important:** The `cluster.x-k8s.io/delete-machine` annotation only guarantees
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

## Releasing

```bash
make release VERSION=v0.2.0
```

That validates the version format, checks for a clean working tree, tags, and pushes. The GitHub Actions release workflow takes it from there — builds binaries for all four platforms and publishes them as release assets with auto-generated notes.

To build locally before releasing:

```bash
make build          # current platform, version from git tag
make build-all      # all platforms into dist/
```

## What's missing

- **No completion wait:** The tool fires and exits. CAPI handles node drain as part of the scale-down process — `nodeDrainTimeout` is the backstop if a pod stalls. The tool doesn't poll for completion though; a `--wait` flag should eventually poll `Machine.status.phase` until each Machine reaches `Deleted`.

- **No rollback:** To undo manually: remove the `cluster.x-k8s.io/delete-machine` annotation from each Machine, then restore the original replica counts in `Cluster.spec.topology.workers.machineDeployments[].replicas`.

- **Standalone Machines:** Machines without a `topology.cluster.x-k8s.io/deployment-name` label get the delete label applied, but their topology entry won't be scaled. These shouldn't exist in a normal VKS cluster, but the tool warns if it encounters them.

- **Error handling:** The first labeling failure aborts the whole run. A future version should collect all failures and report them together.
