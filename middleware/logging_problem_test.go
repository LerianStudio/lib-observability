//go:build unit

package middleware

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	observability "github.com/LerianStudio/lib-observability/v4"
	obslog "github.com/LerianStudio/lib-observability/v4/log"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cpfLike is a CPF-shaped input a refusal body may echo.
const cpfLike = "12345678909"

// libCommonsErrorResponse mirrors lib-commons ErrorResponse (commons/net/http),
// the body of every RespondError: idempotency and rate-limit refusals among them.
type libCommonsErrorResponse struct {
	Code    int    `json:"code"`
	Title   string `json:"title"`
	Message string `json:"message"`
}

func sendBody(status int, contentType, body string) fiber.Handler {
	return func(c fiber.Ctx) error {
		c.Status(status)
		c.Set(fiber.HeaderContentType, contentType)

		return c.SendString(body)
	}
}

// huma422 is a Huma v2.39.1 validation refusal; both messages and every value echo the input.
const huma422 = `{"title":"Unprocessable Entity","status":422,"detail":"validation failed","errors":[` +
	`{"message":"expected length <= 11","location":"body.cpf","value":"` + cpfLike + `00"},` +
	`{"message":"expected string to be RFC 3986 uri: parse \"` + cpfLike + `%zz\": invalid URL escape \"%zz\"",` +
	`"location":"body.site","value":"` + cpfLike + `%zz"}]}`

func gzipped(t *testing.T, text string) []byte {
	t.Helper()

	var buf bytes.Buffer

	writer := gzip.NewWriter(&buf)
	_, err := writer.Write([]byte(text))
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	return buf.Bytes()
}

func humaErrors(n int) string {
	entries := make([]string, n)
	for i := range n {
		entries[i] = fmt.Sprintf(`{"message":"expected number","location":"body.f%d","value":%q}`, i, cpfLike)
	}

	return `{"title":"Unprocessable Entity","detail":"validation failed","errors":[` + strings.Join(entries, ",") + `]}`
}

