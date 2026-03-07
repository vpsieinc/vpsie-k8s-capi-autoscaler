package controller

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	infrav1 "github.com/vpsieinc/cluster-api-provider-vpsie/api/v1alpha1"

	optv1 "github.com/vpsieinc/vpsie-cluster-scaler/api/v1alpha1"
	"github.com/vpsieinc/vpsie-cluster-scaler/internal/pricing"
	"github.com/vpsieinc/vpsie-cluster-scaler/internal/utilization"
	"github.com/vpsieinc/vpsie-cluster-scaler/internal/workload"
)

// TestHorizontal_ScaleUpOnPendingPods verifies that pending pods trigger a
// scale-up by one replica and set the ScaleUpReplicas condition.
func TestHorizontal_ScaleUpOnPendingPods(t *testing.T) {
	objs := newScalingPolicyTestObjects(t, false)

	refreshObject(t, objs.Policy)
	objs.Policy.Spec.Horizontal = optv1.HorizontalSpec{
		Enabled:     true,
		MinReplicas: 1,
		MaxReplicas: 5,
	}
	if err := k8sClient.Update(ctx, objs.Policy); err != nil {
		t.Fatalf("update policy: %v", err)
	}

	// Set MD status so readyReplicas == replicas (no rollout in progress).
	refreshObject(t, objs.MD)
	objs.MD.Status.Deprecated = &clusterv1.MachineDeploymentDeprecatedStatus{
		V1Beta1: &clusterv1.MachineDeploymentV1Beta1DeprecatedStatus{
			ReadyReplicas: 3,
		},
	}
	if err := k8sClient.Status().Update(ctx, objs.MD); err != nil {
		t.Fatalf("update MD status: %v", err)
	}
	refreshObject(t, objs.MD)

	fakeWC := &workload.FakeWorkloadClient{
		PendingPods: []corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "pending-1", Namespace: "default"}},
			{ObjectMeta: metav1.ObjectMeta{Name: "pending-2", Namespace: "default"}},
		},
	}

	r := &ScalingPolicyReconciler{
		Client:          k8sClient,
		Scheme:          testScheme,
		Recorder:        record.NewFakeRecorder(10),
		WorkloadClients: &workload.FakeWorkloadClientFactory{Client: fakeWC},
		caches:          make(map[types.UID]*pricing.Cache),
	}

	refreshObject(t, objs.Policy)
	refreshObject(t, objs.MD)
	err := r.reconcileHorizontal(ctx, objs.Policy, objs.MD, "test-cluster", nil, nil)
	if err != nil {
		t.Fatalf("reconcileHorizontal: %v", err)
	}

	// MachineDeployment replicas should be patched (persisted via r.Patch).
	refreshObject(t, objs.MD)
	if objs.MD.Spec.Replicas == nil || *objs.MD.Spec.Replicas != 4 {
		t.Fatalf("expected 4 replicas, got %v", objs.MD.Spec.Replicas)
	}

	// Condition is set in-memory on the policy.
	cond := meta.FindStatusCondition(objs.Policy.Status.Conditions, optv1.HorizontalScalingCondition)
	if cond == nil {
		t.Fatal("expected HorizontalScaling condition to be set")
	}
	if cond.Reason != optv1.ReasonScaleUpReplicas {
		t.Fatalf("expected reason %s, got %s", optv1.ReasonScaleUpReplicas, cond.Reason)
	}
}

// TestHorizontal_ScaleUpAtMaxReplicas verifies that no scale-up occurs when
// the MachineDeployment is already at the max replicas limit.
func TestHorizontal_ScaleUpAtMaxReplicas(t *testing.T) {
	objs := newScalingPolicyTestObjects(t, false)

	refreshObject(t, objs.Policy)
	objs.Policy.Spec.Horizontal = optv1.HorizontalSpec{
		Enabled:     true,
		MinReplicas: 1,
		MaxReplicas: 3, // MD already has 3 replicas
	}
	if err := k8sClient.Update(ctx, objs.Policy); err != nil {
		t.Fatalf("update policy: %v", err)
	}

	// Set MD status so readyReplicas == replicas.
	refreshObject(t, objs.MD)
	objs.MD.Status.Deprecated = &clusterv1.MachineDeploymentDeprecatedStatus{
		V1Beta1: &clusterv1.MachineDeploymentV1Beta1DeprecatedStatus{
			ReadyReplicas: 3,
		},
	}
	if err := k8sClient.Status().Update(ctx, objs.MD); err != nil {
		t.Fatalf("update MD status: %v", err)
	}
	refreshObject(t, objs.MD)

	fakeWC := &workload.FakeWorkloadClient{
		PendingPods: []corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "pending-1", Namespace: "default"}},
		},
	}

	r := &ScalingPolicyReconciler{
		Client:          k8sClient,
		Scheme:          testScheme,
		Recorder:        record.NewFakeRecorder(10),
		WorkloadClients: &workload.FakeWorkloadClientFactory{Client: fakeWC},
		caches:          make(map[types.UID]*pricing.Cache),
	}

	refreshObject(t, objs.Policy)
	refreshObject(t, objs.MD)
	err := r.reconcileHorizontal(ctx, objs.Policy, objs.MD, "test-cluster", nil, nil)
	if err != nil {
		t.Fatalf("reconcileHorizontal: %v", err)
	}

	// Replicas should remain unchanged.
	refreshObject(t, objs.MD)
	if objs.MD.Spec.Replicas == nil || *objs.MD.Spec.Replicas != 3 {
		t.Fatalf("expected 3 replicas (unchanged), got %v", objs.MD.Spec.Replicas)
	}

	cond := meta.FindStatusCondition(objs.Policy.Status.Conditions, optv1.HorizontalScalingCondition)
	if cond == nil {
		t.Fatal("expected HorizontalScaling condition to be set")
	}
	if cond.Reason != optv1.ReasonMaxReplicasReached {
		t.Fatalf("expected reason %s, got %s", optv1.ReasonMaxReplicasReached, cond.Reason)
	}
}

