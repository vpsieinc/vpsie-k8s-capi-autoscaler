package workload

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestFakeWorkloadClient(t *testing.T) {
	ctx := context.Background()

	nodes := []corev1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
			Status: corev1.NodeStatus{
				Allocatable: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("4Gi"),
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
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
					},
				},
			},
		},
	}

	fake := &FakeWorkloadClient{
		Nodes: nodes,
		Pods:  pods,
	}

	gotNodes, err := fake.ListNodes(ctx, "workers")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gotNodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(gotNodes))
	}

	gotPods, err := fake.ListPods(ctx, []string{"node-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(gotPods) != 1 {
		t.Fatalf("expected 1 pod, got %d", len(gotPods))
	}

	metrics, err := fake.GetNodeMetrics(ctx, []string{"node-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metrics != nil {
		t.Fatal("expected nil metrics for fake with no metrics set")
	}
}

func TestFakeWorkloadClientFactory(t *testing.T) {
	factory := &FakeWorkloadClientFactory{
		Client: &FakeWorkloadClient{},
	}

	client, err := factory.ClientForCluster(context.Background(), "test-cluster", "default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil client")
	}
}
