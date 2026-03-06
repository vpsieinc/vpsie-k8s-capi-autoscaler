package controller

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	optv1 "github.com/vpsieinc/vpsie-cluster-scaler/api/v1alpha1"
	scalermetrics "github.com/vpsieinc/vpsie-cluster-scaler/internal/metrics"
	"github.com/vpsieinc/vpsie-cluster-scaler/internal/selector"
	"github.com/vpsieinc/vpsie-cluster-scaler/internal/utilization"
	"github.com/vpsieinc/vpsie-cluster-scaler/internal/vpsie"
)

const (
	defaultRebalanceInterval = 5 * time.Minute
	minReplicasForRebalance  = 2
)

// Rebalancer periodically checks ScalingPolicies with rebalancing enabled
// and triggers plan switches when cheaper alternatives are available.
// It implements manager.Runnable.
type Rebalancer struct {
	client.Client
	Interval time.Duration

	// NewPricingClient creates a PricingClient for the given API key.
	NewPricingClient func(apiKey string) (vpsie.PricingClient, error)

	// reconciler is used to access shared caches and helper methods.
	reconciler *ScalingPolicyReconciler
}

// NewRebalancer creates a new Rebalancer.
func NewRebalancer(c client.Client, reconciler *ScalingPolicyReconciler, interval time.Duration) *Rebalancer {
	if interval <= 0 {
		interval = defaultRebalanceInterval
	}
	return &Rebalancer{
		Client:     c,
		Interval:   interval,
		reconciler: reconciler,
	}
}

// Start implements manager.Runnable. It runs the periodic rebalancing loop.
func (rb *Rebalancer) Start(ctx context.Context) error {
	klog.Info("starting rebalancer")
	ticker := time.NewTicker(rb.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			klog.Info("rebalancer stopped")
			return nil
		case <-ticker.C:
			rb.runCycle(ctx)
		}
	}
}

// runCycle performs one rebalancing cycle across all eligible ScalingPolicies.
func (rb *Rebalancer) runCycle(ctx context.Context) {
	klog.V(2).Info("rebalancer: starting cycle")

	var policyList optv1.ScalingPolicyList
	if err := rb.List(ctx, &policyList); err != nil {
		klog.Errorf("rebalancer: failed to list ScalingPolicies: %v", err)
		return
	}

	for i := range policyList.Items {
		policy := &policyList.Items[i]

		if !policy.Spec.Rebalancing.Enabled {
			continue
		}

		if err := rb.rebalancePolicy(ctx, policy); err != nil {
			klog.Errorf("rebalancer: failed to rebalance %s/%s: %v", policy.Namespace, policy.Name, err)
		}
	}
}

