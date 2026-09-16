//go:build unit

package metrics

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/LerianStudio/lib-observability/v4/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

func newTestFactory(t *testing.T) *MetricsFactory {
	t.Helper()

	f, err := NewMetricsFactory(noop.NewMeterProvider().Meter("metrics-test"), log.NewNop())
	require.NoError(t, err)

	return f
}

type cacheTestMeter struct {
	metric.Meter
	counter          *cacheTestCounter
	gauge            *cacheTestGauge
	histogram        *cacheTestHistogram
	float64Counter   *cacheTestFloat64Counter
	float64Gauge     *cacheTestFloat64Gauge
	float64Histogram *cacheTestFloat64Histogram
}

type (
	cacheTestCounter          struct{ metric.Int64Counter }
	cacheTestGauge            struct{ metric.Int64Gauge }
	cacheTestHistogram        struct{ metric.Int64Histogram }
	cacheTestFloat64Counter   struct{ metric.Float64Counter }
	cacheTestFloat64Gauge     struct{ metric.Float64Gauge }
	cacheTestFloat64Histogram struct{ metric.Float64Histogram }
)

func newCacheTestFactory(t *testing.T) *MetricsFactory {
	t.Helper()

	meter := noop.NewMeterProvider().Meter("metrics-cache-test")
	counter, err := meter.Int64Counter("counter")
	require.NoError(t, err)
	gauge, err := meter.Int64Gauge("gauge")
	require.NoError(t, err)
	histogram, err := meter.Int64Histogram("histogram")
	require.NoError(t, err)
	f64Counter, err := meter.Float64Counter("f64counter")
	require.NoError(t, err)
	f64Gauge, err := meter.Float64Gauge("f64gauge")
	require.NoError(t, err)
	f64Histogram, err := meter.Float64Histogram("f64histogram")
	require.NoError(t, err)

	f, err := NewMetricsFactory(&cacheTestMeter{
		Meter:            meter,
		counter:          &cacheTestCounter{Int64Counter: counter},
		gauge:            &cacheTestGauge{Int64Gauge: gauge},
		histogram:        &cacheTestHistogram{Int64Histogram: histogram},
		float64Counter:   &cacheTestFloat64Counter{Float64Counter: f64Counter},
		float64Gauge:     &cacheTestFloat64Gauge{Float64Gauge: f64Gauge},
		float64Histogram: &cacheTestFloat64Histogram{Float64Histogram: f64Histogram},
	}, log.NewNop())
	require.NoError(t, err)

	return f
}

func (m *cacheTestMeter) Int64Counter(string, ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	return m.counter, nil
}

func (m *cacheTestMeter) Int64Gauge(string, ...metric.Int64GaugeOption) (metric.Int64Gauge, error) {
	return m.gauge, nil
}

func (m *cacheTestMeter) Int64Histogram(string, ...metric.Int64HistogramOption) (metric.Int64Histogram, error) {
	return m.histogram, nil
}

func (m *cacheTestMeter) Float64Counter(string, ...metric.Float64CounterOption) (metric.Float64Counter, error) {
	return m.float64Counter, nil
}

func (m *cacheTestMeter) Float64Gauge(string, ...metric.Float64GaugeOption) (metric.Float64Gauge, error) {
	return m.float64Gauge, nil
}

func (m *cacheTestMeter) Float64Histogram(string, ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	return m.float64Histogram, nil
}

func TestNewMetricsFactory(t *testing.T) {
	t.Parallel()

	f, err := NewMetricsFactory(noop.NewMeterProvider().Meter("metrics-test"), nil)
	require.NoError(t, err)
	require.NotNil(t, f)

	f, err = NewMetricsFactory(nil, nil)
	require.Nil(t, f)
	assert.ErrorIs(t, err, ErrNilMeter)
}

func TestNewNopFactoryRecords(t *testing.T) {
	t.Parallel()

	f := NewNopFactory()
	require.NotNil(t, f)

	ctx := context.Background()
	assert.NoError(t, f.RecordAccountCreated(ctx))
	assert.NoError(t, f.RecordTransactionProcessed(ctx))
	assert.NoError(t, f.RecordTransactionRouteCreated(ctx))
	assert.NoError(t, f.RecordOperationRouteCreated(ctx))
	assert.NoError(t, f.RecordSystemCPUUsage(ctx, 0))
	assert.NoError(t, f.RecordSystemCPUUsage(ctx, 100))
	assert.NoError(t, f.RecordSystemMemUsage(ctx, 42))
}

