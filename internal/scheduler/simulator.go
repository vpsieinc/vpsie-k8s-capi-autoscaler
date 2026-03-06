package scheduler

import (
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/vpsieinc/vpsie-cluster-scaler/internal/utilization"
	"github.com/vpsieinc/vpsie-cluster-scaler/internal/vpsie"
)

// Simulator evaluates whether downscaling to a smaller plan is safe.
type Simulator interface {
	// SimulateDownscale checks if all non-DaemonSet, non-mirror pods can be
	// scheduled onto nodeCount virtual nodes with the candidate plan's capacity.
	SimulateDownscale(
		nodes []utilization.NodeUtilization,
		candidatePlan vpsie.Plan,
		nodeCount int,
	) SimulationResult
}

// SimulationResult holds the outcome of a scheduling simulation.
type SimulationResult struct {
	// Safe is true if all pods can be placed on the candidate nodes.
	Safe bool

	// BlockingPods lists pods that cannot be scheduled on the candidate plan.
	BlockingPods []BlockingPod

	// Message is a human-readable summary.
	Message string
}

// BlockingPod identifies a pod that blocks downscaling and why.
type BlockingPod struct {
	Name      string
	Namespace string
	Reason    string
}

// DefaultSimulator implements Simulator with first-fit-decreasing bin packing.
type DefaultSimulator struct{}

// NewSimulator creates a new DefaultSimulator.
func NewSimulator() *DefaultSimulator {
	return &DefaultSimulator{}
}

// virtualNode represents a candidate node for bin packing.
type virtualNode struct {
	labels       map[string]string
	taints       []corev1.Taint
	remainCPU    int64 // millicores
	remainMemory int64 // bytes
}

// SimulateDownscale checks if all workload pods fit on nodeCount virtual nodes
// with the candidate plan's capacity.
func (s *DefaultSimulator) SimulateDownscale(
	nodes []utilization.NodeUtilization,
	candidatePlan vpsie.Plan,
	nodeCount int,
) SimulationResult {
	if nodeCount <= 0 {
		return SimulationResult{
			Safe:    false,
			Message: "nodeCount must be > 0",
		}
	}

	// Collect all pods from all nodes, excluding DaemonSet and mirror pods
	var pods []corev1.Pod
	for _, n := range nodes {
		for _, pod := range n.Pods {
			if isDaemonSetPod(&pod) || isMirrorPod(&pod) {
				continue
			}
			pods = append(pods, pod)
		}
	}

	// Collect labels and taints from existing nodes (they stay the same after resize).
	// All nodes in a MachineDeployment are homogeneous, so use the first node.
	var nodeLabels map[string]string
	var nodeTaints []corev1.Taint
	if len(nodes) > 0 {
		nodeLabels = nodes[0].Labels
		nodeTaints = nodes[0].Taints
	}

	// Create virtual nodes with the candidate plan's capacity
	// Allocatable CPU = plan.CPU * 1000 - 100m (kubelet reserved)
	// Allocatable Memory = plan.RAM * 1024 * 1024 - 256Mi (kubelet reserved)
	allocCPU := int64(candidatePlan.CPU)*1000 - 100
	allocMemory := int64(candidatePlan.RAM)*1024*1024 - 256*1024*1024
	if allocCPU < 0 {
		allocCPU = 0
	}
	if allocMemory < 0 {
		allocMemory = 0
	}

	vnodes := make([]virtualNode, nodeCount)
	for i := range vnodes {
		vnodes[i] = virtualNode{
			labels:       nodeLabels,
			taints:       nodeTaints,
			remainCPU:    allocCPU,
			remainMemory: allocMemory,
		}
	}

	// Sort pods by resource requests (largest first) for first-fit-decreasing
	sort.Slice(pods, func(i, j int) bool {
		ri := podTotalCPU(&pods[i]) + podTotalMemory(&pods[i])
		rj := podTotalCPU(&pods[j]) + podTotalMemory(&pods[j])
		return ri > rj
	})

	var blocking []BlockingPod
	for _, pod := range pods {
		podCPU := podTotalCPU(&pod)
		podMem := podTotalMemory(&pod)

		placed := false
		var reason string

		for vi := range vnodes {
			vn := &vnodes[vi]

			// Check resource fit
			if podCPU > vn.remainCPU || podMem > vn.remainMemory {
				reason = fmt.Sprintf("insufficient resources: needs %dm CPU, %d bytes memory",
					podCPU, podMem)
				continue
			}

			// Check tolerations
			if !toleratesTaints(pod.Spec.Tolerations, vn.taints) {
				reason = "pod does not tolerate node taints"
				continue
			}

			// Check nodeSelector
			if !matchesNodeSelector(pod.Spec.NodeSelector, vn.labels) {
				reason = "pod nodeSelector does not match node labels"
				continue
			}

			// Check nodeAffinity (required rules only)
			if !matchesRequiredNodeAffinity(pod.Spec.Affinity, vn.labels) {
				reason = "pod required nodeAffinity does not match node labels"
				continue
			}

			// Place the pod
			vn.remainCPU -= podCPU
			vn.remainMemory -= podMem
			placed = true
			break
		}

		if !placed {
			blocking = append(blocking, BlockingPod{
				Name:      pod.Name,
				Namespace: pod.Namespace,
				Reason:    reason,
			})
		}
	}

	if len(blocking) > 0 {
		return SimulationResult{
			Safe:         false,
			BlockingPods: blocking,
			Message:      fmt.Sprintf("%d pod(s) cannot be scheduled on %s plan", len(blocking), candidatePlan.Nickname),
		}
	}

	return SimulationResult{
		Safe:    true,
		Message: fmt.Sprintf("all %d pods fit on %d x %s nodes", len(pods), nodeCount, candidatePlan.Nickname),
	}
}

