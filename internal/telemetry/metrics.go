package telemetry

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Metrics struct {
	handler   http.Handler
	requests  *prometheus.CounterVec
	latency   *prometheus.HistogramVec
	available *prometheus.GaugeVec
	bytes     *prometheus.CounterVec
	active    *prometheus.GaugeVec
	transfers *prometheus.CounterVec
	failures  *prometheus.CounterVec
	retries   *prometheus.CounterVec
}

func New() *Metrics {
	registry := prometheus.NewRegistry()
	m := &Metrics{
		handler: promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cdn_manager_http_requests_total", Help: "HTTP requests by registered route and response status.",
		}, []string{"route", "method", "status"}),
		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "cdn_manager_http_request_duration_seconds", Help: "Seconds spent serving HTTP requests.",
		}, []string{"route", "method"}),
		available: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cdn_manager_source_available", Help: "Whether a storage source passed its latest health check.",
		}, []string{"source"}),
		bytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cdn_manager_transfer_bytes_total", Help: "Bytes transferred through the manager.",
		}, []string{"source", "direction"}),
		active: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cdn_manager_transfers_active", Help: "Transfers currently in progress.",
		}, []string{"source", "direction"}),
		transfers: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cdn_manager_transfers_total", Help: "Finished transfers by direction and outcome.",
		}, []string{"source", "direction", "outcome"}),
		failures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cdn_manager_s3_errors_total", Help: "S3 operations that ended in an error.",
		}, []string{"source", "operation"}),
		retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cdn_manager_s3_retries_total", Help: "Additional attempts made by the S3 request retryer.",
		}, []string{"source", "operation"}),
	}
	registry.MustRegister(m.requests, m.latency, m.available, m.bytes, m.active, m.transfers, m.failures, m.retries,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return m
}

func (m *Metrics) Handler() http.Handler { return m.handler }

func (m *Metrics) ObserveHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		defer func() {
			status := wrapped.Status()
			if status == 0 {
				status = http.StatusOK
			}
			route := chi.RouteContext(r.Context()).RoutePattern()
			if route == "" {
				route = "unmatched"
			}
			method := r.Method
			switch method {
			case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions:
			default:
				method = "OTHER"
			}
			m.requests.WithLabelValues(route, method, strconv.Itoa(status)).Inc()
			m.latency.WithLabelValues(route, method).Observe(time.Since(start).Seconds())
		}()
		next.ServeHTTP(wrapped, r)
	})
}

func (m *Metrics) SetHealth(source, status string) {
	if status == "healthy" {
		m.available.WithLabelValues(source).Set(1)
	} else {
		m.available.WithLabelValues(source).Set(0)
	}
}

func (m *Metrics) ObserveS3(source, operation string, retries int, err error) {
	m.retries.WithLabelValues(source, operation).Add(float64(retries))
	failures := m.failures.WithLabelValues(source, operation)
	if err != nil {
		failures.Inc()
	}
}

func (m *Metrics) BeginTransfer(source, direction string) func(int64, error) {
	active := m.active.WithLabelValues(source, direction)
	active.Inc()
	return func(size int64, err error) {
		active.Dec()
		outcome := "success"
		if err != nil {
			outcome = "error"
		}
		m.bytes.WithLabelValues(source, direction).Add(float64(max(size, 0)))
		m.transfers.WithLabelValues(source, direction, outcome).Inc()
	}
}
