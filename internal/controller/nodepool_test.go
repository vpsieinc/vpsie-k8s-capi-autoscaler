package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	infrav1 "github.com/vpsieinc/cluster-api-provider-vpsie/api/v1alpha1"

	optv1 "github.com/vpsieinc/vpsie-cluster-scaler/api/v1alpha1"
	"github.com/vpsieinc/vpsie-cluster-scaler/internal/pricing"
	"github.com/vpsieinc/vpsie-cluster-scaler/internal/vpsie"
	"github.com/vpsieinc/vpsie-cluster-scaler/internal/workload"
)

func TestSatelliteMDName(t *testing.T) {
	tests := []struct {
		baseName     string
		planNickname string
		want         string
	}{
		{"workers", "s-4vcpu-8gb", "workers-pool-s-4vcpu-8gb"},
		{"workers", "S 4vCPU 8GB", "workers-pool-s-4vcpu-8gb"},
		{"my-nodes", "High Memory 32", "my-nodes-pool-high-memory-32"},
	}
	for _, tc := range tests {
		got := satelliteMDName(tc.baseName, tc.planNickname)
		if got != tc.want {
			t.Errorf("satelliteMDName(%q, %q) = %q, want %q", tc.baseName, tc.planNickname, got, tc.want)
		}
	}
}

func TestPodResourceRequests(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("1Gi"),
						},
					},
				},
				{
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
					},
				},
			},
		},
	}

	cpuMillis, ramBytes := podResourceRequests(pod)
	if cpuMillis != 750 {
		t.Errorf("expected 750m CPU, got %d", cpuMillis)
	}
	expectedRAM := int64(1536 * 1024 * 1024) // 1.5 GiB
	if ramBytes != expectedRAM {
		t.Errorf("expected %d bytes RAM, got %d", expectedRAM, ramBytes)
	}
}

func TestPodFitsCurrentPlan(t *testing.T) {
	smallPlan := vpsie.Plan{CPU: 2, RAM: 4096} // 2 vCPU, 4GB
	largePlan := vpsie.Plan{CPU: 8, RAM: 32768} // 8 vCPU, 32GB

	smallPod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("500m"),
						corev1.ResourceMemory: resource.MustParse("1Gi"),
					},
				},
			}},
		},
	}

	bigPod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("2"),
						corev1.ResourceMemory: resource.MustParse("28Gi"),
					},
				},
			}},
		},
	}

	// Small pod fits on both plans
	if !podFitsCurrentPlan(smallPod, &smallPlan) {
		t.Error("small pod should fit on small plan")
	}
	if !podFitsCurrentPlan(smallPod, &largePlan) {
		t.Error("small pod should fit on large plan")
	}

	// Big pod only fits on large plan
	if podFitsCurrentPlan(bigPod, &smallPlan) {
		t.Error("big pod should NOT fit on small plan")
	}
	if !podFitsCurrentPlan(bigPod, &largePlan) {
		t.Error("big pod should fit on large plan")
	}
}

func TestFindCheapestFittingPlan(t *testing.T) {
	plans := []vpsie.Plan{
		{Identifier: "small", CPU: 2, RAM: 4096, SSD: 50, PriceMonthly: 12},
		{Identifier: "medium", CPU: 4, RAM: 8192, SSD: 80, PriceMonthly: 24},
		{Identifier: "large", CPU: 8, RAM: 32768, SSD: 160, PriceMonthly: 48},
		{Identifier: "xlarge", CPU: 16, RAM: 65536, SSD: 320, PriceMonthly: 96},
	}
	constraints := optv1.ResourceConstraints{
		MinCPU: 1, MaxCPU: 32,
		MinRAM: 1024, MaxRAM: 131072,
		MinSSD: 20,
	}

	// Pod needing 6GB RAM → should get "medium" (cheapest that fits)
	result := findCheapestFittingPlan(plans, 1000, 6000, constraints)
	if result == nil || result.Identifier != "medium" {
		t.Errorf("expected medium plan, got %v", result)
	}

	// Pod needing 20GB RAM → should get "large"
	result = findCheapestFittingPlan(plans, 2000, 20000, constraints)
	if result == nil || result.Identifier != "large" {
		t.Errorf("expected large plan, got %v", result)
	}

	// Pod needing 200GB RAM → no plan fits
	result = findCheapestFittingPlan(plans, 1000, 200000, constraints)
	if result != nil {
		t.Errorf("expected nil, got %v", result)
	}

	// Excluded plan should be skipped
	constraintsExcluded := optv1.ResourceConstraints{
		MinCPU: 1, MaxCPU: 32,
		MinRAM: 1024, MaxRAM: 131072,
		MinSSD: 20,
		ExcludedPlans: []string{"medium"},
	}
	result = findCheapestFittingPlan(plans, 1000, 6000, constraintsExcluded)
	if result == nil || result.Identifier != "large" {
		t.Errorf("expected large plan (medium excluded), got %v", result)
	}
}