func TestMetricsFactoryNilReceiver(t *testing.T) {
	t.Parallel()

	var f *MetricsFactory
	ctx := context.Background()

	_, err := f.Counter(MetricAccountsCreated)
	assert.ErrorIs(t, err, ErrNilFactory)
	_, err = f.Gauge(MetricSystemCPUUsage)
	assert.ErrorIs(t, err, ErrNilFactory)
	_, err = f.Histogram(Metric{Name: "duration"})
	assert.ErrorIs(t, err, ErrNilFactory)
	_, err = f.Float64Counter(MetricAccountsCreated)
	assert.ErrorIs(t, err, ErrNilFactory)
	_, err = f.Float64Gauge(MetricSystemCPUUsage)
	assert.ErrorIs(t, err, ErrNilFactory)
	_, err = f.Float64Histogram(Metric{Name: "duration"})
	assert.ErrorIs(t, err, ErrNilFactory)
	assert.ErrorIs(t, f.RecordAccountCreated(ctx), ErrNilFactory)
	assert.ErrorIs(t, f.RecordTransactionProcessed(ctx), ErrNilFactory)
	assert.ErrorIs(t, f.RecordTransactionRouteCreated(ctx), ErrNilFactory)
	assert.ErrorIs(t, f.RecordOperationRouteCreated(ctx), ErrNilFactory)
	assert.ErrorIs(t, f.RecordSystemCPUUsage(ctx, 50), ErrNilFactory)
	assert.ErrorIs(t, f.RecordSystemMemUsage(ctx, 50), ErrNilFactory)
}

func TestCounterBuilder(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	f := newTestFactory(t)
	b, err := f.Counter(Metric{Name: "counter_test", Description: "desc", Unit: "1"})
	require.NoError(t, err)

	withLabels := b.WithLabels(map[string]string{"tenant": "t1"})
	require.NotSame(t, b, withLabels)
	assert.Len(t, withLabels.attrs, 1)
	assert.Empty(t, b.attrs)

	withAttrs := withLabels.WithAttributes(attribute.String("region", "br"))
	assert.Len(t, withAttrs.attrs, 2)
	assert.NoError(t, withAttrs.Add(ctx, 2))
	assert.NoError(t, withAttrs.AddOne(ctx))
	assert.ErrorIs(t, withAttrs.Add(ctx, -1), ErrNegativeCounterValue)

	var nilBuilder *CounterBuilder
	assert.Nil(t, nilBuilder.WithLabels(map[string]string{"x": "y"}))
	assert.Nil(t, nilBuilder.WithAttributes(attribute.String("x", "y")))
	assert.ErrorIs(t, nilBuilder.Add(ctx, 1), ErrNilCounterBuilder)
	assert.ErrorIs(t, nilBuilder.AddOne(ctx), ErrNilCounterBuilder)
	assert.ErrorIs(t, (&CounterBuilder{}).Add(ctx, 1), ErrNilCounter)
}

func TestGaugeBuilder(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	f := newTestFactory(t)
	b, err := f.Gauge(Metric{Name: "gauge_test", Description: "desc", Unit: "1"})
	require.NoError(t, err)

	withLabels := b.WithLabels(map[string]string{"tenant": "t1"})
	withAttrs := withLabels.WithAttributes(attribute.String("region", "br"))
	assert.Len(t, withAttrs.attrs, 2)
	assert.NoError(t, withAttrs.Set(ctx, 42))

	var nilBuilder *GaugeBuilder
	assert.Nil(t, nilBuilder.WithLabels(map[string]string{"x": "y"}))
	assert.Nil(t, nilBuilder.WithAttributes(attribute.String("x", "y")))
	assert.ErrorIs(t, nilBuilder.Set(ctx, 1), ErrNilGaugeBuilder)
	assert.ErrorIs(t, (&GaugeBuilder{}).Set(ctx, 1), ErrNilGauge)
}

func TestHistogramBuilder(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	f := newTestFactory(t)
	b, err := f.Histogram(Metric{Name: "request_duration", Description: "desc", Unit: "ms"})
	require.NoError(t, err)

	withLabels := b.WithLabels(map[string]string{"tenant": "t1"})
	withAttrs := withLabels.WithAttributes(attribute.String("region", "br"))
	assert.Len(t, withAttrs.attrs, 2)
	assert.NoError(t, withAttrs.Record(ctx, 123))

	var nilBuilder *HistogramBuilder
	assert.Nil(t, nilBuilder.WithLabels(map[string]string{"x": "y"}))
	assert.Nil(t, nilBuilder.WithAttributes(attribute.String("x", "y")))
	assert.ErrorIs(t, nilBuilder.Record(ctx, 1), ErrNilHistogramBuilder)
	assert.ErrorIs(t, (&HistogramBuilder{}).Record(ctx, 1), ErrNilHistogram)
}