// TestHorizontal_ScaleDownWithBinPack verifies that low utilization triggers
// a drain-then-scale-down flow: cordon + drain a node, set DrainingNode, but
// do NOT reduce replicas yet (that happens on the next reconcile).
func TestHorizontal_ScaleDownWithBinPack(t *testing.T) {
	objs := newScalingPolicyTestObjects(t, false)

	refreshObject(t, objs.Policy)
	objs.Policy.Spec.Horizontal = optv1.HorizontalSpec{
		Enabled:     true,
		MinReplicas: 1,
		MaxReplicas: 5,
	}
	objs.Policy.Spec.TargetUtilization = optv1.UtilizationSpec{
		ScaleUpThreshold:   75,
		ScaleDownThreshold: 5,
	}
	if err := k8sClient.Update(ctx, objs.Policy); err != nil {
		t.Fatalf("update policy: %v", err)
	}

	// Set MD status so readyReplicas == replicas.
	refreshObject(t, objs.MD)
	objs.MD.Status.Deprecated = &clusterv1.MachineDeploymentDeprecatedStatus{
		V1Beta1: &clusterv1.MachineDeploymentV1Beta1DeprecatedStatus{
			ReadyReplicas: 3,
		},
	}
	if err := k8sClient.Status().Update(ctx, objs.MD); err != nil {
		t.Fatalf("update MD status: %v", err)
	}
	refreshObject(t, objs.MD)

	fakeWC := &workload.FakeWorkloadClient{
		PendingPods:  []corev1.Pod{},
		DrainEvicted: 2,
	}

	r := &ScalingPolicyReconciler{
		Client:          k8sClient,
		Scheme:          testScheme,
		Recorder:        record.NewFakeRecorder(10),
		WorkloadClients: &workload.FakeWorkloadClientFactory{Client: fakeWC},
		caches:          make(map[types.UID]*pricing.Cache),
	}

	// Utilization result with 2 nodes at very low utilization (below 5% threshold).
	// Each node has small pods so bin-packing simulation passes.
	utilResult := &utilization.Result{
		ScheduledCPUPercent:    2,
		ScheduledMemoryPercent: 2,
		Source:                 "requests",
		Nodes: []utilization.NodeUtilization{
			{
				NodeName:       "node-1",
				AllocatableCPU: 4000,                 // 4000m
				AllocatableRAM: 8 * 1024 * 1024 * 1024, // 8GiB
				RequestedCPU:   80,                    // 80m
				RequestedRAM:   160 * 1024 * 1024,     // 160MiB
				Pods: []corev1.Pod{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "app-1", Namespace: "default"},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("50m"),
										corev1.ResourceMemory: resource.MustParse("128Mi"),
									},
								},
							}},
						},
					},
				},
			},
			{
				NodeName:       "node-2",
				AllocatableCPU: 4000,
				AllocatableRAM: 8 * 1024 * 1024 * 1024,
				RequestedCPU:   80,
				RequestedRAM:   160 * 1024 * 1024,
				Pods: []corev1.Pod{
					{
						ObjectMeta: metav1.ObjectMeta{Name: "app-2", Namespace: "default"},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    resource.MustParse("50m"),
										corev1.ResourceMemory: resource.MustParse("128Mi"),
									},
								},
							}},
						},
					},
				},
			},
		},
	}

	refreshObject(t, objs.Policy)
	refreshObject(t, objs.MD)
	err := r.reconcileHorizontal(ctx, objs.Policy, objs.MD, "test-cluster", utilResult, nil)
	if err != nil {
		t.Fatalf("reconcileHorizontal: %v", err)
	}

	// Replicas should NOT be reduced yet (drain in progress).
	refreshObject(t, objs.MD)
	if objs.MD.Spec.Replicas == nil || *objs.MD.Spec.Replicas != 3 {
		t.Fatalf("expected 3 replicas (unchanged during drain), got %v", objs.MD.Spec.Replicas)
	}

	// DrainingNode should be set in policy status (in-memory).
	if objs.Policy.Status.DrainingNode == "" {
		t.Fatal("expected DrainingNode to be set")
	}

	// Verify cordon + drain were called on the fake workload client.
	if len(fakeWC.CordonedNodes) == 0 {
		t.Fatal("expected at least one node to be cordoned")
	}
	if len(fakeWC.DrainedNodes) == 0 {
		t.Fatal("expected at least one node to be drained")
	}

	cond := meta.FindStatusCondition(objs.Policy.Status.Conditions, optv1.HorizontalScalingCondition)
	if cond == nil {
		t.Fatal("expected HorizontalScaling condition to be set")
	}
	if cond.Reason != optv1.ReasonScaleDownReplicas {
		t.Fatalf("expected reason %s, got %s", optv1.ReasonScaleDownReplicas, cond.Reason)
	}
}

