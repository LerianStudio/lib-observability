//go:build unit

package middleware

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	obslog "github.com/LerianStudio/lib-observability/v4/log"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cpfLike is a CPF-shaped input a refusal body may echo; it must never reach the line.
const cpfLike = "12345678909"

func sendBody(status int, contentType, body string) fiber.Handler {
	return func(c fiber.Ctx) error {
		c.Status(status)
		c.Set(fiber.HeaderContentType, contentType)

		return c.SendString(body)
	}
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
	hugeBody := `{"code":"X-0001","title":"Bad Request","detail":"` + strings.Repeat("x", 9<<10) + `"}`

	tests := []struct {
		name      string
		handler   fiber.Handler
		want      map[string]any
		wantLevel int
	}{
		{
			name: "huma 422 logs location and message, never the value",
			handler: sendBody(http.StatusUnprocessableEntity, problemJSON, `{"title":"Unprocessable Entity","status":422,`+
				`"detail":"validation failed","errors":[`+
				`{"message":"expected length <= 11","location":"body.cpf","value":"`+cpfLike+`"},`+
				`{"message":"expected string to be RFC 3339 date-time: parsing time \"`+cpfLike+`\"","location":"body.nascimento","value":"`+cpfLike+`"}]}`),
			want: map[string]any{
				"problem_title":  "Unprocessable Entity",
				"problem_detail": "validation failed",
				"problem_errors": []string{"body.cpf: expected length <= 11", "body.nascimento: expected string to be RFC 3339 date-time"},
			},
			wantLevel: obslog.LevelWarn,
		},
		{
			name:    "more than ten errors keeps ten and counts the rest",
			handler: sendBody(http.StatusUnprocessableEntity, problemJSON, humaErrors(12)),
			want: map[string]any{
				"problem_title":  "Unprocessable Entity",
				"problem_detail": "validation failed",
				"problem_errors": []string{
					"body.f0: expected number", "body.f1: expected number", "body.f2: expected number",
					"body.f3: expected number", "body.f4: expected number", "body.f5: expected number",
					"body.f6: expected number", "body.f7: expected number", "body.f8: expected number",
					"body.f9: expected number",
				},
				"problem_errors_dropped": 2,
			},
			wantLevel: obslog.LevelWarn,
		},
		{
			name:      "coded problem omits the detail",
			handler:   sendBody(http.StatusBadRequest, problemJSON, `{"code":"CLT-0042","title":"Bad Request","detail":"cpf `+cpfLike+` invalid"}`),
			want:      map[string]any{"problem_code": "CLT-0042", "problem_title": "Bad Request"},
			wantLevel: obslog.LevelWarn,
		},
		{
			name:      "uncoded detail is capped at 128 bytes",
			handler:   sendBody(http.StatusConflict, problemJSON, `{"title":"Conflict","detail":"`+longDetail+`"}`),
			want:      map[string]any{"problem_title": "Conflict", "problem_detail": longDetail[:128]},
			wantLevel: obslog.LevelWarn,
		},
		{
			name:      "old format code title message",
			handler:   sendBody(http.StatusNotFound, "application/json", `{"code":"0007","title":"Entity Not Found","message":"holder `+cpfLike+` not found"}`),
			want:      map[string]any{"problem_code": "0007", "problem_title": "Entity Not Found"},
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
			name:      "plain text is capped at 120 bytes",
			handler:   sendBody(http.StatusUnauthorized, "text/plain", strings.Repeat("u", 200)),
			want:      map[string]any{"problem_text": strings.Repeat("u", 120)},
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
			name:      "2xx body is never read",
			handler:   sendBody(http.StatusOK, problemJSON, `{"code":"X","title":"T","detail":"D"}`),
			want:      map[string]any{},
			wantLevel: obslog.LevelInfo,
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
			assert.NotContains(t, messages[0]+fmt.Sprint(fields), cpfLike)
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
