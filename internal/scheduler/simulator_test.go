package scheduler

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vpsieinc/vpsie-cluster-scaler/internal/utilization"
	"github.com/vpsieinc/vpsie-cluster-scaler/internal/vpsie"
)

func makePod(name, ns string, cpuReq, memReq string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse(cpuReq),
							corev1.ResourceMemory: resource.MustParse(memReq),
						},
					},
				},
			},
		},
	}
}

func makeDaemonSetPod(name, ns string) corev1.Pod {
	pod := makePod(name, ns, "100m", "128Mi")
	pod.OwnerReferences = []metav1.OwnerReference{
		{Kind: "DaemonSet", Name: "kube-proxy"},
	}
	return pod
}

func makeMirrorPod(name, ns string) corev1.Pod {
	pod := makePod(name, ns, "100m", "128Mi")
	pod.Annotations = map[string]string{
		"kubernetes.io/config.mirror": "abc123",
	}
	return pod
}

func smallPlan() vpsie.Plan {
	return vpsie.Plan{
		Identifier:   "plan-small",
		Nickname:     "s-1vcpu-2gb",
		CPU:          1,
		RAM:          2048,
		SSD:          50,
		PriceMonthly: 12.0,
	}
}

func mediumPlan() vpsie.Plan {
	return vpsie.Plan{
		Identifier:   "plan-medium",
		Nickname:     "s-2vcpu-4gb",
		CPU:          2,
		RAM:          4096,
		SSD:          80,
		PriceMonthly: 24.0,
	}
}

func TestSimulateDownscale_AllPodsFit(t *testing.T) {
	nodes := []utilization.NodeUtilization{
		{
			NodeName: "node-1",
			Pods: []corev1.Pod{
				makePod("app-1", "default", "200m", "256Mi"),
				makePod("app-2", "default", "300m", "512Mi"),
			},
		},
	}

	sim := NewSimulator()
	result := sim.SimulateDownscale(nodes, mediumPlan(), 2)

	if !result.Safe {
		t.Fatalf("expected safe, got unsafe: %s", result.Message)
	}
	if len(result.BlockingPods) != 0 {
		t.Fatalf("expected no blocking pods, got %d", len(result.BlockingPods))
	}
}

func TestSimulateDownscale_InsufficientResources(t *testing.T) {
	// Pod requires 1.5 CPU, small plan only has ~900m allocatable
	nodes := []utilization.NodeUtilization{
		{
			NodeName: "node-1",
			Pods: []corev1.Pod{
				makePod("big-app", "default", "1500m", "1Gi"),
			},
		},
	}

	sim := NewSimulator()
	result := sim.SimulateDownscale(nodes, smallPlan(), 1)

	if result.Safe {
		t.Fatal("expected unsafe due to insufficient resources")
	}
	if len(result.BlockingPods) != 1 {
		t.Fatalf("expected 1 blocking pod, got %d", len(result.BlockingPods))
	}
	if result.BlockingPods[0].Name != "big-app" {
		t.Fatalf("expected big-app to be blocked, got %s", result.BlockingPods[0].Name)
	}
}

func TestSimulateDownscale_DaemonSetPodsExcluded(t *testing.T) {
	nodes := []utilization.NodeUtilization{
		{
			NodeName: "node-1",
			Pods: []corev1.Pod{
				makePod("app-1", "default", "200m", "256Mi"),
				makeDaemonSetPod("kube-proxy-abc", "kube-system"),
			},
		},
	}

	sim := NewSimulator()
	result := sim.SimulateDownscale(nodes, smallPlan(), 1)

	if !result.Safe {
		t.Fatalf("expected safe (DaemonSet pods excluded), got: %s", result.Message)
	}
}

func TestSimulateDownscale_MirrorPodsExcluded(t *testing.T) {
	nodes := []utilization.NodeUtilization{
		{
			NodeName: "node-1",
			Pods: []corev1.Pod{
				makePod("app-1", "default", "200m", "256Mi"),
				makeMirrorPod("kube-apiserver", "kube-system"),
			},
		},
	}

	sim := NewSimulator()
	result := sim.SimulateDownscale(nodes, smallPlan(), 1)

	if !result.Safe {
		t.Fatalf("expected safe (mirror pods excluded), got: %s", result.Message)
	}
}

