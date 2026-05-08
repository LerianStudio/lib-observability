package streaming

import (
	"encoding/json"
	"time"

	"github.com/LerianStudio/lib-observability/log"
	"github.com/LerianStudio/lib-observability/metrics"
	"go.opentelemetry.io/otel/trace"
)

// ProducerConfig holds the configuration values referenced by telemetry instrumentation.
// Only fields needed for span attributes and metrics are kept here; kafka-client
// configuration lives in the service that constructs the producer.
type ProducerConfig struct {
	// ClientID is the Kafka client identifier recorded on streaming.emit spans.
	ClientID string
}

// Event is the CloudEvents-aligned envelope produced by a service method.
// Field names are spelled out and free of Kafka/AMQP vocabulary.
type Event struct {
	TenantID      string
	ResourceType  string
	EventType     string
	EventID       string
	SchemaVersion string
	Timestamp     time.Time
	Source        string

	Subject         string
	DataContentType string
	DataSchema      string

	// SystemEvent marks this event as platform-level (not tenant-scoped).
	SystemEvent bool
	Payload     json.RawMessage
}

// Topic returns the derived Kafka topic name for this event.
// Base form: "lerian.streaming.<ResourceType>.<EventType>".
func (e *Event) Topic() string {
	if e == nil {
		return ""
	}

	return topicPrefix + e.ResourceType + "." + e.EventType
}

const topicPrefix = "lerian.streaming."

// PartitionKey returns the default Kafka partition key for this event.
func (e *Event) PartitionKey() string {
	if e == nil {
		return ""
	}

	if e.SystemEvent {
		return "system:" + e.EventType
	}

	return e.TenantID
}

// Producer holds the telemetry-relevant state for a streaming producer.
// It does NOT hold a kafka client — that lives in the service layer.
type Producer struct {
	cfg        ProducerConfig
	producerID string
	logger     log.Logger
	tracer     trace.Tracer
	partFn     func(Event) string
	metrics    *streamingMetrics
}

// NewProducer constructs a Producer with the given telemetry configuration.
// factory may be nil — in that case metrics are disabled and a single WARN is logged.
func NewProducer(cfg ProducerConfig, producerID string, logger log.Logger, tracer trace.Tracer, factory *metrics.MetricsFactory) *Producer {
	if logger == nil {
		logger = log.NewNop()
	}

	return &Producer{
		cfg:        cfg,
		producerID: producerID,
		logger:     logger,
		tracer:     tracer,
		metrics:    newStreamingMetrics(factory, logger),
	}
}

// WithPartitionKey overrides the default partition key function for this producer.
func (p *Producer) WithPartitionKey(fn func(Event) string) {
	p.partFn = fn
}