// TestNodePool_CreateSatelliteForOversizedPod verifies that a satellite
// MachineDeployment and VPSieMachineTemplate are created when a pending pod
// doesn't fit on the current plan.
func TestNodePool_CreateSatelliteForOversizedPod(t *testing.T) {
	objs := newScalingPolicyTestObjects(t, false)

	refreshObject(t, objs.Policy)
	objs.Policy.Spec.Horizontal = optv1.HorizontalSpec{
		Enabled:     true,
		MinReplicas: 1,
		MaxReplicas: 10,
	}
	objs.Policy.Spec.NodePoolPolicy = &optv1.NodePoolPolicy{
		Enabled:  true,
		MaxPools: 3,
	}
	if err := k8sClient.Update(ctx, objs.Policy); err != nil {
		t.Fatalf("update policy: %v", err)
	}

	// Current plan: plan-large (4 vCPU, 8GB RAM).
	// Create a pending pod that needs 16GB — doesn't fit on plan-large.
	fakeWC := &workload.FakeWorkloadClient{
		PendingPods: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "big-pod", Namespace: "default"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("2"),
								corev1.ResourceMemory: resource.MustParse("16Gi"),
							},
						},
					}},
				},
			},
		},
	}

	// Add a plan large enough to fit the big pod (not in the default test plans).
	fakePricing := newFakePricingClientCtrl()
	fakePricing.plans["cat-shared"] = append(fakePricing.plans["cat-shared"],
		vpsie.Plan{Identifier: "plan-xlarge", Nickname: "s-8vcpu-32gb", CPU: 8, RAM: 32768, SSD: 200, PriceMonthly: 96.0, CategoryID: "cat-shared"},
	)

	r := &ScalingPolicyReconciler{
		Client:   k8sClient,
		Scheme:   testScheme,
		Recorder: record.NewFakeRecorder(10),
		NewPricingClient: func(apiKey string) (vpsie.PricingClient, error) {
			return fakePricing, nil
		},
		WorkloadClients: &workload.FakeWorkloadClientFactory{Client: fakeWC},
		caches:          make(map[types.UID]*pricing.Cache),
	}

	refreshObject(t, objs.Policy)

	// Build cache manually.
	cache, err := r.ensureCache(objs.Policy.UID, "test-key",
		objs.Policy.Spec.DCIdentifier, objs.Policy.Spec.OSIdentifier, nil)
	if err != nil {
		t.Fatalf("ensureCache: %v", err)
	}
	if err := cache.EnsureFresh(ctx); err != nil {
		t.Fatalf("cache.EnsureFresh: %v", err)
	}

	// Resolve current plan.
	refreshObject(t, objs.MD)
	currentPlan, currentTemplate, err := r.resolveCurrentPlan(ctx, objs.MD, cache)
	if err != nil {
		t.Fatalf("resolveCurrentPlan: %v", err)
	}

	clusterName := objs.MD.Labels["cluster.x-k8s.io/cluster-name"]

	// Call reconcileNodePools.
	refreshObject(t, objs.Policy)
	err = r.reconcileNodePools(ctx, objs.Policy, objs.MD, cache, clusterName, currentPlan, currentTemplate)
	if err != nil {
		t.Fatalf("reconcileNodePools: %v", err)
	}

	// Verify satellite MachineDeployment was created.
	expectedPoolName := satelliteMDName(objs.MD.Name, "s-8vcpu-32gb")
	var satelliteMD clusterv1.MachineDeployment
	if err := k8sClient.Get(ctx, types.NamespacedName{
		Namespace: objs.Namespace,
		Name:      expectedPoolName,
	}, &satelliteMD); err != nil {
		t.Fatalf("expected satellite MD %s to exist: %v", expectedPoolName, err)
	}

	if satelliteMD.Spec.Replicas == nil || *satelliteMD.Spec.Replicas != 1 {
		t.Fatalf("expected satellite replicas=1, got %v", satelliteMD.Spec.Replicas)
	}

	if satelliteMD.Labels[satelliteLabel] != objs.MD.Name {
		t.Fatalf("expected satellite label %s=%s, got %s",
			satelliteLabel, objs.MD.Name, satelliteMD.Labels[satelliteLabel])
	}

	// Verify VPSieMachineTemplate was created.
	var tmpl infrav1.VPSieMachineTemplate
	if err := k8sClient.Get(ctx, types.NamespacedName{
		Namespace: objs.Namespace,
		Name:      expectedPoolName,
	}, &tmpl); err != nil {
		t.Fatalf("expected satellite template %s to exist: %v", expectedPoolName, err)
	}

	if tmpl.Spec.Template.Spec.ResourceIdentifier != "plan-xlarge" {
		t.Fatalf("expected template resource=plan-xlarge, got %s", tmpl.Spec.Template.Spec.ResourceIdentifier)
	}

	// Verify status was updated.
	if len(objs.Policy.Status.NodePools) != 1 {
		t.Fatalf("expected 1 node pool in status, got %d", len(objs.Policy.Status.NodePools))
	}
	if objs.Policy.Status.NodePools[0].PlanID != "plan-xlarge" {
		t.Fatalf("expected plan-xlarge in status, got %s", objs.Policy.Status.NodePools[0].PlanID)
	}
}