func TestWithHTTPLoggingLogsProblemReasonOnRefusedRequests(t *testing.T) {
	t.Parallel()

	const problemJSON = "application/problem+json"

	longDetail := strings.Repeat("d", 300)
	runeAtCap := strings.Repeat("a", 127) + "éé"
	gzipBody := gzipped(t, "Bad Request")
	huma422Fields := map[string]any{
		"problem_title":  "Unprocessable Entity",
		"problem_detail": "validation failed",
		"problem_errors": []string{
			"body.cpf: expected length <= 11",
			"body.site: expected string to be RFC 3986 uri",
		},
	}
	firstTen := make([]string, maxProblemErrors)
	for i := range firstTen {
		firstTen[i] = fmt.Sprintf("body.f%d: expected number", i)
	}
	hugeBody := `{"code":"X-0001","title":"Bad Request","detail":"` + strings.Repeat("x", 9<<10) + `"}`

	tests := []struct {
		name      string
		handler   fiber.Handler
		want      map[string]any
		wantLevel int
	}{
		{
			name:      "huma 422 logs location and message, never the input",
			handler:   sendBody(http.StatusUnprocessableEntity, problemJSON, huma422),
			want:      huma422Fields,
			wantLevel: obslog.LevelWarn,
		},
		{
			name:      "json mislabelled as text still goes through the struct",
			handler:   sendBody(http.StatusUnprocessableEntity, "text/plain", "  "+huma422),
			want:      huma422Fields,
			wantLevel: obslog.LevelWarn,
		},
		{
			name:      "json media type matches without regard to case",
			handler:   sendBody(http.StatusUnprocessableEntity, "Application/Problem+JSON", `[{"location":"body.cpf","value":"`+cpfLike+`"}]`),
			want:      map[string]any{},
			wantLevel: obslog.LevelWarn,
		},
		{
			name:      "json array labelled as text never becomes problem_text",
			handler:   sendBody(http.StatusUnprocessableEntity, "text/plain", ` [{"location":"body.cpf","value":"`+cpfLike+`"}]`),
			want:      map[string]any{},
			wantLevel: obslog.LevelWarn,
		},
		{
			name:    "more than ten errors keeps ten and counts the rest",
			handler: sendBody(http.StatusUnprocessableEntity, problemJSON, humaErrors(12)),
			want: map[string]any{
				"problem_title":          "Unprocessable Entity",
				"problem_detail":         "validation failed",
				"problem_errors":         firstTen,
				"problem_errors_dropped": 2,
			},
			wantLevel: obslog.LevelWarn,
		},
		{
			name:      "coded problem logs the detail",
			handler:   sendBody(http.StatusBadRequest, problemJSON, `{"code":"CLT-0042","title":"Bad Request","detail":"cpf `+cpfLike+` invalid"}`),
			want:      map[string]any{"problem_code": "CLT-0042", "problem_title": "Bad Request", "problem_detail": "cpf " + cpfLike + " invalid"},
			wantLevel: obslog.LevelWarn,
		},
		{
			name: "lib-commons int code decodes",
			handler: func(c fiber.Ctx) error {
				return c.Status(http.StatusBadRequest).JSON(libCommonsErrorResponse{
					Code:    http.StatusBadRequest,
					Title:   "IDEMPOTENCY_KEY_REQUIRED",
					Message: "X-Idempotency header is required for this request",
				})
			},
			want: map[string]any{
				"problem_code":   "400",
				"problem_title":  "IDEMPOTENCY_KEY_REQUIRED",
				"problem_detail": "X-Idempotency header is required for this request",
			},
			wantLevel: obslog.LevelWarn,
		},
		{
			name:      "detail equal to the title is omitted",
			handler:   sendBody(http.StatusBadRequest, problemJSON, `{"code":"CLT-0006","title":"Bad Request","detail":"Bad Request"}`),
			want:      map[string]any{"problem_code": "CLT-0006", "problem_title": "Bad Request"},
			wantLevel: obslog.LevelWarn,
		},
		{
			name: "sensitive location logs no value, its parse error cut",
			handler: sendBody(http.StatusUnprocessableEntity, problemJSON, `{"errors":[`+
				`{"location":"body.holder.email","message":"expected string to be RFC 5322 email: mail: no @ in joao.silva","value":"joao.silva"},`+
				`{"location":"header.Authorization","message":"expected string","value":"Bearer eyJhbGciOi"}]}`),
			want: map[string]any{"problem_errors": []string{
				"body.holder.email: expected string to be RFC 5322 email",
				"header.Authorization: expected string",
			}},
			wantLevel: obslog.LevelWarn,
		},
		{
			name: "malformed body is never echoed through the bare body location",
			handler: sendBody(http.StatusBadRequest, problemJSON, `{"title":"Bad Request","errors":[{"location":"body",`+
				`"message":"invalid character '}' looking for beginning of object key string",`+
				`"value":"{\"clientId\":\"abc\",\"clientSecret\":\"s3cr3t-XYZ\",}"}]}`),
			want: map[string]any{
				"problem_title":  "Bad Request",
				"problem_errors": []string{"body: invalid character '}' looking for beginning of object key string"},
			},
			wantLevel: obslog.LevelWarn,
		},
		{
			name: "form body of a 415 is never echoed",
			handler: sendBody(http.StatusUnsupportedMediaType, problemJSON, `{"title":"Unsupported Media Type","errors":[`+
				`{"location":"body","message":"unknown content type","value":"client_id=abc&client_secret=s3cr3t-XYZ"}]}`),
			want: map[string]any{
				"problem_title":  "Unsupported Media Type",
				"problem_errors": []string{"body: unknown content type"},
			},
			wantLevel: obslog.LevelWarn,
		},
		{
			name: "no value is logged: string, number or container",
			handler: sendBody(http.StatusUnprocessableEntity, problemJSON, `{"errors":[`+
				`{"location":"body.ref","message":"expected length <= 10","value":"`+longDetail+`"},`+
				`{"location":"body.document","message":"expected string","value":`+cpfLike+`},`+
				`{"location":"body","message":"expected required property cpf to be present","value":{"nome":"JOAO"}}]}`),
			want: map[string]any{"problem_errors": []string{
				"body.ref: expected length <= 10",
				"body.document: expected string",
				"body: expected required property cpf to be present",
			}},
			wantLevel: obslog.LevelWarn,
		},
		{
			name:      "coded detail is capped at 128 bytes",
			handler:   sendBody(http.StatusUnprocessableEntity, problemJSON, `{"code":"CLT-0006","detail":"`+longDetail+`"}`),
			want:      map[string]any{"problem_code": "CLT-0006", "problem_detail": longDetail[:128]},
			wantLevel: obslog.LevelWarn,
		},
		{
			name:      "coded 5xx logs the detail",
			handler:   sendBody(http.StatusBadGateway, problemJSON, `{"code":"CLT-0099","title":"Bad Gateway","detail":"upstream said no"}`),
			want:      map[string]any{"problem_code": "CLT-0099", "problem_title": "Bad Gateway", "problem_detail": "upstream said no"},
			wantLevel: obslog.LevelError,
		},
		{
			name:      "uncoded detail is capped at 128 bytes",
			handler:   sendBody(http.StatusConflict, problemJSON, `{"title":"Conflict","detail":"`+longDetail+`"}`),
			want:      map[string]any{"problem_title": "Conflict", "problem_detail": longDetail[:128]},
			wantLevel: obslog.LevelWarn,
		},
		{
			name:      "cap never splits a rune",
			handler:   sendBody(http.StatusConflict, problemJSON, `{"detail":"`+runeAtCap+`"}`),
			want:      map[string]any{"problem_detail": runeAtCap[:127]},
			wantLevel: obslog.LevelWarn,
		},
		{
			name:      "old format code title message",
			handler:   sendBody(http.StatusNotFound, "application/json", `{"code":"0007","title":"Entity Not Found","message":"holder `+cpfLike+` not found"}`),
			want:      map[string]any{"problem_code": "0007", "problem_title": "Entity Not Found", "problem_detail": "holder " + cpfLike + " not found"},
			wantLevel: obslog.LevelWarn,
		},
		{
			name:      "old format without code falls back to message",
			handler:   sendBody(http.StatusForbidden, "application/json; charset=utf-8", `{"title":"Forbidden","message":"insufficient privileges"}`),
			want:      map[string]any{"problem_title": "Forbidden", "problem_detail": "insufficient privileges"},
			wantLevel: obslog.LevelWarn,
		},
		{
			name:      "plain text body",
			handler:   sendBody(http.StatusForbidden, "text/plain; charset=utf-8", "Forbidden"),
			want:      map[string]any{"problem_text": "Forbidden"},
			wantLevel: obslog.LevelWarn,
		},
		{
			name:      "plain text is capped at 128 bytes",
			handler:   sendBody(http.StatusUnauthorized, "text/plain", strings.Repeat("u", 200)),
			want:      map[string]any{"problem_text": strings.Repeat("u", 128)},
			wantLevel: obslog.LevelWarn,
		},
		{
			name:      "malformed json adds nothing",
			handler:   sendBody(http.StatusBadRequest, problemJSON, `{"title":"Bad Request","errors":[`),
			want:      map[string]any{},
			wantLevel: obslog.LevelWarn,
		},
		{
			name:      "body over 8 KiB adds nothing",
			handler:   sendBody(http.StatusBadRequest, problemJSON, hugeBody),
			want:      map[string]any{},
			wantLevel: obslog.LevelWarn,
		},
		{
			name: "encoded body adds nothing",
			handler: func(c fiber.Ctx) error {
				c.Status(http.StatusBadRequest)
				c.Set(fiber.HeaderContentType, "text/plain")
				c.Set(fiber.HeaderContentEncoding, "gzip")

				return c.Send(gzipBody)
			},
			want:      map[string]any{},
			wantLevel: obslog.LevelWarn,
		},
		{
			name:      "2xx body is never read",
			handler:   sendBody(http.StatusOK, problemJSON, `{"code":"X","title":"T","detail":"D"}`),
			want:      map[string]any{},
			wantLevel: obslog.LevelDebug,
		},
		{
			name:      "5xx keeps error level and carries the reason",
			handler:   sendBody(http.StatusServiceUnavailable, problemJSON, `{"title":"Service Unavailable","detail":"internal error"}`),
			want:      map[string]any{"problem_title": "Service Unavailable", "problem_detail": "internal error"},
			wantLevel: obslog.LevelError,
		},
		{
			name: "streamed refusal is never consumed",
			handler: func(c fiber.Ctx) error {
				c.Status(http.StatusBadRequest)
				c.Set(fiber.HeaderContentType, "text/plain")

				return c.SendStream(strings.NewReader("Bad Request"))
			},
			want:      map[string]any{},
			wantLevel: obslog.LevelWarn,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			logger := &captureLogger{}
			app := fiber.New()
			app.Use(WithHTTPLogging(WithCustomLogger(logger)))
			app.Post("/v1/holders", tt.handler)

			resp, err := app.Test(httptest.NewRequest(http.MethodPost, "/v1/holders", nil))
			require.NoError(t, err)
			defer func() { require.NoError(t, resp.Body.Close()) }()

			_, err = io.ReadAll(resp.Body)
			require.NoError(t, err)

			messages, fields := logger.snapshot()
			require.Len(t, messages, 1, "one line per request, never a second")
			assert.Equal(t, []int{tt.wantLevel}, logger.levelSnapshot())
			assert.Equal(t, tt.want, problemFieldsOf(fields))
			for _, secret := range []string{"joao.silva", "eyJhbGciOi", "JOAO", "s3cr3t"} {
				assert.NotContains(t, fmt.Sprint(fields), secret)
			}
		})
	}
}

