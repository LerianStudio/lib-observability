// Package zap provides a zap adapter implementing the Logger interface with automatic
// trace_id and span_id injection into every log entry. Bridges zap output to the
// OpenTelemetry Logs SDK via otelzap for unified log collection through the OTLP pipeline.
//
// Config.Encoding picks the encoder ("json" or "console") directly, so a process
// whose own config asks for JSON no longer has to misdeclare its Environment to
// get it; empty keeps the environment-derived default. Config.DisableSampling
// turns off the production profile's 100:100 sampler, which otherwise drops
// every repeat of a message past the 100th inside a second - right for a service
// under load, wrong for a diagnostic log somebody reads afterwards.
package zap
