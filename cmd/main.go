// Package main implements host-local-evict, a tool for draining Cluster API
// Machines from a specific ESXi host before that host enters maintenance mode.
// VMs backed by local host storage cannot be vMotion'd, so this tool marks the
// relevant Machines for CAPI-managed deletion and scales down their topology
// MachineDeployment replica counts in the Cluster object so CAPI removes them
// in a controlled way.
//
// Run this against the vSphere Supervisor cluster kubeconfig (where Cluster
// API lives), not the workload cluster kubeconfig.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

// version is set at build time via -ldflags "-X main.version=vX.Y.Z".
var version = "dev"

const (
	capiGroup               = "cluster.x-k8s.io"
	labelClusterName        = "cluster.x-k8s.io/cluster-name"
	labelTopologyDeployName = "topology.cluster.x-k8s.io/deployment-name"
	labelDeleteMachine      = "cluster.x-k8s.io/delete-machine"
	labelControlPlane       = "cluster.x-k8s.io/control-plane"
	defaultHostLabel        = "node.cluster.x-k8s.io/esxi-host"
)

type config struct {
	kubeconfig string
	namespace  string
	cluster    string
	esxHost    string
	hostLabel  string
	apiVersion string
	dryRun     bool
	yes        bool
}

// mdGroup holds Machines that share the same topology MachineDeployment entry.
type mdGroup struct {
	topologyName string   // name in Cluster spec.topology.workers.machineDeployments[]
	namespace    string
	machines     []string // Machine names
	replicas     int64    // current replicas read from Cluster spec
}

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "-version") {
		fmt.Println(version)
		return
	}
	cfg := parseFlags()
	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func parseFlags() config {
	var cfg config

	defaultKubeconfig := filepath.Join(homedir.HomeDir(), ".kube", "config")
	if kc := os.Getenv("KUBECONFIG"); kc != "" {
		defaultKubeconfig = kc
	}

	flag.StringVar(&cfg.kubeconfig, "kubeconfig", defaultKubeconfig, "path to kubeconfig file")
	flag.StringVar(&cfg.namespace, "namespace", "", "vSphere namespace (empty = all namespaces)")
	flag.StringVar(&cfg.cluster, "cluster", "", "VKS cluster name (required)")
	flag.StringVar(&cfg.esxHost, "esx-host", "", "ESXi host FQDN/name to evacuate (required)")
	flag.StringVar(&cfg.hostLabel, "host-label", defaultHostLabel, "Machine label key carrying the ESXi host name")
	flag.StringVar(&cfg.apiVersion, "api-version", "", "CAPI API version override (e.g. v1beta2); default: autodetect")
	flag.BoolVar(&cfg.dryRun, "dry-run", false, "print plan only, perform no mutations")
	flag.BoolVar(&cfg.yes, "yes", false, "skip confirmation prompt")
	flag.Parse()

	var missing []string
	if cfg.cluster == "" {
		missing = append(missing, "--cluster")
	}
	if cfg.esxHost == "" {
		missing = append(missing, "--esx-host")
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "required flags missing: %s\n", strings.Join(missing, ", "))
		flag.Usage()
		os.Exit(1)
	}
	return cfg
}

