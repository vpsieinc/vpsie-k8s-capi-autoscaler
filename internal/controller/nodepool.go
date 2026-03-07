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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/vpsieinc/cluster-api-provider-vpsie/api/v1alpha1"

	optv1 "github.com/vpsieinc/vpsie-cluster-scaler/api/v1alpha1"
	scalermetrics "github.com/vpsieinc/vpsie-cluster-scaler/internal/metrics"
	"github.com/vpsieinc/vpsie-cluster-scaler/internal/pricing"
	"github.com/vpsieinc/vpsie-cluster-scaler/internal/vpsie"
	"github.com/vpsieinc/vpsie-cluster-scaler/internal/workload"
)

const (
	// satelliteLabel identifies a satellite MachineDeployment and stores its base MD name.
	satelliteLabel = "optimization.vpsie.com/satellite-of"

	defaultMaxPools              = 3
	defaultNodePoolScaleDownDelay = 10 * time.Minute
)

// satelliteMDName returns the name for a satellite MachineDeployment.
func satelliteMDName(baseName, planNickname string) string {
	normalized := strings.ToLower(strings.ReplaceAll(planNickname, " ", "-"))
	return fmt.Sprintf("%s-pool-%s", baseName, normalized)
}

// podResourceRequests returns the total CPU (millis) and memory (bytes) requests
// for a pod's regular containers.
func podResourceRequests(pod *corev1.Pod) (cpuMillis int64, ramBytes int64) {
	for _, c := range pod.Spec.Containers {
		cpuMillis += c.Resources.Requests.Cpu().MilliValue()
		ramBytes += c.Resources.Requests.Memory().Value()
	}
	return
}

// podFitsCurrentPlan returns true if the pod's resource requests can be
// accommodated by a single node running the given plan.
func podFitsCurrentPlan(pod *corev1.Pod, plan *vpsie.Plan) bool {
	cpuMillis, ramBytes := podResourceRequests(pod)
	allocCPU, allocRAM := pricing.PlanCapacity(*plan)
	ramMB := ramBytes / (1024 * 1024)
	return int(cpuMillis) <= allocCPU && int(ramMB) <= allocRAM
}

// findCheapestFittingPlan finds the cheapest plan that can accommodate
// the given resource requirements while respecting constraints.
func findCheapestFittingPlan(plans []vpsie.Plan, cpuMillis, ramMB int, constraints optv1.ResourceConstraints) *vpsie.Plan {
	excluded := make(map[string]bool, len(constraints.ExcludedPlans))
	for _, id := range constraints.ExcludedPlans {
		excluded[id] = true
	}

	var best *vpsie.Plan
	for i := range plans {
		p := &plans[i]
		if excluded[p.Identifier] {
			continue
		}
		if p.CPU < constraints.MinCPU || p.CPU > constraints.MaxCPU {
			continue
		}
		if p.RAM < constraints.MinRAM || p.RAM > constraints.MaxRAM {
			continue
		}
		if p.SSD < constraints.MinSSD {
			continue
		}
		allocCPU, allocRAM := pricing.PlanCapacity(*p)
		if allocCPU < cpuMillis || allocRAM < ramMB {
			continue
		}
		if best == nil || p.PriceMonthly < best.PriceMonthly {
			best = p
		}
	}
	return best
}

