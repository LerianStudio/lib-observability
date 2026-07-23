// Package messagingobs provides thin, nil-safe helpers that instrument RabbitMQ
// producers and consumers with OpenTelemetry spans, trace-context propagation,
// and semantic-convention duration metrics. There is no official RabbitMQ OTel
// instrumentation package, so this is hand-rolled (ADR-006) on top of the
// trace-propagation helpers already in the tracing package
// (InjectTraceHeadersIntoQueue / ExtractTraceContextFromQueueHeaders).
//
// # Boundary (ADR-006, ADR-007)
//
// This package takes NO dependency on an AMQP client. It operates on generic
// header maps (map[string]any, the shape amqp091-go uses for
// amqp.Table/Publishing.Headers and Delivery.Headers). The application owns the
// amqp.Channel: the producer helper returns the headers to attach to the
// outgoing Publishing, and the consumer helper reads the headers off the inbound
// Delivery. This keeps the amqp dependency in the app.
//
// # Emitted telemetry (docs/metrics-contract.md)
//
//   - Producer: a producer-kind span, trace context injected into the returned
//     headers, and messaging.client.operation.duration (Float64 seconds).
//   - Consumer: a consumer-kind span joined to the producer's trace via the
//     inbound headers, and messaging.process.duration (Float64 seconds).
//
// Labels are bounded: messaging.system=rabbitmq, messaging.operation.name,
// messaging.destination.template (a TEMPLATE such as "transactions.{tenant}",
// never a concrete queue name or routing key), messaging.consumer.group.name
// (consumer only), and error.type on failures. The concrete routing key and
// message id are accepted only to be attached to the span body via
// RecordError/logs by the caller if desired — they are NEVER emitted as span or
// metric attributes here, per the FORBIDDEN list.
//
// The messaging.* names/units are shared with lib-streaming (RedPanda/Kafka) so
// a single worker dashboard is transport-agnostic.
//
// # No-op degradation (ADR-008)
//
// With nil telemetry (or a nil MeterProvider/MetricsFactory) the helpers still
// return a valid context, injectable headers, and a finish func that is a
// no-op. They never panic and never break the messaging path.
package messagingobs
