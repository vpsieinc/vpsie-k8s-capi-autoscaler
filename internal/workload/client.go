package workload

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"
	metricsv1beta1 "k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// WorkloadClient provides access to a workload cluster's node and pod data.
type WorkloadClient interface {
	// ListNodes returns the Nodes backing a MachineDeployment in the workload cluster.
	ListNodes(ctx context.Context, machineDeploymentName string) ([]corev1.Node, error)

	// ListPods returns non-terminal pods running on the given nodes.
	ListPods(ctx context.Context, nodeNames []string) ([]corev1.Pod, error)

	// GetNodeMetrics returns metrics-server data for the given nodes.
	// Returns nil, nil if metrics-server is unavailable.
	GetNodeMetrics(ctx context.Context, nodeNames []string) ([]metricsv1beta1.NodeMetrics, error)

	// ListPendingPods returns pods in Pending state that are unschedulable.
	ListPendingPods(ctx context.Context) ([]corev1.Pod, error)

	// CordonNode marks a node as unschedulable.
	CordonNode(ctx context.Context, nodeName string) error

	// DrainNode evicts all non-DaemonSet, non-mirror pods from a node.
	// Returns the number of pods evicted.
	DrainNode(ctx context.Context, nodeName string) (int, error)

	// GetNonSystemPodCount returns the number of non-DaemonSet, non-mirror,
	// non-terminal pods on a node. Used to verify drain completion.
	GetNonSystemPodCount(ctx context.Context, nodeName string) (int, error)
}

// WorkloadClientFactory creates WorkloadClients for workload clusters.
type WorkloadClientFactory interface {
	// ClientForCluster returns a WorkloadClient for the named CAPI cluster.
	ClientForCluster(ctx context.Context, clusterName, namespace string) (WorkloadClient, error)
}

// cachedClient holds a cached workload client with expiration.
type cachedClient struct {
	client    *capiWorkloadClient
	expiresAt time.Time
}

// CAPIClientFactory creates WorkloadClients using CAPI kubeconfig Secrets.
type CAPIClientFactory struct {
	mgmtClient client.Client

	mu    sync.Mutex
	cache map[string]*cachedClient
}

// NewCAPIClientFactory creates a new factory that reads kubeconfig Secrets
// from the management cluster.
func NewCAPIClientFactory(mgmtClient client.Client) *CAPIClientFactory {
	return &CAPIClientFactory{
		mgmtClient: mgmtClient,
		cache:      make(map[string]*cachedClient),
	}
}

const clientCacheTTL = 5 * time.Minute

// ClientForCluster returns a WorkloadClient for the named CAPI cluster.
// It reads the <clusterName>-kubeconfig Secret from the management cluster.
func (f *CAPIClientFactory) ClientForCluster(ctx context.Context, clusterName, namespace string) (WorkloadClient, error) {
	key := namespace + "/" + clusterName

	f.mu.Lock()
	if cached, ok := f.cache[key]; ok && time.Now().Before(cached.expiresAt) {
		f.mu.Unlock()
		return cached.client, nil
	}
	f.mu.Unlock()

	// Read kubeconfig Secret
	var secret corev1.Secret
	secretName := clusterName + "-kubeconfig"
	if err := f.mgmtClient.Get(ctx, types.NamespacedName{
		Namespace: namespace,
		Name:      secretName,
	}, &secret); err != nil {
		return nil, fmt.Errorf("getting kubeconfig secret %s/%s: %w", namespace, secretName, err)
	}

	kubeconfigData, ok := secret.Data["value"]
	if !ok {
		return nil, fmt.Errorf("kubeconfig secret %s/%s has no 'value' key", namespace, secretName)
	}

	restConfig, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigData)
	if err != nil {
		return nil, fmt.Errorf("building rest config from kubeconfig: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating kubernetes clientset: %w", err)
	}

	metricsClientset, err := metricsclient.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating metrics clientset: %w", err)
	}

	wc := &capiWorkloadClient{
		mgmtClient:       f.mgmtClient,
		clientset:        clientset,
		metricsClientset: metricsClientset,
		clusterName:      clusterName,
		namespace:        namespace,
	}

	f.mu.Lock()
	f.cache[key] = &cachedClient{
		client:    wc,
		expiresAt: time.Now().Add(clientCacheTTL),
	}
	f.mu.Unlock()

	return wc, nil
}

// capiWorkloadClient implements WorkloadClient using CAPI Machine references.
type capiWorkloadClient struct {
	mgmtClient       client.Client
	clientset        kubernetes.Interface
	metricsClientset metricsclient.Interface
	clusterName      string
	namespace        string
}

