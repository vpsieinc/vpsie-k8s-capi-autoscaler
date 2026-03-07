package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	optv1 "github.com/vpsieinc/vpsie-cluster-scaler/api/v1alpha1"
	scalermetrics "github.com/vpsieinc/vpsie-cluster-scaler/internal/metrics"
	"github.com/vpsieinc/vpsie-cluster-scaler/internal/pricing"
	"github.com/vpsieinc/vpsie-cluster-scaler/internal/scheduler"
	"github.com/vpsieinc/vpsie-cluster-scaler/internal/selector"
	"github.com/vpsieinc/vpsie-cluster-scaler/internal/utilization"
	"github.com/vpsieinc/vpsie-cluster-scaler/internal/vpsie"
	"github.com/vpsieinc/vpsie-cluster-scaler/internal/workload"

	infrav1 "github.com/vpsieinc/cluster-api-provider-vpsie/api/v1alpha1"
)

const (
	requeueAfterDefault      = 60 * time.Second
	requeueAfterError        = 30 * time.Second
	defaultRefreshInterval   = 5 * time.Minute
	defaultMinSavingsPercent = 15
)

// ScalingPolicyReconciler reconciles a ScalingPolicy object.
type ScalingPolicyReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// NewPricingClient creates a PricingClient for the given API key.
	// When nil, the real vpsie.NewClient is used.
	NewPricingClient func(apiKey string) (vpsie.PricingClient, error)

	// WorkloadClients creates clients for workload cluster access.
	// When nil, utilization-aware scaling is disabled (graceful degradation).
	WorkloadClients workload.WorkloadClientFactory

	// caches stores pricing caches keyed by ScalingPolicy UID.
	caches map[types.UID]*pricing.Cache
}

// SetupWithManager sets up the controller with the Manager.
func (r *ScalingPolicyReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	r.caches = make(map[types.UID]*pricing.Cache)

	return ctrl.NewControllerManagedBy(mgr).
		For(&optv1.ScalingPolicy{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(
			&clusterv1.MachineDeployment{},
			handler.EnqueueRequestsFromMapFunc(r.machineDeploymentToScalingPolicy),
		).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 1,
		}).
		Complete(r)
}

// machineDeploymentToScalingPolicy maps a MachineDeployment change to ScalingPolicy reconcile requests.
func (r *ScalingPolicyReconciler) machineDeploymentToScalingPolicy(ctx context.Context, o client.Object) []reconcile.Request {
	md, ok := o.(*clusterv1.MachineDeployment)
	if !ok {
		return nil
	}

	var policyList optv1.ScalingPolicyList
	if err := r.List(ctx, &policyList, client.InNamespace(md.Namespace)); err != nil {
		klog.V(2).Infof("failed to list ScalingPolicies: %v", err)
		return nil
	}

	var requests []reconcile.Request
	for _, policy := range policyList.Items {
		if policy.Spec.TargetRef.Name == md.Name {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: policy.Namespace,
					Name:      policy.Name,
				},
			})
		}
	}
	return requests
}

