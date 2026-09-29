//go:build unit

package redaction

import (
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

const ph = DocumentPlaceholder

func TestScrubDocuments_Redacts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "bare cpf", in: "52998224725", want: ph},
		{name: "formatted cpf", in: "529.982.247-25", want: ph},
		{name: "bare cnpj", in: "12345678000195", want: ph},
		{name: "formatted cnpj", in: "12.345.678/0001-95", want: ph},
		{name: "bare alphanumeric cnpj", in: "12ABC34501DE35", want: ph},
		{name: "formatted alphanumeric cnpj", in: "12.ABC.345/01DE-35", want: ph},
		{name: "cpf glued to an epoch by a dash", in: "1756339200-52998224725", want: "1756339200-" + ph},
		{name: "cpf between correlation parts", in: "req-52998224725-1756339200", want: "req-" + ph + "-1756339200"},
		{name: "cpf at the start", in: "52998224725 rejected", want: ph + " rejected"},
		{name: "cpf at the end", in: "rejected document 529.982.247-25", want: "rejected document " + ph},
		{name: "cpf in the middle", in: "holder 529.982.247-25 not found", want: "holder " + ph + " not found"},
		{
			name: "several documents in one string",
			in:   "cpf=529.982.247-25 cnpj=12.345.678/0001-95 alt=12ABC34501DE35",
			want: "cpf=" + ph + " cnpj=" + ph + " alt=" + ph,
		},
		{name: "misspelled cpf", in: "doc 529982247-25", want: "doc " + ph},
		{name: "misspelled cpf with a misplaced dot", in: "529.982.24-725", want: ph},
		{name: "misspelled cnpj", in: "12345678/0001-95", want: ph},
		{name: "misspelled cnpj without the check dash", in: "12.345.678/000195", want: ph},
		{name: "surrounding punctuation", in: "CPF:52998224725;", want: "CPF:" + ph + ";"},
		{name: "surrounding utf-8", in: "documento «52998224725» inválido", want: "documento «" + ph + "» inválido"},
		{name: "formatted cnpj glued to a label", in: "CNPJ12.345.678/0001-95", want: "CNPJ" + ph},
		{name: "formatted cpf glued to a label", in: "cpf529.982.247-25", want: "cpf" + ph},
		{name: "misspelled cpf glued to a label", in: "CPF529982247-25", want: "CPF" + ph},
		{
			name: "partial overlap of the two readings is absorbed",
			in:   "529.982.247-25.ABC.123/4567-89",
			want: ph,
		},
		{
			name: "alphanumeric prefix glued to a numeric cnpj",
			in:   "ABCDEFGHIJKL12.345.678/0001-95",
			want: ph,
		},
		{
			name: "alphanumeric token glued to a formatted cpf",
			in:   "token=1ABCDEFGHIJ123.456.789-01",
			want: "token=" + ph,
		},
		{name: "error text", in: `consult failed: holder "529.982.247-25": timeout`, want: `consult failed: holder "` + ph + `": timeout`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ScrubDocuments(tt.in)
			assert.Equal(t, tt.want, got)
			assert.NotContains(t, got, "52998224725")
			assert.NotContains(t, got, "982.247")
			assert.NotContains(t, got, "345.678")
		})
	}
}

