package middleware

import (
	"net/url"
	"strings"

	constant "github.com/LerianStudio/lib-observability/v2/constants"
	"github.com/LerianStudio/lib-observability/v2/redaction"
	"github.com/gofiber/fiber/v3"
)

const redactedBody = "[REDACTED]"

func getBodyObfuscatedString(c fiber.Ctx, bodyBytes []byte) string {
	contentType := strings.ToLower(c.Get(headerContentType))

	switch {
	case strings.Contains(contentType, "application/json"):
		return handleJSONBody(bodyBytes)
	case strings.Contains(contentType, "application/x-www-form-urlencoded"):
		return handleURLEncodedBody(bodyBytes)
	case strings.Contains(contentType, "multipart/form-data"):
		return handleMultipartBody(c)
	default:
		return redactedBody
	}
}

func obfuscateMapRecursively(data map[string]any, depth int) {
	if depth >= maxObfuscationDepth {
		return
	}

	for key, value := range data {
		if redaction.IsSensitiveField(key) {
			data[key] = constant.ObfuscatedValue
			continue
		}

		switch v := value.(type) {
		case map[string]any:
			obfuscateMapRecursively(v, depth+1)
		case []any:
			obfuscateSliceRecursively(v, depth+1)
		}
	}
}

func obfuscateSliceRecursively(data []any, depth int) {
	if depth >= maxObfuscationDepth {
		return
	}

	for _, item := range data {
		switch v := item.(type) {
		case map[string]any:
			obfuscateMapRecursively(v, depth+1)
		case []any:
			obfuscateSliceRecursively(v, depth+1)
		}
	}
}

func handleURLEncodedBody(bodyBytes []byte) string {
	formData, err := url.ParseQuery(string(bodyBytes))
	if err != nil {
		return redactedBody
	}

	updatedBody := url.Values{}

	for key, values := range formData {
		if redaction.IsSensitiveField(key) {
			for range values {
				updatedBody.Add(key, constant.ObfuscatedValue)
			}

			continue
		}

		for _, value := range values {
			updatedBody.Add(key, value)
		}
	}

	return updatedBody.Encode()
}

func handleMultipartBody(c fiber.Ctx) string {
	form, err := c.MultipartForm()
	if err != nil {
		return "[multipart/form-data]"
	}

	result := url.Values{}

	for key, values := range form.Value {
		if redaction.IsSensitiveField(key) {
			for range values {
				result.Add(key, constant.ObfuscatedValue)
			}

			continue
		}

		for _, value := range values {
			result.Add(key, value)
		}
	}

	for key := range form.File {
		if redaction.IsSensitiveField(key) {
			result.Add(key, constant.ObfuscatedValue)
		} else {
			result.Add(key, "[file]")
		}
	}

	return result.Encode()
}

func sanitizeReferer(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "-"
	}

	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""

	return parsed.String()
}
