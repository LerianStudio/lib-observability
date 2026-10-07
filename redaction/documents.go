package redaction

import (
	"slices"
	"strings"
)

// DocumentPlaceholder replaces every CPF/CNPJ-shaped span. It is a fixed,
// greppable token: an operator who sees it knows a document was there and was
// removed on purpose, which is different information from an empty field.
const DocumentPlaceholder = "[REDACTED_DOCUMENT]"

const (
	// cpfLength and cnpjLength are the two character widths of a Brazilian
	// document. A span of any other width is operational data and survives.
	cpfLength  = 11
	cnpjLength = 14
	// cnpjCheckDigits is how many trailing positions of a CNPJ are numeric in
	// every spelling, the alphanumeric one included ([0-9A-Z]{12}[0-9]{2}).
	cnpjCheckDigits = 2
	// maxShapeGroups is the group count of the longest shape, {2,3,3,4,2}.
	maxShapeGroups = 5
)

// ScrubDocuments returns s with every CPF/CNPJ-shaped span replaced by
// DocumentPlaceholder.
//
// It matches by shape, the sequence of separator-delimited group lengths, over
// a numeric and an alphanumeric reading of a document, plus a reading for a
// misspelled one and a reading for the value that follows a cpf or cnpj label;
// the package documentation states the rule, the false positives it accepts and
// the residue it leaves open. It validates no check
// digit, so a mistyped, masked or test document is redacted as well.
//
// It is pure, safe for concurrent use and allocation-free when nothing matches:
// the input string itself is returned. "" in, "" out. Cost is linear in len(s).
//
// It is defence in depth for free text (audit fields, error messages, log and
// span text), not a substitute for keeping identifiers out of that text in the
// first place: it covers CPF and CNPJ only.
func ScrubDocuments(s string) string {
	return replaceSpans(s, documentSpans(s))
}

// ScrubDocumentsUnder is ScrubDocuments for a value stored under a key: a log
// field, a span attribute, a map entry. A key that ends in cpf or cnpj, in any
// case (cnpj, payerCnpj, user_cpf, cpfCnpj, NRCPF), is the value's label, so
// value is also read as if it followed that label in text: the token at its
// start, past up to eight gap bytes, is replaced when the whole token is a
// document of the kind the label names. That is what catches a lowercase
// alphanumeric CNPJ under a cnpj key, which ScrubDocuments alone leaves as an
// ordinary token. Every other match is ScrubDocuments' own.
//
// It shares ScrubDocuments' guarantees: pure, safe for concurrent use, linear
// in len(value), and value itself is returned, without allocating, when
// nothing matches. "" in, "" out, whatever the key.
func ScrubDocumentsUnder(key, value string) string {
	return replaceSpans(value, documentSpansUnder(key, value))
}

// replaceSpans returns s with every span replaced by DocumentPlaceholder, or s
// itself when there is none.
func replaceSpans(s string, spans []span) string {
	if len(spans) == 0 {
		return s
	}

	var builder strings.Builder

	builder.Grow(len(s) + len(spans)*len(DocumentPlaceholder))

	written := 0

	for _, match := range spans {
		builder.WriteString(s[written:match.start])
		builder.WriteString(DocumentPlaceholder)

		written = match.end
	}

	builder.WriteString(s[written:])

	return builder.String()
}

// span is a half-open byte range [start, end) of the scanned string.
type span struct{ start, end int }

// group is one maximal stretch of document characters inside a run. glued
// reports that it touches, on either side, a letter its reading does not own,
// which makes an unseparated group part of a longer identifier.
type group struct {
	start, end int
	glued      bool
}

func (g group) length() int { return g.end - g.start }

// reading is one way of seeing a document in text: which characters form its
// groups, which letters glue a group into an identifier, and which group-length
// sequences spell a document.
type reading struct {
	alphanumeric bool
	shapes       [][]int
}

// numericReading sees a document whose every position is a digit.
var numericReading = reading{
	shapes: [][]int{
		{cpfLength},     // 52998224725
		{3, 3, 3, 2},    // 529.982.247-25
		{cnpjLength},    // 12345678000195
		{2, 3, 3, 4, 2}, // 12.345.678/0001-95
	},
}

// alphanumericReading sees the alphanumeric CNPJ. There is no alphanumeric CPF.
var alphanumericReading = reading{
	alphanumeric: true,
	shapes: [][]int{
		{cnpjLength},    // 12ABC34501DE35
		{2, 3, 3, 4, 2}, // 12.ABC.345/01DE-35
	},
}