// +kubebuilder:rbac:groups=optimization.vpsie.com,resources=scalingpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=optimization.vpsie.com,resources=scalingpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=optimization.vpsie.com,resources=scalingpolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machinedeployments,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=vpsiemachinetemplates,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=vpsieclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *ScalingPolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	klog.V(2).Infof("reconciling ScalingPolicy %s/%s", req.Namespace, req.Name)

	// Fetch the ScalingPolicy
	var policy optv1.ScalingPolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Resolve the target MachineDeployment
	mdNamespace := policy.Spec.TargetRef.Namespace
	if mdNamespace == "" {
		mdNamespace = policy.Namespace
	}
	var md clusterv1.MachineDeployment
	mdKey := types.NamespacedName{Namespace: mdNamespace, Name: policy.Spec.TargetRef.Name}
	if err := r.Get(ctx, mdKey, &md); err != nil {
		if apierrors.IsNotFound(err) {
			meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
				Type:    optv1.TargetResolvedCondition,
				Status:  metav1.ConditionFalse,
				Reason:  optv1.ReasonTargetNotFound,
				Message: fmt.Sprintf("MachineDeployment %s not found", mdKey),
			})
			policy.Status.Phase = optv1.ScalingPolicyPhaseError
			if err := r.Status().Update(ctx, &policy); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: requeueAfterError}, nil
		}
		return ctrl.Result{}, err
	}

	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:    optv1.TargetResolvedCondition,
		Status:  metav1.ConditionTrue,
		Reason:  optv1.ReasonTargetResolved,
		Message: fmt.Sprintf("MachineDeployment %s found", mdKey),
	})

	// Resolve credentials
	apiKey, err := r.resolveCredentials(ctx, &policy)
	if err != nil {
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:    optv1.PricingDataReadyCondition,
			Status:  metav1.ConditionFalse,
			Reason:  optv1.ReasonCredentialsNotFound,
			Message: err.Error(),
		})
		policy.Status.Phase = optv1.ScalingPolicyPhaseError
		if statusErr := r.Status().Update(ctx, &policy); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: requeueAfterError}, nil
	}

	// Get or create pricing cache
	cache, err := r.ensureCache(policy.UID, apiKey, policy.Spec.DCIdentifier, policy.Spec.OSIdentifier, policy.Spec.PlanRefreshInterval)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("creating pricing client: %w", err)
	}

	// Refresh pricing data
	if err := cache.EnsureFresh(ctx); err != nil {
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:    optv1.PricingDataReadyCondition,
			Status:  metav1.ConditionFalse,
			Reason:  optv1.ReasonPricingFetchFailed,
			Message: err.Error(),
		})
		policy.Status.Phase = optv1.ScalingPolicyPhaseError
		if statusErr := r.Status().Update(ctx, &policy); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: requeueAfterError}, nil
	}

	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:    optv1.PricingDataReadyCondition,
		Status:  metav1.ConditionTrue,
		Reason:  optv1.ReasonPricingDataReady,
		Message: "Pricing data loaded",
	})

	// Update available categories in status
	policy.Status.AvailableCategories = cache.Categories()
	lastRefresh := cache.LastRefreshTime()
	policy.Status.LastPlanRefreshTime = &metav1.Time{Time: lastRefresh}

	// Resolve current plan from MachineDeployment's VPSieMachineTemplate
	currentPlan, currentTemplate, err := r.resolveCurrentPlan(ctx, &md, cache)
	if err != nil {
		klog.V(2).Infof("failed to resolve current plan: %v", err)
	}

	if currentPlan != nil {
		policy.Status.CurrentPlan = &optv1.PlanInfo{
			Identifier:   currentPlan.Identifier,
			Nickname:     currentPlan.Nickname,
			CPU:          currentPlan.CPU,
			RAM:          currentPlan.RAM,
			SSD:          currentPlan.SSD,
			PriceMonthly: currentPlan.PriceMonthly,
		}

		scalermetrics.CurrentPlanPrice.WithLabelValues(md.Labels["cluster.x-k8s.io/cluster-name"], md.Name, currentPlan.Nickname).Set(currentPlan.PriceMonthly)
	}

	// Get plans filtered by allowed categories
	plans := cache.Plans(policy.Spec.AllowedCategories)

	// Run selector
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

	// Track current replicas in status
	currentReplicas := int32(1)
	if md.Spec.Replicas != nil {
		currentReplicas = *md.Spec.Replicas
	}
	policy.Status.CurrentReplicas = currentReplicas
	policy.Status.DesiredReplicas = currentReplicas

	// Determine scaling direction from workload utilization
	clusterName := md.Labels["cluster.x-k8s.io/cluster-name"]
	direction, utilResult, _ := r.determineDirection(ctx, &policy, &md, currentPlan, clusterName)

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

	// --- Horizontal scaling: adjust replicas based on pending pods ---
	if err := r.reconcileHorizontal(ctx, &policy, &md, clusterName, utilResult); err != nil {
		klog.V(2).Infof("horizontal scaling error: %v", err)
	}

	// Use constraints for minimum requirements; override with actual pod requests if higher
	requiredCPUMillis := policy.Spec.Constraints.MinCPU * 1000
	requiredRAMMB := policy.Spec.Constraints.MinRAM
	requiredSSDGB := policy.Spec.Constraints.MinSSD

	// When utilization data is available, use actual per-node pod requests (whichever is higher)
	if utilResult != nil {
		var totalReqCPU, totalReqRAM int64
		for _, n := range utilResult.Nodes {
			totalReqCPU += n.RequestedCPU            // millicores
			totalReqRAM += n.RequestedRAM / (1024 * 1024) // bytes -> MB
		}
		// Per-node average (requirements that each node must handle)
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

	// Update recommended plan in status
	if result.Plan != nil {
		policy.Status.RecommendedPlan = &optv1.PlanInfo{
			Identifier:   result.Plan.Identifier,
			Nickname:     result.Plan.Nickname,
			CPU:          result.Plan.CPU,
			RAM:          result.Plan.RAM,
			SSD:          result.Plan.SSD,
			PriceMonthly: result.Plan.PriceMonthly,
		}

		if result.SavingsPercent > 0 {
			savings := currentPrice - result.Plan.PriceMonthly
			replicas := int32(1)
			if md.Spec.Replicas != nil {
				replicas = *md.Spec.Replicas
			}
			monthlySavings := savings * float64(replicas)
			policy.Status.EstimatedMonthlySavings = fmt.Sprintf("$%.2f", monthlySavings)

			clusterName := md.Labels["cluster.x-k8s.io/cluster-name"]
			scalermetrics.MonthlyCostSavings.WithLabelValues(clusterName, md.Name).Set(monthlySavings)
		}
	}

	// Should we switch plans?
	shouldSwitch := result.Plan != nil &&
		result.Plan.Identifier != currentPlanID &&
		result.SavingsPercent >= float64(minSavings)

	if policy.Spec.DryRun {
		policy.Status.Phase = optv1.ScalingPolicyPhaseDryRun
		if shouldSwitch {
			meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
				Type:    optv1.PlanSelectedCondition,
				Status:  metav1.ConditionTrue,
				Reason:  optv1.ReasonDryRun,
				Message: fmt.Sprintf("[DRY RUN] Would switch to %s (saves %.1f%%)", result.Plan.Nickname, result.SavingsPercent),
			})
			r.Recorder.Eventf(&policy, corev1.EventTypeNormal, "DryRun",
				"Would switch MachineDeployment %s to plan %s (saves %.1f%%)",
				md.Name, result.Plan.Nickname, result.SavingsPercent)

			clusterName := md.Labels["cluster.x-k8s.io/cluster-name"]
			scalermetrics.PlanSelectionsTotal.WithLabelValues(clusterName, md.Name, result.Plan.Nickname, "dry_run").Inc()
		} else {
			meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
				Type:    optv1.PlanSelectedCondition,
				Status:  metav1.ConditionTrue,
				Reason:  optv1.ReasonNoBetterPlan,
				Message: "Current plan is optimal",
			})
		}
	} else if shouldSwitch {
		// Create new VPSieMachineTemplate and patch MachineDeployment
		if err := r.switchPlan(ctx, &policy, &md, result.Plan, currentTemplate); err != nil {
			klog.Errorf("failed to switch plan: %v", err)
			policy.Status.Phase = optv1.ScalingPolicyPhaseError
			r.Recorder.Eventf(&policy, corev1.EventTypeWarning, "SwitchFailed",
				"Failed to switch plan: %v", err)
		} else {
			policy.Status.Phase = optv1.ScalingPolicyPhaseActive
			meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
				Type:    optv1.PlanSelectedCondition,
				Status:  metav1.ConditionTrue,
				Reason:  optv1.ReasonPlanSwitched,
				Message: fmt.Sprintf("Switched to %s (saves %.1f%%)", result.Plan.Nickname, result.SavingsPercent),
			})
			r.Recorder.Eventf(&policy, corev1.EventTypeNormal, "PlanSwitched",
				"Switched MachineDeployment %s to plan %s (saves %.1f%%)",
				md.Name, result.Plan.Nickname, result.SavingsPercent)

			clusterName := md.Labels["cluster.x-k8s.io/cluster-name"]
			scalermetrics.PlanSelectionsTotal.WithLabelValues(clusterName, md.Name, result.Plan.Nickname, "switched").Inc()
		}
	} else {
		policy.Status.Phase = optv1.ScalingPolicyPhaseActive
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:    optv1.PlanSelectedCondition,
			Status:  metav1.ConditionTrue,
			Reason:  optv1.ReasonNoBetterPlan,
			Message: "Current plan is optimal",
		})
	}

	if err := r.Status().Update(ctx, &policy); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: requeueAfterDefault}, nil
}

