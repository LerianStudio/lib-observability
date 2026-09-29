// Package zap provides a zap adapter implementing the Logger interface with automatic
// trace_id and span_id injection into every log entry. Bridges zap output to the
// OpenTelemetry Logs SDK via otelzap for unified log collection through the OTLP pipeline.
// The configured level (Config.Level, else LOG_LEVEL, else the environment
// default) gates the OTLP bridge as well as the local sink, so an entry below it
// is exported by neither; Logger.Level().SetLevel moves both.
//
// Config.Encoding picks the encoder ("json" or "console") directly, so a process
// whose own config asks for JSON no longer has to misdeclare its Environment to
// get it; empty keeps the environment-derived default. Config.DisableSampling
// turns off the production profile's 100:100 sampler, which otherwise drops
// every repeat of a message past the 100th inside a second - right for a service
// under load, wrong for a diagnostic log somebody reads afterwards.
//
// Config.ScrubDocuments (off by default) replaces every CPF/CNPJ-shaped span in
// entry messages and string-rendered field values with
// redaction.DocumentPlaceholder, on the local sink and the OTLP bridge alike,
// whether the entry comes through Log, the zap-typed helpers, With or Raw(). A
// structured field is judged as each sink renders it: the local JSON, and the
// bridge's %+v, which also prints unexported and json:"-" struct fields. Raw
// bytes and OpenTelemetry attribute values, which the bridge exports as they
// are, are judged by the text they carry, not by their base64 form. It
// is defence in depth, not a licence to format documents into log lines. The
// stdlib log.GoLogger fallback does not scrub content; call
// redaction.ScrubDocuments yourself on text you hand it.
package zap
