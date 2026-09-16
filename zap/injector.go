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
	// (rotation, redaction, closing); the logger only writes and Syncs it.
	Output io.Writer
}

func (c Config) validate() error {
	if c.OTelLibraryName == "" {
		return errors.New("OTelLibraryName is required")
	}

	switch c.Environment {
	case EnvironmentProduction, EnvironmentStaging, EnvironmentUAT, EnvironmentDevelopment, EnvironmentLocal:
		return nil
	default:
		return fmt.Errorf("invalid environment %q", c.Environment)
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

	baseConfig := buildConfigByEnvironment(cfg.Environment)

	level, err := resolveLevel(cfg)
	if err != nil {
		return nil, err
	}

	baseConfig.Level = level
	baseConfig.DisableStacktrace = true

	coreOptions := []zap.Option{
		zap.AddCallerSkip(callerSkipFrames),
		zap.WrapCore(func(core zapcore.Core) zapcore.Core {
			if cfg.Output != nil {
				core = outputCore(baseConfig, level, cfg.Output)
			}

			return zapcore.NewTee(core, otelzap.NewCore(cfg.OTelLibraryName))
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

func buildConfigByEnvironment(environment Environment) zap.Config {
	encoding := resolveEncoding(environment)

	if environment == EnvironmentDevelopment || environment == EnvironmentLocal {
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

func resolveEncoding(environment Environment) string {
	if enc := strings.TrimSpace(os.Getenv("LOG_ENCODING")); enc != "" {
		if enc == "json" || enc == encodingConsole {
			return enc
		}
	}

	if environment == EnvironmentDevelopment || environment == EnvironmentLocal {
		return encodingConsole
	}

	return "json"
}