// resolveCredentials finds the VPSie API key from the policy's credentialsRef
// or falls back to the VPSieCluster's credentialsRef.
func (r *ScalingPolicyReconciler) resolveCredentials(ctx context.Context, policy *optv1.ScalingPolicy) (string, error) {
	if policy.Spec.CredentialsRef != nil {
		ns := policy.Spec.CredentialsRef.Namespace
		if ns == "" {
			ns = policy.Namespace
		}
		return r.getAPIKeyFromSecret(ctx, ns, policy.Spec.CredentialsRef.Name)
	}

	// Fall back to the VPSieCluster's credentials
	mdNamespace := policy.Spec.TargetRef.Namespace
	if mdNamespace == "" {
		mdNamespace = policy.Namespace
	}

	var md clusterv1.MachineDeployment
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: mdNamespace,
		Name:      policy.Spec.TargetRef.Name,
	}, &md); err != nil {
		return "", fmt.Errorf("getting MachineDeployment for credential fallback: %w", err)
	}

	clusterName := md.Labels["cluster.x-k8s.io/cluster-name"]
	if clusterName == "" {
		return "", fmt.Errorf("MachineDeployment %s has no cluster-name label", md.Name)
	}

	var vpsiecluster infrav1.VPSieCluster
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: mdNamespace,
		Name:      clusterName,
	}, &vpsiecluster); err != nil {
		return "", fmt.Errorf("getting VPSieCluster %s: %w", clusterName, err)
	}

	if vpsiecluster.Spec.CredentialsRef.Name == "" {
		return "", fmt.Errorf("VPSieCluster %s has no credentialsRef", clusterName)
	}

	ns := vpsiecluster.Spec.CredentialsRef.Namespace
	if ns == "" {
		ns = vpsiecluster.Namespace
	}
	return r.getAPIKeyFromSecret(ctx, ns, vpsiecluster.Spec.CredentialsRef.Name)
}

// getAPIKeyFromSecret reads the "apiKey" field from a Secret.
func (r *ScalingPolicyReconciler) getAPIKeyFromSecret(ctx context.Context, namespace, name string) (string, error) {
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &secret); err != nil {
		return "", fmt.Errorf("getting secret %s/%s: %w", namespace, name, err)
	}
	apiKey, ok := secret.Data["apiKey"]
	if !ok || len(apiKey) == 0 {
		return "", fmt.Errorf("secret %s/%s has no apiKey field", namespace, name)
	}
	return string(apiKey), nil
}

// ensureCache gets or creates a pricing cache for the given policy UID.
func (r *ScalingPolicyReconciler) ensureCache(uid types.UID, apiKey, dcID, osID string, refreshInterval *metav1.Duration) (*pricing.Cache, error) {
	if cache, ok := r.caches[uid]; ok {
		return cache, nil
	}

	newClientFn := vpsie.NewClient
	if r.NewPricingClient != nil {
		newClientFn = r.NewPricingClient
	}

	pricingClient, err := newClientFn(apiKey)
	if err != nil {
		return nil, err
	}

	interval := defaultRefreshInterval
	if refreshInterval != nil {
		interval = refreshInterval.Duration
	}

	cache := pricing.NewCache(pricingClient, dcID, osID, interval)
	r.caches[uid] = cache
	return cache, nil
}

