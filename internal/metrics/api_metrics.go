package metrics

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// APIMetrics holds Prometheus instrumentation for the API gateway layer.
type APIMetrics struct {
	HTTPRequestsTotal    *prometheus.CounterVec
	HTTPRequestDuration  *prometheus.HistogramVec
	HTTPRequestsInFlight prometheus.Gauge
	SubmissionsCreated   prometheus.Counter
	SubmissionsBatch     prometheus.Counter
	CacheHits            *prometheus.CounterVec
	CacheMisses          *prometheus.CounterVec
	QueuePublishErrors   prometheus.Counter
}

// NewAPIMetrics registers and returns API-layer Prometheus metrics.
func NewAPIMetrics(namespace string) *APIMetrics {
	if namespace == "" {
		namespace = "coderuntime"
	}
	sub := "api"

	return &APIMetrics{
		HTTPRequestsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: sub,
			Name:      "http_requests_total",
			Help:      "Total number of HTTP requests.",
		}, []string{"method", "path", "status"}),

		HTTPRequestDuration: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: sub,
			Name:      "http_request_duration_seconds",
			Help:      "HTTP request latency in seconds.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"method", "path"}),

		HTTPRequestsInFlight: promauto.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: sub,
			Name:      "http_requests_in_flight",
			Help:      "Current number of HTTP requests being served.",
		}),

		SubmissionsCreated: promauto.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: sub,
			Name:      "submissions_created_total",
			Help:      "Total number of individual submissions created.",
		}),

		SubmissionsBatch: promauto.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: sub,
			Name:      "submissions_batch_total",
			Help:      "Total number of batch submission requests.",
		}),

		CacheHits: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: sub,
			Name:      "cache_hits_total",
			Help:      "Total cache hits.",
		}, []string{"cache"}),

		CacheMisses: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: sub,
			Name:      "cache_misses_total",
			Help:      "Total cache misses.",
		}, []string{"cache"}),

		QueuePublishErrors: promauto.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: sub,
			Name:      "queue_publish_errors_total",
			Help:      "Total errors while publishing to the queue.",
		}),
	}
}

// ObserveRequest records HTTP request metrics.
func (m *APIMetrics) ObserveRequest(method, path string, statusCode int, duration time.Duration) {
	status := strconv.Itoa(statusCode)
	m.HTTPRequestsTotal.WithLabelValues(method, path, status).Inc()
	m.HTTPRequestDuration.WithLabelValues(method, path).Observe(duration.Seconds())
}