func TestScrubDocuments_LeavesOperationalValues(t *testing.T) {
	t.Parallel()

	for _, in := range []string{
		"",
		"f47ac10b-58cc-4372-a567-0e02b2c3d479",                          // UUIDv4
		"01890a5d-ac96-774b-bcce-b302099a8057",                          // UUIDv7
		"1756339200123456789",                                           // epoch nanoseconds
		"2026-08-27T12:34:56.123456789Z",                                // RFC3339Nano
		"45.123456789s",                                                 // nanosecond duration
		"10.100.100.100",                                                // RFC1918 address
		"192.168.100.100:4317",                                          // address and port
		"1.3.6.1.4.1.311.60.2.1.3",                                      // OID
		"123456789012",                                                  // 12 digits
		"1234567890123",                                                 // 13 digits
		"123456789012345",                                               // 15 digits
		"marshalACCS006",                                                // mixed-case identifier
		"abcdefghij1234",                                                // 14-character lowercase token
		"00038166",                                                      // ISPB
		"00038166202609290000123",                                       // NUOp, 23 digits
		"E00038166202609291200kR7mQ2xYz9A",                              // EndToEndID
		"E00038166202609291200KR7MQ2XYZ9A",                              // EndToEndID, uppercase suffix
		"4bf92f3577b34da6a3ce929d0e0e4736",                              // trace id
		"trace 0af7651916cd43dd8448eb211c80319c span b7ad6b7169203331", // trace and span ids
		"sha256:9f86d081884c7d659a2feaa0c55ad0f52998224725b0f00a08",   // hex digest with an 11-digit run
		"deadbeef12345678000195cafe",                                    // hex digest with a 14-digit run
		"cpf52998224725",                                                // bare document glued to letters: identifier
		"AbCDEFGHIJKL1234xy",                                            // mixed-case token, uppercase window
		"5.29982247.25",                                                 // documented residue: dots-only misspelling
		"R$ 1.234,56 em 3 parcelas",                                     // money text
		"v1.26.3",                                                       // version
	} {
		t.Run(in, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, in, ScrubDocuments(in))
		})
	}
}

// TestScrubDocuments_AcceptedFalsePositives pins the collisions the shape rule
// accepts on purpose; see the package doc for why each one stands.
func TestScrubDocuments_AcceptedFalsePositives(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "14-digit timestamp", in: "at 20260929123456", want: "at " + ph},
		{name: "11-digit integer part of an amount", in: "12345678901.00", want: ph + ".00"},
		{name: "14-digit integer part of an amount", in: "12345678901234.50", want: ph + ".50"},
		{name: "uppercase hex digest ending in two digits", in: "ETAG=W/1A2B3C4D5E6F78", want: "ETAG=W/" + ph},
		{name: "11-digit phone number", in: "tel 11987654321", want: "tel " + ph},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tt.want, ScrubDocuments(tt.in))
		})
	}
}

func TestScrubDocuments_Idempotent(t *testing.T) {
	t.Parallel()

	once := ScrubDocuments("cpf=529.982.247-25 cnpj=12ABC34501DE35")
	assert.Equal(t, once, ScrubDocuments(once))
}

func TestScrubDocuments_NoAllocationWithoutMatch(t *testing.T) {
	in := "request 01890a5d-ac96-774b-bcce-b302099a8057 took 45.123456789s at " +
		"2026-08-27T12:34:56.123456789Z from 10.100.100.100 (ISPB 00038166, E2E " +
		"E00038166202609291200KR7MQ2XYZ9A, trace 4bf92f3577b34da6a3ce929d0e0e4736)"

	var out string

	allocs := testing.AllocsPerRun(100, func() {
		out = ScrubDocuments(in)
	})

	assert.Equal(t, in, out)
	assert.Zero(t, allocs, "a string with no document must not allocate")
}

func TestScrubDocuments_Concurrent(t *testing.T) {
	t.Parallel()

	const in = "holder 529.982.247-25 branch 12.345.678/0001-95"

	want := "holder " + ph + " branch " + ph

	var wg sync.WaitGroup

	for range 16 {
		wg.Go(func() {
			for range 200 {
				assert.Equal(t, want, ScrubDocuments(in))
			}
		})
	}

	wg.Wait()
}

func TestScrubDocuments_LinearOnLongInput(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("1.", 50_000) + strings.Repeat("A-", 50_000) + "529.982.247-25"

	got := ScrubDocuments(long)
	assert.True(t, strings.HasSuffix(got, ph))
	assert.NotContains(t, got, "982.247")
}
