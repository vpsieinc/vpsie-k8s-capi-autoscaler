package pricing

import (
	"github.com/vpsieinc/vpsie-cluster-scaler/api/v1alpha1"
	"github.com/vpsieinc/vpsie-cluster-scaler/internal/vpsie"
)

// Weights for plan scoring by aggressiveness.
type Weights struct {
	Price    float64
	Fit      float64
	Headroom float64
}

// WeightsForAggressiveness returns scoring weights for the given aggressiveness level.
func WeightsForAggressiveness(a v1alpha1.Aggressiveness) Weights {
	switch a {
	case v1alpha1.AggressivenessConservative:
		return Weights{Price: 0.3, Fit: 0.3, Headroom: 0.4}
	case v1alpha1.AggressivenessAggressive:
		return Weights{Price: 0.7, Fit: 0.2, Headroom: 0.1}
	default: // moderate
		return Weights{Price: 0.5, Fit: 0.3, Headroom: 0.2}
	}
}

// KubeletReserved defines the resources reserved by kubelet.
const (
	KubeletReservedCPUMillis = 100 // 100m CPU
	KubeletReservedRAMMB     = 256 // 256MB RAM
)

// PlanCapacity returns the allocatable resources for a plan (after kubelet reserved).
func PlanCapacity(p vpsie.Plan) (cpuMillis int, ramMB int) {
	cpuMillis = p.CPU*1000 - KubeletReservedCPUMillis
	ramMB = p.RAM - KubeletReservedRAMMB
	if cpuMillis < 0 {
		cpuMillis = 0
	}
	if ramMB < 0 {
		ramMB = 0
	}
	return
}

// ScorePlan scores a plan given workload requirements and aggressiveness weights.
// Higher score = better plan.
func ScorePlan(p vpsie.Plan, requiredCPUMillis, requiredRAMMB int, w Weights) float64 {
	allocCPU, allocRAM := PlanCapacity(p)
	if allocCPU <= 0 || allocRAM <= 0 || p.PriceMonthly <= 0 {
		return 0
	}

	// Price score: cheaper = better (inverse)
	priceScore := 1.0 / p.PriceMonthly

	// Fit score: tighter fit = less waste (higher ratio = better)
	cpuFit := float64(requiredCPUMillis) / float64(allocCPU)
	ramFit := float64(requiredRAMMB) / float64(allocRAM)
	fitScore := (cpuFit + ramFit) / 2.0
	if fitScore > 1.0 {
		fitScore = 1.0
	}

	// Headroom score: more headroom = better for bursts (inverse of fit)
	headroomScore := 1.0 - fitScore

	return w.Price*priceScore + w.Fit*fitScore + w.Headroom*headroomScore
}

// SavingsPercent calculates the percentage savings between current and new price.
func SavingsPercent(currentPrice, newPrice float64) float64 {
	if currentPrice <= 0 {
		return 0
	}
	return ((currentPrice - newPrice) / currentPrice) * 100
}
