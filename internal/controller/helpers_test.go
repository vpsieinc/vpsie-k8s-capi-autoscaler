package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/vpsieinc/cluster-api-provider-vpsie/api/v1alpha1"

	optv1 "github.com/vpsieinc/vpsie-cluster-scaler/api/v1alpha1"
	"github.com/vpsieinc/vpsie-cluster-scaler/internal/vpsie"
)

// --- Fake pricing client for controller tests ---

type fakePricingClientCtrl struct {
	categories []vpsie.PlanCategory
	plans      map[string][]vpsie.Plan
}

func newFakePricingClientCtrl() *fakePricingClientCtrl {
	return &fakePricingClientCtrl{
		categories: []vpsie.PlanCategory{
			{Identifier: "cat-shared", Name: "Shared CPU"},
		},
		plans: map[string][]vpsie.Plan{
			"cat-shared": {
				{Identifier: "plan-small", Nickname: "s-1vcpu-2gb", CPU: 1, RAM: 2048, SSD: 50, PriceMonthly: 12.0, CategoryID: "cat-shared"},
				{Identifier: "plan-medium", Nickname: "s-2vcpu-4gb", CPU: 2, RAM: 4096, SSD: 80, PriceMonthly: 24.0, CategoryID: "cat-shared"},
				{Identifier: "plan-large", Nickname: "s-4vcpu-8gb", CPU: 4, RAM: 8192, SSD: 160, PriceMonthly: 48.0, CategoryID: "cat-shared"},
			},
		},
	}
}

func (f *fakePricingClientCtrl) FetchCategories(_ context.Context) ([]vpsie.PlanCategory, error) {
	return f.categories, nil
}

func (f *fakePricingClientCtrl) FetchPlans(_ context.Context, _, _ string, catID string) ([]vpsie.Plan, error) {
	return f.plans[catID], nil
}

// --- Test object constructors ---

func newTestNamespace(t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-",
		},
	}
	if err := k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create test namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = k8sClient.Delete(ctx, ns)
	})
	return ns.Name
}

func newCredentialsSecret(t *testing.T, namespace, name string) {
	t.Helper()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"apiKey": []byte("test-api-key"),
		},
	}
	if err := k8sClient.Create(ctx, secret); err != nil {
		t.Fatalf("failed to create credentials secret: %v", err)
	}
}

// scalingPolicyTestObjects groups the objects for a ScalingPolicy test.
type scalingPolicyTestObjects struct {
	Namespace    string
	Cluster      *clusterv1.Cluster
	VPSieCluster *infrav1.VPSieCluster
	MD           *clusterv1.MachineDeployment
	Template     *infrav1.VPSieMachineTemplate
	Policy       *optv1.ScalingPolicy
}

// newScalingPolicyTestObjects creates a full set of test objects for controller tests.
func newScalingPolicyTestObjects(t *testing.T, dryRun bool) scalingPolicyTestObjects {
	t.Helper()
	ns := newTestNamespace(t)
	clusterName := "test-cluster"

	// Credentials secret
	newCredentialsSecret(t, ns, clusterName+"-credentials")

	// VPSieCluster
	vpsieCluster := &infrav1.VPSieCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName,
			Namespace: ns,
		},
		Spec: infrav1.VPSieClusterSpec{
			DCIdentifier:      "dc-test-1",
			ProjectIdentifier: "proj-uuid-123",
			ProjectID:         "123",
			ControlPlaneEndpoint: clusterv1.APIEndpoint{
				Host: "10.0.0.1",
				Port: 6443,
			},
			CredentialsRef: infrav1.VPSieCredentialsRef{
				Name: clusterName + "-credentials",
			},
			Network: infrav1.VPSieNetworkSpec{
				NetworkRange: "10.0.0.0",
				NetworkSize:  "24",
			},
		},
	}
	if err := k8sClient.Create(ctx, vpsieCluster); err != nil {
		t.Fatalf("failed to create VPSieCluster: %v", err)
	}

	// CAPI Cluster
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName,
			Namespace: ns,
		},
		Spec: clusterv1.ClusterSpec{
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: infrav1.GroupVersion.Group,
				Kind:     "VPSieCluster",
				Name:     clusterName,
			},
		},
	}
	if err := k8sClient.Create(ctx, cluster); err != nil {
		t.Fatalf("failed to create Cluster: %v", err)
	}

	// VPSieMachineTemplate
	tmpl := &infrav1.VPSieMachineTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "workers-template",
			Namespace: ns,
		},
		Spec: infrav1.VPSieMachineTemplateSpec{
			Template: infrav1.VPSieMachineTemplateResource{
				Spec: infrav1.VPSieMachineSpec{
					ResourceIdentifier: "plan-large", // currently using expensive plan
					DCIdentifier:       "dc-test-1",
					ImageIdentifier:    "img-talos-123",
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, tmpl); err != nil {
		t.Fatalf("failed to create VPSieMachineTemplate: %v", err)
	}

	// MachineDeployment
	replicas := int32(3)
	bootstrapSecretName := "workers-bootstrap"
	md := &clusterv1.MachineDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "workers",
			Namespace: ns,
			Labels: map[string]string{
				"cluster.x-k8s.io/cluster-name": clusterName,
			},
		},
		Spec: clusterv1.MachineDeploymentSpec{
			ClusterName: clusterName,
			Replicas:    &replicas,
			Selector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"cluster.x-k8s.io/cluster-name": clusterName,
				},
			},
			Template: clusterv1.MachineTemplateSpec{
				Spec: clusterv1.MachineSpec{
					ClusterName: clusterName,
					Bootstrap: clusterv1.Bootstrap{
						DataSecretName: &bootstrapSecretName,
					},
					InfrastructureRef: clusterv1.ContractVersionedObjectReference{
						APIGroup: infrav1.GroupVersion.Group,
						Kind:     "VPSieMachineTemplate",
						Name:     "workers-template",
					},
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, md); err != nil {
		t.Fatalf("failed to create MachineDeployment: %v", err)
	}

	// ScalingPolicy
	policy := &optv1.ScalingPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-policy",
			Namespace: ns,
		},
		Spec: optv1.ScalingPolicySpec{
			TargetRef: optv1.ObjectReference{
				Name: "workers",
			},
			CredentialsRef: &optv1.CredentialsRef{
				Name: clusterName + "-credentials",
			},
			DCIdentifier:   "dc-test-1",
			OSIdentifier:   "os-talos-1",
			Aggressiveness: optv1.AggressivenessModerate,
			DryRun:         dryRun,
			Constraints: optv1.ResourceConstraints{
				MinCPU: 1, MaxCPU: 32,
				MinRAM: 1024, MaxRAM: 131072,
				MinSSD: 20,
			},
			Rebalancing: optv1.RebalancingSpec{
				MinSavingsPercent: 15,
			},
		},
	}
	if err := k8sClient.Create(ctx, policy); err != nil {
		t.Fatalf("failed to create ScalingPolicy: %v", err)
	}

	return scalingPolicyTestObjects{
		Namespace:    ns,
		Cluster:      cluster,
		VPSieCluster: vpsieCluster,
		MD:           md,
		Template:     tmpl,
		Policy:       policy,
	}
}

// refreshObject re-fetches an object from the API server.
func refreshObject(t *testing.T, obj client.Object) {
	t.Helper()
	key := types.NamespacedName{
		Namespace: obj.GetNamespace(),
		Name:      obj.GetName(),
	}
	if err := k8sClient.Get(ctx, key, obj); err != nil {
		t.Fatalf("failed to refresh %T %s: %v", obj, key, err)
	}
}
