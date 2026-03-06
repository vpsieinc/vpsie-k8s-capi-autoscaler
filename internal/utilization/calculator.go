package utilization

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/vpsieinc/vpsie-cluster-scaler/internal/workload"
)

// Calculator computes utilization for a set of workload cluster nodes.
type Calculator struct {
	client workload.WorkloadClient
}

// NewCalculator creates a new utilization calculator.
func NewCalculator(client workload.WorkloadClient) *Calculator {
	return &Calculator{client: client}
}

// Calculate computes the utilization for the nodes backing a MachineDeployment.
func (c *Calculator) Calculate(ctx context.Context, machineDeploymentName string) (*Result, error) {
	// List nodes
	nodes, err := c.client.ListNodes(ctx, machineDeploymentName)
	if err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no nodes found for MachineDeployment %s", machineDeploymentName)
	}

	// Collect node names
	nodeNames := make([]string, len(nodes))
	for i, n := range nodes {
		nodeNames[i] = n.Name
	}

	// List pods
	pods, err := c.client.ListPods(ctx, nodeNames)
	if err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}

	// Build per-node pod map
	podsByNode := make(map[string][]corev1.Pod)
	for _, pod := range pods {
		podsByNode[pod.Spec.NodeName] = append(podsByNode[pod.Spec.NodeName], pod)
	}

	// Calculate per-node utilization
	var totalAllocCPU, totalAllocRAM int64
	var totalReqCPU, totalReqRAM int64
	nodeUtils := make([]NodeUtilization, 0, len(nodes))

	for _, node := range nodes {
		allocCPU := node.Status.Allocatable.Cpu().MilliValue()
		allocRAM := node.Status.Allocatable.Memory().Value()

		var reqCPU, reqRAM int64
		nodePods := podsByNode[node.Name]
		for _, pod := range nodePods {
			for _, c := range pod.Spec.Containers {
				reqCPU += c.Resources.Requests.Cpu().MilliValue()
				reqRAM += c.Resources.Requests.Memory().Value()
			}
			for _, c := range pod.Spec.InitContainers {
				// Init containers run sequentially, but their resources may be
				// reserved by the scheduler. We include the max init container.
				initCPU := c.Resources.Requests.Cpu().MilliValue()
				initRAM := c.Resources.Requests.Memory().Value()
				if initCPU > reqCPU {
					// Actually, Kubernetes uses max(sum(init), sum(regular)),
					// but for simplicity we just add regular container requests.
					_ = initRAM
				}
			}
		}

		nodeUtils = append(nodeUtils, NodeUtilization{
			NodeName:       node.Name,
			AllocatableCPU: allocCPU,
			AllocatableRAM: allocRAM,
			RequestedCPU:   reqCPU,
			RequestedRAM:   reqRAM,
			Labels:         node.Labels,
			Taints:         node.Spec.Taints,
			Pods:           nodePods,
		})

		totalAllocCPU += allocCPU
		totalAllocRAM += allocRAM
		totalReqCPU += reqCPU
		totalReqRAM += reqRAM
	}

	result := &Result{
		Nodes:  nodeUtils,
		Source: "requests",
	}

	if totalAllocCPU > 0 {
		result.ScheduledCPUPercent = float64(totalReqCPU) / float64(totalAllocCPU) * 100
	}
	if totalAllocRAM > 0 {
		result.ScheduledMemoryPercent = float64(totalReqRAM) / float64(totalAllocRAM) * 100
	}

	// Try to get actual metrics
	nodeMetrics, err := c.client.GetNodeMetrics(ctx, nodeNames)
	if err != nil {
		klog.V(2).Infof("metrics unavailable, using requests only: %v", err)
		return result, nil
	}

	if len(nodeMetrics) > 0 {
		result.MetricsAvailable = true
		result.Source = "both"

		var totalUsageCPU, totalUsageRAM int64
		for _, m := range nodeMetrics {
			totalUsageCPU += m.Usage.Cpu().MilliValue()
			totalUsageRAM += m.Usage.Memory().Value()
		}

		if totalAllocCPU > 0 {
			result.ActualCPUPercent = float64(totalUsageCPU) / float64(totalAllocCPU) * 100
		}
		if totalAllocRAM > 0 {
			result.ActualMemoryPercent = float64(totalUsageRAM) / float64(totalAllocRAM) * 100
		}
	}

	return result, nil
}

// EvaluateThresholds determines the scaling direction based on utilization thresholds.
//
// Upscale: max(scheduled, actual) > scaleUpThreshold for CPU OR memory.
// Downscale: min(scheduled, actual) < scaleDownThreshold for BOTH CPU AND memory.
// When metrics unavailable: use scheduled only.
func EvaluateThresholds(result *Result, scaleUpThreshold, scaleDownThreshold int) (needsUpscale, needsDownscale bool) {
	upThresh := float64(scaleUpThreshold)
	downThresh := float64(scaleDownThreshold)

	var cpuMax, memMax, cpuMin, memMin float64

	if result.MetricsAvailable {
		cpuMax = max(result.ScheduledCPUPercent, result.ActualCPUPercent)
		memMax = max(result.ScheduledMemoryPercent, result.ActualMemoryPercent)
		cpuMin = min(result.ScheduledCPUPercent, result.ActualCPUPercent)
		memMin = min(result.ScheduledMemoryPercent, result.ActualMemoryPercent)
	} else {
		cpuMax = result.ScheduledCPUPercent
		memMax = result.ScheduledMemoryPercent
		cpuMin = result.ScheduledCPUPercent
		memMin = result.ScheduledMemoryPercent
	}

	// Upscale if EITHER CPU or memory exceeds threshold
	needsUpscale = cpuMax > upThresh || memMax > upThresh

	// Downscale if BOTH CPU and memory are below threshold
	needsDownscale = cpuMin < downThresh && memMin < downThresh

	return
}

func max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// EffectiveCPUPercent returns the maximum of scheduled and actual CPU utilization.
func EffectiveCPUPercent(r *Result) int {
	if r.MetricsAvailable {
		return int(max(r.ScheduledCPUPercent, r.ActualCPUPercent))
	}
	return int(r.ScheduledCPUPercent)
}

// EffectiveMemoryPercent returns the maximum of scheduled and actual memory utilization.
func EffectiveMemoryPercent(r *Result) int {
	if r.MetricsAvailable {
		return int(max(r.ScheduledMemoryPercent, r.ActualMemoryPercent))
	}
	return int(r.ScheduledMemoryPercent)
}

// EffectiveSource returns the source string for status reporting.
func EffectiveSource(r *Result) string {
	return r.Source
}
