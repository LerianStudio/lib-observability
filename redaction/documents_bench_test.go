//go:build unit

package redaction

import (
	"strings"
	"testing"
)

var benchScrubResult string

// BenchmarkScrubDocuments covers a 1 KiB log line with no document (the
// overwhelming majority, which must not allocate) and the same line carrying
// three documents, plus a 4 KiB variant of each to show the cost stays linear,
// and a line whose documents follow a cpf/cnpj label.
func BenchmarkScrubDocuments(b *testing.B) {
	clean := "request 01890a5d-ac96-774b-bcce-b302099a8057 took 45.123456789s at " +
		"2026-08-27T12:34:56.123456789Z from 10.100.100.100 ISPB 00038166 " +
		"trace 4bf92f3577b34da6a3ce929d0e0e4736 E00038166202609291200KR7MQ2XYZ9A "
	dirty := "holder 529.982.247-25 branch 12.345.678/0001-95 alt 12ABC34501DE35 "
	labelled := "cpfValidator: cpf invalid, cnpj lookup failed, cliente cpf52998224725 cnpj 12abc34501de35 "

	cases := []struct {
		name string
		in   string
	}{
		{name: "1KiB_no_match", in: fill(clean, 1024)},
		{name: "1KiB_3_matches", in: dirty + fill(clean, 1024-len(dirty))},
		{name: "4KiB_no_match", in: fill(clean, 4096)},
		{name: "4KiB_12_matches", in: strings.Repeat(dirty+fill(clean, 1024-len(dirty)), 4)},
		{name: "1KiB_labels_2_matches", in: labelled + fill(clean, 1024-len(labelled))},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(tc.in)))

			for b.Loop() {
				benchScrubResult = ScrubDocuments(tc.in)
			}
		})
	}
}

// fill repeats unit and truncates the result to exactly n bytes.
func fill(unit string, n int) string {
	return strings.Repeat(unit, n/len(unit)+1)[:n]
}
