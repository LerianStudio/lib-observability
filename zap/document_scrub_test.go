//go:build unit

package zap

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	logpkg "github.com/LerianStudio/lib-observability/v4/log"
	"github.com/LerianStudio/lib-observability/v4/redaction"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	scrubTestCPF  = "529.982.247-25"
	scrubTestCNPJ = "12.345.678/0001-95"
	// scrubTestLowerCNPJ is an alphanumeric CNPJ written in lowercase, which is
	// a document only after a cnpj label.
	scrubTestLowerCNPJ = "12abc34501de35"
)

// syncBuffer is a bytes.Buffer safe for the concurrent writes of the race test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func newScrubLogger(t *testing.T, w *syncBuffer, scrub bool, encoding string) *Logger {
	t.Helper()

	logger, err := New(Config{
		Environment:     EnvironmentProduction,
		Level:           "debug",
		OTelLibraryName: "svc",
		Output:          w,
		Encoding:        encoding,
		ScrubDocuments:  scrub,
		DisableSampling: true,
	})
	require.NoError(t, err)

	return logger
}

type docHolder struct {
	Note string `json:"note"`
	N    int    `json:"n"`
}

type docStringer struct{ v string }

func (s docStringer) String() string { return s.v }

type docObject struct{ v string }

func (o docObject) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddString("note", o.v)

	return nil
}

type failingObject struct{ v string }

func (o failingObject) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddString("partial", "ok")

	return errors.New("marshal " + o.v)
}

type docPanickingStringer struct{}

func (docPanickingStringer) String() string { panic("render " + scrubTestCPF) }

type panickingStringer struct{}

func (panickingStringer) String() string { panic("boom") }

type panickingError struct{}

func (panickingError) Error() string { panic("boom") }

func TestScrubDocuments_RedactsEveryLogSurface(t *testing.T) {
	cases := map[string]func(l *Logger){
		"message via Log": func(l *Logger) {
			l.Log(context.Background(), logpkg.LevelInfo, "payer "+scrubTestCPF+" rejected")
		},
		"message via Info": func(l *Logger) { l.Info("payer " + scrubTestCNPJ) },
		"string field via Log": func(l *Logger) {
			l.Log(context.Background(), logpkg.LevelInfo, "m", logpkg.String("note", "doc "+scrubTestCPF))
		},
		"error field via Log": func(l *Logger) {
			l.Log(context.Background(), logpkg.LevelError, "m", logpkg.Err(fmt.Errorf("lookup %s: not found", scrubTestCPF)))
		},
		"error field via zap": func(l *Logger) {
			l.Error("m", zap.Error(fmt.Errorf("lookup %s", scrubTestCNPJ)))
		},
		"struct field via Log": func(l *Logger) {
			l.Log(context.Background(), logpkg.LevelInfo, "m", logpkg.Any("payload", docHolder{Note: scrubTestCPF, N: 1}))
		},
		"stringer field": func(l *Logger) { l.Info("m", zap.Stringer("s", docStringer{v: scrubTestCPF})) },
		"byte string field": func(l *Logger) {
			l.Info("m", zap.ByteString("raw", []byte("doc "+scrubTestCPF)))
		},
		"object marshaler field": func(l *Logger) { l.Info("m", zap.Object("obj", docObject{v: scrubTestCPF})) },
		"string slice field":     func(l *Logger) { l.Info("m", zap.Strings("docs", []string{"a", scrubTestCPF})) },
		"With child": func(l *Logger) {
			l.With(logpkg.String("note", scrubTestCPF)).Log(context.Background(), logpkg.LevelInfo, "m")
		},
		"WithZapFields child": func(l *Logger) {
			l.WithZapFields(zap.String("note", scrubTestCNPJ)).Info("m")
		},
		"Raw logger": func(l *Logger) { l.Raw().Info("raw " + scrubTestCPF) },
		"Raw With child": func(l *Logger) {
			l.Raw().With(zap.String("note", scrubTestCPF)).Info("m")
		},
		"document glued to a label in the message": func(l *Logger) {
			l.Log(context.Background(), logpkg.LevelInfo, "cliente cpf52998224725 bloqueado")
		},
		"lowercase cnpj after a label in a field": func(l *Logger) {
			l.Log(context.Background(), logpkg.LevelInfo, "m", logpkg.String("note", "cnpj "+scrubTestLowerCNPJ))
		},
	}

	for _, encoding := range []string{"json", "console"} {
		for name, emit := range cases {
			t.Run(encoding+"/"+name, func(t *testing.T) {
				var out syncBuffer

				emit(newScrubLogger(t, &out, true, encoding))

				got := out.String()
				require.NotEmpty(t, got, "the entry must still be written")
				assert.NotContains(t, got, scrubTestCPF)
				assert.NotContains(t, got, scrubTestCNPJ)
				assert.NotContains(t, got, "52998224725")
				assert.NotContains(t, got, scrubTestLowerCNPJ)
				assert.Contains(t, got, redaction.DocumentPlaceholder)
			})
		}
	}
}