// isDaemonSetPod checks if a pod is owned by a DaemonSet.
func isDaemonSetPod(pod *corev1.Pod) bool {
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "DaemonSet" {
			return true
		}
	}
	return false
}

// isMirrorPod checks if a pod is a static/mirror pod.
func isMirrorPod(pod *corev1.Pod) bool {
	_, ok := pod.Annotations["kubernetes.io/config.mirror"]
	return ok
}

// podTotalCPU returns the total CPU requests in millicores for a pod.
func podTotalCPU(pod *corev1.Pod) int64 {
	var total int64
	for _, c := range pod.Spec.Containers {
		total += c.Resources.Requests.Cpu().MilliValue()
	}
	return total
}

// podTotalMemory returns the total memory requests in bytes for a pod.
func podTotalMemory(pod *corev1.Pod) int64 {
	var total int64
	for _, c := range pod.Spec.Containers {
		total += c.Resources.Requests.Memory().Value()
	}
	return total
}

// toleratesTaints checks if the pod's tolerations cover all node taints
// with effect NoSchedule or NoExecute.
func toleratesTaints(tolerations []corev1.Toleration, taints []corev1.Taint) bool {
	for _, taint := range taints {
		if taint.Effect != corev1.TaintEffectNoSchedule && taint.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		if !tolerateTaint(tolerations, &taint) {
			return false
		}
	}
	return true
}

// tolerateTaint checks if any toleration matches the given taint.
func tolerateTaint(tolerations []corev1.Toleration, taint *corev1.Taint) bool {
	for _, t := range tolerations {
		if t.Operator == corev1.TolerationOpExists && t.Key == "" {
			return true // tolerates everything
		}
		if t.Key == taint.Key {
			if t.Operator == corev1.TolerationOpExists {
				if t.Effect == "" || t.Effect == taint.Effect {
					return true
				}
			}
			if t.Operator == "" || t.Operator == corev1.TolerationOpEqual {
				if t.Value == taint.Value && (t.Effect == "" || t.Effect == taint.Effect) {
					return true
				}
			}
		}
	}
	return false
}

// matchesNodeSelector checks if all nodeSelector keys match the node's labels.
func matchesNodeSelector(nodeSelector map[string]string, nodeLabels map[string]string) bool {
	for k, v := range nodeSelector {
		if nodeLabels[k] != v {
			return false
		}
	}
	return true
}

// matchesRequiredNodeAffinity checks if the pod's required node affinity rules
// are satisfied by the node's labels.
func matchesRequiredNodeAffinity(affinity *corev1.Affinity, nodeLabels map[string]string) bool {
	if affinity == nil || affinity.NodeAffinity == nil ||
		affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return true
	}

	selector := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution

	// At least one NodeSelectorTerm must match
	for _, term := range selector.NodeSelectorTerms {
		if matchesNodeSelectorTerm(&term, nodeLabels) {
			return true
		}
	}

	return false
}

// matchesNodeSelectorTerm evaluates a single NodeSelectorTerm against labels.
func matchesNodeSelectorTerm(term *corev1.NodeSelectorTerm, labels map[string]string) bool {
	// All match expressions must match
	for _, expr := range term.MatchExpressions {
		if !matchExpression(&expr, labels) {
			return false
		}
	}
	// All match fields would need field evaluation — we skip them (conservative: pass)
	return true
}

// matchExpression evaluates a single NodeSelectorRequirement against labels.
func matchExpression(expr *corev1.NodeSelectorRequirement, labels map[string]string) bool {
	value, exists := labels[expr.Key]
	switch expr.Operator {
	case corev1.NodeSelectorOpIn:
		if !exists {
			return false
		}
		for _, v := range expr.Values {
			if v == value {
				return true
			}
		}
		return false
	case corev1.NodeSelectorOpNotIn:
		if !exists {
			return true
		}
		for _, v := range expr.Values {
			if v == value {
				return false
			}
		}
		return true
	case corev1.NodeSelectorOpExists:
		return exists
	case corev1.NodeSelectorOpDoesNotExist:
		return !exists
	case corev1.NodeSelectorOpGt:
		if !exists || len(expr.Values) == 0 {
			return false
		}
		labelQty := resource.MustParse(value)
		exprQty := resource.MustParse(expr.Values[0])
		return labelQty.Cmp(exprQty) > 0
	case corev1.NodeSelectorOpLt:
		if !exists || len(expr.Values) == 0 {
			return false
		}
		labelQty := resource.MustParse(value)
		exprQty := resource.MustParse(expr.Values[0])
		return labelQty.Cmp(exprQty) < 0
	default:
		return true // unknown operator: conservative pass
	}
}