func TestMetricsFactoryCachesAndInvalidCacheEntries(t *testing.T) {
	t.Parallel()

	f := newCacheTestFactory(t)

	c1, err := f.getOrCreateCounter(Metric{Name: "cached_counter"})
	require.NoError(t, err)
	c2, err := f.getOrCreateCounter(Metric{Name: "cached_counter"})
	require.NoError(t, err)
	assert.Same(t, c1, c2)

	g1, err := f.getOrCreateGauge(Metric{Name: "cached_gauge"})
	require.NoError(t, err)
	g2, err := f.getOrCreateGauge(Metric{Name: "cached_gauge"})
	require.NoError(t, err)
	assert.Same(t, g1, g2)

	h1, err := f.getOrCreateHistogram(Metric{Name: "cached_histogram", Buckets: []float64{5, 1, 2}})
	require.NoError(t, err)
	h2, err := f.getOrCreateHistogram(Metric{Name: "cached_histogram", Buckets: []float64{2, 5, 1}})
	require.NoError(t, err)
	assert.Same(t, h1, h2)

	f.counters.Store("bad_counter", "wrong")
	_, err = f.getOrCreateCounter(Metric{Name: "bad_counter"})
	assert.ErrorContains(t, err, "invalid type")

	f.gauges.Store("bad_gauge", "wrong")
	_, err = f.getOrCreateGauge(Metric{Name: "bad_gauge"})
	assert.ErrorContains(t, err, "invalid type")

	f.histograms.Store("bad_histogram", "wrong")
	_, err = f.getOrCreateHistogram(Metric{Name: "bad_histogram"})
	assert.ErrorContains(t, err, "invalid type")
}

func TestMetricHelpersAndOptions(t *testing.T) {
	t.Parallel()

	assert.Equal(t, DefaultLatencyBuckets, selectDefaultBuckets("http_latency_seconds"))
	assert.Equal(t, DefaultLatencyBuckets, selectDefaultBuckets("job_duration_seconds"))
	assert.Equal(t, DefaultLatencyBuckets, selectDefaultBuckets("queue_time_seconds"))
	assert.Equal(t, DefaultAccountBuckets, selectDefaultBuckets("account_created"))
	assert.Equal(t, DefaultTransactionBuckets, selectDefaultBuckets("transaction_processed"))
	assert.Equal(t, DefaultLatencyBuckets, selectDefaultBuckets("unknown_metric"))

	assert.Equal(t, "hist:1,2,5", histogramCacheKey("hist", []float64{5, 1, 2}))
	assert.Equal(t, "hist", histogramCacheKey("hist", nil))

	f := newTestFactory(t)
	assert.Len(t, f.addCounterOptions(Metric{Description: "d", Unit: "1"}), 2)
	assert.Len(t, f.addGaugeOptions(Metric{Description: "d", Unit: "1"}), 2)
	assert.Len(t, f.addHistogramOptions(Metric{Description: "d", Unit: "ms", Buckets: []float64{1}}), 3)
}

func TestRecordSystemUsageValidatesRange(t *testing.T) {
	t.Parallel()

	f := newTestFactory(t)
	ctx := context.Background()

	for _, err := range []error{
		f.RecordSystemCPUUsage(ctx, -1),
		f.RecordSystemCPUUsage(ctx, 101),
		f.RecordSystemMemUsage(ctx, -1),
		f.RecordSystemMemUsage(ctx, 101),
	} {
		assert.True(t, errors.Is(err, ErrPercentageOutOfRange))
	}
}