// TestNodePool_MaxPoolsLimit verifies that no more satellite MDs are created
// once the maxPools limit is reached.
func TestNodePool_MaxPoolsLimit(t *testing.T) {
	objs := newScalingPolicyTestObjects(t, false)

	refreshObject(t, objs.Policy)
	objs.Policy.Spec.Horizontal = optv1.HorizontalSpec{
		Enabled:     true,
		MinReplicas: 1,
		MaxReplicas: 10,
	}
	objs.Policy.Spec.NodePoolPolicy = &optv1.NodePoolPolicy{
		Enabled:  true,
		MaxPools: 1,
	}
	if err := k8sClient.Update(ctx, objs.Policy); err != nil {
		t.Fatalf("update policy spec: %v", err)
	}
	// Pre-fill status with an existing pool to hit the limit.
	refreshObject(t, objs.Policy)
	now := metav1.Now()
	objs.Policy.Status.NodePools = []optv1.NodePoolStatus{
		{Name: "workers-pool-existing", PlanID: "plan-existing", PlanNickname: "existing", Replicas: 1, LastPodSeen: &now},
	}
	if err := k8sClient.Status().Update(ctx, objs.Policy); err != nil {
		t.Fatalf("update policy status: %v", err)
	}

	fakeWC := &workload.FakeWorkloadClient{
		PendingPods: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "big-pod", Namespace: "default"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("2"),
								corev1.ResourceMemory: resource.MustParse("16Gi"),
							},
						},
					}},
				},
			},
		},
	}

	fakePricing := newFakePricingClientCtrl()
	fakePricing.plans["cat-shared"] = append(fakePricing.plans["cat-shared"],
		vpsie.Plan{Identifier: "plan-xlarge", Nickname: "s-8vcpu-32gb", CPU: 8, RAM: 32768, SSD: 200, PriceMonthly: 96.0, CategoryID: "cat-shared"},
	)

	r := &ScalingPolicyReconciler{
		Client:   k8sClient,
		Scheme:   testScheme,
		Recorder: record.NewFakeRecorder(10),
		NewPricingClient: func(apiKey string) (vpsie.PricingClient, error) {
			return fakePricing, nil
		},
		WorkloadClients: &workload.FakeWorkloadClientFactory{Client: fakeWC},
		caches:          make(map[types.UID]*pricing.Cache),
	}

	refreshObject(t, objs.Policy)

	cache, err := r.ensureCache(objs.Policy.UID, "test-key",
		objs.Policy.Spec.DCIdentifier, objs.Policy.Spec.OSIdentifier, nil)
	if err != nil {
		t.Fatalf("ensureCache: %v", err)
	}
	if err := cache.EnsureFresh(ctx); err != nil {
		t.Fatalf("cache.EnsureFresh: %v", err)
	}

	refreshObject(t, objs.MD)
	currentPlan, currentTemplate, err := r.resolveCurrentPlan(ctx, objs.MD, cache)
	if err != nil {
		t.Fatalf("resolveCurrentPlan: %v", err)
	}

	clusterName := objs.MD.Labels["cluster.x-k8s.io/cluster-name"]

	refreshObject(t, objs.Policy)
	err = r.reconcileNodePools(ctx, objs.Policy, objs.MD, cache, clusterName, currentPlan, currentTemplate)
	if err != nil {
		t.Fatalf("reconcileNodePools: %v", err)
	}

	// Should still have only 1 pool (the pre-existing one), no new satellite created.
	expectedPoolName := satelliteMDName(objs.MD.Name, "s-8vcpu-32gb")
	var satelliteMD clusterv1.MachineDeployment
	err = k8sClient.Get(ctx, types.NamespacedName{
		Namespace: objs.Namespace,
		Name:      expectedPoolName,
	}, &satelliteMD)
	if err == nil {
		t.Fatalf("satellite %s should NOT have been created (max pools reached)", expectedPoolName)
	}
}