// problem_text is dropped only when it repeats the line's error field.
func TestWithHTTPLoggingProblemTextForReturnedError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		errorHandler fiber.ErrorHandler
		returned     error
		wantBody     string
		wantError    string
		want         map[string]any
	}{
		{
			name:         "rendered text equal to the error is omitted",
			errorHandler: fiber.DefaultErrorHandler,
			returned:     fiber.ErrNotFound,
			wantBody:     "Not Found",
			wantError:    "Not Found",
			want:         map[string]any{},
		},
		{
			name: "rendered text different from the error is kept",
			errorHandler: func(c fiber.Ctx, _ error) error {
				return c.Status(http.StatusForbidden).SendString("access denied by policy")
			},
			returned:  errors.New("tenant mismatch"),
			wantBody:  "access denied by policy",
			wantError: "tenant mismatch",
			want:      map[string]any{"problem_text": "access denied by policy"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			logger := &captureLogger{}
			app := fiber.New(fiber.Config{ErrorHandler: tt.errorHandler})
			app.Use(WithHTTPLogging(WithCustomLogger(logger)))
			app.Use(WithHTTPErrorHandling())
			app.Get("/v1/holders", func(fiber.Ctx) error { return tt.returned })

			resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/v1/holders", nil))
			require.NoError(t, err)
			defer func() { require.NoError(t, resp.Body.Close()) }()

			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, tt.wantBody, string(body), "precondition: the error is rendered inside the logging scope")

			_, fields := logger.snapshot()
			assert.Contains(t, fields, obslog.String("error", tt.wantError))
			assert.Equal(t, tt.want, problemFieldsOf(fields))
		})
	}
}

