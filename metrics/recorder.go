package metrics

import (
	"context"
	"errors"
	"math"
)

// ErrHistogramValueNotRepresentable is returned by RecordHistogram when the
// float64 value cannot be recorded on the int64 histogram instrument backing
// this package: NaN, +/-Inf, or a magnitude beyond the int64 range.
var ErrHistogramValueNotRepresentable = errors.New("histogram value is not representable as int64")

// Recorder is the recording surface of MetricsFactory expressed in universal
// Go types only.
//
// # Why this exists
//
// MetricsFactory propagates its major version across module boundaries the
// same way a logger interface does, and worse. Its methods are declared in
// terms of types DEFINED here: Counter takes a Metric and returns a
// *CounterBuilder; Gauge returns a *GaugeBuilder. Those are nominal types,
// and they are concrete structs used through a fluent chain
// (f.Counter(m) -> .WithLabels(...) -> .AddOne(ctx)). A consumer cannot
// describe that shape in its own package at all, so anything that wants to
// emit a metric had to import lib-observability and inherit its major.
//
// Recorder flattens the builder chain into one call per instrument and uses
// only string, map[string]string, int64, float64, []float64, error and
// context.Context. A consumer can write this in its own package, import
// nothing, and accept a factory from any version of this module:
//
//	type Recorder interface {
//		AddCounter(ctx context.Context, name, description, unit string, attrs map[string]string, delta int64) error
//		SetGauge(ctx context.Context, name, description, unit string, attrs map[string]string, value int64) error
//		RecordHistogram(ctx context.Context, name, description, unit string, attrs map[string]string, value float64, buckets []float64) error
//	}
//
// # These are methods on *MetricsFactory, not a wrapper
//
// *MetricsFactory implements Recorder directly. That is deliberate: an
// adapter function would mean every call site in the fleet had to wrap its
// factory before handing it to anything that wants the universal shape, which
// is the churn this design exists to avoid. Because the methods live on the
// concrete type, an existing caller passing tl.MetricsFactory to a function
// whose parameter was widened to a universal interface keeps compiling with
// no edit at all.
//
// # Instrument identity
//
// name, description and unit are the fields of Metric; the underlying factory
// caches instruments by name (histograms by name plus bucket set), so
// repeated calls with the same name reuse one instrument. attrs are applied
// as string labels - keep their cardinality bounded, exactly as with
// CounterBuilder.WithLabels.
//
// # Histogram value precision
//
// RecordHistogram accepts a float64, but the instruments this package creates
// are OpenTelemetry Int64Histograms. The value is rounded to the nearest
// integer (half away from zero) before recording. Sub-integer measurements -
// notably durations expressed in SECONDS - collapse to 0. Record durations in
// milliseconds (or another unit whose useful range is integral) and set unit
// accordingly. buckets stay float64 and are passed through unchanged.
type Recorder interface {
	AddCounter(ctx context.Context, name, description, unit string, attrs map[string]string, delta int64) error
	SetGauge(ctx context.Context, name, description, unit string, attrs map[string]string, value int64) error
	RecordHistogram(ctx context.Context, name, description, unit string, attrs map[string]string, value float64, buckets []float64) error
}

// Compile-time proof that the concrete factory carries the universal surface,
// so no adapter is ever needed at a call site.
var _ Recorder = (*MetricsFactory)(nil)

// AddCounter adds delta to the counter identified by name, creating the
// instrument on first use. It is the flattened equivalent of
// Counter(Metric{...}).WithLabels(attrs).Add(ctx, delta).
//
// A nil receiver returns ErrNilFactory, matching Counter.
func (f *MetricsFactory) AddCounter(
	ctx context.Context,
	name, description, unit string,
	attrs map[string]string,
	delta int64,
) error {
	builder, err := f.Counter(Metric{Name: name, Description: description, Unit: unit})
	if err != nil {
		return err
	}

	return builder.WithLabels(attrs).Add(ctx, delta)
}

// SetGauge sets the gauge identified by name to value, creating the
// instrument on first use. It is the flattened equivalent of
// Gauge(Metric{...}).WithLabels(attrs).Set(ctx, value).
//
// A nil receiver returns ErrNilFactory, matching Gauge.
func (f *MetricsFactory) SetGauge(
	ctx context.Context,
	name, description, unit string,
	attrs map[string]string,
	value int64,
) error {
	builder, err := f.Gauge(Metric{Name: name, Description: description, Unit: unit})
	if err != nil {
		return err
	}

	return builder.WithLabels(attrs).Set(ctx, value)
}

// RecordHistogram records value on the histogram identified by name, creating
// the instrument on first use with the given buckets (nil selects the package
// default). It is the flattened equivalent of
// Histogram(Metric{...}).WithLabels(attrs).Record(ctx, value).
//
// value is rounded to the nearest int64, half away from zero, because the
// backing instrument is an Int64Histogram. A value that cannot be represented
// as an int64 - NaN, +/-Inf, or out of range - returns
// ErrHistogramValueNotRepresentable rather than recording a garbage sample.
//
// A nil receiver returns ErrNilFactory, matching Histogram.
func (f *MetricsFactory) RecordHistogram(
	ctx context.Context,
	name, description, unit string,
	attrs map[string]string,
	value float64,
	buckets []float64,
) error {
	rounded, ok := int64FromFloat(value)
	if !ok {
		return ErrHistogramValueNotRepresentable
	}

	builder, err := f.Histogram(Metric{Name: name, Description: description, Unit: unit, Buckets: buckets})
	if err != nil {
		return err
	}

	return builder.WithLabels(attrs).Record(ctx, rounded)
}

// int64FromFloat rounds value half away from zero and reports whether the
// result is representable as an int64.
//
// The bound is expressed against 2^63 rather than math.MaxInt64 because
// float64 cannot represent math.MaxInt64 exactly - it rounds up to 2^63 - so
// comparing against the converted constant would let a value one ULP too
// large through and overflow the conversion.
func int64FromFloat(value float64) (int64, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}

	rounded := math.Round(value)
	if rounded >= math.Pow(2, 63) || rounded < -math.Pow(2, 63) {
		return 0, false
	}

	return int64(rounded), true
}