func TestSimulateDownscale_TaintBlocking(t *testing.T) {
	// Node has a taint, pod does not tolerate it
	nodes := []utilization.NodeUtilization{
		{
			NodeName: "node-1",
			Taints: []corev1.Taint{
				{Key: "gpu", Value: "true", Effect: corev1.TaintEffectNoSchedule},
			},
			Pods: []corev1.Pod{
				makePod("no-toleration", "default", "100m", "128Mi"),
			},
		},
	}

	sim := NewSimulator()
	result := sim.SimulateDownscale(nodes, mediumPlan(), 1)

	if result.Safe {
		t.Fatal("expected unsafe: pod does not tolerate taint")
	}
	if len(result.BlockingPods) != 1 {
		t.Fatalf("expected 1 blocking pod, got %d", len(result.BlockingPods))
	}
}

func TestSimulateDownscale_TaintTolerated(t *testing.T) {
	pod := makePod("tolerated", "default", "100m", "128Mi")
	pod.Spec.Tolerations = []corev1.Toleration{
		{Key: "gpu", Value: "true", Effect: corev1.TaintEffectNoSchedule, Operator: corev1.TolerationOpEqual},
	}

	nodes := []utilization.NodeUtilization{
		{
			NodeName: "node-1",
			Taints: []corev1.Taint{
				{Key: "gpu", Value: "true", Effect: corev1.TaintEffectNoSchedule},
			},
			Pods: []corev1.Pod{pod},
		},
	}

	sim := NewSimulator()
	result := sim.SimulateDownscale(nodes, mediumPlan(), 1)

	if !result.Safe {
		t.Fatalf("expected safe (toleration matches), got: %s", result.Message)
	}
}

func TestSimulateDownscale_NodeSelectorBlocking(t *testing.T) {
	pod := makePod("selector-pod", "default", "100m", "128Mi")
	pod.Spec.NodeSelector = map[string]string{"disktype": "ssd"}

	nodes := []utilization.NodeUtilization{
		{
			NodeName: "node-1",
			Labels:   map[string]string{"disktype": "hdd"}, // doesn't match
			Pods:     []corev1.Pod{pod},
		},
	}

	sim := NewSimulator()
	result := sim.SimulateDownscale(nodes, mediumPlan(), 1)

	if result.Safe {
		t.Fatal("expected unsafe: nodeSelector does not match")
	}
}

func TestSimulateDownscale_NodeSelectorMatches(t *testing.T) {
	pod := makePod("selector-pod", "default", "100m", "128Mi")
	pod.Spec.NodeSelector = map[string]string{"disktype": "ssd"}

	nodes := []utilization.NodeUtilization{
		{
			NodeName: "node-1",
			Labels:   map[string]string{"disktype": "ssd"},
			Pods:     []corev1.Pod{pod},
		},
	}

	sim := NewSimulator()
	result := sim.SimulateDownscale(nodes, mediumPlan(), 1)

	if !result.Safe {
		t.Fatalf("expected safe (nodeSelector matches), got: %s", result.Message)
	}
}

func TestSimulateDownscale_NodeAffinityBlocking(t *testing.T) {
	pod := makePod("affinity-pod", "default", "100m", "128Mi")
	pod.Spec.Affinity = &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{
						MatchExpressions: []corev1.NodeSelectorRequirement{
							{
								Key:      "topology.kubernetes.io/zone",
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{"us-east-1a"},
							},
						},
					},
				},
			},
		},
	}

	nodes := []utilization.NodeUtilization{
		{
			NodeName: "node-1",
			Labels:   map[string]string{"topology.kubernetes.io/zone": "us-west-2a"}, // wrong zone
			Pods:     []corev1.Pod{pod},
		},
	}

	sim := NewSimulator()
	result := sim.SimulateDownscale(nodes, mediumPlan(), 1)

	if result.Safe {
		t.Fatal("expected unsafe: nodeAffinity zone mismatch")
	}
}

func TestSimulateDownscale_NodeAffinityMatches(t *testing.T) {
	pod := makePod("affinity-pod", "default", "100m", "128Mi")
	pod.Spec.Affinity = &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{
						MatchExpressions: []corev1.NodeSelectorRequirement{
							{
								Key:      "topology.kubernetes.io/zone",
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{"us-east-1a"},
							},
						},
					},
				},
			},
		},
	}

	nodes := []utilization.NodeUtilization{
		{
			NodeName: "node-1",
			Labels:   map[string]string{"topology.kubernetes.io/zone": "us-east-1a"},
			Pods:     []corev1.Pod{pod},
		},
	}

	sim := NewSimulator()
	result := sim.SimulateDownscale(nodes, mediumPlan(), 1)

	if !result.Safe {
		t.Fatalf("expected safe (affinity matches), got: %s", result.Message)
	}
}

