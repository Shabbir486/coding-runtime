package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds all Prometheus metrics for the queue subsystem.
type Metrics struct {
	JobsPublished     *prometheus.CounterVec
	JobsConsumed      *prometheus.CounterVec
	JobsFailed        *prometheus.CounterVec
	JobsDLQ           *prometheus.CounterVec
	JobDuration       *prometheus.HistogramVec
	InFlightJobs      *prometheus.GaugeVec
	QueueDepth        *prometheus.GaugeVec
	RetryAttempts     *prometheus.CounterVec
	AckLatency        *prometheus.HistogramVec
}

// New creates and registers all Prometheus metrics.
func New(namespace string) *Metrics {
	if namespace == "" {
		namespace = "coderuntime"
	}

	return &Metrics{
		JobsPublished: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "queue",
			Name:      "jobs_published_total",
			Help:      "Total number of jobs published to NATS.",
		}, []string{"subject", "language_id"}),

		JobsConsumed: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "queue",
			Name:      "jobs_consumed_total",
			Help:      "Total number of jobs consumed by workers.",
		}, []string{"worker_id", "status"}),

		JobsFailed: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "queue",
			Name:      "jobs_failed_total",
			Help:      "Total number of jobs that failed processing.",
		}, []string{"worker_id", "failure_type"}),

		JobsDLQ: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "queue",
			Name:      "jobs_dlq_total",
			Help:      "Total number of jobs sent to the dead-letter queue.",
		}, []string{"reason"}),

		JobDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: "queue",
			Name:      "job_duration_seconds",
			Help:      "Duration of job execution in seconds.",
			Buckets:   []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300},
		}, []string{"worker_id", "language_id"}),

		InFlightJobs: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: "queue",
			Name:      "in_flight_jobs",
			Help:      "Current number of jobs being processed.",
		}, []string{"worker_id"}),

		QueueDepth: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: "queue",
			Name:      "depth",
			Help:      "Approximate number of messages pending in each stream.",
		}, []string{"stream"}),

		RetryAttempts: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: "queue",
			Name:      "retry_attempts_total",
			Help:      "Total number of job retry attempts.",
		}, []string{"worker_id"}),

		AckLatency: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: "queue",
			Name:      "ack_latency_seconds",
			Help:      "Time between message delivery and acknowledgement.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"worker_id"}),
	}
}