// rebalancePolicy evaluates and potentially rebalances a single ScalingPolicy.
func (rb *Rebalancer) rebalancePolicy(ctx context.Context, policy *optv1.ScalingPolicy) error {
	klog.V(2).Infof("rebalancer: evaluating %s/%s", policy.Namespace, policy.Name)

	// Check cooldown
	if policy.Status.LastRebalanceTime != nil {
		cooldown := rb.cooldownForAggressiveness(policy.Spec.Aggressiveness)
		if policy.Spec.Rebalancing.CooldownPeriod != nil {
			cooldown = policy.Spec.Rebalancing.CooldownPeriod.Duration
		}
		if time.Since(policy.Status.LastRebalanceTime.Time) < cooldown {
			klog.V(2).Infof("rebalancer: %s/%s still in cooldown", policy.Namespace, policy.Name)
			return nil
		}
	}

	// Resolve MachineDeployment
	mdNamespace := policy.Spec.TargetRef.Namespace
	if mdNamespace == "" {
		mdNamespace = policy.Namespace
	}
	var md clusterv1.MachineDeployment
	if err := rb.Get(ctx, types.NamespacedName{
		Namespace: mdNamespace,
		Name:      policy.Spec.TargetRef.Name,
	}, &md); err != nil {
		return fmt.Errorf("getting MachineDeployment: %w", err)
	}

	// Check minimum replicas
	replicas := int32(1)
	if md.Spec.Replicas != nil {
		replicas = *md.Spec.Replicas
	}
	if replicas < minReplicasForRebalance {
		klog.V(2).Infof("rebalancer: %s has %d replicas (min: %d), skipping",
			md.Name, replicas, minReplicasForRebalance)
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:    optv1.RebalancingCondition,
			Status:  metav1.ConditionFalse,
			Reason:  optv1.ReasonInsufficientReplicas,
			Message: fmt.Sprintf("Need at least %d replicas, have %d", minReplicasForRebalance, replicas),
		})
		return rb.Status().Update(ctx, policy)
	}

	// Check if MachineDeployment is mid-rollout using v1beta2 status fields.
	// If UpToDateReplicas != Replicas, a rollout is in progress.
	if !replicasMatch(md.Status.UpToDateReplicas, md.Status.Replicas) {
		klog.V(2).Infof("rebalancer: %s is mid-rollout, skipping", md.Name)
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:    optv1.RebalancingCondition,
			Status:  metav1.ConditionFalse,
			Reason:  optv1.ReasonRolloutInProgress,
			Message: "MachineDeployment is mid-rollout",
		})
		return rb.Status().Update(ctx, policy)
	}

	// Get pricing cache
	cache, ok := rb.reconciler.caches[policy.UID]
	if !ok {
		klog.V(2).Infof("rebalancer: no cache for %s/%s, waiting for reconciler", policy.Namespace, policy.Name)
		return nil
	}

	if err := cache.EnsureFresh(ctx); err != nil {
		return fmt.Errorf("refreshing pricing cache: %w", err)
	}

	// Resolve current plan
	currentPlan, currentTemplate, err := rb.reconciler.resolveCurrentPlan(ctx, &md, cache)
	if err != nil {
		return fmt.Errorf("resolving current plan: %w", err)
	}

	// Get available plans
	plans := cache.Plans(policy.Spec.AllowedCategories)

	var currentPlanID string
	var currentPrice float64
	if currentPlan != nil {
		currentPlanID = currentPlan.Identifier
		currentPrice = currentPlan.PriceMonthly
	}

	minSavings := policy.Spec.Rebalancing.MinSavingsPercent
	if minSavings <= 0 {
		minSavings = defaultMinSavingsPercent
	}

	// Determine scaling direction from workload utilization
	clusterName := md.Labels["cluster.x-k8s.io/cluster-name"]
	direction, utilResult, _ := rb.reconciler.determineDirection(ctx, policy, &md, currentPlan, clusterName)

	// Update utilization status
	if utilResult != nil {
		now := metav1.Now()
		policy.Status.CurrentUtilization = &optv1.UtilizationStatus{
			CPUPercent:    utilization.EffectiveCPUPercent(utilResult),
			MemoryPercent: utilization.EffectiveMemoryPercent(utilResult),
			Source:        utilization.EffectiveSource(utilResult),
			LastUpdated:   now,
		}
	}

	requiredCPUMillis := policy.Spec.Constraints.MinCPU * 1000
	requiredRAMMB := policy.Spec.Constraints.MinRAM
	requiredSSDGB := policy.Spec.Constraints.MinSSD

	// Use actual pod requests if higher than static constraints
	if utilResult != nil {
		var totalReqCPU, totalReqRAM int64
		for _, n := range utilResult.Nodes {
			totalReqCPU += n.RequestedCPU
			totalReqRAM += n.RequestedRAM / (1024 * 1024)
		}
		nodeCount := len(utilResult.Nodes)
		if nodeCount > 0 {
			perNodeCPU := int(totalReqCPU) / nodeCount
			perNodeRAM := int(totalReqRAM) / nodeCount
			if perNodeCPU > requiredCPUMillis {
				requiredCPUMillis = perNodeCPU
			}
			if perNodeRAM > requiredRAMMB {
				requiredRAMMB = perNodeRAM
			}
		}
	}

	var curCPU, curRAM int
	if currentPlan != nil {
		curCPU = currentPlan.CPU
		curRAM = currentPlan.RAM
	}

	result := selector.Select(
		plans,
		policy.Spec.Constraints,
		requiredCPUMillis, requiredRAMMB, requiredSSDGB,
		currentPlanID, currentPrice,
		curCPU, curRAM,
		policy.Spec.Aggressiveness,
		minSavings,
		direction,
	)

	shouldSwitch := result.Plan != nil &&
		result.Plan.Identifier != currentPlanID &&
		result.SavingsPercent >= float64(minSavings)

	if !shouldSwitch {
		klog.V(2).Infof("rebalancer: %s/%s — no better plan found", policy.Namespace, policy.Name)
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:    optv1.RebalancingCondition,
			Status:  metav1.ConditionFalse,
			Reason:  optv1.ReasonNoBetterPlan,
			Message: "Current plan is optimal",
		})
		return rb.Status().Update(ctx, policy)
	}

	if policy.Spec.DryRun {
		klog.V(2).Infof("rebalancer: [DRY RUN] would switch %s to %s (saves %.1f%%)",
			md.Name, result.Plan.Nickname, result.SavingsPercent)
		scalermetrics.RebalancingOperationsTotal.WithLabelValues(clusterName, md.Name, "dry_run").Inc()
		return nil
	}

	// Perform the switch
	klog.Infof("rebalancer: switching %s to plan %s (saves %.1f%%)",
		md.Name, result.Plan.Nickname, result.SavingsPercent)

	if err := rb.reconciler.switchPlan(ctx, policy, &md, result.Plan, currentTemplate); err != nil {
		policy.Status.Phase = optv1.ScalingPolicyPhaseError
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:    optv1.RebalancingCondition,
			Status:  metav1.ConditionFalse,
			Reason:  "SwitchFailed",
			Message: err.Error(),
		})
		scalermetrics.RebalancingOperationsTotal.WithLabelValues(clusterName, md.Name, "failed").Inc()
		return rb.Status().Update(ctx, policy)
	}

	now := metav1.Now()
	policy.Status.LastRebalanceTime = &now
	policy.Status.Phase = optv1.ScalingPolicyPhaseRebalancing
	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:    optv1.RebalancingCondition,
		Status:  metav1.ConditionTrue,
		Reason:  optv1.ReasonRebalancingInProgress,
		Message: fmt.Sprintf("Switching to %s (saves %.1f%%)", result.Plan.Nickname, result.SavingsPercent),
	})

	scalermetrics.RebalancingOperationsTotal.WithLabelValues(clusterName, md.Name, "success").Inc()

	savings := currentPrice - result.Plan.PriceMonthly
	scalermetrics.MonthlyCostSavings.WithLabelValues(clusterName, md.Name).Set(savings * float64(replicas))

	return rb.Status().Update(ctx, policy)
}

// cooldownForAggressiveness returns the default cooldown duration.
func (rb *Rebalancer) cooldownForAggressiveness(a optv1.Aggressiveness) time.Duration {
	switch a {
	case optv1.AggressivenessConservative:
		return 30 * time.Minute
	case optv1.AggressivenessAggressive:
		return 5 * time.Minute
	default:
		return 15 * time.Minute
	}
}

// NeedLeaderElection implements manager.LeaderElectionRunnable.
func (rb *Rebalancer) NeedLeaderElection() bool {
	return true
}

// replicasMatch compares two optional int32 pointers for equality.
func replicasMatch(a, b *int32) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}
