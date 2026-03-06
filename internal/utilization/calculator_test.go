package utilization

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"

	"github.com/vpsieinc/vpsie-cluster-scaler/internal/workload"
)

func newFakeClient(nodes []corev1.Node, pods []corev1.Pod, metrics []metricsv1beta1.NodeMetrics) *workload.FakeWorkloadClient {
	return &workload.FakeWorkloadClient{
		Nodes:       nodes,
		Pods:        pods,
		NodeMetrics: metrics,
	}
}

func TestCalculator_RequestsOnly(t *testing.T) {
	nodes := []corev1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),     // 4000m
					corev1.ResourceMemory: resource.MustParse("8Gi"),   // ~8.59GB
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-2"},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("8Gi"),
				},
			},
		},
	}

	pods := []corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "default"},
			Spec: corev1.PodSpec{
				NodeName: "node-1",
				Containers: []corev1.Container{
					{
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("2"),    // 2000m
								corev1.ResourceMemory: resource.MustParse("4Gi"),
							},
						},
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "pod-2", Namespace: "default"},
			Spec: corev1.PodSpec{
				NodeName: "node-2",
				Containers: []corev1.Container{
					{
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("1"),
								corev1.ResourceMemory: resource.MustParse("2Gi"),
							},
						},
					},
				},
			},
		},
	}

	fake := newFakeClient(nodes, pods, nil)
	calc := NewCalculator(fake)

	result, err := calc.Calculate(context.Background(), "workers")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Total allocatable: 8000m CPU, 16Gi RAM
	// Total requested: 3000m CPU, 6Gi RAM
	// Expected: CPU=37.5%, Memory=37.5%
	if result.ScheduledCPUPercent < 37 || result.ScheduledCPUPercent > 38 {
		t.Fatalf("expected ~37.5%% CPU utilization, got %.1f%%", result.ScheduledCPUPercent)
	}
	if result.ScheduledMemoryPercent < 37 || result.ScheduledMemoryPercent > 38 {
		t.Fatalf("expected ~37.5%% memory utilization, got %.1f%%", result.ScheduledMemoryPercent)
	}
	if result.MetricsAvailable {
		t.Fatal("expected MetricsAvailable=false")
	}
	if result.Source != "requests" {
		t.Fatalf("expected source=requests, got %s", result.Source)
	}
	if len(result.Nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(result.Nodes))
	}
}

func TestCalculator_WithMetrics(t *testing.T) {
	nodes := []corev1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("8Gi"),
				},
			},
		},
	}

	pods := []corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: "default"},
			Spec: corev1.PodSpec{
				NodeName: "node-1",
				Containers: []corev1.Container{
					{
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("1"),
								corev1.ResourceMemory: resource.MustParse("2Gi"),
							},
						},
					},
				},
			},
		},
	}

	metrics := []metricsv1beta1.NodeMetrics{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
			Usage: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("3"),    // 3000m actual
				corev1.ResourceMemory: resource.MustParse("6Gi"), // 6Gi actual
			},
		},
	}

	fake := newFakeClient(nodes, pods, metrics)
	calc := NewCalculator(fake)

	result, err := calc.Calculate(context.Background(), "workers")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Scheduled: CPU=25%, Memory=25%
	if result.ScheduledCPUPercent < 24 || result.ScheduledCPUPercent > 26 {
		t.Fatalf("expected ~25%% scheduled CPU, got %.1f%%", result.ScheduledCPUPercent)
	}

	// Actual: CPU=75%, Memory=75%
	if result.ActualCPUPercent < 74 || result.ActualCPUPercent > 76 {
		t.Fatalf("expected ~75%% actual CPU, got %.1f%%", result.ActualCPUPercent)
	}

	if !result.MetricsAvailable {
		t.Fatal("expected MetricsAvailable=true")
	}
	if result.Source != "both" {
		t.Fatalf("expected source=both, got %s", result.Source)
	}
}

func TestEvaluateThresholds_UpscaleOnHighCPU(t *testing.T) {
	result := &Result{
		ScheduledCPUPercent:    80,
		ScheduledMemoryPercent: 30,
	}

	up, down := EvaluateThresholds(result, 75, 5)
	if !up {
		t.Fatal("expected upscale when CPU > 75%")
	}
	if down {
		t.Fatal("should not downscale when CPU > 5%")
	}
}

