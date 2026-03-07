package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	PlanSelectionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vpsie_scaler_plan_selections_total",
			Help: "Total number of plan selection decisions",
		},
		[]string{"cluster", "machinedeployment", "plan", "action"},
	)

	RebalancingOperationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vpsie_scaler_rebalancing_operations_total",
			Help: "Total number of rebalancing operations",
		},
		[]string{"cluster", "machinedeployment", "result"},
	)

	MonthlyCostSavings = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vpsie_scaler_monthly_cost_savings_dollars",
			Help: "Estimated monthly cost savings in dollars",
		},
		[]string{"cluster", "machinedeployment"},
	)

	CurrentPlanPrice = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vpsie_scaler_current_plan_price_dollars",
			Help: "Current plan monthly price in dollars",
		},
		[]string{"cluster", "machinedeployment", "plan"},
	)

	PricingCacheRefreshDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "vpsie_scaler_pricing_cache_refresh_duration_seconds",
			Help:    "Duration of pricing cache refresh operations",
			Buckets: prometheus.DefBuckets,
		},
	)

	NodeCPUUtilizationPercent = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vpsie_scaler_node_cpu_utilization_percent",
			Help: "Observed CPU utilization percentage for workload cluster nodes",
		},
		[]string{"cluster", "machinedeployment", "source"},
	)

	NodeMemoryUtilizationPercent = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vpsie_scaler_node_memory_utilization_percent",
			Help: "Observed memory utilization percentage for workload cluster nodes",
		},
		[]string{"cluster", "machinedeployment", "source"},
	)

	SchedulingSimulationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vpsie_scaler_scheduling_simulations_total",
			Help: "Total number of scheduling simulations performed",
		},
		[]string{"cluster", "machinedeployment", "result"},
	)

	// DrainOperationsTotal counts drain operations by result.
	DrainOperationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vpsie_scaler_drain_operations_total",
			Help: "Total number of node drain operations by result",
		},
		[]string{"cluster", "machinedeployment", "result"},
	)

	// NodePoolOperationsTotal counts satellite node pool operations.
	NodePoolOperationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vpsie_scaler_nodepool_operations_total",
			Help: "Total number of satellite node pool operations by action",
		},
		[]string{"cluster", "machinedeployment", "action"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		PlanSelectionsTotal,
		RebalancingOperationsTotal,
		MonthlyCostSavings,
		CurrentPlanPrice,
		PricingCacheRefreshDuration,
		NodeCPUUtilizationPercent,
		NodeMemoryUtilizationPercent,
		SchedulingSimulationsTotal,
		DrainOperationsTotal,
		NodePoolOperationsTotal,
	)
}
