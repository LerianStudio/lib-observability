//go:build unit

package constants

import (
	"testing"

	"github.com/stretchr/testify/assert"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/semconv/v1.41.0/genaiconv"
)

// The constants package itself declares these as plain strings so it keeps zero
// imports. semconv is imported here only, to prove the strings still match.
func TestGenAIAttributesMatchSemconv(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		constant string
		semconv  string
	}{
		{"provider name", AttrGenAIProviderName, string(semconv.GenAIProviderNameKey)},
		{"operation name", AttrGenAIOperationName, string(semconv.GenAIOperationNameKey)},
		{"request model", AttrGenAIRequestModel, string(semconv.GenAIRequestModelKey)},
		{"response model", AttrGenAIResponseModel, string(semconv.GenAIResponseModelKey)},
		{"finish reasons", AttrGenAIResponseFinishReasons, string(semconv.GenAIResponseFinishReasonsKey)},
		{"token type", AttrGenAITokenType, string(semconv.GenAITokenTypeKey)},
		{"input tokens", AttrGenAIUsageInputTokens, string(semconv.GenAIUsageInputTokensKey)},
		{"output tokens", AttrGenAIUsageOutputTokens, string(semconv.GenAIUsageOutputTokensKey)},
		{"cache read input tokens", AttrGenAIUsageCacheReadInputTokens, string(semconv.GenAIUsageCacheReadInputTokensKey)},
		{"cache creation input tokens", AttrGenAIUsageCacheCreationInputTokens, string(semconv.GenAIUsageCacheCreationInputTokensKey)},
	} {
		assert.Equal(t, tc.semconv, tc.constant, tc.name)
	}
}

func TestGenAIMetricNamesMatchSemconv(t *testing.T) {
	t.Parallel()

	assert.Equal(t, genaiconv.ClientTokenUsage{}.Name(), MetricGenAIClientTokenUsage)
	assert.Equal(t, genaiconv.ClientOperationDuration{}.Name(), MetricGenAIClientOperationDuration)
}

func TestDBSystemSQLiteMatchesSemconv(t *testing.T) {
	t.Parallel()

	assert.Equal(t, semconv.DBSystemNameSQLite.Value.AsString(), DBSystemSQLite)
}