func problemFieldsOf(fields []obslog.Field) map[string]any {
	out := map[string]any{}

	for _, f := range fields {
		if strings.HasPrefix(f.Key, "problem_") {
			out[f.Key] = f.Value
		}
	}

	return out
}

// A 5xx body hides its cause; the line's error field carries it.
func TestWithHTTPLoggingRecordedCause(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		returned  error
		wantError string
	}{
		{name: "recorded cause fills the error field", wantError: "pq: connection refused"},
		{name: "chain error wins over the cause", returned: errors.New("chain failed"), wantError: "chain failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			logger := &captureLogger{}
			app := fiber.New()
			app.Use(WithHTTPLogging(WithCustomLogger(logger)))
			app.Get("/v1/holders", func(c fiber.Ctx) error {
				observability.RecordErrorCause(c.Context(), errors.New("pq: connection refused"))

				if tt.returned != nil {
					return tt.returned
				}

				return sendBody(http.StatusInternalServerError, "application/problem+json",
					`{"title":"Internal Server Error","status":500,"detail":"internal error"}`)(c)
			})

			resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/v1/holders", nil))
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())

			_, fields := logger.snapshot()

			var errorFields []obslog.Field

			for _, f := range fields {
				if f.Key == "error" {
					errorFields = append(errorFields, f)
				}
			}

			assert.Equal(t, []obslog.Field{obslog.String("error", tt.wantError)}, errorFields)
		})
	}
}