// inGroup reports whether b is a document character under this reading.
func (r *reading) inGroup(b byte) bool {
	if r.alphanumeric {
		return isDigit(b) || isUpperAlpha(b)
	}

	return isDigit(b)
}

// inRun reports whether b may appear inside a run: a document character or one
// of the three separators the document formats use.
func (r *reading) inRun(b byte) bool { return r.inGroup(b) || isSeparator(b) }

// glues reports whether b, adjacent to a group, makes the group part of a longer
// identifier: any ASCII letter for the numeric reading, a lowercase one for the
// alphanumeric reading (whose groups already own the uppercase letters).
func (r *reading) glues(b byte) bool {
	if r.alphanumeric {
		return isLowerAlpha(b)
	}

	return isUpperAlpha(b) || isLowerAlpha(b)
}

// documentSpans returns the ordered, non-overlapping byte ranges of s that read
// as a document: the numeric and alphanumeric shapes, and the values that follow
// a cpf or cnpj label. It returns nil, without allocating, when there are none.
// The alphanumeric pass is skipped when s carries no uppercase letter, since
// every match it could return needs one.
func documentSpans(s string) []span {
	numeric := numericReading.scan(s)
	labelled := labelledSpans(s)

	if !hasUpperAlpha(s) {
		return mergeSpans(numeric, labelled)
	}

	return mergeSpans(numeric, alphanumericReading.scan(s), labelled)
}

// scan returns the matches of this reading in s, run by run, in order.
func (r *reading) scan(s string) []span {
	var matches []span

	for index := 0; index < len(s); {
		if !r.inRun(s[index]) {
			index++

			continue
		}

		start := index
		for index < len(s) && r.inRun(s[index]) {
			index++
		}

		matches = r.scanRun(s, start, index, matches)
	}

	return matches
}

// scanRun appends the matches inside the run s[start:end] to matches.
//
// A misspelled document consumes the whole run, and every shape match inside
// the run lies within its body, so it is returned alone. Otherwise the shapes
// are tried left to right over a sliding window of at most maxShapeGroups
// groups, restarting after each match, so a run carrying two documents loses
// both.
func (r *reading) scanRun(s string, start, end int, matches []span) []span {
	if !r.alphanumeric {
		if body, found := misspelledBody(s, start, end); found {
			return append(matches, body)
		}
	}

	var window [maxShapeGroups]group

	filled, pos, exhausted := 0, start, false

	for {
		for filled < len(window) && !exhausted {
			next, after, found := r.nextGroup(s, pos, end)
			pos = after

			if !found {
				exhausted = true

				break
			}

			window[filled] = next
			filled++
		}

		if filled == 0 {
			return matches
		}

		consumed := 1

		if match, size := r.shapeAt(s, window[:filled]); size > 0 {
			matches = append(matches, match)
			consumed = size
		}

		filled = copy(window[:], window[consumed:filled])
	}
}

// nextGroup returns the first group of s[pos:end] and the position after it.
func (r *reading) nextGroup(s string, pos, end int) (group, int, bool) {
	for pos < end && !r.inGroup(s[pos]) {
		pos++
	}

	if pos >= end {
		return group{}, pos, false
	}

	start := pos
	for pos < end && r.inGroup(s[pos]) {
		pos++
	}

	glued := (start > 0 && r.glues(s[start-1])) || (pos < len(s) && r.glues(s[pos]))

	return group{start: start, end: pos, glued: glued}, pos, true
}

// shapeAt returns the span and group count of the shape that starts at
// groups[0], or a zero count when none does.
//
// A single-group shape is refused when the group is glued to a letter: an
// unseparated run of digits inside a hex digest, a trace id or a base64 token is
// part of that identifier. A multi-group shape needs no such guard, because its
// separators already spell a document, but only when at least one of them is a
// document separator: a dots-only 3.3.3.2 run is an IPv4 address or a version.
func (r *reading) shapeAt(s string, groups []group) (span, int) {
	for _, shape := range r.shapes {
		if !shapeFits(groups, shape) {
			continue
		}

		matched := groups[:len(shape)]

		if len(matched) == 1 && matched[0].glued {
			continue
		}

		if len(matched) > 1 && !hasDocumentSeparator(s[matched[0].start:matched[len(matched)-1].end]) {
			continue
		}

		if r.alphanumeric && !isAlphanumericCNPJ(s, matched) {
			continue
		}

		return span{start: matched[0].start, end: matched[len(matched)-1].end}, len(matched)
	}

	return span{}, 0
}

