//go:build unit

// This file deliberately lives in the EXTERNAL test package (log_test) and
// declares its interfaces using ONLY universal types. It is the regression
// test for the reason log.Contract exists: if anyone reintroduces a
// package-defined type (log.Level, log.Field, or a self-returning method)
// into a Contract signature, the declarations below stop compiling.
//
// Read it as a stand-in for lib-commons, midaz, or any other consumer that
// wants to depend on "something that logs" without importing this module.
package log_test

import (
	"context"
	"testing"

	"github.com/LerianStudio/lib-observability/v3/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// consumerLogger is what a downstream module declares in its OWN package.
// Note what it does NOT reference: no log.Level, no log.Field, no
// self-returning method. Only predeclared types and context.Context.
//
// This exact declaration, copied into a consumer's package, lets that
// consumer accept log.AsContract(...) with no import of lib-observability at
// all — which is what keeps this module's major version from crossing the
// module boundary.
type consumerLogger interface {
	Log(ctx context.Context, level int, msg string, kv ...any)
	Enabled(level int) bool
	Sync(ctx context.Context) error
}

// Compile-time proof, in both directions:
//
//  1. the adapter satisfies a locally declared, structurally identical
//     interface; and
//  2. log.Contract itself is assignable to it, so the two are the
//     same interface type as far as Go is concerned.
var (
	_ consumerLogger = log.AsContract(log.NewNop())
	_ consumerLogger = (log.Contract)(nil)
	_ log.Contract   = (consumerLogger)(nil)
)

// countingConsumerLogger is a consumer-side implementation built only from
// universal types, proving the traffic also flows the other way: a consumer
// can hand ITS logger to code expecting a log.Contract.
type countingConsumerLogger struct {
	logs    int
	lastKV  []any
	enabled bool
}

func (c *countingConsumerLogger) Log(_ context.Context, _ int, _ string, kv ...any) {
	c.logs++
	c.lastKV = kv
}

func (c *countingConsumerLogger) Enabled(int) bool { return c.enabled }

func (c *countingConsumerLogger) Sync(context.Context) error { return nil }

func TestConsumerDeclaredInterfaceIsSatisfiedByAdapter(t *testing.T) {
	inner := log.NewNop()

	// The assignment itself is the assertion: it only compiles while every
	// Contract method uses universal types.
	var declaredLocally consumerLogger = log.AsContract(inner)

	require.NotNil(t, declaredLocally)

	// And back again, without a conversion.
	var asLibType log.Contract = declaredLocally

	require.NotNil(t, asLibType)

	assert.NotPanics(t, func() {
		declaredLocally.Log(context.Background(), 2, "msg", "k", "v")
	})
	assert.False(t, declaredLocally.Enabled(0))
	require.NoError(t, declaredLocally.Sync(context.Background()))
}

func TestConsumerImplementationIsAcceptedByLibHelpers(t *testing.T) {
	consumer := &countingConsumerLogger{enabled: true}

	// A consumer-authored implementation, never having seen log.Logger,
	// flows through the library's composition helpers.
	var u log.Contract = consumer

	u = log.ContractWith(u, "service", "ledger")
	u = log.ContractWithGroup(u, "http")
	u.Log(context.Background(), 2, "msg", "request_id", "abc")

	assert.Equal(t, 1, consumer.logs)
	assert.Equal(t, []any{"group", "http", "service", "ledger", "request_id", "abc"}, consumer.lastKV)
	assert.True(t, u.Enabled(3))
}

func TestContractAdapterHasNoSelfReturningMethod(t *testing.T) {
	// ContractWith and ContractWithGroup are free functions precisely so
	// that Contract has no self-returning method. If either ever
	// became a method, consumerLogger above would no longer be assignable
	// from the adapter and this package would fail to compile.
	u := log.AsContract(log.NewNop())

	assert.NotNil(t, log.ContractWith(u, "k", "v"))
	assert.NotNil(t, log.ContractWithGroup(u, "g"))
}