func run(cfg config) error {
	ctx := context.Background()

	restCfg, err := clientcmd.BuildConfigFromFlags("", cfg.kubeconfig)
	if err != nil {
		return fmt.Errorf("load kubeconfig %q: %w", cfg.kubeconfig, err)
	}

	dynClient, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("create dynamic client: %w", err)
	}

	discClient, err := discovery.NewDiscoveryClientForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("create discovery client: %w", err)
	}

	apiVersion, err := resolveCAPIVersion(discClient, cfg.apiVersion)
	if err != nil {
		return err
	}
	fmt.Printf("CAPI API version: %s/%s\n", capiGroup, apiVersion)

	machineGVR := schema.GroupVersionResource{Group: capiGroup, Version: apiVersion, Resource: "machines"}
	clusterGVR := schema.GroupVersionResource{Group: capiGroup, Version: apiVersion, Resource: "clusters"}

	// Server-side filter by cluster name; host label filter happens client-side
	// because the key is user-configurable and can't be a server-side requirement.
	listOpts := metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", labelClusterName, cfg.cluster),
	}

	var rawList *unstructured.UnstructuredList
	if cfg.namespace != "" {
		rawList, err = dynClient.Resource(machineGVR).Namespace(cfg.namespace).List(ctx, listOpts)
	} else {
		rawList, err = dynClient.Resource(machineGVR).List(ctx, listOpts)
	}
	if err != nil {
		return fmt.Errorf("list machines in cluster %q: %w", cfg.cluster, err)
	}

	// Client-side: keep only worker Machines on the target host.
	// Machines with cluster.x-k8s.io/control-plane label are always skipped —
	// evicting control plane nodes via this tool would break the cluster.
	var matched []unstructured.Unstructured
	var skippedCP int
	for _, m := range rawList.Items {
		labels := m.GetLabels()
		if _, isCP := labels[labelControlPlane]; isCP {
			skippedCP++
			continue
		}
		if labels[cfg.hostLabel] == cfg.esxHost {
			matched = append(matched, m)
		}
	}
	if skippedCP > 0 {
		fmt.Printf("Skipped %d control plane Machine(s) (label %s present).\n", skippedCP, labelControlPlane)
	}

	if len(matched) == 0 {
		fmt.Printf("No Machines found on host %q in cluster %q.\n", cfg.esxHost, cfg.cluster)
		fmt.Printf("Verify the label key with:\n")
		fmt.Printf("  kubectl get machines -A --show-labels\n")
		return nil
	}

	// Determine the namespace for the Cluster object. All Machines of a cluster
	// share its namespace, so use the first match when --namespace is not set.
	clusterNS := cfg.namespace
	if clusterNS == "" {
		clusterNS = matched[0].GetNamespace()
	}

	// Group Machines by their topology MachineDeployment name.
	// This is the name in Cluster spec.topology.workers.machineDeployments[].name,
	// carried on Machines via topology.cluster.x-k8s.io/deployment-name.
	// Machines without this label are noted separately.
	mdMap := map[string]*mdGroup{}
	var standalone []string

	for _, m := range matched {
		topoName := m.GetLabels()[labelTopologyDeployName]
		if topoName == "" {
			standalone = append(standalone, m.GetNamespace()+"/"+m.GetName())
			continue
		}
		if _, ok := mdMap[topoName]; !ok {
			mdMap[topoName] = &mdGroup{topologyName: topoName, namespace: m.GetNamespace()}
		}
		mdMap[topoName].machines = append(mdMap[topoName].machines, m.GetName())
	}

	if len(standalone) > 0 {
		fmt.Printf("WARNING: %d Machine(s) have no %s label; delete-machine will be\n"+
			"applied but replica counts will NOT be adjusted:\n", len(standalone), labelTopologyDeployName)
		for _, s := range standalone {
			fmt.Printf("  %s\n", s)
		}
	}

	// Read current replica counts from the Cluster object's topology spec.
	// Patching the Cluster is the correct path for ClusterClass-based clusters —
	// the topology controller owns the MachineDeployment objects and will overwrite
	// direct patches to them.
	clusterObj, err := dynClient.Resource(clusterGVR).Namespace(clusterNS).Get(ctx, cfg.cluster, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get Cluster %s/%s: %w", clusterNS, cfg.cluster, err)
	}

	topologyMDs, found, err := unstructured.NestedSlice(clusterObj.Object, "spec", "topology", "workers", "machineDeployments")
	if err != nil {
		return fmt.Errorf("read spec.topology.workers.machineDeployments: %w", err)
	}
	if !found {
		return fmt.Errorf("cluster %s/%s has no spec.topology.workers.machineDeployments; is this a ClusterClass cluster?", clusterNS, cfg.cluster)
	}

	replicasByName := make(map[string]int64, len(topologyMDs))
	for _, entry := range topologyMDs {
		mdEntry, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := mdEntry["name"].(string)
		replicasByName[name] = toInt64(mdEntry["replicas"])
	}

	groups := make([]*mdGroup, 0, len(mdMap))
	for _, grp := range mdMap {
		grp.replicas = replicasByName[grp.topologyName]
		groups = append(groups, grp)
	}

	printPlan(cfg, matched, groups, clusterNS)

	if cfg.dryRun {
		fmt.Println("\n[dry-run] No mutations performed.")
		return nil
	}

	if !cfg.yes {
		if !confirm("\nProceed with eviction?") {
			fmt.Println("Aborted.")
			return nil
		}
	}

	// Label each matched Machine so CAPI knows to delete it during scale-down.
	labelPatch, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{
			"labels": map[string]interface{}{
				labelDeleteMachine: "",
			},
		},
	})
	if err != nil {
		return fmt.Errorf("marshal label patch: %w", err)
	}

	for _, m := range matched {
		fmt.Printf("Labeling Machine %s/%s...\n", m.GetNamespace(), m.GetName())
		// TODO: collect per-machine errors and continue rather than aborting on first
		// failure; report all failures at the end with per-machine exit codes.
		if _, err := dynClient.Resource(machineGVR).Namespace(m.GetNamespace()).Patch(
			ctx, m.GetName(), types.MergePatchType, labelPatch, metav1.PatchOptions{},
		); err != nil {
			return fmt.Errorf("label machine %s/%s: %w", m.GetNamespace(), m.GetName(), err)
		}
	}

	// Scale down via the Cluster object's topology spec — not the MachineDeployment
	// directly. For ClusterClass clusters the topology controller owns MachineDeployment
	// objects; direct patches to them are overwritten on the next reconcile.
	//
	// Re-read the Cluster to get a fresh resourceVersion before mutating.
	clusterObj, err = dynClient.Resource(clusterGVR).Namespace(clusterNS).Get(ctx, cfg.cluster, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("re-read Cluster %s/%s before patch: %w", clusterNS, cfg.cluster, err)
	}

	topologyMDs, _, err = unstructured.NestedSlice(clusterObj.Object, "spec", "topology", "workers", "machineDeployments")
	if err != nil {
		return fmt.Errorf("re-read spec.topology.workers.machineDeployments: %w", err)
	}

	for _, grp := range groups {
		newReplicas := grp.replicas - int64(len(grp.machines))
		if newReplicas < 0 {
			newReplicas = 0
		}
		fmt.Printf("Scaling Cluster topology MD %q: %d → %d...\n", grp.topologyName, grp.replicas, newReplicas)

		for i, entry := range topologyMDs {
			mdEntry, ok := entry.(map[string]interface{})
			if !ok {
				continue
			}
			if mdEntry["name"] == grp.topologyName {
				mdEntry["replicas"] = newReplicas
				topologyMDs[i] = mdEntry
				break
			}
		}
	}

	scalePatch, err := json.Marshal(map[string]interface{}{
		"spec": map[string]interface{}{
			"topology": map[string]interface{}{
				"workers": map[string]interface{}{
					"machineDeployments": topologyMDs,
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("marshal Cluster topology patch: %w", err)
	}

	if _, err := dynClient.Resource(clusterGVR).Namespace(clusterNS).Patch(
		ctx, cfg.cluster, types.MergePatchType, scalePatch, metav1.PatchOptions{},
	); err != nil {
		return fmt.Errorf("patch Cluster %s/%s topology replicas: %w", clusterNS, cfg.cluster, err)
	}

	// TODO: poll Machine.status.phase until each Machine reaches "Deleted" before
	// returning success. With local storage, nodes may not drain cleanly (PVs can't
	// move), so nodeDrainTimeout governs how long CAPI waits before force-deleting.
	// Surface a --drain-timeout flag and wait here so callers can block on
	// actual completion rather than fire-and-forget.

	// TODO: implement --rollback: remove the delete-machine label from each Machine
	// and restore the original replica counts in the Cluster topology spec.

	fmt.Println("\nDone. CAPI will now delete the marked Machines.")
	fmt.Printf("Monitor with:\n  kubectl get machines -A -l %s=%s -w\n", labelClusterName, cfg.cluster)

	return nil
}

// resolveCAPIVersion returns the best available cluster.x-k8s.io API version
// from the server, preferring v1beta2 then v1beta1. If override is non-empty
// it is returned immediately without hitting the discovery API.
func resolveCAPIVersion(disc discovery.DiscoveryInterface, override string) (string, error) {
	if override != "" {
		return override, nil
	}

	groups, err := disc.ServerGroups()
	if err != nil {
		return "", fmt.Errorf("discover server API groups: %w", err)
	}

	for _, grp := range groups.Groups {
		if grp.Name != capiGroup {
			continue
		}
		for _, want := range []string{"v1beta2", "v1beta1"} {
			for _, v := range grp.Versions {
				if v.Version == want {
					return want, nil
				}
			}
		}
		// Unexpected version; use whatever the server advertises first.
		if len(grp.Versions) > 0 {
			return grp.Versions[0].Version, nil
		}
	}

	return "", fmt.Errorf("%s API group not found; is this the Supervisor cluster kubeconfig?", capiGroup)
}

// toInt64 converts numeric values from unstructured JSON (float64 or int64) to int64.
func toInt64(v interface{}) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	}
	return 0
}

func printPlan(cfg config, machines []unstructured.Unstructured, groups []*mdGroup, clusterNS string) {
	fmt.Println("\n=== Eviction Plan ===")
	fmt.Printf("Cluster:    %s/%s\n", clusterNS, cfg.cluster)
	fmt.Printf("ESXi host:  %s\n", cfg.esxHost)
	fmt.Printf("Host label: %s\n", cfg.hostLabel)

	fmt.Printf("\nMachines to evict (%d):\n", len(machines))
	for _, m := range machines {
		fmt.Printf("  %s/%s\n", m.GetNamespace(), m.GetName())
	}

	if len(groups) > 0 {
		fmt.Println("\nCluster topology MachineDeployment scale-downs:")
		for _, grp := range groups {
			newReplicas := grp.replicas - int64(len(grp.machines))
			if newReplicas < 0 {
				newReplicas = 0
			}
			fmt.Printf("  %q  replicas: %d → %d  (removing %d)\n",
				grp.topologyName, grp.replicas, newReplicas, len(grp.machines))
		}
	}

	fmt.Println()
	fmt.Println("WARNING: The cluster.x-k8s.io/delete-machine label guarantees deletion")
	fmt.Println("priority only during MachineSet scale-down. If a MachineDeployment is")
	fmt.Println("mid-rollout with multiple live MachineSets, CAPI may shrink a different")
	fmt.Println("MachineSet and leave the labeled Machines running. Verify no rollout is")
	fmt.Println("in progress before continuing:")
	fmt.Printf("  kubectl get machinesets -A -l %s=%s\n", labelClusterName, cfg.cluster)
}

func confirm(prompt string) bool {
	fmt.Printf("%s [y/N] ", prompt)
	scanner := bufio.NewScanner(os.Stdin)
	if scanner.Scan() {
		return strings.EqualFold(strings.TrimSpace(scanner.Text()), "y")
	}
	return false
}
