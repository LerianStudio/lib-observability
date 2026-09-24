package zap

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelzap"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	callerSkipFrames = 1
	encodingConsole  = "console"
	encodingJSON     = "json"
)

// Environment controls the baseline logger profile.
type Environment string

const (
	// EnvironmentProduction enables production-safe logging defaults.
	EnvironmentProduction Environment = "production"
	// EnvironmentStaging enables staging-safe logging defaults.
	EnvironmentStaging Environment = "staging"
	// EnvironmentUAT enables UAT-safe logging defaults.
	EnvironmentUAT Environment = "uat"
	// EnvironmentDevelopment enables verbose development logging defaults.
	EnvironmentDevelopment Environment = "development"
	// EnvironmentLocal enables verbose local-development logging defaults.
	EnvironmentLocal Environment = "local"
)

// Config contains all required logger initialization inputs.
type Config struct {
	Environment     Environment
	Level           string
	OTelLibraryName string
	// Output receives every encoded log entry when set. Nil keeps today's
	// behaviour: zap's own stderr sink. The caller owns the writer's lifecycle
	// (rotation, redaction, flushing, closing); the logger only writes to it.
	// Logger.Sync forwards to Output only when Output implements Sync() error;
	// a buffered writer without one (a *bufio.Writer) must be flushed by the
	// caller, or Sync reports success with entries still in the buffer.
	Output io.Writer
	// Encoding selects the encoder: "json" or "console". Empty keeps the
	// package default. Precedence mirrors Level versus LOG_LEVEL: this field
	// wins when set, LOG_ENCODING is the fallback when it is empty, and the
	// Environment decides when neither is set (console for development and
	// local, json everywhere else). An unrecognised value makes New return an
	// error - never a silent fallback - so a config typo is visible at start-up.
	// Set this when the deployment environment and the wanted encoding
	// disagree: a development-environment process whose own config asks for
	// JSON no longer has to claim it runs in production.
	Encoding string
	// DisableSampling removes the sampler from the logger, whatever the
	// profile. The production profile samples at 100:100: past the 100th copy
	// of one message inside a second, only every 100th is written and the rest
	// are dropped with no record that they existed. That is the right trade for
	// a service under load and the wrong one for a diagnostic log a human reads
	// afterwards - an agent harness, a CLI, a job whose output is the
	// deliverable - where a dropped line is a lost clue. False keeps sampling.
	DisableSampling bool
}

func (c Config) validate() error {
	if c.OTelLibraryName == "" {
		return errors.New("OTelLibraryName is required")
	}

	switch c.Environment {
	case EnvironmentProduction, EnvironmentStaging, EnvironmentUAT, EnvironmentDevelopment, EnvironmentLocal:
	default:
		return fmt.Errorf("invalid environment %q", c.Environment)
	}

	switch c.Encoding {
	case "", encodingJSON, encodingConsole:
		return nil
	default:
		return fmt.Errorf("invalid encoding %q", c.Encoding)
	}
}

// New creates a structured logger from the given configuration.
//
// The returned Logger implements log.Logger and stores the runtime-adjustable
// level handle internally. Use Logger.Level() to access it.
func New(cfg Config) (*Logger, error) {
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid zap config: %w", err)
	}

	baseConfig := buildConfigByEnvironment(cfg)

	level, err := resolveLevel(cfg)
	if err != nil {
		return nil, err
	}

	baseConfig.Level = level
	baseConfig.DisableStacktrace = true

	// Both samplers read baseConfig.Sampling: zap.Config.Build installs one
	// around the default stderr core, and outputCore installs one around the
	// Output core. Clearing it here removes the knob's target on both roads.
	if cfg.DisableSampling {
		baseConfig.Sampling = nil
	}

	coreOptions := []zap.Option{
		zap.AddCallerSkip(callerSkipFrames),
		zap.WrapCore(func(core zapcore.Core) zapcore.Core {
			if cfg.Output != nil {
				core = outputCore(baseConfig, level, cfg.Output)
			}

			return zapcore.NewTee(core, levelGate{inner: otelzap.NewCore(cfg.OTelLibraryName), level: level})
		}),
	}

	built, err := baseConfig.Build(coreOptions...)
	if err != nil {
		return nil, fmt.Errorf("failed to build logger: %w", err)
	}

	return &Logger{
		logger:          built,
		atomicLevel:     level,
		consoleEncoding: baseConfig.Encoding == encodingConsole,
	}, nil
}

