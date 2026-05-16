// Package zap provides a zap adapter implementing the Logger interface with automatic
// trace_id and span_id injection into every log entry. Bridges zap output to the
// OpenTelemetry Logs SDK via otelzap for unified log collection through the OTLP pipeline.
package zap
