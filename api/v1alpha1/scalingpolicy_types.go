package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ObjectReference identifies a target MachineDeployment.
type ObjectReference struct {
	// Name is the name of the MachineDeployment.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace is the namespace of the MachineDeployment.
	// If empty, defaults to the ScalingPolicy's namespace.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// CredentialsRef references a Secret containing VPSie API credentials.
type CredentialsRef struct {
	// Name is the name of the Secret. Must contain an "apiKey" key.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace is the namespace of the Secret.
	// +optional
	Namespace string `json:"namespace,omitempty"`
}

// ResourceConstraints defines min/max resource boundaries for plan selection.
type ResourceConstraints struct {
	// MinCPU is the minimum number of vCPUs.
	// +optional
	// +kubebuilder:default=1
	MinCPU int `json:"minCPU,omitempty"`

	// MaxCPU is the maximum number of vCPUs.
	// +optional
	// +kubebuilder:default=32
	MaxCPU int `json:"maxCPU,omitempty"`

	// MinRAM is the minimum RAM in MB.
	// +optional
	// +kubebuilder:default=1024
	MinRAM int `json:"minRAM,omitempty"`

	// MaxRAM is the maximum RAM in MB.
	// +optional
	// +kubebuilder:default=131072
	MaxRAM int `json:"maxRAM,omitempty"`

	// MinSSD is the minimum SSD in GB.
	// +optional
	// +kubebuilder:default=20
	MinSSD int `json:"minSSD,omitempty"`

	// ExcludedPlans is a list of plan identifiers to exclude from selection.
	// +optional
	ExcludedPlans []string `json:"excludedPlans,omitempty"`
}

// RebalancingSpec configures periodic node rebalancing.
type RebalancingSpec struct {
	// Enabled controls whether periodic rebalancing is active.
	// +optional
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// CooldownPeriod is the minimum time between rebalancing operations.
	// Defaults vary by aggressiveness: 30m (conservative), 15m (moderate), 5m (aggressive).
	// +optional
	CooldownPeriod *metav1.Duration `json:"cooldownPeriod,omitempty"`

	// MaxConcurrentReplacements is the maximum number of nodes being replaced simultaneously.
	// +optional
	// +kubebuilder:default=1
	MaxConcurrentReplacements int `json:"maxConcurrentReplacements,omitempty"`

	// MinSavingsPercent is the minimum cost savings required to trigger a rebalance.
	// +optional
	// +kubebuilder:default=15
	MinSavingsPercent int `json:"minSavingsPercent,omitempty"`
}

// UtilizationSpec defines directional utilization thresholds for scaling decisions.
type UtilizationSpec struct {
	// ScaleUpThreshold is the utilization percentage above which upscaling is triggered.
	// If CPU OR memory exceeds this threshold, a bigger VM plan is selected.
	// +optional
	// +kubebuilder:default=75
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	ScaleUpThreshold int `json:"scaleUpThreshold,omitempty"`

	// ScaleDownThreshold is the utilization percentage below which downscaling is triggered.
	// Both CPU AND memory must be below this threshold, and scheduling simulation must pass.
	// +optional
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=99
	ScaleDownThreshold int `json:"scaleDownThreshold,omitempty"`
}

// UtilizationStatus reports the observed utilization of the workload cluster nodes.
type UtilizationStatus struct {
	// CPUPercent is the observed CPU utilization percentage.
	CPUPercent int `json:"cpuPercent"`

	// MemoryPercent is the observed memory utilization percentage.
	MemoryPercent int `json:"memoryPercent"`

	// Source indicates how utilization was measured: "requests", "metrics", or "both".
	Source string `json:"source"`

	// LastUpdated is the timestamp of the last utilization measurement.
	LastUpdated metav1.Time `json:"lastUpdated"`
}

// PlanInfo holds metadata about a VPSie VM plan.
type PlanInfo struct {
	// Identifier is the VPSie plan identifier (UUID).
	Identifier string `json:"identifier"`

	// Nickname is the human-readable plan name.
	// +optional
	Nickname string `json:"nickname,omitempty"`

	// CPU is the number of vCPUs.
	CPU int `json:"cpu"`

	// RAM is the amount of RAM in MB.
	RAM int `json:"ram"`

	// SSD is the amount of SSD storage in GB.
	SSD int `json:"ssd"`

	// PriceMonthly is the monthly price in USD.
	PriceMonthly float64 `json:"priceMonthly"`
}

// Aggressiveness defines how aggressively the scaler optimizes cost.
// +kubebuilder:validation:Enum=conservative;moderate;aggressive
type Aggressiveness string

const (
	AggressivenessConservative Aggressiveness = "conservative"
	AggressivenessModerate     Aggressiveness = "moderate"
	AggressivenessAggressive   Aggressiveness = "aggressive"
)

// ScalingPolicyPhase defines the phase of a ScalingPolicy.
type ScalingPolicyPhase string

const (
	ScalingPolicyPhaseActive      ScalingPolicyPhase = "Active"
	ScalingPolicyPhaseRebalancing ScalingPolicyPhase = "Rebalancing"
	ScalingPolicyPhaseDryRun      ScalingPolicyPhase = "DryRun"
	ScalingPolicyPhaseError       ScalingPolicyPhase = "Error"
)

// HorizontalSpec configures horizontal scaling (adjusting MachineDeployment replicas).
type HorizontalSpec struct {
	// Enabled controls whether horizontal scaling is active.
	// When enabled, the scaler adds nodes when pods are unschedulable
	// and removes nodes when utilization is low across all nodes.
	// +optional
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// MinReplicas is the minimum number of replicas. The scaler will not scale below this.
	// +optional
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	MinReplicas int32 `json:"minReplicas,omitempty"`

	// MaxReplicas is the maximum number of replicas. The scaler will not scale above this.
	// +optional
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	MaxReplicas int32 `json:"maxReplicas,omitempty"`

	// ScaleDownStabilization is the minimum time to wait after the last scale-up
	// before allowing a scale-down. Prevents flapping.
	// +optional
	ScaleDownStabilization *metav1.Duration `json:"scaleDownStabilization,omitempty"`
}

// NodePoolPolicy controls automatic creation of separate MachineDeployments
// for workloads whose resource requests exceed the base plan's capacity.
type NodePoolPolicy struct {
	// Enabled enables automatic node pool splitting.
	// When enabled, the scaler detects pending pods whose resource requests
	// exceed the current plan's capacity and creates satellite MachineDeployments
	// with appropriately sized plans.
	// +optional
	// +kubebuilder:default=false
	Enabled bool `json:"enabled,omitempty"`

	// MaxPools is the maximum number of satellite pools to create.
	// +optional
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=10
	MaxPools int `json:"maxPools,omitempty"`

	// ScaleDownDelay is how long a satellite pool must have zero workload pods
	// before it is deleted. Defaults to 10 minutes.
	// +optional
	ScaleDownDelay *metav1.Duration `json:"scaleDownDelay,omitempty"`
}

// NodePoolStatus tracks a satellite MachineDeployment created for oversized workloads.
type NodePoolStatus struct {
	// Name is the name of the satellite MachineDeployment.
	Name string `json:"name"`

	// PlanID is the VPSie plan identifier used for this pool.
	PlanID string `json:"planId"`

	// PlanNickname is the human-readable plan name.
	PlanNickname string `json:"planNickname"`

	// Replicas is the current replica count.
	Replicas int32 `json:"replicas"`

	// LastPodSeen is the last time a workload pod was observed on this pool's nodes.
	// +optional
	LastPodSeen *metav1.Time `json:"lastPodSeen,omitempty"`
}

// ScalingPolicySpec defines the desired state of ScalingPolicy.
type ScalingPolicySpec struct {
	// TargetRef references the MachineDeployment to optimize.
	TargetRef ObjectReference `json:"targetRef"`

	// CredentialsRef is the VPSie API key Secret.
	// Falls back to the VPSieCluster's credentialsRef if not set.
	// +optional
	CredentialsRef *CredentialsRef `json:"credentialsRef,omitempty"`

	// DCIdentifier is the datacenter identifier for plan lookups.
	// +kubebuilder:validation:MinLength=1
	DCIdentifier string `json:"dcIdentifier"`

	// OSIdentifier is the image identifier for plan lookups.
	// +kubebuilder:validation:MinLength=1
	OSIdentifier string `json:"osIdentifier"`

	// AllowedCategories lists plan category names to consider.
	// Empty means all categories are allowed.
	// Controller resolves names to UUIDs via GET /api/v2/plans/category.
	// NOTE: Category "A" (Shared CPU) uses memory ballooning which Talos does not
	// support. VMs get the balloon minimum (~1-2 GiB) instead of the advertised RAM.
	// Exclude "A" for Talos-based clusters: use ["C","M","G","N"] instead.
	// +optional
	AllowedCategories []string `json:"allowedCategories,omitempty"`

	// Constraints defines resource boundaries for plan selection.
	// +optional
	Constraints ResourceConstraints `json:"constraints,omitempty"`

	// Aggressiveness controls cost optimization aggressiveness.
	// +optional
	// +kubebuilder:default=moderate
	Aggressiveness Aggressiveness `json:"aggressiveness,omitempty"`

	// Rebalancing configures periodic node rebalancing.
	// +optional
	Rebalancing RebalancingSpec `json:"rebalancing,omitempty"`

	// TargetUtilization defines CPU/memory utilization thresholds.
	// +optional
	TargetUtilization UtilizationSpec `json:"targetUtilization,omitempty"`

	// Horizontal configures horizontal scaling (replica count adjustments).
	// +optional
	Horizontal HorizontalSpec `json:"horizontal,omitempty"`

	// NodePoolPolicy controls automatic node pool splitting for heterogeneous workloads.
	// When enabled, the scaler auto-detects pending pods that don't fit on the current
	// plan and creates satellite MachineDeployments with bigger plans.
	// +optional
	NodePoolPolicy *NodePoolPolicy `json:"nodePoolPolicy,omitempty"`

	// DryRun enables log-only mode without making changes.
	// +optional
	// +kubebuilder:default=false
	DryRun bool `json:"dryRun,omitempty"`

	// PlanRefreshInterval is how often to refresh pricing data.
	// +optional
	PlanRefreshInterval *metav1.Duration `json:"planRefreshInterval,omitempty"`
}

// ScalingPolicyStatus defines the observed state of ScalingPolicy.
type ScalingPolicyStatus struct {
	// CurrentPlan is the plan currently used by the MachineDeployment.
	// +optional
	CurrentPlan *PlanInfo `json:"currentPlan,omitempty"`

	// RecommendedPlan is the plan the controller recommends switching to.
	// +optional
	RecommendedPlan *PlanInfo `json:"recommendedPlan,omitempty"`

	// AvailableCategories lists discovered plan category names from the API.
	// +optional
	AvailableCategories []string `json:"availableCategories,omitempty"`

	// LastRebalanceTime is the timestamp of the last rebalancing operation.
	// +optional
	LastRebalanceTime *metav1.Time `json:"lastRebalanceTime,omitempty"`

	// LastPlanRefreshTime is the timestamp of the last pricing data refresh.
	// +optional
	LastPlanRefreshTime *metav1.Time `json:"lastPlanRefreshTime,omitempty"`

	// CurrentUtilization reports the observed utilization of the workload cluster.
	// +optional
	CurrentUtilization *UtilizationStatus `json:"currentUtilization,omitempty"`

	// EstimatedMonthlySavings is the estimated monthly cost savings.
	// +optional
	EstimatedMonthlySavings string `json:"estimatedMonthlySavings,omitempty"`

	// PendingPods is the number of unschedulable pods detected in the workload cluster.
	// +optional
	PendingPods int `json:"pendingPods,omitempty"`

	// CurrentReplicas is the current replica count of the MachineDeployment.
	// +optional
	CurrentReplicas int32 `json:"currentReplicas,omitempty"`

	// DesiredReplicas is the desired replica count after horizontal scaling.
	// +optional
	DesiredReplicas int32 `json:"desiredReplicas,omitempty"`

	// LastScaleTime is the timestamp of the last horizontal scaling operation.
	// +optional
	LastScaleTime *metav1.Time `json:"lastScaleTime,omitempty"`

	// DrainingNode is the name of the node currently being drained for scale-down.
	// When set, the controller will verify the drain is complete before reducing replicas.
	// +optional
	DrainingNode string `json:"drainingNode,omitempty"`

	// DrainingStartedAt is the timestamp when the current drain operation began.
	// Used to enforce a drain timeout (default 5 minutes).
	// +optional
	DrainingStartedAt *metav1.Time `json:"drainingStartedAt,omitempty"`

	// PreviousInfraTemplate is the name of the VPSieMachineTemplate that was in use
	// before the last plan switch. Used to revert on stalled rollouts.
	// +optional
	PreviousInfraTemplate string `json:"previousInfraTemplate,omitempty"`

	// NodePools tracks satellite MachineDeployments created for workloads
	// that don't fit on the base plan.
	// +optional
	NodePools []NodePoolStatus `json:"nodePools,omitempty"`

	// Phase is the current phase of the ScalingPolicy.
	// +optional
	Phase ScalingPolicyPhase `json:"phase,omitempty"`

	// Conditions defines current state of the ScalingPolicy.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=scalingpolicies,scope=Namespaced,shortName=sp
// +kubebuilder:printcolumn:name="Target",type="string",JSONPath=".spec.targetRef.name",description="Target MachineDeployment"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase",description="Current phase"
// +kubebuilder:printcolumn:name="Current Plan",type="string",JSONPath=".status.currentPlan.nickname",description="Current VM plan"
// +kubebuilder:printcolumn:name="Recommended",type="string",JSONPath=".status.recommendedPlan.nickname",description="Recommended VM plan"
// +kubebuilder:printcolumn:name="Replicas",type="integer",JSONPath=".status.currentReplicas",description="Current replicas"
// +kubebuilder:printcolumn:name="Pending",type="integer",JSONPath=".status.pendingPods",description="Pending pods"
// +kubebuilder:printcolumn:name="Savings",type="string",JSONPath=".status.estimatedMonthlySavings",description="Estimated monthly savings"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ScalingPolicy is the Schema for the scalingpolicies API.
// It defines cost optimization rules for a MachineDeployment's worker nodes.
type ScalingPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ScalingPolicySpec   `json:"spec,omitempty"`
	Status ScalingPolicyStatus `json:"status,omitempty"`
}

// GetConditions returns the conditions for a ScalingPolicy.
func (sp *ScalingPolicy) GetConditions() []metav1.Condition {
	return sp.Status.Conditions
}

// SetConditions sets conditions on a ScalingPolicy.
func (sp *ScalingPolicy) SetConditions(conditions []metav1.Condition) {
	sp.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// ScalingPolicyList contains a list of ScalingPolicy.
type ScalingPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ScalingPolicy `json:"items"`
}