// ListNodes finds CAPI Machines for the MachineDeployment, extracts their
// status.nodeRef.name, and fetches the corresponding Nodes from the workload cluster.
func (c *capiWorkloadClient) ListNodes(ctx context.Context, machineDeploymentName string) ([]corev1.Node, error) {
	// List Machines with the deployment-name label on the management cluster
	var machineList clusterv1.MachineList
	if err := c.mgmtClient.List(ctx, &machineList,
		client.InNamespace(c.namespace),
		client.MatchingLabels{
			"cluster.x-k8s.io/deployment-name": machineDeploymentName,
		},
	); err != nil {
		return nil, fmt.Errorf("listing machines for deployment %s: %w", machineDeploymentName, err)
	}

	// Collect node names from Machine status.nodeRef
	var nodeNames []string
	for _, m := range machineList.Items {
		if m.Status.NodeRef.IsDefined() {
			nodeNames = append(nodeNames, m.Status.NodeRef.Name)
		}
	}

	if len(nodeNames) == 0 {
		return nil, nil
	}

	// Fetch nodes from the workload cluster
	var nodes []corev1.Node
	for _, name := range nodeNames {
		node, err := c.clientset.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			klog.V(2).Infof("failed to get node %s from workload cluster: %v", name, err)
			continue
		}
		nodes = append(nodes, *node)
	}

	return nodes, nil
}

// ListPods returns non-terminal pods running on the given nodes.
func (c *capiWorkloadClient) ListPods(ctx context.Context, nodeNames []string) ([]corev1.Pod, error) {
	var allPods []corev1.Pod
	for _, nodeName := range nodeNames {
		podList, err := c.clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
			FieldSelector: "spec.nodeName=" + nodeName,
		})
		if err != nil {
			return nil, fmt.Errorf("listing pods on node %s: %w", nodeName, err)
		}
		for _, pod := range podList.Items {
			// Exclude terminal pods
			if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
				continue
			}
			allPods = append(allPods, pod)
		}
	}
	return allPods, nil
}

// ListPendingPods returns pods in Pending state that have an Unschedulable condition.
func (c *capiWorkloadClient) ListPendingPods(ctx context.Context) ([]corev1.Pod, error) {
	podList, err := c.clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "status.phase=Pending",
	})
	if err != nil {
		return nil, fmt.Errorf("listing pending pods: %w", err)
	}

	var unschedulable []corev1.Pod
	for _, pod := range podList.Items {
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse && cond.Reason == corev1.PodReasonUnschedulable {
				unschedulable = append(unschedulable, pod)
				break
			}
		}
	}
	return unschedulable, nil
}

// CordonNode marks a node as unschedulable in the workload cluster.
func (c *capiWorkloadClient) CordonNode(ctx context.Context, nodeName string) error {
	node, err := c.clientset.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting node %s: %w", nodeName, err)
	}
	if node.Spec.Unschedulable {
		return nil // already cordoned
	}
	node.Spec.Unschedulable = true
	_, err = c.clientset.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("cordoning node %s: %w", nodeName, err)
	}
	klog.V(2).Infof("cordoned node %s in cluster %s", nodeName, c.clusterName)
	return nil
}

// DrainNode evicts all non-DaemonSet, non-mirror, non-terminal pods from a node.
func (c *capiWorkloadClient) DrainNode(ctx context.Context, nodeName string) (int, error) {
	podList, err := c.clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return 0, fmt.Errorf("listing pods on node %s: %w", nodeName, err)
	}

	evicted := 0
	for _, pod := range podList.Items {
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		if isDaemonSetPod(&pod) || isMirrorPod(&pod) {
			continue
		}
		eviction := &policyv1.Eviction{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pod.Name,
				Namespace: pod.Namespace,
			},
		}
		if err := c.clientset.CoreV1().Pods(pod.Namespace).EvictV1(ctx, eviction); err != nil {
			klog.V(2).Infof("failed to evict pod %s/%s from node %s: %v", pod.Namespace, pod.Name, nodeName, err)
			continue
		}
		evicted++
	}
	klog.V(2).Infof("evicted %d pods from node %s in cluster %s", evicted, nodeName, c.clusterName)
	return evicted, nil
}

// GetNonSystemPodCount returns the number of non-DaemonSet, non-mirror,
// non-terminal pods still on a node.
func (c *capiWorkloadClient) GetNonSystemPodCount(ctx context.Context, nodeName string) (int, error) {
	podList, err := c.clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + nodeName,
	})
	if err != nil {
		return 0, fmt.Errorf("listing pods on node %s: %w", nodeName, err)
	}
	count := 0
	for _, pod := range podList.Items {
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		if isDaemonSetPod(&pod) || isMirrorPod(&pod) {
			continue
		}
		count++
	}
	return count, nil
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

// GetNodeMetrics returns metrics-server data for the given nodes.
// Returns nil, nil if metrics-server is unavailable.
func (c *capiWorkloadClient) GetNodeMetrics(ctx context.Context, nodeNames []string) ([]metricsv1beta1.NodeMetrics, error) {
	metricsList, err := c.metricsClientset.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.V(2).Infof("metrics-server unavailable for cluster %s: %v", c.clusterName, err)
		return nil, nil // graceful degradation
	}

	nameSet := make(map[string]bool, len(nodeNames))
	for _, n := range nodeNames {
		nameSet[n] = true
	}

	var result []metricsv1beta1.NodeMetrics
	for _, m := range metricsList.Items {
		if nameSet[m.Name] {
			result = append(result, m)
		}
	}
	return result, nil
}