// resolveCurrentPlan reads the VPSieMachineTemplate from the MachineDeployment's
// infrastructureRef and finds the matching plan in the cache.
func (r *ScalingPolicyReconciler) resolveCurrentPlan(ctx context.Context, md *clusterv1.MachineDeployment, cache *pricing.Cache) (*vpsie.Plan, *infrav1.VPSieMachineTemplate, error) {
	infraRef := md.Spec.Template.Spec.InfrastructureRef
	if infraRef.Name == "" {
		return nil, nil, fmt.Errorf("MachineDeployment has no infrastructureRef")
	}

	// ContractVersionedObjectReference has no Namespace field in CAPI v1beta2;
	// the template is always in the same namespace as the MachineDeployment.
	ns := md.Namespace

	var tmpl infrav1.VPSieMachineTemplate
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: infraRef.Name}, &tmpl); err != nil {
		return nil, nil, fmt.Errorf("getting VPSieMachineTemplate %s: %w", infraRef.Name, err)
	}

	resourceID := tmpl.Spec.Template.Spec.ResourceIdentifier
	plan, found := cache.FindPlanByID(resourceID)
	if !found {
		return nil, &tmpl, fmt.Errorf("plan %s not found in cache", resourceID)
	}

	return &plan, &tmpl, nil
}

// switchPlan creates a new VPSieMachineTemplate with the recommended plan
// and patches the MachineDeployment to use it.
func (r *ScalingPolicyReconciler) switchPlan(
	ctx context.Context,
	policy *optv1.ScalingPolicy,
	md *clusterv1.MachineDeployment,
	plan *vpsie.Plan,
	currentTemplate *infrav1.VPSieMachineTemplate,
) error {
	if currentTemplate == nil {
		return fmt.Errorf("no current template to base new template on")
	}

	// Create new VPSieMachineTemplate with the new plan
	newTemplate := &infrav1.VPSieMachineTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", md.Name, strings.ToLower(plan.Nickname)),
			Namespace: md.Namespace,
			Labels:    currentTemplate.Labels,
		},
		Spec: infrav1.VPSieMachineTemplateSpec{
			Template: infrav1.VPSieMachineTemplateResource{
				Spec: infrav1.VPSieMachineSpec{
					ResourceIdentifier: plan.Identifier,
					DCIdentifier:       currentTemplate.Spec.Template.Spec.DCIdentifier,
					ImageIdentifier:    currentTemplate.Spec.Template.Spec.ImageIdentifier,
					AdditionalTags:     currentTemplate.Spec.Template.Spec.AdditionalTags,
					ServerGroup:        currentTemplate.Spec.Template.Spec.ServerGroup,
				},
			},
		},
	}

	// Check if template already exists
	var existing infrav1.VPSieMachineTemplate
	if err := r.Get(ctx, types.NamespacedName{Namespace: newTemplate.Namespace, Name: newTemplate.Name}, &existing); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("checking existing template: %w", err)
		}
		// Create new template
		if err := r.Create(ctx, newTemplate); err != nil {
			return fmt.Errorf("creating VPSieMachineTemplate %s: %w", newTemplate.Name, err)
		}
		klog.V(2).Infof("created VPSieMachineTemplate %s with plan %s", newTemplate.Name, plan.Nickname)
	}

	// Patch MachineDeployment's infrastructureRef to point to the new template
	patch := client.MergeFrom(md.DeepCopy())
	md.Spec.Template.Spec.InfrastructureRef.Name = newTemplate.Name
	if err := r.Patch(ctx, md, patch); err != nil {
		return fmt.Errorf("patching MachineDeployment %s infrastructureRef: %w", md.Name, err)
	}

	klog.V(2).Infof("patched MachineDeployment %s to use template %s (plan: %s)",
		md.Name, newTemplate.Name, plan.Nickname)
	return nil
}