// TestHorizontal_DrainVerifyAndReduce verifies that when a drain is complete
// (0 non-system pods remaining, 0 pending pods), the controller reduces replicas.
func TestHorizontal_DrainVerifyAndReduce(t *testing.T) {
	objs := newScalingPolicyTestObjects(t, false)

	refreshObject(t, objs.Policy)
	objs.Policy.Spec.Horizontal = optv1.HorizontalSpec{
		Enabled:     true,
		MinReplicas: 1,
		MaxReplicas: 5,
	}
	if err := k8sClient.Update(ctx, objs.Policy); err != nil {
		t.Fatalf("update policy: %v", err)
	}

	// Simulate a drain in progress from the previous reconcile.
	refreshObject(t, objs.Policy)
	now := metav1.Now()
	objs.Policy.Status.DrainingNode = "node-1"
	objs.Policy.Status.DrainingStartedAt = &now

	fakeWC := &workload.FakeWorkloadClient{
		NonSystemPodCount: 0,
		PendingPods:       []corev1.Pod{}, // no pending pods
	}

	r := &ScalingPolicyReconciler{
		Client:          k8sClient,
		Scheme:          testScheme,
		Recorder:        record.NewFakeRecorder(10),
		WorkloadClients: &workload.FakeWorkloadClientFactory{Client: fakeWC},
		caches:          make(map[types.UID]*pricing.Cache),
	}

	refreshObject(t, objs.MD)
	err := r.reconcileHorizontal(ctx, objs.Policy, objs.MD, "test-cluster", nil, nil)
	if err != nil {
		t.Fatalf("reconcileHorizontal: %v", err)
	}

	// Replicas should be reduced from 3 to 2.
	refreshObject(t, objs.MD)
	if objs.MD.Spec.Replicas == nil || *objs.MD.Spec.Replicas != 2 {
		t.Fatalf("expected 2 replicas after drain complete, got %v", objs.MD.Spec.Replicas)
	}

	// DrainingNode and DrainingStartedAt should be cleared.
	if objs.Policy.Status.DrainingNode != "" {
		t.Fatalf("expected DrainingNode to be cleared, got %s", objs.Policy.Status.DrainingNode)
	}
	if objs.Policy.Status.DrainingStartedAt != nil {
		t.Fatal("expected DrainingStartedAt to be cleared")
	}

	cond := meta.FindStatusCondition(objs.Policy.Status.Conditions, optv1.HorizontalScalingCondition)
	if cond == nil {
		t.Fatal("expected HorizontalScaling condition to be set")
	}
	if cond.Reason != optv1.ReasonScaleDownReplicas {
		t.Fatalf("expected reason %s, got %s", optv1.ReasonScaleDownReplicas, cond.Reason)
	}
}

// TestHorizontal_DrainAbortOnPendingPods verifies that when a drain completes
// but new pending pods appear, the controller aborts the scale-down by
// uncordoning the node and clearing the drain state.
func TestHorizontal_DrainAbortOnPendingPods(t *testing.T) {
	objs := newScalingPolicyTestObjects(t, false)

	refreshObject(t, objs.Policy)
	objs.Policy.Spec.Horizontal = optv1.HorizontalSpec{
		Enabled:     true,
		MinReplicas: 1,
		MaxReplicas: 5,
	}
	if err := k8sClient.Update(ctx, objs.Policy); err != nil {
		t.Fatalf("update policy: %v", err)
	}

	// Simulate a drain in progress.
	refreshObject(t, objs.Policy)
	now := metav1.Now()
	objs.Policy.Status.DrainingNode = "node-1"
	objs.Policy.Status.DrainingStartedAt = &now

	fakeWC := &workload.FakeWorkloadClient{
		NonSystemPodCount: 0, // drain complete
		PendingPods: []corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "new-pending", Namespace: "default"}},
		},
	}

	r := &ScalingPolicyReconciler{
		Client:          k8sClient,
		Scheme:          testScheme,
		Recorder:        record.NewFakeRecorder(10),
		WorkloadClients: &workload.FakeWorkloadClientFactory{Client: fakeWC},
		caches:          make(map[types.UID]*pricing.Cache),
	}

	refreshObject(t, objs.MD)
	err := r.reconcileHorizontal(ctx, objs.Policy, objs.MD, "test-cluster", nil, nil)
	if err != nil {
		t.Fatalf("reconcileHorizontal: %v", err)
	}

	// Replicas should remain unchanged.
	refreshObject(t, objs.MD)
	if objs.MD.Spec.Replicas == nil || *objs.MD.Spec.Replicas != 3 {
		t.Fatalf("expected 3 replicas (unchanged), got %v", objs.MD.Spec.Replicas)
	}

	// DrainingNode should be cleared.
	if objs.Policy.Status.DrainingNode != "" {
		t.Fatalf("expected DrainingNode to be cleared, got %s", objs.Policy.Status.DrainingNode)
	}

	// Node should have been uncordoned.
	if len(fakeWC.UncordonedNodes) == 0 {
		t.Fatal("expected node-1 to be uncordoned")
	}
	found := false
	for _, n := range fakeWC.UncordonedNodes {
		if n == "node-1" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected node-1 in uncordoned list, got %v", fakeWC.UncordonedNodes)
	}

	cond := meta.FindStatusCondition(objs.Policy.Status.Conditions, optv1.HorizontalScalingCondition)
	if cond == nil {
		t.Fatal("expected HorizontalScaling condition to be set")
	}
	if cond.Reason != optv1.ReasonDrainAborted {
		t.Fatalf("expected reason %s, got %s", optv1.ReasonDrainAborted, cond.Reason)
	}
}

