package redaction

import "strings"

const (
	// labelGapLimit is how many gap bytes may sit between a label and its value:
	// room for the common serialized spellings, the widest being escaped JSON
	// with spaces around the colon (\" : \", 7 bytes).
	labelGapLimit = 8
	// labelledTokenLimit bounds how far a label reads into the text: the longest
	// document spelling (12.345.678/0001-95, 18 bytes) plus room for separators
	// trimmed from either end. A longer run is no document, whatever it holds, and
	// the bound is what keeps a string of labels linear.
	labelledTokenLimit = 24
)

// cnpjLayout is the group sequence of a formatted CNPJ, alphanumeric or not.
var cnpjLayout = [maxShapeGroups]int{2, 3, 3, 4, 2}

// labelledSpans returns the documents that follow a cpf or cnpj label, in
// order. It returns nil, without allocating, when there are none.
//
// A label is cpf or cnpj in any case, at the start of s, after a byte that is
// neither a letter nor a digit, or at a camelCase boundary (an uppercase C after
// a lowercase letter). Up to labelGapLimit gap bytes may follow it, then
// the token: the longest run of letters, digits and separators, trimmed of
// separators at both ends. The token is replaced only when the whole of it is a
// document (isLabelledDocument); the label stays.
func labelledSpans(s string) []span {
	var matches []span

	for index := 0; index < len(s); index++ {
		valueStart, cnpj, found := labelAt(s, index)
		if !found {
			continue
		}

		token, found := labelledToken(s, valueStart)
		if !found || !isLabelledDocument(s[token.start:token.end], cnpj) {
			continue
		}

		matches = append(matches, token)
		index = token.end - 1
	}

	return matches
}

// labelAt reports whether a label starts at s[index], where its value may start,
// and whether the label is cnpj.
func labelAt(s string, index int) (int, bool, bool) {
	if s[index] != 'c' && s[index] != 'C' {
		return 0, false, false
	}

	if index > 0 && isAlphanumeric(rune(s[index-1])) && (s[index] != 'C' || !isLowerAlpha(s[index-1])) {
		return 0, false, false
	}

	if hasPrefixFold(s[index:], "cnpj") {
		return index + len("cnpj"), true, true
	}

	if hasPrefixFold(s[index:], "cpf") {
		return index + len("cpf"), false, true
	}

	return 0, false, false
}

// labelledToken skips the gap after a label at pos and returns the token that
// follows it, trimmed of separators. It reports false when the gap is too wide,
// the token is empty, or the run is longer than labelledTokenLimit.
func labelledToken(s string, pos int) (span, bool) {
	for gap := 0; gap < labelGapLimit && pos < len(s) && isLabelGap(s[pos]); gap++ {
		pos++
	}

	start := pos

	for pos < len(s) && isLabelledTokenByte(s[pos]) {
		pos++

		if pos-start > labelledTokenLimit {
			return span{}, false
		}
	}

	end := pos

	for start < end && isSeparator(s[start]) {
		start++
	}

	for end > start && isSeparator(s[end-1]) {
		end--
	}

	return span{start: start, end: end}, start < end
}

// isLabelledDocument reports whether token, which follows a label, is a whole
// document. After either label it is numeric: only digits and separators, and
// 11 or 14 digits. After cnpj it may also be an alphanumeric CNPJ in any case:
// one group of 14 or the 2.3.3/4-2 layout, at least one letter, and two
// trailing digits. Separators are not judged here; the label already says what
// the value is.
func isLabelledDocument(token string, cnpj bool) bool {
	var groups [maxShapeGroups]int

	count, digits, letters := 0, 0, 0
	inGroup := false

	for index := range len(token) {
		b := token[index]
		if isSeparator(b) {
			inGroup = false

			continue
		}

		if !inGroup {
			if count == len(groups) {
				return false
			}

			inGroup = true
			count++
		}

		groups[count-1]++

		if isDigit(b) {
			digits++
		} else {
			letters++
		}
	}

	if letters == 0 {
		return digits == cpfLength || digits == cnpjLength
	}

	return cnpj && hasCheckDigits(token) &&
		((count == 1 && groups[0] == cnpjLength) || (count == len(cnpjLayout) && groups == cnpjLayout))
}

// hasCheckDigits reports whether token ends in two digits.
func hasCheckDigits(token string) bool {
	return len(token) >= cnpjCheckDigits &&
		isDigit(token[len(token)-1]) && isDigit(token[len(token)-2])
}

// hasPrefixFold reports whether s starts with the lowercase ASCII prefix, in
// any case, without allocating.
func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

func isLabelledTokenByte(b byte) bool { return isAlphanumeric(rune(b)) || isSeparator(b) }

// isLabelGap reports whether b may sit between a label and its value: any byte
// that is not an ASCII letter or digit, so quotes, escapes, brackets, markup,
// punctuation and the bytes of a UTF-8 character all count. A letter or digit
// ends the gap and starts the token.
func isLabelGap(b byte) bool { return !isAlphanumeric(rune(b)) }
