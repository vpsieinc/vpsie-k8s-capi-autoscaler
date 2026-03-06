package main

import (
	"flag"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/textlogger"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"

	infrav1 "github.com/vpsieinc/cluster-api-provider-vpsie/api/v1alpha1"

	optv1 "github.com/vpsieinc/vpsie-cluster-scaler/api/v1alpha1"
	scalercontroller "github.com/vpsieinc/vpsie-cluster-scaler/internal/controller"
	_ "github.com/vpsieinc/vpsie-cluster-scaler/internal/metrics" // register metrics
	"github.com/vpsieinc/vpsie-cluster-scaler/internal/workload"
)

var (
	scheme = runtime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(clusterv1.AddToScheme(scheme))
	utilruntime.Must(infrav1.AddToScheme(scheme))
	utilruntime.Must(optv1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr          string
		healthProbeAddr      string
		enableLeaderElection bool
		leaderElectionID     string
		rebalanceInterval    time.Duration
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8082",
		"The address the metric endpoint binds to.")
	flag.StringVar(&healthProbeAddr, "health-probe-bind-address", ":8083",
		"The address the health probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager.")
	flag.StringVar(&leaderElectionID, "leader-election-id", "vpsie-scaler-leader-election",
		"The name of the resource used for leader election.")
	flag.DurationVar(&rebalanceInterval, "rebalance-interval", 5*time.Minute,
		"Interval between rebalancing cycles.")

	klog.InitFlags(nil)
	flag.Parse()

	ctrl.SetLogger(textlogger.NewLogger(textlogger.NewConfig()))

	klog.Infof("starting vpsie-cluster-scaler controller manager")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: healthProbeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       leaderElectionID,
	})
	if err != nil {
		klog.Fatalf("unable to create manager: %v", err)
	}

	ctx := ctrl.SetupSignalHandler()

	// Set up the workload client factory for utilization-aware scaling.
	wcFactory := workload.NewCAPIClientFactory(mgr.GetClient())

	// Set up the ScalingPolicy controller.
	reconciler := &scalercontroller.ScalingPolicyReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		Recorder:        mgr.GetEventRecorderFor("vpsie-cluster-scaler"),
		WorkloadClients: wcFactory,
	}
	if err := reconciler.SetupWithManager(ctx, mgr); err != nil {
		klog.Fatalf("unable to create ScalingPolicy controller: %v", err)
	}

	// Set up the rebalancer as a background runnable.
	rebalancer := scalercontroller.NewRebalancer(mgr.GetClient(), reconciler, rebalanceInterval)
	if err := mgr.Add(rebalancer); err != nil {
		klog.Fatalf("unable to add rebalancer: %v", err)
	}

	// Health checks.
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		klog.Fatalf("unable to set up health check: %v", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		klog.Fatalf("unable to set up ready check: %v", err)
	}

	klog.Info("starting controller manager")
	if err := mgr.Start(ctx); err != nil {
		klog.Fatalf("controller manager exited with error: %v", err)
	}
}