// outputCore mirrors the core zap.Config.Build would have produced for stderr -
// same encoder, same level, same sampling - but writes to w instead.
func outputCore(baseConfig zap.Config, level zap.AtomicLevel, w io.Writer) zapcore.Core {
	var encoder zapcore.Encoder
	if baseConfig.Encoding == encodingConsole {
		encoder = zapcore.NewConsoleEncoder(baseConfig.EncoderConfig)
	} else {
		encoder = zapcore.NewJSONEncoder(baseConfig.EncoderConfig)
	}

	core := zapcore.NewCore(encoder, zapcore.Lock(zapcore.AddSync(w)), level)

	if s := baseConfig.Sampling; s != nil {
		core = zapcore.NewSamplerWithOptions(core, time.Second, s.Initial, s.Thereafter)
	}

	return core
}

// levelGate holds a core to the logger's configured level. The otelzap bridge
// core carries no level of its own - it asks the OTel LoggerProvider, and the
// SDK provider enables every severity - while zapcore.Tee accepts an entry when
// any child does. Ungated, the bridge would export over OTLP every entry the
// Output core drops. The gate shares the logger's AtomicLevel, so a runtime
// Level().SetLevel reaches both outputs.
//
// zapcore.NewIncreaseLevelCore is not usable here: its constructor refuses a
// core that is disabled at a level the enabler allows, and the OTel global
// delegate reports every level disabled until an SDK provider is installed,
// which services do after building this logger.
type levelGate struct {
	inner zapcore.Core
	level zap.AtomicLevel
}

func (g levelGate) Enabled(l zapcore.Level) bool {
	return g.level.Enabled(l) && g.inner.Enabled(l)
}

func (g levelGate) With(fields []zapcore.Field) zapcore.Core {
	return levelGate{inner: g.inner.With(fields), level: g.level}
}

func (g levelGate) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if !g.level.Enabled(ent.Level) {
		return ce
	}

	return g.inner.Check(ent, ce)
}

func (g levelGate) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	return g.inner.Write(ent, fields)
}

func (g levelGate) Sync() error {
	return g.inner.Sync()
}

func resolveLevel(cfg Config) (zap.AtomicLevel, error) {
	levelStr := cfg.Level
	if strings.TrimSpace(levelStr) == "" {
		levelStr = strings.TrimSpace(os.Getenv("LOG_LEVEL"))
	}

	if levelStr != "" {
		var parsed zapcore.Level
		if err := parsed.Set(levelStr); err != nil {
			return zap.AtomicLevel{}, fmt.Errorf("invalid level %q: %w", levelStr, err)
		}

		return zap.NewAtomicLevelAt(parsed), nil
	}

	if cfg.Environment == EnvironmentDevelopment || cfg.Environment == EnvironmentLocal {
		return zap.NewAtomicLevelAt(zapcore.DebugLevel), nil
	}

	return zap.NewAtomicLevelAt(zapcore.InfoLevel), nil
}

func buildConfigByEnvironment(c Config) zap.Config {
	encoding := resolveEncoding(c)

	if c.Environment == EnvironmentDevelopment || c.Environment == EnvironmentLocal {
		cfg := zap.NewDevelopmentConfig()
		cfg.Encoding = encoding
		cfg.EncoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder
		cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
		cfg.EncoderConfig.TimeKey = "timestamp"

		if encoding == encodingConsole {
			cfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
		}

		return cfg
	}

	cfg := zap.NewProductionConfig()
	cfg.Encoding = encoding
	cfg.EncoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	cfg.EncoderConfig.TimeKey = "timestamp"

	return cfg
}

// resolveEncoding mirrors resolveLevel: the Config field wins, the environment
// variable is the fallback, and the Environment supplies the default. An
// unrecognised Config.Encoding never reaches here - validate rejects it first -
// while an unrecognised LOG_ENCODING keeps its long-standing behaviour of being
// ignored.
func resolveEncoding(c Config) string {
	if strings.TrimSpace(c.Encoding) != "" {
		return c.Encoding
	}

	if enc := strings.TrimSpace(os.Getenv("LOG_ENCODING")); enc == encodingJSON || enc == encodingConsole {
		return enc
	}

	if c.Environment == EnvironmentDevelopment || c.Environment == EnvironmentLocal {
		return encodingConsole
	}

	return encodingJSON
}
