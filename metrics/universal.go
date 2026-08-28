package metrics

import (
	"context"
	"errors"
	"fmt"
	"math"
)

// ErrHistogramValueNotRepresentable is returned by
// UniversalRecorder.RecordHistogram when the float64 value cannot be recorded
// on the int64 histogram instrument backing this package: NaN, ±Inf, or a
// magnitude beyond the int64 range.
var ErrHistogramValueNotRepresentable = errors.New("histogram value is not representable as int64")

// UniversalRecorder mirrors the recording surface of MetricsFactory using
// only universal Go types, so that a consumer module can declare an
// equivalent interface locally, in its own package, without importing this
// one.
//
// # Why this exists
//
// MetricsFactory leaks across module boundaries the same way log.Logger does,
// and for the same reason: its methods are declared in terms of types DEFINED
// here. Counter takes a Metric and returns a *CounterBuilder; Gauge returns a
// *GaugeBuilder. Those are nominal types, and worse, they are concrete
// structs used through a fluent chain
// (f.Counter(m) -> .WithLabels(...) -> .Add(ctx, n)). A consumer cannot
// describe that shape in its own package at all, so anything that wants to
// emit a metric must import lib-observability — and inherit its major
// version, forcing lockstep releases across the fleet.
//
// UniversalRecorder flattens the builder chain into a single call per
// instrument and uses only string, map[string]string, int64, float64,
// []float64, error and context.Context. A consumer can write this in its own
// package with no import of lib-observability:
//
//	type Recorder interface {
//		AddCounter(ctx context.Context, name, description, unit string, attrs map[string]string, delta int64) error
//		SetGauge(ctx context.Context, name, description, unit string, attrs map[string]string, value int64) error
//		RecordHistogram(ctx context.Context, name, description, unit string, attrs map[string]string, value float64, buckets []float64) error
//	}
//
// # Instrument identity
//
// name, description and unit are the fields of Metric; the underlying factory
// caches instruments by name (histograms by name plus bucket set), so
// repeated calls with the same name reuse one instrument. attrs are applied
// as string labels — keep their cardinality bounded, exactly as with
// CounterBuilder.WithLabels.
//
// # Histogram value precision
//
// RecordHistogram accepts a float64, but the instruments this package creates
// are OpenTelemetry Int64Histograms. The value is therefore rounded to the
// nearest integer (half away from zero) before recording. Sub-integer
// measurements — notably durations expressed in SECONDS — collapse to 0.
// Record durations in milliseconds (or another unit whose useful range is
// integral) and set unit accordingly. buckets stay float64 and are passed
// through to the instrument unchanged.
type UniversalRecorder interface {
	AddCounter(ctx context.Context, name, description, unit string, attrs map[string]string, delta int64) error
	SetGauge(ctx context.Context, name, description, unit string, attrs map[string]string, value int64) error
	RecordHistogram(ctx context.Context, name, description, unit string, attrs map[string]string, value float64, buckets []float64) error
}

// universalRecorder is the UniversalRecorder view of a *MetricsFactory.
type universalRecorder struct {
	factory *MetricsFactory
}

// UniversalMetrics adapts a MetricsFactory to the universal form described on
// UniversalRecorder.
//
// UniversalMetrics(nil) returns a working no-op recorder rather than a nil
// interface: every method does nothing and returns nil, so a service running
// without metrics configured neither panics nor floods its callers with
// errors. Use NewNopFactory instead when you want a real (no-op meter)
// factory.
//
//nolint:ireturn // returning the interface is the whole point of the adapter.
func UniversalMetrics(f *MetricsFactory) UniversalRecorder {
	return &universalRecorder{factory: f}
}

// AddCounter increments a counter, flattening
// Counter -> WithLabels -> Add into one call.
//
// A negative delta returns ErrNegativeCounterValue, as counters are
// monotonically increasing.
func (r *universalRecorder) AddCounter(ctx context.Context, name, description, unit string, attrs map[string]string, delta int64) error {
	if r == nil || r.factory == nil {
		return nil
	}

	builder, err := r.factory.Counter(Metric{Name: name, Description: description, Unit: unit})
	if err != nil {
		return fmt.Errorf("universal counter %q: %w", name, err)
	}

	if err := builder.WithLabels(attrs).Add(ctx, delta); err != nil {
		return fmt.Errorf("universal counter %q: %w", name, err)
	}

	return nil
}

// SetGauge records the current value of a gauge, flattening
// Gauge -> WithLabels -> Set into one call.
func (r *universalRecorder) SetGauge(ctx context.Context, name, description, unit string, attrs map[string]string, value int64) error {
	if r == nil || r.factory == nil {
		return nil
	}

	builder, err := r.factory.Gauge(Metric{Name: name, Description: description, Unit: unit})
	if err != nil {
		return fmt.Errorf("universal gauge %q: %w", name, err)
	}

	if err := builder.WithLabels(attrs).Set(ctx, value); err != nil {
		return fmt.Errorf("universal gauge %q: %w", name, err)
	}

	return nil
}

// RecordHistogram records a histogram observation, flattening
// Histogram -> WithLabels -> Record into one call.
//
// A nil buckets slice lets the factory pick defaults from the metric name
// (see selectDefaultBuckets). value is rounded to the nearest int64 before
// recording — see the precision note on UniversalRecorder — and a value that
// cannot be represented (NaN, ±Inf, out of int64 range) returns
// ErrHistogramValueNotRepresentable without touching the instrument.
func (r *universalRecorder) RecordHistogram(ctx context.Context, name, description, unit string, attrs map[string]string, value float64, buckets []float64) error {
	if r == nil || r.factory == nil {
		return nil
	}

	rounded, err := int64FromFloat(value)
	if err != nil {
		return fmt.Errorf("universal histogram %q: %w", name, err)
	}

	builder, err := r.factory.Histogram(Metric{Name: name, Description: description, Unit: unit, Buckets: buckets})
	if err != nil {
		return fmt.Errorf("universal histogram %q: %w", name, err)
	}

	if err := builder.WithLabels(attrs).Record(ctx, rounded); err != nil {
		return fmt.Errorf("universal histogram %q: %w", name, err)
	}

	return nil
}

// int64FromFloat rounds value to the nearest integer (half away from zero)
// and reports whether the result fits an int64, so the conversion is never a
// blind cast onto an undefined value.
func int64FromFloat(value float64) (int64, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("%w: %v", ErrHistogramValueNotRepresentable, value)
	}

	rounded := math.Round(value)

	// float64(math.MaxInt64) rounds UP to 2^63, which is out of range, hence
	// the inclusive upper comparison. float64(math.MinInt64) is exactly
	// -2^63 and is in range, hence the exclusive lower one.
	if rounded >= float64(math.MaxInt64) || rounded < float64(math.MinInt64) {
		return 0, fmt.Errorf("%w: %v", ErrHistogramValueNotRepresentable, value)
	}

	return int64(rounded), nil
}