// TestHorizontal_DrainTimeout verifies that a drain that exceeds the timeout
// results in uncordoning the node and aborting the scale-down.
func TestHorizontal_DrainTimeout(t *testing.T) {
	objs := newScalingPolicyTestObjects(t, false)

	refreshObject(t, objs.Policy)
	objs.Policy.Spec.Horizontal = optv1.HorizontalSpec{
		Enabled:     true,
		MinReplicas: 1,
		MaxReplicas: 5,
	}
	if err := k8sClient.Update(ctx, objs.Policy); err != nil {
		t.Fatalf("update policy: %v", err)
	}

	// Simulate a drain that started 10 minutes ago (well past the 5min timeout).
	refreshObject(t, objs.Policy)
	tenMinAgo := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	objs.Policy.Status.DrainingNode = "node-1"
	objs.Policy.Status.DrainingStartedAt = &tenMinAgo

	fakeWC := &workload.FakeWorkloadClient{
		NonSystemPodCount: 3, // still has pods (drain not complete)
	}

	r := &ScalingPolicyReconciler{
		Client:          k8sClient,
		Scheme:          testScheme,
		Recorder:        record.NewFakeRecorder(10),
		WorkloadClients: &workload.FakeWorkloadClientFactory{Client: fakeWC},
		caches:          make(map[types.UID]*pricing.Cache),
	}

	refreshObject(t, objs.MD)
	err := r.reconcileHorizontal(ctx, objs.Policy, objs.MD, "test-cluster", nil, nil)
	if err != nil {
		t.Fatalf("reconcileHorizontal: %v", err)
	}

	// DrainingNode and DrainingStartedAt should be cleared.
	if objs.Policy.Status.DrainingNode != "" {
		t.Fatalf("expected DrainingNode to be cleared, got %s", objs.Policy.Status.DrainingNode)
	}
	if objs.Policy.Status.DrainingStartedAt != nil {
		t.Fatal("expected DrainingStartedAt to be cleared")
	}

	// Node should have been uncordoned.
	found := false
	for _, n := range fakeWC.UncordonedNodes {
		if n == "node-1" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected node-1 in uncordoned list, got %v", fakeWC.UncordonedNodes)
	}

	cond := meta.FindStatusCondition(objs.Policy.Status.Conditions, optv1.HorizontalScalingCondition)
	if cond == nil {
		t.Fatal("expected HorizontalScaling condition to be set")
	}
	if cond.Reason != optv1.ReasonDrainTimeout {
		t.Fatalf("expected reason %s, got %s", optv1.ReasonDrainTimeout, cond.Reason)
	}
}

// TestHorizontal_StabilizationWindow verifies that scale-down is blocked
// when the last scale event is within the stabilization window.
func TestHorizontal_StabilizationWindow(t *testing.T) {
	objs := newScalingPolicyTestObjects(t, false)

	refreshObject(t, objs.Policy)
	objs.Policy.Spec.Horizontal = optv1.HorizontalSpec{
		Enabled:     true,
		MinReplicas: 1,
		MaxReplicas: 5,
	}
	objs.Policy.Spec.TargetUtilization = optv1.UtilizationSpec{
		ScaleUpThreshold:   75,
		ScaleDownThreshold: 5,
	}
	if err := k8sClient.Update(ctx, objs.Policy); err != nil {
		t.Fatalf("update policy: %v", err)
	}

	// Set MD status so readyReplicas == replicas.
	refreshObject(t, objs.MD)
	objs.MD.Status.Deprecated = &clusterv1.MachineDeploymentDeprecatedStatus{
		V1Beta1: &clusterv1.MachineDeploymentV1Beta1DeprecatedStatus{
			ReadyReplicas: 3,
		},
	}
	if err := k8sClient.Status().Update(ctx, objs.MD); err != nil {
		t.Fatalf("update MD status: %v", err)
	}
	refreshObject(t, objs.MD)

	// Set LastScaleTime to 1 minute ago (within 5min default stabilization).
	refreshObject(t, objs.Policy)
	oneMinAgo := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	objs.Policy.Status.LastScaleTime = &oneMinAgo

	fakeWC := &workload.FakeWorkloadClient{
		PendingPods: []corev1.Pod{},
	}

	r := &ScalingPolicyReconciler{
		Client:          k8sClient,
		Scheme:          testScheme,
		Recorder:        record.NewFakeRecorder(10),
		WorkloadClients: &workload.FakeWorkloadClientFactory{Client: fakeWC},
		caches:          make(map[types.UID]*pricing.Cache),
	}

	// Low utilization that would normally trigger a scale-down.
	utilResult := &utilization.Result{
		ScheduledCPUPercent:    2,
		ScheduledMemoryPercent: 2,
		Source:                 "requests",
		Nodes: []utilization.NodeUtilization{
			{
				NodeName:       "node-1",
				AllocatableCPU: 4000,
				AllocatableRAM: 8 * 1024 * 1024 * 1024,
				RequestedCPU:   80,
				RequestedRAM:   160 * 1024 * 1024,
			},
		},
	}

	err := r.reconcileHorizontal(ctx, objs.Policy, objs.MD, "test-cluster", utilResult, nil)
	if err != nil {
		t.Fatalf("reconcileHorizontal: %v", err)
	}

	// Replicas should remain unchanged due to stabilization window.
	refreshObject(t, objs.MD)
	if objs.MD.Spec.Replicas == nil || *objs.MD.Spec.Replicas != 3 {
		t.Fatalf("expected 3 replicas (stabilization active), got %v", objs.MD.Spec.Replicas)
	}

	cond := meta.FindStatusCondition(objs.Policy.Status.Conditions, optv1.HorizontalScalingCondition)
	if cond == nil {
		t.Fatal("expected HorizontalScaling condition to be set")
	}
	if cond.Reason != optv1.ReasonStabilizationActive {
		t.Fatalf("expected reason %s, got %s", optv1.ReasonStabilizationActive, cond.Reason)
	}
}