// determineDirection computes the scaling direction based on workload cluster utilization.
// It returns DirectionAny when utilization data is unavailable (graceful degradation).
func (r *ScalingPolicyReconciler) determineDirection(
	ctx context.Context,
	policy *optv1.ScalingPolicy,
	md *clusterv1.MachineDeployment,
	currentPlan *vpsie.Plan,
	clusterName string,
) (selector.ScalingDirection, *utilization.Result, error) {
	if r.WorkloadClients == nil {
		return selector.DirectionAny, nil, nil
	}

	wc, err := r.WorkloadClients.ClientForCluster(ctx, clusterName, md.Namespace)
	if err != nil {
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:    optv1.UtilizationReadyCondition,
			Status:  metav1.ConditionFalse,
			Reason:  optv1.ReasonWorkloadAccessFailed,
			Message: err.Error(),
		})
		klog.V(2).Infof("workload cluster access failed for %s: %v", clusterName, err)
		return selector.DirectionAny, nil, nil // graceful degradation
	}

	calc := utilization.NewCalculator(wc)
	result, err := calc.Calculate(ctx, md.Name)
	if err != nil {
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:    optv1.UtilizationReadyCondition,
			Status:  metav1.ConditionFalse,
			Reason:  optv1.ReasonWorkloadAccessFailed,
			Message: err.Error(),
		})
		klog.V(2).Infof("utilization calculation failed for %s/%s: %v", md.Namespace, md.Name, err)
		return selector.DirectionAny, nil, nil // graceful degradation
	}

	// Update utilization condition
	reason := optv1.ReasonUtilizationCalculated
	if !result.MetricsAvailable {
		reason = optv1.ReasonMetricsUnavailable
	}
	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:   optv1.UtilizationReadyCondition,
		Status: metav1.ConditionTrue,
		Reason: reason,
		Message: fmt.Sprintf("CPU: %.1f%% scheduled, %.1f%% actual; Memory: %.1f%% scheduled, %.1f%% actual",
			result.ScheduledCPUPercent, result.ActualCPUPercent,
			result.ScheduledMemoryPercent, result.ActualMemoryPercent),
	})

	// Update Prometheus metrics
	scalermetrics.NodeCPUUtilizationPercent.WithLabelValues(
		clusterName, md.Name, result.Source,
	).Set(float64(utilization.EffectiveCPUPercent(result)))
	scalermetrics.NodeMemoryUtilizationPercent.WithLabelValues(
		clusterName, md.Name, result.Source,
	).Set(float64(utilization.EffectiveMemoryPercent(result)))

	// Evaluate thresholds
	upThresh := policy.Spec.TargetUtilization.ScaleUpThreshold
	downThresh := policy.Spec.TargetUtilization.ScaleDownThreshold
	if upThresh == 0 {
		upThresh = 75
	}
	if downThresh == 0 {
		downThresh = 5
	}

	needsUp, needsDown := utilization.EvaluateThresholds(result, upThresh, downThresh)

	if needsUp {
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:    optv1.PlanSelectedCondition,
			Status:  metav1.ConditionTrue,
			Reason:  optv1.ReasonScaleUpTriggered,
			Message: fmt.Sprintf("Utilization exceeds %d%% threshold", upThresh),
		})
		return selector.DirectionUp, result, nil
	}

	if needsDown && currentPlan != nil {
		// Run scheduling simulation before allowing downscale
		replicas := int32(1)
		if md.Spec.Replicas != nil {
			replicas = *md.Spec.Replicas
		}

		sim := scheduler.NewSimulator()
		// For simulation, we use the current plan as the candidate since we don't
		// know the target plan yet. The caller will re-evaluate after selection.
		// We simulate with a "one size smaller" approach.
		simResult := sim.SimulateDownscale(result.Nodes, *currentPlan, int(replicas))

		if simResult.Safe {
			meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
				Type:    optv1.SchedulingSimulationCondition,
				Status:  metav1.ConditionTrue,
				Reason:  optv1.ReasonScaleDownTriggered,
				Message: simResult.Message,
			})
			scalermetrics.SchedulingSimulationsTotal.WithLabelValues(clusterName, md.Name, "safe").Inc()
			return selector.DirectionDown, result, nil
		}

		// Downscale blocked by scheduling constraints
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:    optv1.SchedulingSimulationCondition,
			Status:  metav1.ConditionFalse,
			Reason:  optv1.ReasonScaleDownBlocked,
			Message: simResult.Message,
		})
		scalermetrics.SchedulingSimulationsTotal.WithLabelValues(clusterName, md.Name, "blocked").Inc()
		klog.V(2).Infof("downscale blocked for %s/%s: %s", md.Namespace, md.Name, simResult.Message)
		return selector.DirectionAny, result, nil
	}

	// Utilization is in range
	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:    optv1.UtilizationReadyCondition,
		Status:  metav1.ConditionTrue,
		Reason:  optv1.ReasonUtilizationInRange,
		Message: fmt.Sprintf("Utilization within thresholds (%d%%-%d%%)", downThresh, upThresh),
	})
	return selector.DirectionAny, result, nil
}

const defaultScaleDownStabilization = 5 * time.Minute
const defaultDrainTimeout = 5 * time.Minute
const defaultRolloutStallTimeout = 15 * time.Minute

