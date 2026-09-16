package constants

// Generative-AI telemetry names, aligned to the OpenTelemetry semantic
// conventions v1.41.0 (`go.opentelemetry.io/otel/semconv/v1.41.0`, the newest
// version in the otel module that still declares the gen_ai group; v1.42.0
// dropped it from the Go package).
//
// Every name below is a semconv string, not a Lerian extension. They are
// declared here as plain constants so this package keeps zero imports and
// callers do not inherit a semconv version pin; constants/genai_test.go
// asserts each one against the semconv package.

// Telemetry attribute keys for generative-AI calls.
//
//nolint:gosec // G101 false positive: these are OpenTelemetry attribute and metric names, not credentials.
const (
	// AttrGenAIProviderName is the provider serving the request, e.g. "anthropic".
	// It supersedes the older gen_ai.system, which semconv has removed.
	AttrGenAIProviderName = "gen_ai.provider.name"
	// AttrGenAIOperationName is the operation performed, e.g. "chat" or "execute_tool".
	AttrGenAIOperationName = "gen_ai.operation.name"
	// AttrGenAIRequestModel is the model the client asked for.
	AttrGenAIRequestModel = "gen_ai.request.model"
	// AttrGenAIResponseModel is the model that actually answered.
	AttrGenAIResponseModel = "gen_ai.response.model"
	// AttrGenAIResponseFinishReasons lists why the model stopped generating.
	AttrGenAIResponseFinishReasons = "gen_ai.response.finish_reasons"
	// AttrGenAITokenType distinguishes input from output tokens.
	// Required dimension of MetricGenAIClientTokenUsage.
	AttrGenAITokenType = "gen_ai.token.type"
)

// Telemetry attribute keys for generative-AI token usage.
//
//nolint:gosec // G101 false positive: these are OpenTelemetry attribute and metric names, not credentials.
const (
	// AttrGenAIUsageInputTokens counts tokens sent to the model.
	AttrGenAIUsageInputTokens = "gen_ai.usage.input_tokens"
	// AttrGenAIUsageOutputTokens counts tokens produced by the model.
	AttrGenAIUsageOutputTokens = "gen_ai.usage.output_tokens"
	// AttrGenAIUsageCacheReadInputTokens counts input tokens served from the provider's prompt cache.
	AttrGenAIUsageCacheReadInputTokens = "gen_ai.usage.cache_read.input_tokens"
	// AttrGenAIUsageCacheCreationInputTokens counts input tokens written to the provider's prompt cache.
	AttrGenAIUsageCacheCreationInputTokens = "gen_ai.usage.cache_creation.input_tokens"
)

// Telemetry metric names for generative-AI clients.
//
//nolint:gosec // G101 false positive: these are OpenTelemetry attribute and metric names, not credentials.
const (
	// MetricGenAIClientTokenUsage is the histogram of tokens used per operation.
	// Dimensions: AttrGenAIOperationName, AttrGenAIProviderName, AttrGenAITokenType.
	MetricGenAIClientTokenUsage = "gen_ai.client.token.usage"
	// MetricGenAIClientOperationDuration is the histogram of client operation duration, in seconds.
	MetricGenAIClientOperationDuration = "gen_ai.client.operation.duration"
)
