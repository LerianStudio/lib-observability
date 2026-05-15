//go:build unit

package constants

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSanitizeMetricLabel(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "short", SanitizeMetricLabel("short"))
	assert.Len(t, SanitizeMetricLabel(strings.Repeat("a", MaxMetricLabelLength+10)), MaxMetricLabelLength)

	multibyte := strings.Repeat("ç", MaxMetricLabelLength+10)
	got := SanitizeMetricLabel(multibyte)
	assert.Equal(t, MaxMetricLabelLength, len([]rune(got)))
	assert.Equal(t, strings.Repeat("ç", MaxMetricLabelLength), got)
}
