// Package telemetrycore holds the transport-agnostic, Fiber-free telemetry
// primitives shared by the HTTP (middleware) and gRPC (grpcmiddleware) packages.
//
// It exists so that the background system-metrics collector is a single
// process-wide singleton regardless of whether an application wires up the HTTP
// middleware, the gRPC interceptors, or both. Keeping this logic out of the
// Fiber-importing middleware package also lets Fiber-v2 applications consume the
// gRPC interceptors and the collector without pulling in Fiber v3.
package telemetrycore

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	observability "github.com/LerianStudio/lib-observability/v3"
	"github.com/LerianStudio/lib-observability/v3/runtime"
	"github.com/LerianStudio/lib-observability/v3/tracing"
)

// DefaultMetricsCollectionInterval is the default interval for collecting system metrics.
// Can be overridden via METRICS_COLLECTION_INTERVAL environment variable.
const DefaultMetricsCollectionInterval = 5 * time.Second

// Metrics collector singleton state.
var (
	metricsCollectorOnce     = &sync.Once{}
	metricsCollectorShutdown chan struct{}
	metricsCollectorMu       sync.Mutex
	metricsCollectorStarted  bool
	metricsCollectorInitErr  error
)

// telemetryRuntimeLogger returns the runtime logger from the telemetry, or nil.
func telemetryRuntimeLogger(tl *tracing.Telemetry) runtime.Logger {
	if tl == nil {
		return nil
	}

	return tl.Logger
}

// EnsureMetricsCollector lazily starts the background metrics collector singleton
// for the given telemetry. It is safe to call from both the HTTP middleware and
// the gRPC interceptors: only the first successful call starts the collector, so
// an application wiring up both transports still runs exactly one collector.
func EnsureMetricsCollector(tl *tracing.Telemetry) error {
	if tl == nil {
		return nil
	}

	if tl.MeterProvider == nil {
		return nil
	}

	metricsCollectorMu.Lock()
	defer metricsCollectorMu.Unlock()

	if metricsCollectorStarted {
		return nil
	}

	if metricsCollectorInitErr != nil {
		metricsCollectorOnce = &sync.Once{}
		metricsCollectorInitErr = nil
	}

	metricsCollectorOnce.Do(func() {
		factory := tl.MetricsFactory
		if factory == nil {
			metricsCollectorInitErr = errors.New("telemetry MetricsFactory is nil, cannot start system metrics collector")
			return
		}

		shutdown := make(chan struct{})
		metricsCollectorShutdown = shutdown
		ticker := time.NewTicker(getMetricsCollectionInterval())

		runtime.SafeGoWithContextAndComponent(
			context.Background(),
			telemetryRuntimeLogger(tl),
			"http",
			"metrics_collector",
			runtime.KeepRunning,
			func(_ context.Context) {
				observability.GetCPUUsage(context.Background(), factory)
				observability.GetMemUsage(context.Background(), factory)

				for {
					select {
					case <-shutdown:
						ticker.Stop()
						return
					case <-ticker.C:
						observability.GetCPUUsage(context.Background(), factory)
						observability.GetMemUsage(context.Background(), factory)
					}
				}
			},
		)

		metricsCollectorStarted = true
	})

	return metricsCollectorInitErr
}

// getMetricsCollectionInterval returns the metrics collection interval.
// Can be configured via METRICS_COLLECTION_INTERVAL environment variable.
// Accepts Go duration format (e.g., "10s", "1m", "500ms").
// Falls back to DefaultMetricsCollectionInterval if not set or invalid.
func getMetricsCollectionInterval() time.Duration {
	if envInterval := os.Getenv("METRICS_COLLECTION_INTERVAL"); envInterval != "" {
		if parsed, err := time.ParseDuration(envInterval); err == nil && parsed > 0 {
			return parsed
		}
	}

	return DefaultMetricsCollectionInterval
}

// StopMetricsCollector stops the background metrics collector goroutine.
// Should be called during application shutdown for graceful cleanup.
// After calling this function, the collector can be restarted by new requests.
//
// Implementation note: This function intentionally resets sync.Once to a new instance
// to allow the collector to be restarted after being stopped. This is an unusual but
// intentional pattern - the mutex ensures thread-safety during the reset operation,
// preventing race conditions between Stop and subsequent Start calls.
func StopMetricsCollector() {
	metricsCollectorMu.Lock()
	defer metricsCollectorMu.Unlock()

	if metricsCollectorStarted && metricsCollectorShutdown != nil {
		close(metricsCollectorShutdown)

		metricsCollectorStarted = false
		metricsCollectorOnce = &sync.Once{}
		metricsCollectorInitErr = nil
	}
}