// shapeFits reports whether the leading group lengths are exactly shape. Every
// group is compared, which is what keeps {2,9} (the seconds and nanoseconds of a
// timestamp) from matching although it totals 11.
func shapeFits(groups []group, shape []int) bool {
	if len(shape) > len(groups) {
		return false
	}

	for offset, want := range shape {
		if groups[offset].length() != want {
			return false
		}
	}

	return true
}

// isAlphanumericCNPJ is the extra condition of the alphanumeric reading: the two
// trailing positions are digits (the check digits are numeric in every
// spelling) and at least one position is a letter (an all-digit span belongs to
// the numeric reading).
func isAlphanumericCNPJ(s string, groups []group) bool {
	last := groups[len(groups)-1]
	if last.length() < cnpjCheckDigits {
		return false
	}

	for index := last.end - cnpjCheckDigits; index < last.end; index++ {
		if !isDigit(s[index]) {
			return false
		}
	}

	for _, g := range groups {
		for index := g.start; index < g.end; index++ {
			if isUpperAlpha(s[index]) {
				return true
			}
		}
	}

	return false
}

// misspelledBody returns the first-digit-to-last-digit range of the run
// s[start:end] when the whole run holds exactly one document's worth of digits
// (11 or 14) in at least two groups, spelled with at least one `-` or `/`, and
// neither end of that range touches a letter.
//
// The match is over the whole run, never a window inside it: a window would be
// "any 11 digits anywhere" again, and a document embedded in a longer run is
// the shape readings' job. The range is bounded by the first and last group, so
// a leading or trailing separator stays in the output. A range glued to a
// letter is the digit-and-dash stretch of a hex identifier (a UUID such as
// 0c499500-0758-4071-a522-... reads 499500-0758-4071 between two letters), the
// same reason the unseparated shapes refuse a glued group.
func misspelledBody(s string, start, end int) (span, bool) {
	var first, last group

	groups, digits, pos := 0, 0, start

	for {
		next, after, found := numericReading.nextGroup(s, pos, end)
		if !found {
			break
		}

		if groups == 0 {
			first = next
		}

		last = next
		groups++
		digits += next.length()
		pos = after
	}

	if groups < 2 || (digits != cpfLength && digits != cnpjLength) {
		return span{}, false
	}

	body := span{start: first.start, end: last.end}
	if !hasDocumentSeparator(s[body.start:body.end]) {
		return span{}, false
	}

	if (body.start > 0 && numericReading.glues(s[body.start-1])) ||
		(body.end < len(s) && numericReading.glues(s[body.end])) {
		return span{}, false
	}

	return body, true
}

// mergeSpans returns the union of ordered match sets as one ordered,
// non-overlapping list. It allocates only when two or more sets are non-empty.
// An overlapping span is absorbed, never dropped: two readings that overlap only
// partially each claim bytes the other does not (529.982.247-25.ABC.123/4567-89
// is a numeric CPF followed by an alphanumeric CNPJ sharing "25"), and dropping
// the later span would emit its tail in clear.
func mergeSpans(sets ...[]span) []span {
	var only []span

	nonEmpty, total := 0, 0

	for _, set := range sets {
		if len(set) > 0 {
			only = set
			nonEmpty++
			total += len(set)
		}
	}

	if nonEmpty < 2 {
		return only
	}

	all := make([]span, 0, total)
	for _, set := range sets {
		all = append(all, set...)
	}

	slices.SortFunc(all, func(a, b span) int {
		if a.start != b.start {
			return a.start - b.start
		}

		return b.end - a.end
	})

	merged := all[:1]

	for _, candidate := range all[1:] {
		tail := &merged[len(merged)-1]
		if candidate.start < tail.end {
			tail.end = max(tail.end, candidate.end)

			continue
		}

		merged = append(merged, candidate)
	}

	return merged
}

func isSeparator(b byte) bool { return b == '.' || b == '-' || b == '/' }

// hasDocumentSeparator reports whether s carries `-` or `/`: every document
// format puts `-` before the check digits and `/` before the CNPJ branch, so a
// dots-only spelling is an address, a version, a decimal or an OID instead.
func hasDocumentSeparator(s string) bool { return strings.ContainsAny(s, "-/") }

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

func isUpperAlpha(b byte) bool { return b >= 'A' && b <= 'Z' }

func isLowerAlpha(b byte) bool { return b >= 'a' && b <= 'z' }

// hasUpperAlpha is the cheap guard that keeps a line without uppercase letters
// to the numeric pass alone.
func hasUpperAlpha(s string) bool {
	for index := range len(s) {
		if isUpperAlpha(s[index]) {
			return true
		}
	}

	return false
}
