package controller

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"

	optv1 "github.com/vpsieinc/vpsie-cluster-scaler/api/v1alpha1"
	"github.com/vpsieinc/vpsie-cluster-scaler/internal/pricing"
	"github.com/vpsieinc/vpsie-cluster-scaler/internal/vpsie"
)

func TestScalingPolicyReconciler_DryRun(t *testing.T) {
	objs := newScalingPolicyTestObjects(t, true /* dryRun */)

	fakeClient := newFakePricingClientCtrl()

	r := &ScalingPolicyReconciler{
		Client:   k8sClient,
		Scheme:   testScheme,
		Recorder: record.NewFakeRecorder(10),
		NewPricingClient: func(_ string) (vpsie.PricingClient, error) {
			return fakeClient, nil
		},
		caches: make(map[types.UID]*pricing.Cache),
	}

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Namespace: objs.Namespace,
			Name:      "test-policy",
		},
	})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatal("expected requeue")
	}

	// Verify status was updated
	refreshObject(t, objs.Policy)
	if objs.Policy.Status.Phase != optv1.ScalingPolicyPhaseDryRun {
		t.Fatalf("expected DryRun phase, got %s", objs.Policy.Status.Phase)
	}

	// Verify pricing data was loaded
	if len(objs.Policy.Status.AvailableCategories) == 0 {
		t.Fatal("expected available categories to be populated")
	}

	// Current plan should be set (plan-large)
	if objs.Policy.Status.CurrentPlan == nil {
		t.Fatal("expected current plan to be set")
	}
	if objs.Policy.Status.CurrentPlan.Identifier != "plan-large" {
		t.Fatalf("expected current plan plan-large, got %s", objs.Policy.Status.CurrentPlan.Identifier)
	}

	// Recommended plan should be cheaper
	if objs.Policy.Status.RecommendedPlan == nil {
		t.Fatal("expected recommended plan to be set")
	}
	if objs.Policy.Status.RecommendedPlan.PriceMonthly >= objs.Policy.Status.CurrentPlan.PriceMonthly {
		t.Fatalf("expected recommended plan to be cheaper: recommended=$%.2f, current=$%.2f",
			objs.Policy.Status.RecommendedPlan.PriceMonthly, objs.Policy.Status.CurrentPlan.PriceMonthly)
	}

	// MachineDeployment should NOT have been modified (dry run)
	refreshObject(t, objs.MD)
	if objs.MD.Spec.Template.Spec.InfrastructureRef.Name != "workers-template" {
		t.Fatalf("dry run should not modify MachineDeployment, but infrastructureRef changed to %s",
			objs.MD.Spec.Template.Spec.InfrastructureRef.Name)
	}
}

func TestScalingPolicyReconciler_PlanSwitch(t *testing.T) {
	objs := newScalingPolicyTestObjects(t, false /* dryRun */)

	fakeClient := newFakePricingClientCtrl()

	r := &ScalingPolicyReconciler{
		Client:   k8sClient,
		Scheme:   testScheme,
		Recorder: record.NewFakeRecorder(10),
		NewPricingClient: func(_ string) (vpsie.PricingClient, error) {
			return fakeClient, nil
		},
		caches: make(map[types.UID]*pricing.Cache),
	}

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Namespace: objs.Namespace,
			Name:      "test-policy",
		},
	})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatal("expected requeue")
	}

	// Verify MachineDeployment was updated to use cheaper template
	refreshObject(t, objs.MD)
	if objs.MD.Spec.Template.Spec.InfrastructureRef.Name == "workers-template" {
		t.Fatal("expected MachineDeployment to be updated to a new template")
	}

	// Verify status shows plan switched
	refreshObject(t, objs.Policy)
	if objs.Policy.Status.Phase != optv1.ScalingPolicyPhaseActive {
		t.Fatalf("expected Active phase, got %s", objs.Policy.Status.Phase)
	}
}

func TestScalingPolicyReconciler_TargetNotFound(t *testing.T) {
	ns := newTestNamespace(t)

	// Create a policy pointing to a nonexistent MachineDeployment
	newCredentialsSecret(t, ns, "creds")
	policy := &optv1.ScalingPolicy{}
	policy.Name = "bad-target-policy"
	policy.Namespace = ns
	policy.Spec = optv1.ScalingPolicySpec{
		TargetRef: optv1.ObjectReference{
			Name: "nonexistent-md",
		},
		CredentialsRef: &optv1.CredentialsRef{
			Name: "creds",
		},
		DCIdentifier: "dc-1",
		OSIdentifier: "os-1",
		Constraints: optv1.ResourceConstraints{
			MinCPU: 1, MaxCPU: 32,
			MinRAM: 1024, MaxRAM: 131072,
			MinSSD: 20,
		},
	}
	if err := k8sClient.Create(ctx, policy); err != nil {
		t.Fatalf("failed to create policy: %v", err)
	}

	r := &ScalingPolicyReconciler{
		Client:   k8sClient,
		Scheme:   testScheme,
		Recorder: record.NewFakeRecorder(10),
		caches:   make(map[types.UID]*pricing.Cache),
	}

	result, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{
			Namespace: ns,
			Name:      "bad-target-policy",
		},
	})
	if err != nil {
		t.Fatalf("Reconcile should not return error for not-found target: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatal("expected requeue after error")
	}

	// Verify error phase
	refreshObject(t, policy)
	if policy.Status.Phase != optv1.ScalingPolicyPhaseError {
		t.Fatalf("expected Error phase, got %s", policy.Status.Phase)
	}
}