// TestNodePool_DryRunSkipsCreation verifies that dry-run mode doesn't
// create satellite MDs.
func TestNodePool_DryRunSkipsCreation(t *testing.T) {
	objs := newScalingPolicyTestObjects(t, true) // dryRun=true

	refreshObject(t, objs.Policy)
	objs.Policy.Spec.Horizontal = optv1.HorizontalSpec{
		Enabled:     true,
		MinReplicas: 1,
		MaxReplicas: 10,
	}
	objs.Policy.Spec.NodePoolPolicy = &optv1.NodePoolPolicy{
		Enabled:  true,
		MaxPools: 3,
	}
	if err := k8sClient.Update(ctx, objs.Policy); err != nil {
		t.Fatalf("update policy: %v", err)
	}

	fakeWC := &workload.FakeWorkloadClient{
		PendingPods: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "big-pod", Namespace: "default"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("2"),
								corev1.ResourceMemory: resource.MustParse("16Gi"),
							},
						},
					}},
				},
			},
		},
	}

	fakePricing := newFakePricingClientCtrl()
	fakePricing.plans["cat-shared"] = append(fakePricing.plans["cat-shared"],
		vpsie.Plan{Identifier: "plan-xlarge", Nickname: "s-8vcpu-32gb", CPU: 8, RAM: 32768, SSD: 200, PriceMonthly: 96.0, CategoryID: "cat-shared"},
	)

	r := &ScalingPolicyReconciler{
		Client:   k8sClient,
		Scheme:   testScheme,
		Recorder: record.NewFakeRecorder(10),
		NewPricingClient: func(apiKey string) (vpsie.PricingClient, error) {
			return fakePricing, nil
		},
		WorkloadClients: &workload.FakeWorkloadClientFactory{Client: fakeWC},
		caches:          make(map[types.UID]*pricing.Cache),
	}

	refreshObject(t, objs.Policy)

	cache, err := r.ensureCache(objs.Policy.UID, "test-key",
		objs.Policy.Spec.DCIdentifier, objs.Policy.Spec.OSIdentifier, nil)
	if err != nil {
		t.Fatalf("ensureCache: %v", err)
	}
	if err := cache.EnsureFresh(ctx); err != nil {
		t.Fatalf("cache.EnsureFresh: %v", err)
	}

	refreshObject(t, objs.MD)
	currentPlan, currentTemplate, err := r.resolveCurrentPlan(ctx, objs.MD, cache)
	if err != nil {
		t.Fatalf("resolveCurrentPlan: %v", err)
	}

	clusterName := objs.MD.Labels["cluster.x-k8s.io/cluster-name"]

	refreshObject(t, objs.Policy)
	err = r.reconcileNodePools(ctx, objs.Policy, objs.MD, cache, clusterName, currentPlan, currentTemplate)
	if err != nil {
		t.Fatalf("reconcileNodePools: %v", err)
	}

	// Satellite should NOT be created in dry-run mode.
	expectedPoolName := satelliteMDName(objs.MD.Name, "s-8vcpu-32gb")
	var satelliteMD clusterv1.MachineDeployment
	err = k8sClient.Get(ctx, types.NamespacedName{
		Namespace: objs.Namespace,
		Name:      expectedPoolName,
	}, &satelliteMD)
	if err == nil {
		t.Fatalf("satellite %s should NOT have been created in dry-run mode", expectedPoolName)
	}
}

