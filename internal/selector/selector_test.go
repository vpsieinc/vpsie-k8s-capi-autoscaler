package selector

import (
	"testing"

	"github.com/vpsieinc/vpsie-cluster-scaler/api/v1alpha1"
	"github.com/vpsieinc/vpsie-cluster-scaler/internal/vpsie"
)

func testPlans() []vpsie.Plan {
	return []vpsie.Plan{
		{Identifier: "plan-small", Nickname: "s-1vcpu-1gb", CPU: 1, RAM: 1024, SSD: 25, PriceMonthly: 6.0},
		{Identifier: "plan-medium", Nickname: "s-2vcpu-4gb", CPU: 2, RAM: 4096, SSD: 80, PriceMonthly: 24.0},
		{Identifier: "plan-large", Nickname: "s-4vcpu-8gb", CPU: 4, RAM: 8192, SSD: 160, PriceMonthly: 48.0},
		{Identifier: "plan-xlarge", Nickname: "s-8vcpu-16gb", CPU: 8, RAM: 16384, SSD: 320, PriceMonthly: 96.0},
	}
}

func defaultConstraints() v1alpha1.ResourceConstraints {
	return v1alpha1.ResourceConstraints{
		MinCPU: 1, MaxCPU: 32,
		MinRAM: 1024, MaxRAM: 131072,
		MinSSD: 20,
	}
}

func TestSelect_FindsCheapestFittingPlan(t *testing.T) {
	plans := testPlans()

	// Need 1000m CPU, 2048MB RAM — plan-medium (2vcpu, 4gb) is the cheapest fit
	result := Select(plans, defaultConstraints(), 1000, 2048, 20,
		"plan-xlarge", 96.0, 8, 16384,
		v1alpha1.AggressivenessModerate, 15, DirectionAny)

	if result.Plan == nil {
		t.Fatal("expected a plan to be selected")
	}
	if result.Plan.Identifier != "plan-medium" {
		t.Fatalf("expected plan-medium, got %s", result.Plan.Identifier)
	}
	if result.SavingsPercent <= 0 {
		t.Fatalf("expected positive savings, got %.1f%%", result.SavingsPercent)
	}
}

func TestSelect_NoChange_WhenCurrentIsOptimal(t *testing.T) {
	plans := testPlans()

	// Currently using plan-medium, which is already the cheapest fit
	result := Select(plans, defaultConstraints(), 1000, 2048, 20,
		"plan-medium", 24.0, 2, 4096,
		v1alpha1.AggressivenessModerate, 15, DirectionAny)

	// Should return plan-medium but with no savings (same plan)
	if result.Plan == nil {
		t.Fatal("expected a plan to be returned")
	}
	if result.Plan.Identifier != "plan-medium" {
		t.Fatalf("expected plan-medium, got %s", result.Plan.Identifier)
	}
	// SavingsPercent should be 0 since it's the same plan
	if result.SavingsPercent != 0 {
		t.Fatalf("expected 0%% savings for same plan, got %.1f%%", result.SavingsPercent)
	}
}

func TestSelect_RespectsConstraints(t *testing.T) {
	plans := testPlans()
	constraints := v1alpha1.ResourceConstraints{
		MinCPU: 4, MaxCPU: 8, // exclude 1 and 2 vcpu plans
		MinRAM: 4096, MaxRAM: 131072,
		MinSSD: 20,
	}

	result := Select(plans, constraints, 1000, 2048, 20,
		"plan-xlarge", 96.0, 8, 16384,
		v1alpha1.AggressivenessModerate, 15, DirectionAny)

	if result.Plan == nil {
		t.Fatal("expected a plan to be selected")
	}
	if result.Plan.CPU < 4 {
		t.Fatalf("expected plan with at least 4 CPUs, got %d", result.Plan.CPU)
	}
}

func TestSelect_ExcludedPlans(t *testing.T) {
	plans := testPlans()
	constraints := v1alpha1.ResourceConstraints{
		MinCPU: 1, MaxCPU: 32,
		MinRAM: 1024, MaxRAM: 131072,
		MinSSD:        20,
		ExcludedPlans: []string{"plan-medium"}, // exclude the cheapest fit
	}

	result := Select(plans, constraints, 1000, 2048, 20,
		"plan-xlarge", 96.0, 8, 16384,
		v1alpha1.AggressivenessModerate, 15, DirectionAny)

	if result.Plan == nil {
		t.Fatal("expected a plan")
	}
	if result.Plan.Identifier == "plan-medium" {
		t.Fatal("plan-medium should have been excluded")
	}
}

func TestSelect_NoPlanFits(t *testing.T) {
	plans := testPlans()

	// Require more than any plan can provide
	result := Select(plans, defaultConstraints(), 100000, 200000, 20,
		"plan-xlarge", 96.0, 8, 16384,
		v1alpha1.AggressivenessModerate, 15, DirectionAny)

	if result.Plan != nil {
		t.Fatalf("expected no plan, got %s", result.Plan.Identifier)
	}
}