func TestFloat64CounterBuilder(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	f := newTestFactory(t)
	b, err := f.Float64Counter(Metric{Name: "cost_usd_total", Description: "desc", Unit: "{USD}"})
	require.NoError(t, err)

	withLabels := b.WithLabels(map[string]string{"tenant": "t1"})
	require.NotSame(t, b, withLabels)
	assert.Len(t, withLabels.attrs, 1)
	assert.Empty(t, b.attrs)

	withAttrs := withLabels.WithAttributes(attribute.String("region", "br"))
	assert.Len(t, withAttrs.attrs, 2)
	assert.NoError(t, withAttrs.Add(ctx, 0.000375))
	assert.NoError(t, withAttrs.AddOne(ctx))
	assert.ErrorIs(t, withAttrs.Add(ctx, -0.5), ErrNegativeCounterValue)

	var nilBuilder *Float64CounterBuilder
	assert.Nil(t, nilBuilder.WithLabels(map[string]string{"x": "y"}))
	assert.Nil(t, nilBuilder.WithAttributes(attribute.String("x", "y")))
	assert.ErrorIs(t, nilBuilder.Add(ctx, 1), ErrNilCounterBuilder)
	assert.ErrorIs(t, nilBuilder.AddOne(ctx), ErrNilCounterBuilder)
	assert.ErrorIs(t, (&Float64CounterBuilder{}).Add(ctx, 1), ErrNilCounter)
}

func TestFloat64GaugeBuilder(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	f := newTestFactory(t)
	b, err := f.Float64Gauge(Metric{Name: "gauge_test_f64", Description: "desc", Unit: "1"})
	require.NoError(t, err)

	withLabels := b.WithLabels(map[string]string{"tenant": "t1"})
	withAttrs := withLabels.WithAttributes(attribute.String("region", "br"))
	assert.Len(t, withAttrs.attrs, 2)
	assert.NoError(t, withAttrs.Set(ctx, 42.5))

	var nilBuilder *Float64GaugeBuilder
	assert.Nil(t, nilBuilder.WithLabels(map[string]string{"x": "y"}))
	assert.Nil(t, nilBuilder.WithAttributes(attribute.String("x", "y")))
	assert.ErrorIs(t, nilBuilder.Set(ctx, 1), ErrNilGaugeBuilder)
	assert.ErrorIs(t, (&Float64GaugeBuilder{}).Set(ctx, 1), ErrNilGauge)
}

func TestFloat64HistogramBuilder(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	f := newTestFactory(t)
	b, err := f.Float64Histogram(Metric{Name: "llm_call_duration", Description: "desc", Unit: "s"})
	require.NoError(t, err)

	withLabels := b.WithLabels(map[string]string{"tenant": "t1"})
	withAttrs := withLabels.WithAttributes(attribute.String("region", "br"))
	assert.Len(t, withAttrs.attrs, 2)
	assert.NoError(t, withAttrs.Record(ctx, 1.732))

	var nilBuilder *Float64HistogramBuilder
	assert.Nil(t, nilBuilder.WithLabels(map[string]string{"x": "y"}))
	assert.Nil(t, nilBuilder.WithAttributes(attribute.String("x", "y")))
	assert.ErrorIs(t, nilBuilder.Record(ctx, 1), ErrNilHistogramBuilder)
	assert.ErrorIs(t, (&Float64HistogramBuilder{}).Record(ctx, 1), ErrNilHistogram)
}

func TestFloat64HistogramUsesSecondsDefaultBuckets(t *testing.T) {
	t.Parallel()

	f := newTestFactory(t)

	// Buckets are nil on the way in, so the factory fills them from the metric
	// name; the cache key proves which set it picked.
	_, err := f.Float64Histogram(Metric{Name: "llm_request_duration"})
	require.NoError(t, err)

	_, cached := f.float64Histograms.Load(histogramCacheKey("llm_request_duration", DefaultLatencyBuckets))
	assert.True(t, cached, "expected the duration histogram to be cached under the seconds buckets")
}