// TestNodePool_HorizontalFilterOversizedPods verifies that reconcileHorizontal
// does NOT scale up the base MD for oversized pods when nodePoolPolicy is enabled.
func TestNodePool_HorizontalFilterOversizedPods(t *testing.T) {
	objs := newScalingPolicyTestObjects(t, false)

	refreshObject(t, objs.Policy)
	objs.Policy.Spec.Horizontal = optv1.HorizontalSpec{
		Enabled:     true,
		MinReplicas: 1,
		MaxReplicas: 10,
	}
	objs.Policy.Spec.NodePoolPolicy = &optv1.NodePoolPolicy{
		Enabled:  true,
		MaxPools: 3,
	}
	if err := k8sClient.Update(ctx, objs.Policy); err != nil {
		t.Fatalf("update policy: %v", err)
	}

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

	// All pending pods are oversized (don't fit on plan-large: 4 vCPU, 8GB).
	fakeWC := &workload.FakeWorkloadClient{
		PendingPods: []corev1.Pod{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "big-pod-1", Namespace: "default"},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("2"),
								corev1.ResourceMemory: resource.MustParse("16Gi"),
							},
						},
					}},
				},
			},
		},
	}

	r := &ScalingPolicyReconciler{
		Client:          k8sClient,
		Scheme:          testScheme,
		Recorder:        record.NewFakeRecorder(10),
		WorkloadClients: &workload.FakeWorkloadClientFactory{Client: fakeWC},
		caches:          make(map[types.UID]*pricing.Cache),
	}

	// Current plan: plan-large (4 vCPU, 8GB).
	currentPlan := &vpsie.Plan{Identifier: "plan-large", CPU: 4, RAM: 8192, SSD: 160, PriceMonthly: 48.0}

	refreshObject(t, objs.Policy)
	refreshObject(t, objs.MD)
	err := r.reconcileHorizontal(ctx, objs.Policy, objs.MD, "test-cluster", nil, currentPlan)
	if err != nil {
		t.Fatalf("reconcileHorizontal: %v", err)
	}

	// Base MD replicas should NOT have changed (oversized pods filtered out).
	refreshObject(t, objs.MD)
	if objs.MD.Spec.Replicas == nil || *objs.MD.Spec.Replicas != 3 {
		t.Fatalf("expected replicas unchanged at 3, got %v", objs.MD.Spec.Replicas)
	}
}
