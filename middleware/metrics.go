package middleware

import (
	"context"

	"github.com/LerianStudio/lib-observability/v3/telemetrycore"
)

// DefaultMetricsCollectionInterval is re-exported from telemetrycore for
// backward compatibility with callers of the middleware package.
const DefaultMetricsCollectionInterval = telemetrycore.DefaultMetricsCollectionInterval

// StopMetricsCollector stops the background metrics collector goroutine.
// It delegates to telemetrycore so the HTTP middleware and the gRPC
// interceptors share a single collector singleton. Re-exported here for
// backward compatibility with callers of the middleware package.
func StopMetricsCollector() {
	telemetrycore.StopMetricsCollector()
}

// collectMetrics ensures the background metrics collector goroutine is running.
func (tm *TelemetryMiddleware) collectMetrics(_ context.Context) error {
	if tm == nil {
		return nil
	}

	return telemetrycore.EnsureMetricsCollector(tm.Telemetry)
}