func TestFloat64AndInt64InstrumentsDoNotAlias(t *testing.T) {
	t.Parallel()

	f := newCacheTestFactory(t)
	const name = "shared_name"

	i64Counter, err := f.getOrCreateCounter(Metric{Name: name})
	require.NoError(t, err)
	f64Counter, err := f.getOrCreateFloat64Counter(Metric{Name: name})
	require.NoError(t, err)

	i64Gauge, err := f.getOrCreateGauge(Metric{Name: name})
	require.NoError(t, err)
	f64Gauge, err := f.getOrCreateFloat64Gauge(Metric{Name: name})
	require.NoError(t, err)

	i64Histogram, err := f.getOrCreateHistogram(Metric{Name: name, Buckets: []float64{1, 2}})
	require.NoError(t, err)
	f64Histogram, err := f.getOrCreateFloat64Histogram(Metric{Name: name, Buckets: []float64{1, 2}})
	require.NoError(t, err)

	// Each kind still resolves to its own instrument after the other kind
	// registered the same name. A shared cache would fail the type assertion
	// here instead, and return an "invalid type" error.
	gotI64Counter, err := f.getOrCreateCounter(Metric{Name: name})
	require.NoError(t, err)
	assert.Same(t, i64Counter, gotI64Counter)

	gotF64Counter, err := f.getOrCreateFloat64Counter(Metric{Name: name})
	require.NoError(t, err)
	assert.Same(t, f64Counter, gotF64Counter)

	gotI64Gauge, err := f.getOrCreateGauge(Metric{Name: name})
	require.NoError(t, err)
	assert.Same(t, i64Gauge, gotI64Gauge)

	gotF64Gauge, err := f.getOrCreateFloat64Gauge(Metric{Name: name})
	require.NoError(t, err)
	assert.Same(t, f64Gauge, gotF64Gauge)

	gotI64Histogram, err := f.getOrCreateHistogram(Metric{Name: name, Buckets: []float64{2, 1}})
	require.NoError(t, err)
	assert.Same(t, i64Histogram, gotI64Histogram)

	gotF64Histogram, err := f.getOrCreateFloat64Histogram(Metric{Name: name, Buckets: []float64{2, 1}})
	require.NoError(t, err)
	assert.Same(t, f64Histogram, gotF64Histogram)
}

func TestFloat64CacheRejectsInvalidEntries(t *testing.T) {
	t.Parallel()

	f := newCacheTestFactory(t)

	f.float64Counters.Store("bad_counter", "wrong")
	_, err := f.getOrCreateFloat64Counter(Metric{Name: "bad_counter"})
	assert.ErrorContains(t, err, "invalid type")

	f.float64Gauges.Store("bad_gauge", "wrong")
	_, err = f.getOrCreateFloat64Gauge(Metric{Name: "bad_gauge"})
	assert.ErrorContains(t, err, "invalid type")

	f.float64Histograms.Store("bad_histogram", "wrong")
	_, err = f.getOrCreateFloat64Histogram(Metric{Name: "bad_histogram"})
	assert.ErrorContains(t, err, "invalid type")
}

func TestNewNopFactoryFloat64(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	f := NewNopFactory()
	require.NotNil(t, f)

	counter, err := f.Float64Counter(Metric{Name: "nop_cost_usd"})
	require.NoError(t, err)
	assert.NoError(t, counter.Add(ctx, 1.5))

	gauge, err := f.Float64Gauge(Metric{Name: "nop_gauge"})
	require.NoError(t, err)
	assert.NoError(t, gauge.Set(ctx, 1.5))

	histogram, err := f.Float64Histogram(Metric{Name: "nop_duration"})
	require.NoError(t, err)
	assert.NoError(t, histogram.Record(ctx, 1.5))

	assert.Len(t, f.addFloat64CounterOptions(Metric{Description: "d", Unit: "1"}), 2)
	assert.Len(t, f.addFloat64GaugeOptions(Metric{Description: "d", Unit: "1"}), 2)
	assert.Len(t, f.addFloat64HistogramOptions(Metric{Description: "d", Unit: "s", Buckets: []float64{1}}), 3)
}

func TestFloat64BuildersRejectNonFiniteValues(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	f := newTestFactory(t)

	counter, err := f.Float64Counter(Metric{Name: "finite_cost_usd"})
	require.NoError(t, err)
	gauge, err := f.Float64Gauge(Metric{Name: "finite_gauge"})
	require.NoError(t, err)
	histogram, err := f.Float64Histogram(Metric{Name: "finite_duration"})
	require.NoError(t, err)

	for _, tc := range []struct {
		name  string
		value float64
	}{
		{"NaN", math.NaN()},
		{"positive infinity", math.Inf(1)},
		{"negative infinity", math.Inf(-1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// -Inf must report as not-finite rather than as a negative value,
			// so the finite check runs before the counter's sign check.
			assert.ErrorIs(t, counter.Add(ctx, tc.value), ErrValueNotFinite)
			assert.ErrorIs(t, gauge.Set(ctx, tc.value), ErrValueNotFinite)
			assert.ErrorIs(t, histogram.Record(ctx, tc.value), ErrValueNotFinite)
		})
	}

	// Finite values still pass, and a negative finite counter value still
	// reports the sign error rather than the new one.
	assert.NoError(t, counter.Add(ctx, 0.5))
	assert.NoError(t, gauge.Set(ctx, -3.25))
	assert.NoError(t, histogram.Record(ctx, 0))
	assert.ErrorIs(t, counter.Add(ctx, -0.5), ErrNegativeCounterValue)
}
