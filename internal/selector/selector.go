package selector

import (
	"k8s.io/klog/v2"

	"github.com/vpsieinc/vpsie-cluster-scaler/api/v1alpha1"
	"github.com/vpsieinc/vpsie-cluster-scaler/internal/pricing"
	"github.com/vpsieinc/vpsie-cluster-scaler/internal/vpsie"
)

// ScalingDirection constrains plan selection to a specific direction.
type ScalingDirection int

const (
	// DirectionAny allows any plan (current behavior).
	DirectionAny ScalingDirection = iota
	// DirectionUp skips plans that are not larger than the current plan.
	DirectionUp
	// DirectionDown skips plans that are not smaller than the current plan.
	DirectionDown
)

// Result holds the output of plan selection.
type Result struct {
	// Plan is the selected plan, nil if no suitable plan was found.
	Plan *vpsie.Plan

	// Score is the plan's computed score.
	Score float64

	// SavingsPercent is the cost savings vs the current plan.
	SavingsPercent float64

	// Candidates is the number of plans that passed filtering.
	Candidates int
}

// Select picks the best plan from available plans given constraints and workload requirements.
//
// Parameters:
//   - plans: available plans (pre-filtered by allowed categories)
//   - constraints: min/max resource boundaries
//   - requiredCPUMillis: total CPU request across pods (in millicores)
//   - requiredRAMMB: total RAM request across pods (in MB)
//   - requiredSSDGB: minimum SSD required
//   - currentPlanID: identifier of the currently used plan (for savings calc)
//   - currentPrice: monthly price of the current plan
//   - currentCPU: vCPU count of the current plan (used for directional filtering)
//   - currentRAM: RAM in MB of the current plan (used for directional filtering)
//   - aggressiveness: scoring weight configuration
//   - minSavingsPercent: minimum savings threshold to recommend a change
//   - direction: constrains selection to upscale, downscale, or any direction
func Select(
	plans []vpsie.Plan,
	constraints v1alpha1.ResourceConstraints,
	requiredCPUMillis, requiredRAMMB, requiredSSDGB int,
	currentPlanID string,
	currentPrice float64,
	currentCPU, currentRAM int,
	aggressiveness v1alpha1.Aggressiveness,
	minSavingsPercent int,
	direction ScalingDirection,
) Result {
	weights := pricing.WeightsForAggressiveness(aggressiveness)
	excluded := make(map[string]bool, len(constraints.ExcludedPlans))
	for _, id := range constraints.ExcludedPlans {
		excluded[id] = true
	}

	// For upscaling, we don't require minimum savings — we're scaling for capacity.
	effectiveMinSavings := minSavingsPercent
	if direction == DirectionUp {
		effectiveMinSavings = 0
	}

	var bestPlan *vpsie.Plan
	var bestScore float64
	candidates := 0

	for i := range plans {
		p := &plans[i]

		// Skip excluded plans
		if excluded[p.Identifier] {
			continue
		}

		// Apply resource constraints
		if p.CPU < constraints.MinCPU || p.CPU > constraints.MaxCPU {
			continue
		}
		if p.RAM < constraints.MinRAM || p.RAM > constraints.MaxRAM {
			continue
		}
		if p.SSD < constraints.MinSSD {
			continue
		}

		// Directional filtering
		switch direction {
		case DirectionUp:
			// Skip plans that are not bigger than current
			if p.CPU <= currentCPU && p.RAM <= currentRAM {
				continue
			}
		case DirectionDown:
			// Skip plans that are not smaller than current
			if p.CPU >= currentCPU && p.RAM >= currentRAM {
				continue
			}
		}

		// Fit check: plan capacity must fit workload requirements
		allocCPU, allocRAM := pricing.PlanCapacity(*p)
		if allocCPU < requiredCPUMillis || allocRAM < requiredRAMMB {
			continue
		}
		if p.SSD < requiredSSDGB {
			continue
		}

		candidates++

		score := pricing.ScorePlan(*p, requiredCPUMillis, requiredRAMMB, weights)

		if score > bestScore {
			bestScore = score
			bestPlan = p
		}
	}

	if bestPlan == nil {
		klog.V(2).Infof("no suitable plan found from %d candidates", candidates)
		return Result{Candidates: candidates}
	}

	savings := pricing.SavingsPercent(currentPrice, bestPlan.PriceMonthly)

	// Don't recommend switching to the same plan
	if bestPlan.Identifier == currentPlanID {
		klog.V(2).Infof("best plan %s is already the current plan", bestPlan.Nickname)
		return Result{
			Plan:       bestPlan,
			Score:      bestScore,
			Candidates: candidates,
		}
	}

	// Check minimum savings threshold
	if savings < float64(effectiveMinSavings) {
		klog.V(2).Infof("best plan %s saves %.1f%% (threshold: %d%%), not switching",
			bestPlan.Nickname, savings, effectiveMinSavings)
		return Result{
			Plan:           bestPlan,
			Score:          bestScore,
			SavingsPercent: savings,
			Candidates:     candidates,
		}
	}

	klog.V(2).Infof("selected plan %s (%s): $%.2f/mo, saves %.1f%% from $%.2f/mo",
		bestPlan.Nickname, bestPlan.Identifier, bestPlan.PriceMonthly, savings, currentPrice)

	return Result{
		Plan:           bestPlan,
		Score:          bestScore,
		SavingsPercent: savings,
		Candidates:     candidates,
	}
}
