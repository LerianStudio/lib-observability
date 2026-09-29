// Package redaction provides sensitive field detection for log and span attribute
// redaction, and content scrubbing of Brazilian documents in free text.
//
// # Field names
//
// IsSensitiveField checks a field name against a default list of credentials,
// PII, and financial identifiers, with support for camelCase normalization,
// plural folding ("tokens" is judged as "token") and word-boundary matching.
// Callers may extend the default list with additional field names via the
// variadic extra parameter.
//
// # Document content
//
// A field name says nothing about free text: an error message, an audit note or
// a log line can carry a CPF or a CNPJ under any name, or under none.
// ScrubDocuments replaces every CPF/CNPJ-shaped span of a string with
// DocumentPlaceholder. It is defence in depth for text that reaches telemetry,
// not a substitute for keeping identifiers out of messages and errors at their
// origin, and it covers CPF and CNPJ only: account numbers, keys and payment
// identifiers are not content-scrubbed.
//
// A document is matched by shape, the sequence of separator-delimited group
// lengths, never by a digit total. "Redact long digit runs" would destroy the
// values an incident is diagnosed from (epoch nanoseconds, timestamps, addresses,
// latencies), and an operator would switch the scrubber off. A document has four
// numeric spellings and two alphanumeric ones:
//
//	{11}            52998224725
//	{3,3,3,2}       529.982.247-25
//	{14}            12345678000195
//	{2,3,3,4,2}     12.345.678/0001-95
//	{14} alnum      12ABC34501DE35
//	{2,3,3,4,2}     12.ABC.345/01DE-35
//
// Groups are maximal stretches of digits (or of [0-9A-Z] for the alphanumeric
// reading), and a run is a maximal stretch of group characters and the three
// separators `.`, `-` and `/`. Anchoring shapes to group boundaries still
// catches a document glued to another number by a separator
// (1756339200-52998224725 splits into {10,11}, and the {11} tail matches), while
// a 19-digit epoch is one group of 19 and matches no shape.
//
// Unseparated shapes ({11}, {14}) additionally require the group not to touch a
// letter: a digit run inside a hex digest, a trace id or a base64 token is part
// of that identifier, and without this guard roughly 2% of 32-character trace
// ids would lose a slice. Separated shapes need no such guard, because their
// separators already spell a document, so CNPJ12.345.678/0001-95 is still
// redacted.
//
// # Misspelled documents
//
// A whole CPF or CNPJ whose separators sit where no canonical format puts them
// (529982247-25, 529.982.24-725, 12345678/0001-95, 12.345.678/000195) is none of
// the six shapes. A third reading catches it: a run is a document when its
// digits total exactly 11 or 14, the whole run is consumed (never a window
// inside it), and the run carries at least one `-` or `/`. The last clause is a
// property of the domain: every document format puts `-` before the check digits
// and `/` before the CNPJ branch. A dots-only run is therefore not a document
// spelling, and it is exactly where operational values live: 56.123456789 (the
// fraction of an RFC 3339 timestamp), 10.100.100.100 (an address) and
// 1.3.6.1.4.1.311.60.2.1.3 (an OID) all survive.
//
// The misspelled reading is not extended to the alphanumeric class: [0-9A-Z-] is
// the alphabet correlation ids are built from, and a character-total rule there
// would redact REQ-ABCDEFG-1234 and every id like it.
//
// # Alphanumeric CNPJ
//
// Since IN RFB 2.229/2024 the first twelve positions of a CNPJ may be letters and
// only its two check digits are numeric. The alphanumeric reading matches only
// the two CNPJ shapes, only uppercase (a lowercase document is not a valid
// spelling, and widening the class would put every lowercase token at risk),
// only when the last two positions are digits and at least one position is a
// letter (an all-digit span is the numeric reading's). Groups are maximal, so a
// 20-character token is one group of 20 and no 14-character window is cut out
// of it, and a {14} group touching a lowercase letter is part of a mixed-case
// token and does not match. When the two readings overlap partially, the union
// is redacted, never only one side.
//
// # No checksum
//
// ScrubDocuments validates no check digit. A mistyped, masked or test document
// is still a document fragment in a log, so the scrubber fails safe: it redacts
// by shape alone. The cost is the false positives below.
//
// # Accepted false positives
//
// These collide with a document shape and are redacted by design:
//
//   - 14-digit yyyyMMddHHmmss timestamps;
//   - the 11- or 14-digit integer part of an amount (12345678901.00);
//   - 11-digit phone numbers (PII anyway);
//   - a 14-character uppercase hex digest that ends in two digits and stands
//     alone (about 39% of them). The collision is inherent: the canonical
//     alphanumeric CNPJ 12ABC34501DE35 is itself valid uppercase hex, so nothing
//     distinguishes the two. A redacted digest costs one correlation; an emitted
//     CNPJ is a data-protection incident.
//
// UUIDs essentially never match: a UUID is hex groups of {8,4,4,4,12}, and only
// a UUID whose leading groups happen to be all digits and total 11 or 14 across
// a `-` collides with the misspelled reading.
//
// # Accepted residues
//
// These carry a document and are not redacted:
//
//   - a dots-only misspelling such as 5.29982247.25: closing it means telling a
//     dotted run apart from decimals, addresses and OIDs by group count and
//     width, a pile of exceptions that would destroy an OID the first time one of
//     them was stated slightly wrong;
//   - an unseparated document glued to letters on either side, such as
//     cpf52998224725: it reads exactly like a digit run inside an identifier.
//
// # Cost
//
// ScrubDocuments is a single-pass shape scanner without regular expressions. It
// is linear in the input, allocates nothing when the input carries no
// document, and skips the alphanumeric pass when the input has no uppercase
// letter.
package redaction