// The text zap writes for a failure (a marshaler's error, a Stringer's panic)
// goes to the sinks under "<key>Error"; it is scrubbed like any other value,
// and the failure is still reported.
func TestScrubDocuments_RedactsFailureText(t *testing.T) {
	var out syncBuffer

	logger := newScrubLogger(t, &out, true, "json")

	assert.NotPanics(t, func() {
		logger.Info("m",
			zap.Object("obj", failingObject{v: scrubTestCPF}),
			zap.Stringer("st", docPanickingStringer{}))
	})

	got := out.String()
	assert.NotContains(t, got, scrubTestCPF)
	assert.Contains(t, got, `"objError":"marshal `+redaction.DocumentPlaceholder+`"`)
	assert.Contains(t, got, `"stError":"PANIC=render `+redaction.DocumentPlaceholder+`"`)
}

func TestScrubDocuments_DisabledOutputIsByteIdentical(t *testing.T) {
	emit := func(l *Logger) {
		l.Log(context.Background(), logpkg.LevelError, "payer "+scrubTestCPF,
			logpkg.String("note", scrubTestCNPJ),
			logpkg.Err(errors.New("doc "+scrubTestCPF)),
			logpkg.Any("payload", docHolder{Note: scrubTestCPF}))
		l.Info("m", zap.Stringer("s", docStringer{v: scrubTestCPF}))
	}

	var def, off syncBuffer

	defLogger, err := New(Config{
		Environment: EnvironmentProduction, Level: "debug", OTelLibraryName: "svc",
		Output: &def, DisableSampling: true,
	})
	require.NoError(t, err)

	emit(defLogger)
	emit(newScrubLogger(t, &off, false, "json"))

	stripTime := func(s string) string {
		lines := strings.Split(strings.TrimSpace(s), "\n")
		for i, line := range lines {
			if start := strings.Index(line, `"timestamp":"`); start >= 0 {
				end := strings.Index(line[start+len(`"timestamp":"`):], `"`)
				lines[i] = line[:start] + line[start+len(`"timestamp":"`)+end+1:]
			}
		}

		return strings.Join(lines, "\n")
	}

	assert.Equal(t, stripTime(def.String()), stripTime(off.String()))
	assert.Contains(t, off.String(), scrubTestCPF, "the scrub is opt-in: disabled output keeps the text")
}

