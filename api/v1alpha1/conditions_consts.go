package v1alpha1

const (
	// PricingDataReadyCondition indicates pricing data has been successfully fetched.
	PricingDataReadyCondition = "PricingDataReady"

	// TargetResolvedCondition indicates the target MachineDeployment has been found.
	TargetResolvedCondition = "TargetResolved"

	// PlanSelectedCondition indicates a plan has been selected/evaluated.
	PlanSelectedCondition = "PlanSelected"

	// RebalancingCondition indicates a rebalancing operation is in progress.
	RebalancingCondition = "Rebalancing"

	// UtilizationReadyCondition indicates utilization data has been successfully collected.
	UtilizationReadyCondition = "UtilizationReady"

	// SchedulingSimulationCondition indicates the result of the scheduling simulation for downscaling.
	SchedulingSimulationCondition = "SchedulingSimulation"

	// HorizontalScalingCondition indicates the result of horizontal scaling evaluation.
	HorizontalScalingCondition = "HorizontalScaling"

	// Reasons
	ReasonPricingFetchFailed    = "PricingFetchFailed"
	ReasonPricingDataReady      = "PricingDataReady"
	ReasonTargetNotFound        = "TargetNotFound"
	ReasonTargetResolved        = "TargetResolved"
	ReasonCredentialsNotFound   = "CredentialsNotFound"
	ReasonNoBetterPlan          = "NoBetterPlan"
	ReasonBetterPlanFound       = "BetterPlanFound"
	ReasonPlanSwitched          = "PlanSwitched"
	ReasonDryRun                = "DryRun"
	ReasonRebalancingInProgress = "RebalancingInProgress"
	ReasonRebalancingComplete   = "RebalancingComplete"
	ReasonRolloutInProgress     = "RolloutInProgress"
	ReasonInsufficientReplicas  = "InsufficientReplicas"
	ReasonCooldownActive        = "CooldownActive"
	ReasonWorkloadAccessFailed  = "WorkloadAccessFailed"
	ReasonUtilizationCalculated = "UtilizationCalculated"
	ReasonMetricsUnavailable    = "MetricsUnavailable"
	ReasonScaleUpTriggered      = "ScaleUpTriggered"
	ReasonScaleDownTriggered    = "ScaleDownTriggered"
	ReasonScaleDownBlocked      = "ScaleDownBlocked"
	ReasonUtilizationInRange    = "UtilizationInRange"
	ReasonScaleUpReplicas       = "ScaleUpReplicas"
	ReasonScaleDownReplicas     = "ScaleDownReplicas"
	ReasonMaxReplicasReached    = "MaxReplicasReached"
	ReasonMinReplicasReached    = "MinReplicasReached"
	ReasonStabilizationActive   = "StabilizationActive"
	ReasonNoPendingPods         = "NoPendingPods"
	ReasonDrainInProgress       = "DrainInProgress"
	ReasonDrainTimeout          = "DrainTimeout"
	ReasonDrainAborted          = "DrainAborted"
	ReasonRolloutStalled        = "RolloutStalled"
)