// reconcileNodePools manages satellite MachineDeployments for pods that don't fit
// on the base plan. It detects oversized pending pods, creates satellite MDs with
// bigger plans, scales them up as needed, and cleans up empty satellites.
func (r *ScalingPolicyReconciler) reconcileNodePools(
	ctx context.Context,
	policy *optv1.ScalingPolicy,
	md *clusterv1.MachineDeployment,
	cache *pricing.Cache,
	clusterName string,
	currentPlan *vpsie.Plan,
	currentTemplate *infrav1.VPSieMachineTemplate,
) error {
	if policy.Spec.NodePoolPolicy == nil || !policy.Spec.NodePoolPolicy.Enabled {
		return nil
	}
	if currentPlan == nil || currentTemplate == nil {
		return nil
	}
	if r.WorkloadClients == nil {
		return nil
	}

	wc, err := r.WorkloadClients.ClientForCluster(ctx, clusterName, md.Namespace)
	if err != nil {
		return fmt.Errorf("getting workload client for node pools: %w", err)
	}

	pendingPods, err := wc.ListPendingPods(ctx)
	if err != nil {
		return fmt.Errorf("listing pending pods for node pools: %w", err)
	}

	// Find pods that don't fit on current plan
	var oversizedPods []corev1.Pod
	for i := range pendingPods {
		if !podFitsCurrentPlan(&pendingPods[i], currentPlan) {
			oversizedPods = append(oversizedPods, pendingPods[i])
		}
	}

	plans := cache.Plans(policy.Spec.AllowedCategories)

	maxPools := defaultMaxPools
	if policy.Spec.NodePoolPolicy.MaxPools > 0 {
		maxPools = policy.Spec.NodePoolPolicy.MaxPools
	}

	// Group oversized pods by the cheapest plan that fits them.
	type planGroup struct {
		plan *vpsie.Plan
		pods []corev1.Pod
	}
	groups := make(map[string]*planGroup)

	for i := range oversizedPods {
		pod := &oversizedPods[i]
		cpuMillis, ramBytes := podResourceRequests(pod)
		ramMB := int(ramBytes / (1024 * 1024))

		plan := findCheapestFittingPlan(plans, int(cpuMillis), ramMB, policy.Spec.Constraints)
		if plan == nil {
			klog.V(2).Infof("nodepool: no plan can fit pod %s/%s (cpu=%dm, mem=%dMB)",
				pod.Namespace, pod.Name, cpuMillis, ramMB)
			continue
		}

		if _, ok := groups[plan.Identifier]; !ok {
			groups[plan.Identifier] = &planGroup{plan: plan}
		}
		groups[plan.Identifier].pods = append(groups[plan.Identifier].pods, *pod)
	}

	// Create or scale satellite MDs for each plan tier.
	for _, g := range groups {
		poolName := satelliteMDName(md.Name, g.plan.Nickname)

		var satelliteMD clusterv1.MachineDeployment
		err := r.Get(ctx, types.NamespacedName{Namespace: md.Namespace, Name: poolName}, &satelliteMD)

		if apierrors.IsNotFound(err) {
			// Check max pools
			if countActivePools(policy.Status.NodePools) >= maxPools {
				klog.V(2).Infof("nodepool: max pools (%d) reached, cannot create %s", maxPools, poolName)
				meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
					Type:    optv1.NodePoolCondition,
					Status:  metav1.ConditionFalse,
					Reason:  optv1.ReasonMaxPoolsReached,
					Message: fmt.Sprintf("Cannot create pool for plan %s: max pools (%d) reached", g.plan.Nickname, maxPools),
				})
				continue
			}

			if policy.Spec.DryRun {
				klog.V(2).Infof("nodepool: [DRY RUN] would create satellite %s with plan %s for %d pods",
					poolName, g.plan.Nickname, len(g.pods))
				r.Recorder.Eventf(policy, corev1.EventTypeNormal, "DryRun",
					"[DRY RUN] Would create satellite pool %s with plan %s for %d oversized pods",
					poolName, g.plan.Nickname, len(g.pods))
				continue
			}

			if err := r.createSatelliteMD(ctx, policy, md, currentTemplate, g.plan); err != nil {
				klog.V(2).Infof("nodepool: failed to create satellite %s: %v", poolName, err)
				r.Recorder.Eventf(policy, corev1.EventTypeWarning, "NodePoolCreateFailed",
					"Failed to create satellite pool %s: %v", poolName, err)
				continue
			}

			r.Recorder.Eventf(policy, corev1.EventTypeNormal, "NodePoolCreated",
				"Created satellite pool %s with plan %s (%d vCPU, %d MB RAM) for %d oversized pods",
				poolName, g.plan.Nickname, g.plan.CPU, g.plan.RAM, len(g.pods))
			scalermetrics.NodePoolOperationsTotal.WithLabelValues(clusterName, md.Name, "created").Inc()

			now := metav1.Now()
			policy.Status.NodePools = append(policy.Status.NodePools, optv1.NodePoolStatus{
				Name:         poolName,
				PlanID:       g.plan.Identifier,
				PlanNickname: g.plan.Nickname,
				Replicas:     1,
				LastPodSeen:  &now,
			})

			meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
				Type:    optv1.NodePoolCondition,
				Status:  metav1.ConditionTrue,
				Reason:  optv1.ReasonNodePoolCreated,
				Message: fmt.Sprintf("Created satellite pool %s with plan %s", poolName, g.plan.Nickname),
			})
			continue
		}

		if err != nil {
			klog.V(2).Infof("nodepool: error checking satellite %s: %v", poolName, err)
			continue
		}

		// Satellite exists — check if it needs scale-up.
		if policy.Spec.DryRun {
			continue
		}

		satelliteReplicas := int32(1)
		if satelliteMD.Spec.Replicas != nil {
			satelliteReplicas = *satelliteMD.Spec.Replicas
		}

		// Don't scale up if rollout is in progress.
		var readyReplicas int32
		if satelliteMD.Status.Deprecated != nil && satelliteMD.Status.Deprecated.V1Beta1 != nil {
			readyReplicas = satelliteMD.Status.Deprecated.V1Beta1.ReadyReplicas //nolint:staticcheck
		} else if satelliteMD.Status.ReadyReplicas != nil {
			readyReplicas = *satelliteMD.Status.ReadyReplicas
		}

		if satelliteReplicas > readyReplicas {
			klog.V(2).Infof("nodepool: satellite %s rollout in progress (%d/%d ready)",
				poolName, readyReplicas, satelliteReplicas)
			continue
		}

		maxR := policy.Spec.Horizontal.MaxReplicas
		if maxR <= 0 {
			maxR = 10
		}
		if satelliteReplicas >= maxR {
			klog.V(2).Infof("nodepool: satellite %s at max replicas %d", poolName, maxR)
			continue
		}

		desired := satelliteReplicas + 1
		patch := client.MergeFrom(satelliteMD.DeepCopy())
		satelliteMD.Spec.Replicas = &desired
		if err := r.Patch(ctx, &satelliteMD, patch); err != nil {
			klog.V(2).Infof("nodepool: failed to scale satellite %s: %v", poolName, err)
			continue
		}

		klog.V(2).Infof("nodepool: scaled satellite %s %d→%d for %d oversized pods",
			poolName, satelliteReplicas, desired, len(g.pods))
		r.Recorder.Eventf(policy, corev1.EventTypeNormal, "NodePoolScaleUp",
			"Scaled satellite pool %s from %d to %d replicas", poolName, satelliteReplicas, desired)

		updateNodePoolReplicas(policy, poolName, desired)

		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:    optv1.NodePoolCondition,
			Status:  metav1.ConditionTrue,
			Reason:  optv1.ReasonNodePoolScaleUp,
			Message: fmt.Sprintf("Scaled satellite %s to %d replicas", poolName, desired),
		})
	}

	// Cleanup empty satellite pools.
	return r.cleanupNodePools(ctx, policy, clusterName, md.Namespace, wc)
}