func TestEvaluateThresholds_UpscaleOnHighMemory(t *testing.T) {
	result := &Result{
		ScheduledCPUPercent:    30,
		ScheduledMemoryPercent: 80,
	}

	up, down := EvaluateThresholds(result, 75, 5)
	if !up {
		t.Fatal("expected upscale when memory > 75%")
	}
	if down {
		t.Fatal("should not downscale")
	}
}

func TestEvaluateThresholds_DownscaleOnBothLow(t *testing.T) {
	result := &Result{
		ScheduledCPUPercent:    3,
		ScheduledMemoryPercent: 2,
	}

	up, down := EvaluateThresholds(result, 75, 5)
	if up {
		t.Fatal("should not upscale when both < 75%")
	}
	if !down {
		t.Fatal("expected downscale when both CPU and memory < 5%")
	}
}

func TestEvaluateThresholds_NoDownscaleWhenOnlyOneLow(t *testing.T) {
	result := &Result{
		ScheduledCPUPercent:    3,
		ScheduledMemoryPercent: 10, // > 5%
	}

	_, down := EvaluateThresholds(result, 75, 5)
	if down {
		t.Fatal("should not downscale when only CPU is below threshold")
	}
}

func TestEvaluateThresholds_WithMetrics_UsesMaxForUpscale(t *testing.T) {
	result := &Result{
		ScheduledCPUPercent: 50, // below threshold
		ActualCPUPercent:    80, // above threshold
		MetricsAvailable:    true,
		ScheduledMemoryPercent: 30,
		ActualMemoryPercent:    30,
	}

	up, _ := EvaluateThresholds(result, 75, 5)
	if !up {
		t.Fatal("expected upscale based on actual CPU > 75%")
	}
}

func TestEvaluateThresholds_WithMetrics_UsesMinForDownscale(t *testing.T) {
	// Scheduled says low, but actual says moderate → no downscale
	result := &Result{
		ScheduledCPUPercent:    2,
		ActualCPUPercent:       10, // min(2,10)=2, but for memory min(3,15)=3
		ScheduledMemoryPercent: 3,
		ActualMemoryPercent:    15, // min > 5? 3 < 5 yes. But 10 for cpu: min(2,10)=2 < 5
		MetricsAvailable:       true,
	}

	// CPU min: min(2,10)=2 < 5 ✓
	// Memory min: min(3,15)=3 < 5 ✓
	_, down := EvaluateThresholds(result, 75, 5)
	if !down {
		t.Fatal("expected downscale when min values for both are below threshold")
	}

	// Now make actual memory high enough that min > 5
	result.ActualMemoryPercent = 6 // min(3,6)=3 < 5 still
	_, down = EvaluateThresholds(result, 75, 5)
	if !down {
		t.Fatal("expected downscale: min(3,6)=3 < 5")
	}

	result.ScheduledMemoryPercent = 6 // min(6,6)=6 > 5
	_, down = EvaluateThresholds(result, 75, 5)
	if down {
		t.Fatal("should not downscale: min(6,6)=6 > 5")
	}
}

func TestEvaluateThresholds_InRange(t *testing.T) {
	result := &Result{
		ScheduledCPUPercent:    40,
		ScheduledMemoryPercent: 50,
	}

	up, down := EvaluateThresholds(result, 75, 5)
	if up {
		t.Fatal("should not upscale when in range")
	}
	if down {
		t.Fatal("should not downscale when in range")
	}
}

func TestEffectiveCPUPercent(t *testing.T) {
	r := &Result{
		ScheduledCPUPercent: 50,
		ActualCPUPercent:    70,
		MetricsAvailable:    true,
	}
	if got := EffectiveCPUPercent(r); got != 70 {
		t.Fatalf("expected 70, got %d", got)
	}

	r.MetricsAvailable = false
	if got := EffectiveCPUPercent(r); got != 50 {
		t.Fatalf("expected 50, got %d", got)
	}
}
