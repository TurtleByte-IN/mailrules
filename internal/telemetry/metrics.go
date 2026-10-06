package telemetry

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics is the daemon's Prometheus registry. This file is the only place that imports
// the Prometheus client: the HTTP layer reports through ObserveHTTP and serves Handler.
// A nil *Metrics records nothing.
type Metrics struct {
	reg      *prometheus.Registry
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

// NewMetrics returns a registry with the Go runtime and process collectors, the build
// version and the HTTP request metrics.
func NewMetrics(version string) *Metrics {
	m := &Metrics{
		reg: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mailrules_http_requests_total", Help: "HTTP requests served, by method, route and status code.",
		}, []string{"method", "route", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "mailrules_http_request_duration_seconds", Help: "Time to serve an HTTP request, by method and route.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
	}
	build := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "mailrules_build_info", Help: "Always 1; the version label names the build."}, []string{"version"})
	build.WithLabelValues(version).Set(1)
	m.reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}), build, m.requests, m.duration)
	return m
}

// ObserveHTTP records one served request. route is the matched route pattern, never the
// raw path, so ids do not become label values.
func (m *Metrics) ObserveHTTP(method, route string, status int, took time.Duration) {
	if m == nil {
		return
	}
	m.requests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()
	m.duration.WithLabelValues(method, route).Observe(took.Seconds())
}

// Handler serves the registry in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{})
}
