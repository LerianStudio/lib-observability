//go:build unit

package metrics

import (
	"errors"
	"math"
	"testing"

	"github.com/LerianStudio/lib-observability/v4/log"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric/noop"
)

// A NaN boundary sorts first and compares false against every neighbour, so
// the SDK's own i >= j check lets it through; the factory must refuse it on
// both histogram paths, and a duplicate must be refused the same way.
func TestHistogramBuckets_RejectNaNAndNonIncreasing(t *testing.T) {
	f, err := NewMetricsFactory(noop.NewMeterProvider().Meter("m"), log.NewNop())
	require.NoError(t, err)

	cases := map[string][]float64{
		"nan first":      {math.NaN(), 1, 2},
		"nan in middle":  {1, math.NaN(), 2},
		"single nan":     {math.NaN()},
		"duplicate":      {1, 2, 2},
		"unsorted dupes": {3, 1, 3},
	}

	for name, buckets := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := f.Histogram(Metric{Name: "int_" + name, Buckets: buckets})
			require.ErrorIs(t, err, ErrInvalidBuckets, "int64 path")

			_, err = f.Float64Histogram(Metric{Name: "float_" + name, Buckets: buckets})
			require.ErrorIs(t, err, ErrInvalidBuckets, "float64 path")
		})
	}
}

func TestHistogramBuckets_AcceptIncreasingAndInfinite(t *testing.T) {
	f, err := NewMetricsFactory(noop.NewMeterProvider().Meter("m"), log.NewNop())
	require.NoError(t, err)

	for name, buckets := range map[string][]float64{
		"nil":            nil,
		"single":         {1},
		"increasing":     {0.005, 0.01, 0.1, 1},
		"unsorted valid": {1, 0.1, 10},
		"plus infinity":  {1, 10, math.Inf(1)},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := f.Histogram(Metric{Name: "int_" + name, Buckets: buckets})
			require.NoError(t, err)

			_, err = f.Float64Histogram(Metric{Name: "float_" + name, Buckets: buckets})
			require.NoError(t, err)
		})
	}
}

func TestValidateBuckets_IsTheSentinel(t *testing.T) {
	require.True(t, errors.Is(validateBuckets([]float64{math.NaN()}), ErrInvalidBuckets))
	require.NoError(t, validateBuckets(nil))
}
