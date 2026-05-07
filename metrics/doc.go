// Package metrics provides a thread-safe MetricsFactory with lazy instrument caching
// and a fluent builder API for Counters, Gauges, and Histograms. All operations return
// explicit errors. Includes pre-configured domain metric recorders and system
// infrastructure gauges.
package metrics