// TestHorizontal_RolloutInProgress verifies that no scaling action is taken
// when a MachineDeployment rollout is in progress (readyReplicas < replicas).
func TestHorizontal_RolloutInProgress(t *testing.T) {
	objs := newScalingPolicyTestObjects(t, false)

	refreshObject(t, objs.Policy)
	objs.Policy.Spec.Horizontal = optv1.HorizontalSpec{
		Enabled:     true,
		MinReplicas: 1,
		MaxReplicas: 5,
	}
	if err := k8sClient.Update(ctx, objs.Policy); err != nil {
		t.Fatalf("update policy: %v", err)
	}

	// Set readyReplicas < replicas to simulate rollout in progress.
	refreshObject(t, objs.MD)
	objs.MD.Status.Deprecated = &clusterv1.MachineDeploymentDeprecatedStatus{
		V1Beta1: &clusterv1.MachineDeploymentV1Beta1DeprecatedStatus{
			ReadyReplicas: 2,
		},
	}
	if err := k8sClient.Status().Update(ctx, objs.MD); err != nil {
		t.Fatalf("update MD status: %v", err)
	}
	refreshObject(t, objs.MD)

	fakeWC := &workload.FakeWorkloadClient{
		PendingPods: []corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "pending-1", Namespace: "default"}},
		},
	}

	r := &ScalingPolicyReconciler{
		Client:          k8sClient,
		Scheme:          testScheme,
		Recorder:        record.NewFakeRecorder(10),
		WorkloadClients: &workload.FakeWorkloadClientFactory{Client: fakeWC},
		caches:          make(map[types.UID]*pricing.Cache),
	}

	refreshObject(t, objs.Policy)
	refreshObject(t, objs.MD)
	err := r.reconcileHorizontal(ctx, objs.Policy, objs.MD, "test-cluster", nil, nil)
	if err != nil {
		t.Fatalf("reconcileHorizontal: %v", err)
	}

	// Replicas should remain unchanged.
	refreshObject(t, objs.MD)
	if objs.MD.Spec.Replicas == nil || *objs.MD.Spec.Replicas != 3 {
		t.Fatalf("expected 3 replicas (rollout in progress), got %v", objs.MD.Spec.Replicas)
	}

	cond := meta.FindStatusCondition(objs.Policy.Status.Conditions, optv1.HorizontalScalingCondition)
	if cond == nil {
		t.Fatal("expected HorizontalScaling condition to be set")
	}
	if cond.Reason != optv1.ReasonRolloutInProgress {
		t.Fatalf("expected reason %s, got %s", optv1.ReasonRolloutInProgress, cond.Reason)
	}
}

// TestHorizontal_RolloutStalled verifies that when a rollout has been in progress
// beyond the stall timeout, the controller reports a stalled condition.
func TestHorizontal_RolloutStalled(t *testing.T) {
	objs := newScalingPolicyTestObjects(t, false)

	refreshObject(t, objs.Policy)
	objs.Policy.Spec.Horizontal = optv1.HorizontalSpec{
		Enabled:     true,
		MinReplicas: 1,
		MaxReplicas: 5,
	}
	if err := k8sClient.Update(ctx, objs.Policy); err != nil {
		t.Fatalf("update policy: %v", err)
	}

	// Set readyReplicas < replicas to simulate rollout in progress.
	refreshObject(t, objs.MD)
	objs.MD.Status.Deprecated = &clusterv1.MachineDeploymentDeprecatedStatus{
		V1Beta1: &clusterv1.MachineDeploymentV1Beta1DeprecatedStatus{
			ReadyReplicas: 2,
		},
	}
	if err := k8sClient.Status().Update(ctx, objs.MD); err != nil {
		t.Fatalf("update MD status: %v", err)
	}
	refreshObject(t, objs.MD)

	// Set LastScaleTime to 20 minutes ago (beyond 15min stall timeout).
	refreshObject(t, objs.Policy)
	twentyMinAgo := metav1.NewTime(time.Now().Add(-20 * time.Minute))
	objs.Policy.Status.LastScaleTime = &twentyMinAgo

	fakeWC := &workload.FakeWorkloadClient{
		PendingPods: []corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "pending-1", Namespace: "default"}},
		},
	}

	r := &ScalingPolicyReconciler{
		Client:          k8sClient,
		Scheme:          testScheme,
		Recorder:        record.NewFakeRecorder(10),
		WorkloadClients: &workload.FakeWorkloadClientFactory{Client: fakeWC},
		caches:          make(map[types.UID]*pricing.Cache),
	}

	refreshObject(t, objs.MD)
	err := r.reconcileHorizontal(ctx, objs.Policy, objs.MD, "test-cluster", nil, nil)
	if err != nil {
		t.Fatalf("reconcileHorizontal: %v", err)
	}

	// Replicas should remain unchanged.
	refreshObject(t, objs.MD)
	if objs.MD.Spec.Replicas == nil || *objs.MD.Spec.Replicas != 3 {
		t.Fatalf("expected 3 replicas (stalled), got %v", objs.MD.Spec.Replicas)
	}

	cond := meta.FindStatusCondition(objs.Policy.Status.Conditions, optv1.HorizontalScalingCondition)
	if cond == nil {
		t.Fatal("expected HorizontalScaling condition to be set")
	}
	if cond.Reason != optv1.ReasonRolloutStalled {
		t.Fatalf("expected reason %s, got %s", optv1.ReasonRolloutStalled, cond.Reason)
	}
}