func TestScrubDocuments_UnmatchedFieldsKeepTheirShape(t *testing.T) {
	var on, off syncBuffer

	emit := func(l *Logger) {
		l.Info("order 42 settled",
			zap.String("s", "tenant-a"),
			zap.Any("payload", docHolder{Note: "ok", N: 7}),
			zap.Stringer("st", docStringer{v: "fine"}),
			zap.Object("obj", docObject{v: "fine"}),
			zap.Any("attr", attribute.ByteSliceValue([]byte("fine"))),
			zap.Any("attrs", map[string]attribute.Value{"k": attribute.StringValue("fine")}),
			zap.Binary("bin", []byte("fine")),
			zap.Int64("n", 12345))
	}

	emit(newScrubLogger(t, &on, true, "json"))
	emit(newScrubLogger(t, &off, false, "json"))

	assert.Contains(t, on.String(), `"payload":{"note":"ok","n":7}`)
	assert.Contains(t, on.String(), `"obj":{"note":"fine"}`)

	cut := func(s string) string { return s[strings.Index(s, `"msg"`):] }
	assert.Equal(t, cut(off.String()), cut(on.String()))
}

func TestScrubDocuments_UnrenderableValuesDoNotPanic(t *testing.T) {
	var out syncBuffer

	logger := newScrubLogger(t, &out, true, "json")

	var typedNil *docStringer

	assert.NotPanics(t, func() {
		logger.Info("m",
			zap.Stringer("panics", panickingStringer{}),
			zap.Error(panickingError{}),
			zap.Any("nilptr", typedNil),
			zap.Any("unmarshalable", make(chan int)))
	})
	assert.NotEmpty(t, out.String())
}

func TestScrubDocuments_NilAndZeroLoggerStayNoOps(t *testing.T) {
	var nilLogger *Logger

	zero := &Logger{}

	assert.NotPanics(t, func() {
		nilLogger.Log(context.Background(), logpkg.LevelInfo, scrubTestCPF)
		zero.Log(context.Background(), logpkg.LevelInfo, scrubTestCPF)
		zero.With(logpkg.String("k", scrubTestCPF)).Log(context.Background(), logpkg.LevelInfo, "m")
	})
}

func TestScrubDocuments_LevelAndSamplingStillApply(t *testing.T) {
	var out syncBuffer

	logger, err := New(Config{
		Environment:     EnvironmentProduction,
		Level:           "warn",
		OTelLibraryName: "svc",
		Output:          &out,
		ScrubDocuments:  true,
	})
	require.NoError(t, err)

	logger.Info("below level " + scrubTestCPF)
	assert.Empty(t, out.String(), "an entry below the level is never written")

	for range 250 {
		logger.Warn("repeat " + scrubTestCPF)
	}

	lines := strings.Count(strings.TrimSpace(out.String()), "\n") + 1
	assert.Equal(t, 101, lines, "the production 100:100 sampler must still drop repeats")

	logger.Level().SetLevel(zapcore.DebugLevel)
	logger.Debug("now enabled")
	assert.Contains(t, out.String(), "now enabled")
}

func TestScrubDocuments_ConcurrentLogging(t *testing.T) {
	var out syncBuffer

	logger := newScrubLogger(t, &out, true, "json")
	child := logger.With(logpkg.String("note", scrubTestCNPJ))

	var wg sync.WaitGroup

	for i := range 8 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for j := range 50 {
				child.Log(context.Background(), logpkg.LevelInfo, fmt.Sprintf("w%d-%d %s", i, j, scrubTestCPF),
					logpkg.Err(errors.New(scrubTestCPF)))
			}
		}()
	}

	wg.Wait()

	got := out.String()
	assert.Equal(t, 400, strings.Count(got, "\n"))
	assert.NotContains(t, got, scrubTestCPF)
	assert.NotContains(t, got, scrubTestCNPJ)
}

// Keys of a reflected map that print alike (1 and "1") must not collapse into
// one entry while the values are inspected: the one dropped could be the one
// holding the document.
func TestPlainAttributes_KeepsEveryMapEntry(t *testing.T) {
	value := map[any]attribute.Value{
		1:   attribute.ByteSliceValue([]byte(scrubTestCPF)),
		"1": attribute.StringValue("ok"),
	}

	plain, held := plainAttributes(value, 0)
	require.True(t, held)

	rendered, complete := bridgeRendering(plain)
	require.True(t, complete)
	assert.Contains(t, rendered, scrubTestCPF)
	assert.Contains(t, rendered, "ok")
}
