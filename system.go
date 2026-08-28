package observability

import (
	"context"
	"time"

	"github.com/LerianStudio/lib-observability/v3/log"
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/mem"
)

// SystemRecorder is the minimal metric surface the system readings need.
//
// It is a local interface built from universal types rather than
// *metrics.MetricsFactory, so a consumer can supply its own recorder without
// importing lib-observability. *metrics.MetricsFactory satisfies it directly,
// so existing callers passing tl.MetricsFactory are unaffected.
type SystemRecorder interface {
	RecordSystemCPUUsage(ctx context.Context, percentage int64) error
	RecordSystemMemUsage(ctx context.Context, percentage int64) error
}

// GetCPUUsage reads the current CPU usage and records it through the recorder's gauge.
// If factory is nil, the reading is performed but metric recording is skipped.
func GetCPUUsage(ctx context.Context, factory SystemRecorder) {
	logger := NewLoggerFromContext(ctx)

	out, err := cpu.Percent(100*time.Millisecond, false)
	if err != nil {
		logger.Log(ctx, log.LevelWarn, "error getting CPU usage", log.Err(err))
		return
	}

	var percentageCPU int64 = 0
	if len(out) > 0 {
		percentageCPU = int64(out[0])
	}

	if log.IsNil(factory) {
		logger.Log(ctx, log.LevelWarn, "metrics factory is nil, skipping CPU usage recording")
		return
	}

	if err := factory.RecordSystemCPUUsage(ctx, percentageCPU); err != nil {
		logger.Log(ctx, log.LevelWarn, "error recording CPU gauge", log.Err(err))
	}
}

// GetMemUsage reads the current memory usage and records it through the recorder's gauge.
// If factory is nil, the reading is performed but metric recording is skipped.
func GetMemUsage(ctx context.Context, factory SystemRecorder) {
	logger := NewLoggerFromContext(ctx)

	out, err := mem.VirtualMemory()
	if err != nil {
		logger.Log(ctx, log.LevelWarn, "error getting memory info", log.Err(err))
		return
	}

	percentageMem := int64(out.UsedPercent)

	if log.IsNil(factory) {
		logger.Log(ctx, log.LevelWarn, "metrics factory is nil, skipping memory usage recording")
		return
	}

	if err := factory.RecordSystemMemUsage(ctx, percentageMem); err != nil {
		logger.Log(ctx, log.LevelWarn, "error recording memory gauge", log.Err(err))
	}
}
