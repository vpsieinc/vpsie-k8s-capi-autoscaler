package workload

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
)

// FakeWorkloadClient implements WorkloadClient for testing.
type FakeWorkloadClient struct {
	Nodes        []corev1.Node
	Pods         []corev1.Pod
	PendingPods  []corev1.Pod
	NodeMetrics  []metricsv1beta1.NodeMetrics
	NodesErr     error
	PodsErr      error
	PendingErr   error
	MetricsErr   error
}

func (f *FakeWorkloadClient) ListNodes(_ context.Context, _ string) ([]corev1.Node, error) {
	return f.Nodes, f.NodesErr
}

func (f *FakeWorkloadClient) ListPods(_ context.Context, _ []string) ([]corev1.Pod, error) {
	return f.Pods, f.PodsErr
}

func (f *FakeWorkloadClient) ListPendingPods(_ context.Context) ([]corev1.Pod, error) {
	return f.PendingPods, f.PendingErr
}

func (f *FakeWorkloadClient) GetNodeMetrics(_ context.Context, _ []string) ([]metricsv1beta1.NodeMetrics, error) {
	return f.NodeMetrics, f.MetricsErr
}

// FakeWorkloadClientFactory implements WorkloadClientFactory for testing.
type FakeWorkloadClientFactory struct {
	Client WorkloadClient
	Err    error
}

func (f *FakeWorkloadClientFactory) ClientForCluster(_ context.Context, _, _ string) (WorkloadClient, error) {
	return f.Client, f.Err
}
