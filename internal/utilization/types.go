package utilization

import corev1 "k8s.io/api/core/v1"

// Result holds the aggregated utilization data for a set of nodes.
type Result struct {
	// ScheduledCPUPercent is the aggregate CPU utilization based on pod requests.
	ScheduledCPUPercent float64

	// ScheduledMemoryPercent is the aggregate memory utilization based on pod requests.
	ScheduledMemoryPercent float64

	// ActualCPUPercent is the aggregate CPU utilization based on metrics-server data.
	ActualCPUPercent float64

	// ActualMemoryPercent is the aggregate memory utilization based on metrics-server data.
	ActualMemoryPercent float64

	// MetricsAvailable indicates whether metrics-server data was used.
	MetricsAvailable bool

	// Nodes holds per-node utilization data.
	Nodes []NodeUtilization

	// Source describes how utilization was determined: "requests", "metrics", or "both".
	Source string
}

// NodeUtilization holds utilization data for a single node.
type NodeUtilization struct {
	// NodeName is the name of the node.
	NodeName string

	// AllocatableCPU is the node's allocatable CPU in millicores.
	AllocatableCPU int64

	// AllocatableRAM is the node's allocatable memory in bytes.
	AllocatableRAM int64

	// RequestedCPU is the total CPU requests of pods on this node, in millicores.
	RequestedCPU int64

	// RequestedRAM is the total memory requests of pods on this node, in bytes.
	RequestedRAM int64

	// Labels are the node's labels (used for scheduling simulation).
	Labels map[string]string

	// Taints are the node's taints (used for scheduling simulation).
	Taints []corev1.Taint

	// Pods are the non-terminal pods running on this node.
	Pods []corev1.Pod
}
