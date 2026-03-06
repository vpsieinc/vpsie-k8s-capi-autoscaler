package pricing

import (
	"math"
	"testing"

	"github.com/vpsieinc/vpsie-cluster-scaler/api/v1alpha1"
	"github.com/vpsieinc/vpsie-cluster-scaler/internal/vpsie"
)

func TestPlanCapacity(t *testing.T) {
	plan := vpsie.Plan{CPU: 2, RAM: 4096}
	cpuMillis, ramMB := PlanCapacity(plan)

	expectedCPU := 2*1000 - KubeletReservedCPUMillis
	expectedRAM := 4096 - KubeletReservedRAMMB

	if cpuMillis != expectedCPU {
		t.Fatalf("expected CPU %d, got %d", expectedCPU, cpuMillis)
	}
	if ramMB != expectedRAM {
		t.Fatalf("expected RAM %d, got %d", expectedRAM, ramMB)
	}
}

func TestPlanCapacity_TinyPlan(t *testing.T) {
	// A plan with resources less than reserved should return 0
	plan := vpsie.Plan{CPU: 0, RAM: 100}
	cpuMillis, ramMB := PlanCapacity(plan)

	if cpuMillis != 0 {
		t.Fatalf("expected CPU 0, got %d", cpuMillis)
	}
	if ramMB != 0 {
		t.Fatalf("expected RAM 0, got %d", ramMB)
	}
}

func TestScorePlan(t *testing.T) {
	plan := vpsie.Plan{CPU: 4, RAM: 8192, PriceMonthly: 48.0}
	weights := WeightsForAggressiveness(v1alpha1.AggressivenessModerate)

	score := ScorePlan(plan, 1000, 2048, weights)
	if score <= 0 {
		t.Fatalf("expected positive score, got %f", score)
	}
}

func TestScorePlan_CheaperBetter(t *testing.T) {
	weights := WeightsForAggressiveness(v1alpha1.AggressivenessAggressive)

	cheap := vpsie.Plan{CPU: 2, RAM: 4096, PriceMonthly: 24.0}
	expensive := vpsie.Plan{CPU: 2, RAM: 4096, PriceMonthly: 48.0}

	cheapScore := ScorePlan(cheap, 1000, 2048, weights)
	expensiveScore := ScorePlan(expensive, 1000, 2048, weights)

	// With aggressive weights (price=0.7), cheaper plan should score higher
	if cheapScore <= expensiveScore {
		t.Fatalf("expected cheaper plan to score higher: cheap=%f, expensive=%f", cheapScore, expensiveScore)
	}
}

func TestScorePlan_ZeroPrice(t *testing.T) {
	plan := vpsie.Plan{CPU: 2, RAM: 4096, PriceMonthly: 0}
	weights := WeightsForAggressiveness(v1alpha1.AggressivenessModerate)
	score := ScorePlan(plan, 1000, 2048, weights)
	if score != 0 {
		t.Fatalf("expected 0 score for zero-price plan, got %f", score)
	}
}

func TestSavingsPercent(t *testing.T) {
	tests := []struct {
		current  float64
		new      float64
		expected float64
	}{
		{100, 80, 20.0},
		{50, 25, 50.0},
		{100, 100, 0.0},
		{0, 50, 0.0}, // zero current price
	}

	for _, tt := range tests {
		got := SavingsPercent(tt.current, tt.new)
		if math.Abs(got-tt.expected) > 0.01 {
			t.Errorf("SavingsPercent(%f, %f) = %f, want %f", tt.current, tt.new, got, tt.expected)
		}
	}
}

func TestWeightsForAggressiveness(t *testing.T) {
	tests := []struct {
		agg           v1alpha1.Aggressiveness
		expectedPrice float64
		expectedFit   float64
	}{
		{v1alpha1.AggressivenessConservative, 0.3, 0.3},
		{v1alpha1.AggressivenessModerate, 0.5, 0.3},
		{v1alpha1.AggressivenessAggressive, 0.7, 0.2},
	}

	for _, tt := range tests {
		w := WeightsForAggressiveness(tt.agg)
		if w.Price != tt.expectedPrice {
			t.Errorf("%s: price weight = %f, want %f", tt.agg, w.Price, tt.expectedPrice)
		}
		if w.Fit != tt.expectedFit {
			t.Errorf("%s: fit weight = %f, want %f", tt.agg, w.Fit, tt.expectedFit)
		}
		// Weights should sum to 1.0
		sum := w.Price + w.Fit + w.Headroom
		if math.Abs(sum-1.0) > 0.001 {
			t.Errorf("%s: weights sum to %f, want 1.0", tt.agg, sum)
		}
	}
}