func TestSimulateDownscale_BinPacking_MultipleNodes(t *testing.T) {
	// 3 pods, each needs 500m CPU and 512Mi RAM
	// Medium plan: ~1900m CPU, ~3840Mi RAM allocatable
	// 2 nodes should fit (2 on one, 1 on another)
	nodes := []utilization.NodeUtilization{
		{
			NodeName: "node-1",
			Pods: []corev1.Pod{
				makePod("app-1", "default", "500m", "512Mi"),
				makePod("app-2", "default", "500m", "512Mi"),
				makePod("app-3", "default", "500m", "512Mi"),
			},
		},
	}

	sim := NewSimulator()
	result := sim.SimulateDownscale(nodes, mediumPlan(), 2)

	if !result.Safe {
		t.Fatalf("expected safe with 2 medium nodes, got: %s", result.Message)
	}
}

func TestSimulateDownscale_ZeroNodeCount(t *testing.T) {
	nodes := []utilization.NodeUtilization{
		{
			NodeName: "node-1",
			Pods:     []corev1.Pod{makePod("app-1", "default", "100m", "128Mi")},
		},
	}

	sim := NewSimulator()
	result := sim.SimulateDownscale(nodes, smallPlan(), 0)

	if result.Safe {
		t.Fatal("expected unsafe with 0 nodes")
	}
}

func TestSimulateDownscale_NoPods(t *testing.T) {
	nodes := []utilization.NodeUtilization{
		{NodeName: "node-1"},
	}

	sim := NewSimulator()
	result := sim.SimulateDownscale(nodes, smallPlan(), 1)

	if !result.Safe {
		t.Fatalf("expected safe with no pods, got: %s", result.Message)
	}
}

func TestSimulateDownscale_TolerationOpExists(t *testing.T) {
	pod := makePod("tolerates-all", "default", "100m", "128Mi")
	pod.Spec.Tolerations = []corev1.Toleration{
		{Operator: corev1.TolerationOpExists}, // tolerates everything
	}

	nodes := []utilization.NodeUtilization{
		{
			NodeName: "node-1",
			Taints: []corev1.Taint{
				{Key: "special", Value: "yes", Effect: corev1.TaintEffectNoSchedule},
			},
			Pods: []corev1.Pod{pod},
		},
	}

	sim := NewSimulator()
	result := sim.SimulateDownscale(nodes, mediumPlan(), 1)

	if !result.Safe {
		t.Fatalf("expected safe (toleration exists matches all), got: %s", result.Message)
	}
}

func TestSimulateDownscale_AffinityNotIn(t *testing.T) {
	pod := makePod("notin-pod", "default", "100m", "128Mi")
	pod.Spec.Affinity = &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{
						MatchExpressions: []corev1.NodeSelectorRequirement{
							{
								Key:      "env",
								Operator: corev1.NodeSelectorOpNotIn,
								Values:   []string{"production"},
							},
						},
					},
				},
			},
		},
	}

	// Node labeled env=staging — NotIn production is satisfied
	nodes := []utilization.NodeUtilization{
		{
			NodeName: "node-1",
			Labels:   map[string]string{"env": "staging"},
			Pods:     []corev1.Pod{pod},
		},
	}

	sim := NewSimulator()
	result := sim.SimulateDownscale(nodes, mediumPlan(), 1)

	if !result.Safe {
		t.Fatalf("expected safe (NotIn production satisfied), got: %s", result.Message)
	}

	// Node labeled env=production — NotIn production is NOT satisfied
	nodes[0].Labels = map[string]string{"env": "production"}
	result = sim.SimulateDownscale(nodes, mediumPlan(), 1)
	if result.Safe {
		t.Fatal("expected unsafe (NotIn production not satisfied for env=production)")
	}
}

func TestSimulateDownscale_AffinityExists(t *testing.T) {
	pod := makePod("exists-pod", "default", "100m", "128Mi")
	pod.Spec.Affinity = &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{
						MatchExpressions: []corev1.NodeSelectorRequirement{
							{
								Key:      "gpu",
								Operator: corev1.NodeSelectorOpExists,
							},
						},
					},
				},
			},
		},
	}

	// Node has gpu label
	nodes := []utilization.NodeUtilization{
		{
			NodeName: "node-1",
			Labels:   map[string]string{"gpu": "true"},
			Pods:     []corev1.Pod{pod},
		},
	}

	sim := NewSimulator()
	result := sim.SimulateDownscale(nodes, mediumPlan(), 1)
	if !result.Safe {
		t.Fatalf("expected safe (Exists gpu satisfied), got: %s", result.Message)
	}

	// No gpu label
	nodes[0].Labels = map[string]string{}
	result = sim.SimulateDownscale(nodes, mediumPlan(), 1)
	if result.Safe {
		t.Fatal("expected unsafe (Exists gpu not satisfied)")
	}
}