// TestHorizontal_DryRunScaleUp verifies that in dry-run mode, pending pods
// trigger a DryRun condition but do not actually change replicas.
func TestHorizontal_DryRunScaleUp(t *testing.T) {
	objs := newScalingPolicyTestObjects(t, false)

	refreshObject(t, objs.Policy)
	objs.Policy.Spec.Horizontal = optv1.HorizontalSpec{
		Enabled:     true,
		MinReplicas: 1,
		MaxReplicas: 5,
	}
	objs.Policy.Spec.DryRun = true
	if err := k8sClient.Update(ctx, objs.Policy); err != nil {
		t.Fatalf("update policy: %v", err)
	}

	// Set MD status so readyReplicas == replicas.
	refreshObject(t, objs.MD)
	objs.MD.Status.Deprecated = &clusterv1.MachineDeploymentDeprecatedStatus{
		V1Beta1: &clusterv1.MachineDeploymentV1Beta1DeprecatedStatus{
			ReadyReplicas: 3,
		},
	}
	if err := k8sClient.Status().Update(ctx, objs.MD); err != nil {
		t.Fatalf("update MD status: %v", err)
	}
	refreshObject(t, objs.MD)

	fakeWC := &workload.FakeWorkloadClient{
		PendingPods: []corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "pending-1", Namespace: "default"}},
		},
	}

	r := &ScalingPolicyReconciler{
		Client:          k8sClient,
		Scheme:          testScheme,
		Recorder:        record.NewFakeRecorder(10),
		WorkloadClients: &workload.FakeWorkloadClientFactory{Client: fakeWC},
		caches:          make(map[types.UID]*pricing.Cache),
	}

	refreshObject(t, objs.Policy)
	refreshObject(t, objs.MD)
	err := r.reconcileHorizontal(ctx, objs.Policy, objs.MD, "test-cluster", nil, nil)
	if err != nil {
		t.Fatalf("reconcileHorizontal: %v", err)
	}

	// Replicas should NOT change in dry-run mode.
	refreshObject(t, objs.MD)
	if objs.MD.Spec.Replicas == nil || *objs.MD.Spec.Replicas != 3 {
		t.Fatalf("expected 3 replicas (dry run), got %v", objs.MD.Spec.Replicas)
	}

	cond := meta.FindStatusCondition(objs.Policy.Status.Conditions, optv1.HorizontalScalingCondition)
	if cond == nil {
		t.Fatal("expected HorizontalScaling condition to be set")
	}
	if cond.Reason != optv1.ReasonDryRun {
		t.Fatalf("expected reason %s, got %s", optv1.ReasonDryRun, cond.Reason)
	}
}

// TestLeastUtilizedNode is a pure unit test (no envtest) that verifies
// leastUtilizedNode returns the node with the lowest combined CPU+memory ratio.
func TestLeastUtilizedNode(t *testing.T) {
	r := &ScalingPolicyReconciler{}

	utilResult := &utilization.Result{
		Nodes: []utilization.NodeUtilization{
			{
				NodeName:       "node-high",
				AllocatableCPU: 4000,
				AllocatableRAM: 8 * 1024 * 1024 * 1024,
				RequestedCPU:   3000,                      // 75% CPU
				RequestedRAM:   6 * 1024 * 1024 * 1024,    // 75% RAM
			},
			{
				NodeName:       "node-low",
				AllocatableCPU: 4000,
				AllocatableRAM: 8 * 1024 * 1024 * 1024,
				RequestedCPU:   200,                       // 5% CPU
				RequestedRAM:   400 * 1024 * 1024,         // ~5% RAM
			},
			{
				NodeName:       "node-medium",
				AllocatableCPU: 4000,
				AllocatableRAM: 8 * 1024 * 1024 * 1024,
				RequestedCPU:   2000,                      // 50% CPU
				RequestedRAM:   4 * 1024 * 1024 * 1024,    // 50% RAM
			},
		},
	}

	result := r.leastUtilizedNode(utilResult)
	if result != "node-low" {
		t.Fatalf("expected node-low as least utilized, got %s", result)
	}

	// Test nil result.
	if got := r.leastUtilizedNode(nil); got != "" {
		t.Fatalf("expected empty string for nil result, got %s", got)
	}

	// Test empty nodes.
	if got := r.leastUtilizedNode(&utilization.Result{}); got != "" {
		t.Fatalf("expected empty string for empty nodes, got %s", got)
	}
}