func TestSelect_InsufficientSavings(t *testing.T) {
	plans := testPlans()

	// Currently on plan-large ($48), best fit is plan-medium ($24) — 50% savings
	// but threshold is 60%, so should not recommend switch
	result := Select(plans, defaultConstraints(), 1000, 2048, 20,
		"plan-large", 48.0, 4, 8192,
		v1alpha1.AggressivenessModerate, 60, DirectionAny) // very high threshold

	// Should find plan-medium but savings (50%) < threshold (60%)
	if result.Plan != nil && result.SavingsPercent >= 60 {
		t.Fatalf("should not recommend switch with 60%% threshold, savings=%.1f%%", result.SavingsPercent)
	}
}

func TestSelect_AggressivenessAffectsChoice(t *testing.T) {
	// With identical plans at different prices, aggressive should prefer cheaper
	plans := testPlans()

	conservative := Select(plans, defaultConstraints(), 500, 512, 20,
		"plan-xlarge", 96.0, 8, 16384,
		v1alpha1.AggressivenessConservative, 5, DirectionAny)

	aggressive := Select(plans, defaultConstraints(), 500, 512, 20,
		"plan-xlarge", 96.0, 8, 16384,
		v1alpha1.AggressivenessAggressive, 5, DirectionAny)

	// Both should find a plan
	if conservative.Plan == nil || aggressive.Plan == nil {
		t.Fatal("both should find a plan")
	}

	// Aggressive should prefer cheaper (or equal) plan
	if aggressive.Plan.PriceMonthly > conservative.Plan.PriceMonthly {
		t.Fatalf("aggressive should pick cheaper plan: aggressive=$%.2f, conservative=$%.2f",
			aggressive.Plan.PriceMonthly, conservative.Plan.PriceMonthly)
	}
}

func TestSelect_DirectionUp_FiltersSmaller(t *testing.T) {
	plans := testPlans()

	// Currently on plan-medium (2cpu, 4gb). DirectionUp should only consider larger plans.
	result := Select(plans, defaultConstraints(), 500, 512, 20,
		"plan-medium", 24.0, 2, 4096,
		v1alpha1.AggressivenessModerate, 0, DirectionUp)

	if result.Plan == nil {
		t.Fatal("expected a plan to be selected")
	}
	// Should not pick plan-small (smaller) or plan-medium (same)
	if result.Plan.CPU <= 2 && result.Plan.RAM <= 4096 {
		t.Fatalf("DirectionUp should skip plans not bigger than current, got %s (cpu=%d, ram=%d)",
			result.Plan.Identifier, result.Plan.CPU, result.Plan.RAM)
	}
}

func TestSelect_DirectionUp_RelaxesSavings(t *testing.T) {
	plans := testPlans()

	// DirectionUp should not require minimum savings (we're scaling for capacity).
	// Current plan is cheapest; upscaling means paying more.
	result := Select(plans, defaultConstraints(), 500, 512, 20,
		"plan-small", 6.0, 1, 1024,
		v1alpha1.AggressivenessModerate, 50, DirectionUp) // 50% savings threshold

	// Even with 50% threshold, DirectionUp relaxes it to 0.
	// Savings will be negative (more expensive), but SavingsPercent should be set.
	if result.Plan == nil {
		t.Fatal("expected a plan to be selected for upscale")
	}
	if result.Plan.CPU <= 1 && result.Plan.RAM <= 1024 {
		t.Fatal("expected a larger plan")
	}
}

func TestSelect_DirectionDown_FiltersLarger(t *testing.T) {
	plans := testPlans()

	// Currently on plan-large (4cpu, 8gb). DirectionDown should only consider smaller plans.
	result := Select(plans, defaultConstraints(), 500, 512, 20,
		"plan-large", 48.0, 4, 8192,
		v1alpha1.AggressivenessModerate, 0, DirectionDown)

	if result.Plan == nil {
		t.Fatal("expected a plan to be selected")
	}
	if result.Plan.CPU >= 4 && result.Plan.RAM >= 8192 {
		t.Fatalf("DirectionDown should skip plans not smaller than current, got %s (cpu=%d, ram=%d)",
			result.Plan.Identifier, result.Plan.CPU, result.Plan.RAM)
	}
}

func TestSelect_DirectionDown_NoPlanSmaller(t *testing.T) {
	plans := testPlans()

	// Currently on the smallest plan — DirectionDown should find nothing.
	result := Select(plans, defaultConstraints(), 500, 512, 20,
		"plan-small", 6.0, 1, 1024,
		v1alpha1.AggressivenessModerate, 0, DirectionDown)

	if result.Plan != nil {
		t.Fatalf("expected no plan for downscale from smallest, got %s", result.Plan.Identifier)
	}
}
