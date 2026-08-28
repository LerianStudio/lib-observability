// Package metrics provides a thread-safe MetricsFactory with lazy instrument caching
// and a fluent builder API for Counters, Gauges, and Histograms. All operations return
// explicit errors. Includes pre-configured domain metric recorders and system
// infrastructure gauges.
//
// For consumers in OTHER modules, the package also exposes Recorder and
// the AsRecorder adapter: the recording surface flattened into three calls
// declared with universal types only, so a downstream module can declare an
// equivalent interface in its own package and stop inheriting this module's major
// version. See universal.go for the full rationale.
package metrics