// TestHorizontal_RolloutStalledWithRevert verifies that when a vertical rollout
// stalls and PreviousInfraTemplate is set, the controller reverts the MD's
// infrastructureRef.Name to the previous template and clears PreviousInfraTemplate.
func TestHorizontal_RolloutStalledWithRevert(t *testing.T) {
	objs := newScalingPolicyTestObjects(t, false)

	refreshObject(t, objs.Policy)
	objs.Policy.Spec.Horizontal = optv1.HorizontalSpec{
		Enabled:     true,
		MinReplicas: 1,
		MaxReplicas: 5,
	}
	if err := k8sClient.Update(ctx, objs.Policy); err != nil {
		t.Fatalf("update policy: %v", err)
	}

	// Create the "new" template that caused the stall.
	newTmpl := &infrav1.VPSieMachineTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "workers-s-1vcpu-2gb",
			Namespace: objs.Namespace,
		},
		Spec: infrav1.VPSieMachineTemplateSpec{
			Template: infrav1.VPSieMachineTemplateResource{
				Spec: infrav1.VPSieMachineSpec{
					ResourceIdentifier: "plan-small",
					DCIdentifier:       "dc-test-1",
					ImageIdentifier:    "img-talos-123",
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, newTmpl); err != nil {
		t.Fatalf("create new VPSieMachineTemplate: %v", err)
	}

	// Patch MD's infrastructureRef to the new template (simulating a plan switch happened).
	refreshObject(t, objs.MD)
	objs.MD.Spec.Template.Spec.InfrastructureRef.Name = "workers-s-1vcpu-2gb"
	if err := k8sClient.Update(ctx, objs.MD); err != nil {
		t.Fatalf("update MD infraRef: %v", err)
	}

	// Set PreviousInfraTemplate to the original template.
	refreshObject(t, objs.Policy)
	objs.Policy.Status.PreviousInfraTemplate = "workers-template"

	// Set readyReplicas < replicas (rollout in progress).
	refreshObject(t, objs.MD)
	objs.MD.Status.Deprecated = &clusterv1.MachineDeploymentDeprecatedStatus{
		V1Beta1: &clusterv1.MachineDeploymentV1Beta1DeprecatedStatus{
			ReadyReplicas: 2,
		},
	}
	if err := k8sClient.Status().Update(ctx, objs.MD); err != nil {
		t.Fatalf("update MD status: %v", err)
	}
	refreshObject(t, objs.MD)

	// Set LastScaleTime to 20 minutes ago (beyond 15min stall timeout).
	refreshObject(t, objs.Policy)
	twentyMinAgo := metav1.NewTime(time.Now().Add(-20 * time.Minute))
	objs.Policy.Status.LastScaleTime = &twentyMinAgo
	objs.Policy.Status.PreviousInfraTemplate = "workers-template"

	fakeWC := &workload.FakeWorkloadClient{
		PendingPods: []corev1.Pod{},
	}

	r := &ScalingPolicyReconciler{
		Client:          k8sClient,
		Scheme:          testScheme,
		Recorder:        record.NewFakeRecorder(10),
		WorkloadClients: &workload.FakeWorkloadClientFactory{Client: fakeWC},
		caches:          make(map[types.UID]*pricing.Cache),
	}

	refreshObject(t, objs.MD)
	err := r.reconcileHorizontal(ctx, objs.Policy, objs.MD, "test-cluster", nil, nil)
	if err != nil {
		t.Fatalf("reconcileHorizontal: %v", err)
	}

	// MD's infrastructureRef should be reverted to the original template.
	refreshObject(t, objs.MD)
	if objs.MD.Spec.Template.Spec.InfrastructureRef.Name != "workers-template" {
		t.Fatalf("expected infrastructureRef.Name reverted to workers-template, got %s",
			objs.MD.Spec.Template.Spec.InfrastructureRef.Name)
	}

	// PreviousInfraTemplate should be cleared.
	if objs.Policy.Status.PreviousInfraTemplate != "" {
		t.Fatalf("expected PreviousInfraTemplate to be cleared, got %s",
			objs.Policy.Status.PreviousInfraTemplate)
	}

	// PlanSelectedCondition should exist with reason TemplateReverted.
	cond := meta.FindStatusCondition(objs.Policy.Status.Conditions, optv1.PlanSelectedCondition)
	if cond == nil {
		t.Fatal("expected PlanSelected condition to be set")
	}
	if cond.Reason != optv1.ReasonTemplateReverted {
		t.Fatalf("expected reason %s, got %s", optv1.ReasonTemplateReverted, cond.Reason)
	}
}

// TestHorizontal_RolloutStalledWithoutPreviousTemplate verifies that when a
// rollout stalls but NO PreviousInfraTemplate is set (horizontal stall),
// only an alert is emitted and no revert occurs.
func TestHorizontal_RolloutStalledWithoutPreviousTemplate(t *testing.T) {
	objs := newScalingPolicyTestObjects(t, false)

	refreshObject(t, objs.Policy)
	objs.Policy.Spec.Horizontal = optv1.HorizontalSpec{
		Enabled:     true,
		MinReplicas: 1,
		MaxReplicas: 5,
	}
	if err := k8sClient.Update(ctx, objs.Policy); err != nil {
		t.Fatalf("update policy: %v", err)
	}

	// Set readyReplicas < replicas to simulate rollout in progress.
	refreshObject(t, objs.MD)
	objs.MD.Status.Deprecated = &clusterv1.MachineDeploymentDeprecatedStatus{
		V1Beta1: &clusterv1.MachineDeploymentV1Beta1DeprecatedStatus{
			ReadyReplicas: 2,
		},
	}
	if err := k8sClient.Status().Update(ctx, objs.MD); err != nil {
		t.Fatalf("update MD status: %v", err)
	}
	refreshObject(t, objs.MD)

	// Set LastScaleTime to 20 minutes ago (beyond 15min stall timeout).
	// Do NOT set PreviousInfraTemplate.
	refreshObject(t, objs.Policy)
	twentyMinAgo := metav1.NewTime(time.Now().Add(-20 * time.Minute))
	objs.Policy.Status.LastScaleTime = &twentyMinAgo

	fakeWC := &workload.FakeWorkloadClient{
		PendingPods: []corev1.Pod{
			{ObjectMeta: metav1.ObjectMeta{Name: "pending-1", Namespace: "default"}},
		},
	}

	r := &ScalingPolicyReconciler{
		Client:          k8sClient,
		Scheme:          testScheme,
		Recorder:        record.NewFakeRecorder(10),
		WorkloadClients: &workload.FakeWorkloadClientFactory{Client: fakeWC},
		caches:          make(map[types.UID]*pricing.Cache),
	}

	refreshObject(t, objs.MD)
	err := r.reconcileHorizontal(ctx, objs.Policy, objs.MD, "test-cluster", nil, nil)
	if err != nil {
		t.Fatalf("reconcileHorizontal: %v", err)
	}

	// MD's infrastructureRef should remain unchanged.
	refreshObject(t, objs.MD)
	if objs.MD.Spec.Template.Spec.InfrastructureRef.Name != "workers-template" {
		t.Fatalf("expected infrastructureRef.Name unchanged as workers-template, got %s",
			objs.MD.Spec.Template.Spec.InfrastructureRef.Name)
	}

	// HorizontalScalingCondition should exist with reason RolloutStalled.
	cond := meta.FindStatusCondition(objs.Policy.Status.Conditions, optv1.HorizontalScalingCondition)
	if cond == nil {
		t.Fatal("expected HorizontalScaling condition to be set")
	}
	if cond.Reason != optv1.ReasonRolloutStalled {
		t.Fatalf("expected reason %s, got %s", optv1.ReasonRolloutStalled, cond.Reason)
	}

	// PlanSelectedCondition with reason TemplateReverted should NOT exist.
	planCond := meta.FindStatusCondition(objs.Policy.Status.Conditions, optv1.PlanSelectedCondition)
	if planCond != nil && planCond.Reason == optv1.ReasonTemplateReverted {
		t.Fatal("expected NO PlanSelected condition with reason TemplateReverted")
	}
}

// TestHorizontal_ClearPreviousTemplateOnSuccess verifies that when a rollout
// completes successfully, PreviousInfraTemplate is cleared.
func TestHorizontal_ClearPreviousTemplateOnSuccess(t *testing.T) {
	objs := newScalingPolicyTestObjects(t, false)

	refreshObject(t, objs.Policy)
	objs.Policy.Spec.Horizontal = optv1.HorizontalSpec{
		Enabled:     true,
		MinReplicas: 1,
		MaxReplicas: 5,
	}
	if err := k8sClient.Update(ctx, objs.Policy); err != nil {
		t.Fatalf("update policy: %v", err)
	}

	// Set readyReplicas == replicas (rollout complete).
	refreshObject(t, objs.MD)
	objs.MD.Status.Deprecated = &clusterv1.MachineDeploymentDeprecatedStatus{
		V1Beta1: &clusterv1.MachineDeploymentV1Beta1DeprecatedStatus{
			ReadyReplicas: 3,
		},
	}
	if err := k8sClient.Status().Update(ctx, objs.MD); err != nil {
		t.Fatalf("update MD status: %v", err)
	}
	refreshObject(t, objs.MD)

	// Set PreviousInfraTemplate to simulate a previous plan switch.
	refreshObject(t, objs.Policy)
	objs.Policy.Status.PreviousInfraTemplate = "workers-old-template"

	// Set LastScaleTime to 2 minutes ago (recent, not stalled).
	twoMinAgo := metav1.NewTime(time.Now().Add(-2 * time.Minute))
	objs.Policy.Status.LastScaleTime = &twoMinAgo

	fakeWC := &workload.FakeWorkloadClient{
		PendingPods: []corev1.Pod{},
		Nodes:       []corev1.Node{},
		Pods:        []corev1.Pod{},
	}

	r := &ScalingPolicyReconciler{
		Client:          k8sClient,
		Scheme:          testScheme,
		Recorder:        record.NewFakeRecorder(10),
		WorkloadClients: &workload.FakeWorkloadClientFactory{Client: fakeWC},
		caches:          make(map[types.UID]*pricing.Cache),
	}

	refreshObject(t, objs.MD)
	err := r.reconcileHorizontal(ctx, objs.Policy, objs.MD, "test-cluster", nil, nil)
	if err != nil {
		t.Fatalf("reconcileHorizontal: %v", err)
	}

	// PreviousInfraTemplate should be cleared after successful rollout.
	if objs.Policy.Status.PreviousInfraTemplate != "" {
		t.Fatalf("expected PreviousInfraTemplate to be cleared, got %s",
			objs.Policy.Status.PreviousInfraTemplate)
	}
}
