//go:build unit

package observability

import (
	"context"
	"testing"

	"github.com/LerianStudio/lib-observability/v4/metrics"
	"github.com/stretchr/testify/assert"
)

func TestSystemUsageHelpers(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	assert.NotPanics(t, func() { GetCPUUsage(ctx, nil) })
	assert.NotPanics(t, func() { GetMemUsage(ctx, nil) })
	assert.NotPanics(t, func() { GetCPUUsage(ctx, metrics.NewNopFactory()) })
	assert.NotPanics(t, func() { GetMemUsage(ctx, metrics.NewNopFactory()) })
}
