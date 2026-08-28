package runtime

import (
	"context"
	"sync"

	constant "github.com/LerianStudio/lib-observability/v4/constants"
	"github.com/LerianStudio/lib-observability/v4/log"
	"github.com/LerianStudio/lib-observability/v4/metrics"
)

// Recorder is the minimal metric-recording interface this package needs.
//
// It uses only universal types, so *metrics.MetricsFactory satisfies it
// natively (see metrics.Recorder) and so can a recorder declared in a package
// that has never imported lib-observability. Naming *metrics.MetricsFactory
// here would make this package's major version propagate to every caller that
// initializes panic metrics.
type Recorder interface {
	AddCounter(ctx context.Context, name, description, unit string, attrs map[string]string, delta int64) error
}

// PanicMetrics provides panic-related metrics using OpenTelemetry.
// It records through a Recorder, so any metrics factory satisfies it.
type PanicMetrics struct {
	factory Recorder
	logger  Logger
}

// panicRecoveredMetric defines the metric for counting recovered panics.
var panicRecoveredMetric = metrics.Metric{
	Name:        constant.MetricPanicRecoveredTotal,
	Unit:        "1",
	Description: "Total number of recovered panics",
}

// panicMetricsInstance is the singleton instance for panic metrics.
// It is initialized lazily via InitPanicMetrics.
var (
	panicMetricsInstance *PanicMetrics
	panicMetricsMu       sync.RWMutex
)

// InitPanicMetrics initializes panic metrics with the provided recorder.
//
// The parameter is the universal Recorder interface rather than
// *metrics.MetricsFactory. Existing callers are unaffected -
// *metrics.MetricsFactory implements Recorder directly, so
// InitPanicMetrics(tl.MetricsFactory) still compiles unchanged.
//
// Backward compatibility:
//   - InitPanicMetrics(factory)
//   - InitPanicMetrics(factory, logger)
//
// The logger is optional and used only for metric recording diagnostics.
// This should be called once during application startup after telemetry is initialized.
// It is safe to call multiple times; subsequent calls are no-ops.
//
// Example:
//
//	tl, err := tracing.NewTelemetry(cfg)
//	if err != nil {
//	    // handle error
//	}
//	tl.ApplyGlobals()
//	runtime.InitPanicMetrics(tl.MetricsFactory)
func InitPanicMetrics(factory Recorder, logger ...Logger) {
	panicMetricsMu.Lock()
	defer panicMetricsMu.Unlock()

	if log.IsNil(factory) {
		return
	}

	if panicMetricsInstance != nil {
		return // Already initialized
	}

	var l Logger
	if len(logger) > 0 {
		l = logger[0]
	}

	panicMetricsInstance = &PanicMetrics{
		factory: factory,
		logger:  l,
	}
}

// GetPanicMetrics returns the singleton PanicMetrics instance.
// Returns nil if InitPanicMetrics has not been called.
func GetPanicMetrics() *PanicMetrics {
	panicMetricsMu.RLock()
	defer panicMetricsMu.RUnlock()

	return panicMetricsInstance
}

// ResetPanicMetrics clears the panic metrics singleton.
// This is primarily intended for testing to ensure test isolation.
// In production, this should generally not be called.
func ResetPanicMetrics() {
	panicMetricsMu.Lock()
	defer panicMetricsMu.Unlock()

	panicMetricsInstance = nil
}

// RecordPanicRecovered increments the panic_recovered_total counter with the given labels.
// If metrics are not initialized, this is a no-op.
//
// Parameters:
//   - ctx: Context for metric recording (may contain trace correlation)
//   - component: The component where the panic occurred (e.g., "transaction", "onboarding", "crm")
//   - goroutineName: The name of the goroutine or handler (e.g., "http_handler", "rabbitmq_worker")
func (pm *PanicMetrics) RecordPanicRecovered(ctx context.Context, component, goroutineName string) {
	if pm == nil || log.IsNil(pm.factory) {
		return
	}

	err := pm.factory.AddCounter(ctx,
		panicRecoveredMetric.Name,
		panicRecoveredMetric.Description,
		panicRecoveredMetric.Unit,
		map[string]string{
			"component":      constant.SanitizeMetricLabel(component),
			"goroutine_name": constant.SanitizeMetricLabel(goroutineName),
		},
		1,
	)
	if err != nil {
		if pm.logger != nil {
			pm.logger.Log(ctx, log.LevelWarn, "failed to record panic metric", log.Err(err))
		}

		return
	}
}

// recordPanicMetric is a package-level helper that records a panic metric if metrics are initialized.
// This is called internally by recovery functions.
func recordPanicMetric(ctx context.Context, component, goroutineName string) {
	pm := GetPanicMetrics()
	if pm != nil {
		pm.RecordPanicRecovered(ctx, component, goroutineName)
	}
}