// createSatelliteMD creates a satellite MachineDeployment and VPSieMachineTemplate
// for an oversized plan. It clones the base MD's bootstrap config and DC/image settings.
func (r *ScalingPolicyReconciler) createSatelliteMD(
	ctx context.Context,
	policy *optv1.ScalingPolicy,
	baseMD *clusterv1.MachineDeployment,
	baseTemplate *infrav1.VPSieMachineTemplate,
	plan *vpsie.Plan,
) error {
	poolName := satelliteMDName(baseMD.Name, plan.Nickname)

	// Create VPSieMachineTemplate for the satellite plan.
	tmpl := &infrav1.VPSieMachineTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      poolName,
			Namespace: baseMD.Namespace,
			Labels: map[string]string{
				satelliteLabel: baseMD.Name,
			},
		},
		Spec: infrav1.VPSieMachineTemplateSpec{
			Template: infrav1.VPSieMachineTemplateResource{
				Spec: infrav1.VPSieMachineSpec{
					ResourceIdentifier: plan.Identifier,
					DCIdentifier:       baseTemplate.Spec.Template.Spec.DCIdentifier,
					ImageIdentifier:    baseTemplate.Spec.Template.Spec.ImageIdentifier,
					AdditionalTags:     baseTemplate.Spec.Template.Spec.AdditionalTags,
					ServerGroup:        baseTemplate.Spec.Template.Spec.ServerGroup,
				},
			},
		},
	}

	var existingTmpl infrav1.VPSieMachineTemplate
	if err := r.Get(ctx, types.NamespacedName{Namespace: tmpl.Namespace, Name: tmpl.Name}, &existingTmpl); err != nil {
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("checking satellite template %s: %w", tmpl.Name, err)
		}
		if err := r.Create(ctx, tmpl); err != nil {
			return fmt.Errorf("creating satellite template %s: %w", tmpl.Name, err)
		}
		klog.V(2).Infof("nodepool: created VPSieMachineTemplate %s with plan %s", tmpl.Name, plan.Nickname)
	}

	// Create satellite MachineDeployment.
	replicas := int32(1)
	clusterName := baseMD.Labels["cluster.x-k8s.io/cluster-name"]

	satelliteMD := &clusterv1.MachineDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      poolName,
			Namespace: baseMD.Namespace,
			Labels: map[string]string{
				"cluster.x-k8s.io/cluster-name": clusterName,
				satelliteLabel:                  baseMD.Name,
			},
		},
		Spec: clusterv1.MachineDeploymentSpec{
			ClusterName: baseMD.Spec.ClusterName,
			Replicas:    &replicas,
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"cluster.x-k8s.io/cluster-name": clusterName,
				},
			},
			Template: clusterv1.MachineTemplateSpec{
				Spec: clusterv1.MachineSpec{
					ClusterName: baseMD.Spec.ClusterName,
					Bootstrap:   *baseMD.Spec.Template.Spec.Bootstrap.DeepCopy(),
					InfrastructureRef: clusterv1.ContractVersionedObjectReference{
						APIGroup: baseMD.Spec.Template.Spec.InfrastructureRef.APIGroup,
						Kind:     baseMD.Spec.Template.Spec.InfrastructureRef.Kind,
						Name:     poolName,
					},
				},
			},
		},
	}

	if err := r.Create(ctx, satelliteMD); err != nil {
		return fmt.Errorf("creating satellite MachineDeployment %s: %w", poolName, err)
	}

	klog.V(2).Infof("nodepool: created satellite MachineDeployment %s with plan %s (%d vCPU, %d MB RAM)",
		poolName, plan.Nickname, plan.CPU, plan.RAM)
	return nil
}

