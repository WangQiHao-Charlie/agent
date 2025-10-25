package agent

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	actionDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "action_duration_seconds",
		Help:    "Duration of actions in seconds",
		Buckets: prometheus.DefBuckets,
	}, []string{"runtime", "instruction"})

	actionSuccess = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "action_success_total",
		Help: "Total actions by success and exit code",
	}, []string{"runtime", "instruction", "exit_code", "success"})

	activeNodeactions = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "active_nodeactions",
		Help: "Active nodeactions by runtime and instruction",
	}, []string{"runtime", "instruction"})
)

func init() {
	prometheus.MustRegister(actionDuration, actionSuccess, activeNodeactions)
}

func MetricsHandler() http.Handler {
	return promhttp.Handler()
}