// reconcileHorizontal checks for unschedulable pods and adjusts MachineDeployment replicas.
func (r *ScalingPolicyReconciler) reconcileHorizontal(
	ctx context.Context,
	policy *optv1.ScalingPolicy,
	md *clusterv1.MachineDeployment,
	clusterName string,
	utilResult *utilization.Result,
) error {
	if !policy.Spec.Horizontal.Enabled {
		return nil
	}

	if r.WorkloadClients == nil {
		return nil
	}

	wc, err := r.WorkloadClients.ClientForCluster(ctx, clusterName, md.Namespace)
	if err != nil {
		return fmt.Errorf("getting workload client: %w", err)
	}

	pendingPods, err := wc.ListPendingPods(ctx)
	if err != nil {
		return fmt.Errorf("listing pending pods: %w", err)
	}

	policy.Status.PendingPods = len(pendingPods)

	currentReplicas := int32(1)
	if md.Spec.Replicas != nil {
		currentReplicas = *md.Spec.Replicas
	}

	minReplicas := policy.Spec.Horizontal.MinReplicas
	if minReplicas <= 0 {
		minReplicas = 1
	}
	maxReplicas := policy.Spec.Horizontal.MaxReplicas
	if maxReplicas <= 0 {
		maxReplicas = 10
	}

	// Check if a drain is in progress from a previous reconcile.
	// If so, verify pods have been rescheduled before reducing replicas.
	if policy.Status.DrainingNode != "" {
		nodeName := policy.Status.DrainingNode

		// Check drain timeout
		if policy.Status.DrainingStartedAt != nil && time.Since(policy.Status.DrainingStartedAt.Time) > defaultDrainTimeout {
			klog.V(2).Infof("horizontal: drain timeout on %s after %s", nodeName, time.Since(policy.Status.DrainingStartedAt.Time).Round(time.Second))
			if err := wc.UncordonNode(ctx, nodeName); err != nil {
				klog.V(2).Infof("horizontal: failed to uncordon %s after timeout: %v", nodeName, err)
			}
			policy.Status.DrainingNode = ""
			policy.Status.DrainingStartedAt = nil
			meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
				Type:    optv1.HorizontalScalingCondition,
				Status:  metav1.ConditionFalse,
				Reason:  optv1.ReasonDrainTimeout,
				Message: fmt.Sprintf("Drain timeout on node %s — uncordoned and aborted", nodeName),
			})
			r.Recorder.Eventf(policy, corev1.EventTypeWarning, "DrainTimeout",
				"Drain timed out on node %s after %s — uncordoned", nodeName, defaultDrainTimeout)
			scalermetrics.DrainOperationsTotal.WithLabelValues(clusterName, md.Name, "timeout").Inc()
			return nil
		}

		remaining, err := wc.GetNonSystemPodCount(ctx, nodeName)
		if err != nil {
			klog.V(2).Infof("horizontal: error checking drain status for %s: %v", nodeName, err)
			return fmt.Errorf("checking drain status for %s: %w", nodeName, err)
		}

		if remaining > 0 {
			klog.V(2).Infof("horizontal: drain in progress on %s, %d pods remaining", nodeName, remaining)
			meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
				Type:    optv1.HorizontalScalingCondition,
				Status:  metav1.ConditionTrue,
				Reason:  optv1.ReasonDrainInProgress,
				Message: fmt.Sprintf("Drain in progress on %s (%d pods remaining)", nodeName, remaining),
			})
			return nil // requeue will check again
		}

		// Drain complete — check no new pending pods before reducing replicas
		postDrainPending, err := wc.ListPendingPods(ctx)
		if err != nil {
			return fmt.Errorf("listing pending pods after drain: %w", err)
		}
		if len(postDrainPending) > 0 {
			klog.V(2).Infof("horizontal: drain of %s complete but %d pods pending — aborting scale-down", nodeName, len(postDrainPending))
			if err := wc.UncordonNode(ctx, nodeName); err != nil {
				klog.V(2).Infof("horizontal: failed to uncordon %s after abort: %v", nodeName, err)
			}
			policy.Status.DrainingNode = ""
			policy.Status.DrainingStartedAt = nil
			meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
				Type:    optv1.HorizontalScalingCondition,
				Status:  metav1.ConditionFalse,
				Reason:  optv1.ReasonDrainAborted,
				Message: fmt.Sprintf("Drain complete on %s but %d pods pending — uncordoned and aborted", nodeName, len(postDrainPending)),
			})
			scalermetrics.DrainOperationsTotal.WithLabelValues(clusterName, md.Name, "aborted").Inc()
			return nil
		}

		// All pods rescheduled, no pending pods — safe to reduce replicas
		desired := currentReplicas - 1
		if desired < minReplicas {
			desired = minReplicas
		}

		patch := client.MergeFrom(md.DeepCopy())
		md.Spec.Replicas = &desired
		if err := r.Patch(ctx, md, patch); err != nil {
			return fmt.Errorf("patching replicas after drain: %w", err)
		}

		now := metav1.Now()
		policy.Status.LastScaleTime = &now
		policy.Status.CurrentReplicas = desired
		policy.Status.DrainingNode = ""
		policy.Status.DrainingStartedAt = nil

		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:    optv1.HorizontalScalingCondition,
			Status:  metav1.ConditionTrue,
			Reason:  optv1.ReasonScaleDownReplicas,
			Message: fmt.Sprintf("Scaled down %d→%d after draining node %s", currentReplicas, desired, nodeName),
		})

		r.Recorder.Eventf(policy, corev1.EventTypeNormal, "HorizontalScaleDown",
			"Scaled MachineDeployment %s from %d to %d replicas after draining node %s",
			md.Name, currentReplicas, desired, nodeName)
		scalermetrics.DrainOperationsTotal.WithLabelValues(clusterName, md.Name, "completed").Inc()

		klog.V(2).Infof("horizontal: scaled %s %d→%d after draining %s",
			md.Name, currentReplicas, desired, nodeName)
		return nil
	}

	// Don't scale if a previous scale operation is still in progress
	// (new machines are still provisioning — avoids feedback loop)
	//
	// Use the deprecated v1beta1 readyReplicas field because the v1beta2
	// field requires ALL conditions (including infra conditions like
	// FirewallAttached) to be True, which CAPV doesn't always set for workers.
	var readyReplicas int32
	if md.Status.Deprecated != nil && md.Status.Deprecated.V1Beta1 != nil {
		readyReplicas = md.Status.Deprecated.V1Beta1.ReadyReplicas //nolint:staticcheck // intentional: top-level ReadyReplicas requires all conditions True
	} else if md.Status.ReadyReplicas != nil {
		readyReplicas = *md.Status.ReadyReplicas
	}
	if currentReplicas > readyReplicas {
		// Check if the rollout has stalled (no progress for too long)
		if policy.Status.LastScaleTime != nil && time.Since(policy.Status.LastScaleTime.Time) > defaultRolloutStallTimeout {
			meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
				Type:    optv1.HorizontalScalingCondition,
				Status:  metav1.ConditionFalse,
				Reason:  optv1.ReasonRolloutStalled,
				Message: fmt.Sprintf("Rollout stalled: %d/%d ready for %s", readyReplicas, currentReplicas, time.Since(policy.Status.LastScaleTime.Time).Round(time.Second)),
			})
			r.Recorder.Eventf(policy, corev1.EventTypeWarning, "RolloutStalled",
				"MachineDeployment %s rollout stalled: %d/%d ready for %s",
				md.Name, readyReplicas, currentReplicas, time.Since(policy.Status.LastScaleTime.Time).Round(time.Second))
			klog.V(2).Infof("horizontal: rollout stalled for %s (%d/%d ready, last scale %s ago)",
				md.Name, readyReplicas, currentReplicas, time.Since(policy.Status.LastScaleTime.Time).Round(time.Second))
		} else {
			meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
				Type:    optv1.HorizontalScalingCondition,
				Status:  metav1.ConditionTrue,
				Reason:  optv1.ReasonRolloutInProgress,
				Message: fmt.Sprintf("Waiting for nodes to be ready (%d/%d ready)", readyReplicas, currentReplicas),
			})
			klog.V(2).Infof("horizontal: waiting for nodes %d/%d ready", readyReplicas, currentReplicas)
		}
		return nil
	}

	// Scale up: pending pods exist
	if len(pendingPods) > 0 {
		if currentReplicas >= maxReplicas {
			meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
				Type:    optv1.HorizontalScalingCondition,
				Status:  metav1.ConditionFalse,
				Reason:  optv1.ReasonMaxReplicasReached,
				Message: fmt.Sprintf("%d pending pods but already at max replicas (%d)", len(pendingPods), maxReplicas),
			})
			klog.V(2).Infof("horizontal: %d pending pods but at max replicas %d", len(pendingPods), maxReplicas)
			return nil
		}

		// Scale up by 1 at a time to avoid over-provisioning.
		// The next reconcile will add another if pods are still pending.
		desired := currentReplicas + 1
		if desired > maxReplicas {
			desired = maxReplicas
		}

		policy.Status.DesiredReplicas = desired

		if policy.Spec.DryRun {
			meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
				Type:    optv1.HorizontalScalingCondition,
				Status:  metav1.ConditionTrue,
				Reason:  optv1.ReasonDryRun,
				Message: fmt.Sprintf("[DRY RUN] Would scale up %d→%d (%d pending pods)", currentReplicas, desired, len(pendingPods)),
			})
			klog.V(2).Infof("horizontal: [DRY RUN] would scale %s %d→%d for %d pending pods",
				md.Name, currentReplicas, desired, len(pendingPods))
			return nil
		}

		// Patch MachineDeployment replicas
		patch := client.MergeFrom(md.DeepCopy())
		md.Spec.Replicas = &desired
		if err := r.Patch(ctx, md, patch); err != nil {
			return fmt.Errorf("patching replicas: %w", err)
		}

		now := metav1.Now()
		policy.Status.LastScaleTime = &now
		policy.Status.CurrentReplicas = desired

		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:    optv1.HorizontalScalingCondition,
			Status:  metav1.ConditionTrue,
			Reason:  optv1.ReasonScaleUpReplicas,
			Message: fmt.Sprintf("Scaled up %d→%d (%d pending pods)", currentReplicas, desired, len(pendingPods)),
		})

		r.Recorder.Eventf(policy, corev1.EventTypeNormal, "HorizontalScaleUp",
			"Scaled MachineDeployment %s from %d to %d replicas (%d pending pods)",
			md.Name, currentReplicas, desired, len(pendingPods))

		klog.V(2).Infof("horizontal: scaled %s %d→%d for %d pending pods",
			md.Name, currentReplicas, desired, len(pendingPods))
		return nil
	}

	// Scale down: no pending pods, check if nodes are underutilized
	if utilResult == nil || currentReplicas <= minReplicas {
		if currentReplicas <= minReplicas {
			meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
				Type:    optv1.HorizontalScalingCondition,
				Status:  metav1.ConditionTrue,
				Reason:  optv1.ReasonNoPendingPods,
				Message: fmt.Sprintf("No pending pods, at min replicas (%d)", minReplicas),
			})
		}
		return nil
	}

	// Check stabilization window
	stabilization := defaultScaleDownStabilization
	if policy.Spec.Horizontal.ScaleDownStabilization != nil {
		stabilization = policy.Spec.Horizontal.ScaleDownStabilization.Duration
	}
	if policy.Status.LastScaleTime != nil && time.Since(policy.Status.LastScaleTime.Time) < stabilization {
		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:    optv1.HorizontalScalingCondition,
			Status:  metav1.ConditionTrue,
			Reason:  optv1.ReasonStabilizationActive,
			Message: fmt.Sprintf("Within stabilization window (last scale: %s ago)", time.Since(policy.Status.LastScaleTime.Time).Round(time.Second)),
		})
		return nil
	}

	// Check if all nodes are below the scale-down threshold
	downThresh := policy.Spec.TargetUtilization.ScaleDownThreshold
	if downThresh == 0 {
		downThresh = 5
	}

	cpuPct := utilization.EffectiveCPUPercent(utilResult)
	memPct := utilization.EffectiveMemoryPercent(utilResult)

	if cpuPct < downThresh && memPct < downThresh && currentReplicas > minReplicas {
		desired := currentReplicas - 1
		if desired < minReplicas {
			desired = minReplicas
		}

		// Simulate bin-packing: verify all non-system pods fit on N-1 nodes
		// before actually scaling down. Construct a synthetic plan from the
		// first node's allocatable resources.
		if len(utilResult.Nodes) > 0 {
			n := utilResult.Nodes[0]
			syntheticPlan := vpsie.Plan{
				CPU: int((n.AllocatableCPU + 100) / 1000),
				RAM: int((n.AllocatableRAM + 256*1024*1024) / (1024 * 1024)),
			}

			sim := scheduler.NewSimulator()
			simResult := sim.SimulateDownscale(utilResult.Nodes, syntheticPlan, int(desired))
			if !simResult.Safe {
				meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
					Type:    optv1.HorizontalScalingCondition,
					Status:  metav1.ConditionTrue,
					Reason:  optv1.ReasonNoPendingPods,
					Message: fmt.Sprintf("Scale-down blocked: pods don't fit on %d nodes (%s)", desired, simResult.Message),
				})
				klog.V(2).Infof("horizontal: scale-down %d→%d blocked by bin-packing: %s",
					currentReplicas, desired, simResult.Message)
				return nil
			}
			klog.V(2).Infof("horizontal: bin-packing safe for %d→%d: %s",
				currentReplicas, desired, simResult.Message)
		}

		policy.Status.DesiredReplicas = desired

		if policy.Spec.DryRun {
			meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
				Type:    optv1.HorizontalScalingCondition,
				Status:  metav1.ConditionTrue,
				Reason:  optv1.ReasonDryRun,
				Message: fmt.Sprintf("[DRY RUN] Would scale down %d→%d (CPU: %d%%, Mem: %d%%)", currentReplicas, desired, cpuPct, memPct),
			})
			return nil
		}

		// Multi-phase scale-down: cordon → drain → verify → reduce replicas
		// Pick the least-utilized node as the drain target.
		targetNode := r.leastUtilizedNode(utilResult)
		if targetNode == "" {
			klog.V(2).Infof("horizontal: no target node for scale-down")
			return nil
		}

		// Phase 1: Cordon the target node
		if err := wc.CordonNode(ctx, targetNode); err != nil {
			return fmt.Errorf("cordoning node %s: %w", targetNode, err)
		}

		// Phase 2: Drain the target node (evict pods)
		evicted, err := wc.DrainNode(ctx, targetNode)
		if err != nil {
			return fmt.Errorf("draining node %s: %w", targetNode, err)
		}
		klog.V(2).Infof("horizontal: drained node %s, evicted %d pods", targetNode, evicted)

		// Track the draining node so next reconcile can verify completion
		policy.Status.DrainingNode = targetNode
		now := metav1.Now()
		policy.Status.DrainingStartedAt = &now
		scalermetrics.DrainOperationsTotal.WithLabelValues(clusterName, md.Name, "started").Inc()

		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:    optv1.HorizontalScalingCondition,
			Status:  metav1.ConditionTrue,
			Reason:  optv1.ReasonScaleDownReplicas,
			Message: fmt.Sprintf("Draining node %s (evicted %d pods), waiting for pods to reschedule", targetNode, evicted),
		})

		r.Recorder.Eventf(policy, corev1.EventTypeNormal, "HorizontalDrain",
			"Draining node %s for scale-down %d→%d (evicted %d pods)",
			targetNode, currentReplicas, desired, evicted)
		return nil
	}

	meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:    optv1.HorizontalScalingCondition,
		Status:  metav1.ConditionTrue,
		Reason:  optv1.ReasonNoPendingPods,
		Message: "No pending pods, utilization within range",
	})
	return nil
}

// leastUtilizedNode returns the name of the node with the lowest combined
// CPU+memory utilization (by requests). Used to pick the drain target for
// scale-down, minimizing disruption.
func (r *ScalingPolicyReconciler) leastUtilizedNode(utilResult *utilization.Result) string {
	if utilResult == nil || len(utilResult.Nodes) == 0 {
		return ""
	}

	bestIdx := 0
	bestScore := float64(2) // > max possible (1.0 + 1.0)
	for i, n := range utilResult.Nodes {
		var cpuRatio, memRatio float64
		if n.AllocatableCPU > 0 {
			cpuRatio = float64(n.RequestedCPU) / float64(n.AllocatableCPU)
		}
		if n.AllocatableRAM > 0 {
			memRatio = float64(n.RequestedRAM) / float64(n.AllocatableRAM)
		}
		score := cpuRatio + memRatio
		if score < bestScore {
			bestScore = score
			bestIdx = i
		}
	}
	return utilResult.Nodes[bestIdx].NodeName
}