// cleanupNodePools deletes satellite MachineDeployments that have had zero
// workload pods for longer than the configured scaleDownDelay.
func (r *ScalingPolicyReconciler) cleanupNodePools(
	ctx context.Context,
	policy *optv1.ScalingPolicy,
	clusterName string,
	namespace string,
	wc workload.WorkloadClient,
) error {
	scaleDownDelay := defaultNodePoolScaleDownDelay
	if policy.Spec.NodePoolPolicy != nil && policy.Spec.NodePoolPolicy.ScaleDownDelay != nil {
		scaleDownDelay = policy.Spec.NodePoolPolicy.ScaleDownDelay.Duration
	}

	var updatedPools []optv1.NodePoolStatus

	for _, pool := range policy.Status.NodePools {
		var satelliteMD clusterv1.MachineDeployment
		err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: pool.Name}, &satelliteMD)
		if apierrors.IsNotFound(err) {
			// MD was deleted externally, remove from status.
			continue
		}
		if err != nil {
			updatedPools = append(updatedPools, pool)
			continue
		}

		// Check if satellite has workload pods.
		nodes, err := wc.ListNodes(ctx, pool.Name)
		if err != nil || len(nodes) == 0 {
			updatedPools = append(updatedPools, pool)
			continue
		}

		totalWorkloadPods := 0
		for _, node := range nodes {
			count, err := wc.GetNonSystemPodCount(ctx, node.Name)
			if err != nil {
				klog.V(2).Infof("nodepool: failed to count pods on node %s: %v", node.Name, err)
				continue
			}
			totalWorkloadPods += count
		}

		if totalWorkloadPods > 0 {
			now := metav1.Now()
			pool.LastPodSeen = &now
			updatedPools = append(updatedPools, pool)
			continue
		}

		// No workload pods — check delay.
		if pool.LastPodSeen == nil {
			// First time seeing zero pods — start the delay timer.
			now := metav1.Now()
			pool.LastPodSeen = &now
			updatedPools = append(updatedPools, pool)
			continue
		}
		if time.Since(pool.LastPodSeen.Time) < scaleDownDelay {
			updatedPools = append(updatedPools, pool)
			continue
		}

		if policy.Spec.DryRun {
			klog.V(2).Infof("nodepool: [DRY RUN] would delete empty satellite %s", pool.Name)
			updatedPools = append(updatedPools, pool)
			continue
		}

		// Delete satellite MD and template.
		if err := r.Delete(ctx, &satelliteMD); err != nil && !apierrors.IsNotFound(err) {
			klog.V(2).Infof("nodepool: failed to delete satellite MD %s: %v", pool.Name, err)
			updatedPools = append(updatedPools, pool)
			continue
		}

		var tmpl infrav1.VPSieMachineTemplate
		if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: pool.Name}, &tmpl); err == nil {
			if err := r.Delete(ctx, &tmpl); err != nil && !apierrors.IsNotFound(err) {
				klog.V(2).Infof("nodepool: failed to delete satellite template %s: %v", pool.Name, err)
			}
		}

		klog.V(2).Infof("nodepool: deleted empty satellite %s (no pods for %s)", pool.Name, scaleDownDelay)
		r.Recorder.Eventf(policy, corev1.EventTypeNormal, "NodePoolDeleted",
			"Deleted empty satellite pool %s (plan: %s)", pool.Name, pool.PlanNickname)
		scalermetrics.NodePoolOperationsTotal.WithLabelValues(clusterName, pool.Name, "deleted").Inc()

		meta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:    optv1.NodePoolCondition,
			Status:  metav1.ConditionTrue,
			Reason:  optv1.ReasonNodePoolDeleted,
			Message: fmt.Sprintf("Deleted empty satellite pool %s", pool.Name),
		})
		// Not added to updatedPools — removes from status.
	}

	policy.Status.NodePools = updatedPools
	return nil
}

// countActivePools returns the number of pools in the status list.
func countActivePools(pools []optv1.NodePoolStatus) int {
	return len(pools)
}

// updateNodePoolReplicas updates the replica count for a pool in status.
func updateNodePoolReplicas(policy *optv1.ScalingPolicy, poolName string, replicas int32) {
	for i := range policy.Status.NodePools {
		if policy.Status.NodePools[i].Name == poolName {
			policy.Status.NodePools[i].Replicas = replicas
			now := metav1.Now()
			policy.Status.NodePools[i].LastPodSeen = &now
			return
		}
	}
}
