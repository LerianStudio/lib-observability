package observability

import (
	"context"
	"time"

	"github.com/LerianStudio/lib-observability/log"
	"github.com/LerianStudio/lib-observability/metrics"
	"github.com/shirou/gopsutil/cpu"
	"github.com/shirou/gopsutil/mem"
)

// GetCPUUsage reads the current CPU usage and records it through the MetricsFactory gauge.
// If factory is nil, the reading is performed but metric recording is skipped.
func GetCPUUsage(ctx context.Context, factory *metrics.MetricsFactory) {
	logger := NewLoggerFromContext(ctx)

	out, err := cpu.Percent(100*time.Millisecond, false)
	if err != nil {
		logger.Log(ctx, log.LevelWarn, "error getting CPU usage", log.Err(err))
	}

	var percentageCPU int64 = 0
	if len(out) > 0 {
		percentageCPU = int64(out[0])
	}

	if factory == nil {
		logger.Log(ctx, log.LevelWarn, "metrics factory is nil, skipping CPU usage recording")
		return
	}

	if err := factory.RecordSystemCPUUsage(ctx, percentageCPU); err != nil {
		logger.Log(ctx, log.LevelWarn, "error recording CPU gauge", log.Err(err))
	}
}

// GetMemUsage reads the current memory usage and records it through the MetricsFactory gauge.
// If factory is nil, the reading is performed but metric recording is skipped.
func GetMemUsage(ctx context.Context, factory *metrics.MetricsFactory) {
	logger := NewLoggerFromContext(ctx)

	var percentageMem int64 = 0

	out, err := mem.VirtualMemory()
	if err != nil {
		logger.Log(ctx, log.LevelWarn, "error getting memory info", log.Err(err))
	} else {
		percentageMem = int64(out.UsedPercent)
	}

	if factory == nil {
		logger.Log(ctx, log.LevelWarn, "metrics factory is nil, skipping memory usage recording")
		return
	}

	if err := factory.RecordSystemMemUsage(ctx, percentageMem); err != nil {
		logger.Log(ctx, log.LevelWarn, "error recording memory gauge", log.Err(err))
	}
}
