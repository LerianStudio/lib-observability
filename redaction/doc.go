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
// redacted. They do need at least one `-` or `/` among those separators, for
// the reason the misspelled reading below states: a dots-only 3.3.3.2 run is an
// IPv4 address (192.168.100.10) or a version (1.234.567.890.12), not a CPF.
//
// # Misspelled documents
//
// A whole CPF or CNPJ whose separators sit where no canonical format puts them
// (529982247-25, 529.982.24-725, 12345678/0001-95, 12.345.678/000195) is none of
// the six shapes. A third reading catches it: a run is a document when its
// digits total exactly 11 or 14, the whole run is consumed (never a window
// inside it), the run carries at least one `-` or `/`, and the digits from the
// first to the last do not touch a letter on either side. The separator clause
// is a property of the domain: every document format puts `-` before the check
// digits and `/` before the CNPJ branch. A dots-only run is therefore not a
// document spelling, and it is exactly where operational values live:
// 56.123456789 (the fraction of an RFC 3339 timestamp), 10.100.100.100 and
// 192.168.100.10 (addresses) and 1.3.6.1.4.1.311.60.2.1.3 (an OID) all survive.
// The letter clause is the unseparated shapes' guard applied to the ends of the
// run: 499500-0758-4071 between the letters of 0c499500-0758-4071-a522-... is
// the digit-and-dash stretch of a UUID, and without the guard about 2.5% of
// random UUIDs would lose a slice.
//
// The misspelled reading is not extended to the alphanumeric class: [0-9A-Z-] is
// the alphabet correlation ids are built from, and a character-total rule there
// would redact REQ-ABCDEFG-1234 and every id like it.
//
// # Alphanumeric CNPJ
//
// Since IN RFB 2.229/2024 the first twelve positions of a CNPJ may be letters and
// only its two check digits are numeric. Without a label (see below) the
// alphanumeric reading matches only the two CNPJ shapes, only uppercase (the
// canonical spelling; widening the class to lowercase would put every
// lowercase token at risk: my.app.com/user-42 has the 2.3.3/4-2 layout and ends
// in two digits), only when the last two positions are digits and at least one
// position is a letter (an all-digit span is the numeric reading's). Groups are maximal, so a
// 20-character token is one group of 20 and no 14-character window is cut out
// of it, and a {14} group touching a lowercase letter is part of a mixed-case
// token and does not match. When the two readings overlap partially, the union
// is redacted, never only one side.
//
// # Labelled documents
//
// A cpf or cnpj label says what the value after it is, so the value is read
// with the label's meaning instead of on its shape alone. This catches what the
// readings above refuse on purpose: a document glued to its label
// (cliente cpf52998224725 bloqueado, CNPJ12ABC34501DE35, CPF529982247-25) and
// an alphanumeric CNPJ written in lowercase (cnpj 12abc34501de35).
//
//   - The label is cpf or cnpj in any case, at the start of the text, after a
//     byte that is neither a letter nor a digit, or at a camelCase boundary (an
//     uppercase C right after a lowercase letter): user_cpf, "cpf":, <CNPJ>,
//     payerCnpj and holderCPF are labels; clientecpf, HOLDERCPF and 9cpf are not.
//   - Up to eight gap bytes may follow it, any byte that is not an ASCII letter
//     or digit: quotes, backslashes, brackets, markup, punctuation, whitespace
//     and UTF-8 characters. That covers {"cnpj": "..."}, escaped JSON inside
//     error text ({\"cnpj\": \"...\"}), <CNPJ>...</CNPJ>, cnpj = '...',
//     cnpj (...) and cnpj=[...]. A letter or digit ends the gap.
//   - The value is the run of letters, digits and . - / that follows, trimmed of
//     separators at both ends, and it is replaced only when the whole run is a
//     document. After either label that is a run of digits and separators
//     holding exactly 11 or 14 digits, separated any way; after cnpj it may also
//     be an alphanumeric CNPJ in any case, one group of 14 or the 2.3.3/4-2
//     layout, with at least one letter and two trailing digits.
//   - The label stays in the output (cliente cpf[REDACTED_DOCUMENT] bloqueado).
//   - A run longer than 24 bytes is no document, which bounds the work per label
//     and keeps the scan linear however many labels the text carries.
//
// The whole-run rule is what keeps false positives bounded: cpfValidator,
// isCnpjValid, cnpjRoot12345678, cnpj 12345678 (a CNPJ root), cpf: invalid, a
// trace id or an EndToEndID after a label all survive: the gap and the camelCase
// boundary decide only where a value may start, never what counts as a
// document. What it accepts by design is listed under the false positives
// below.
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
//   - after a cnpj label, a 14-character word ending in two digits
//     (cnpj notavailable12, statusCnpj=notavailable12, cnpj: (notavailable12)),
//     and after either label, any run of digits and separators holding 11 or
//     14 digits (cpf 192.168.100.10): the label is taken at its word, up to
//     eight bytes of punctuation away and inside a camelCase key.
//
// UUIDs never match. A UUID is hex groups of {8,4,4,4,12}: no run of its digits
// fits a separated shape (the inner groups are whole 4-digit segments), an
// unseparated shape (a partial segment touches a letter, a whole one is 8 or 12
// digits), or the misspelled reading (a run that does not touch a letter holds
// 12, 16 or 20 digits, never 11 or 14).
//
// # Accepted residues
//
// These carry a document and are not redacted:
//
//   - a dots-only spelling, canonical widths (529.982.247.25) or not
//     (5.29982247.25), with no label in front: closing it means telling a dotted
//     run apart from decimals, addresses, versions and OIDs by group count and
//     width, a pile of exceptions that would destroy an address the first time
//     one of them was stated slightly wrong. After a label it is redacted;
//   - an unseparated or misspelled document glued to letters that are not a
//     label (ABC529982247-25, or clientecpf52998224725 and HOLDERCPF52998224725,
//     where the label sits inside a word with no camelCase boundary): it reads
//     exactly like the digit stretch of an identifier;
//   - a lowercase or mixed-case alphanumeric CNPJ with no label right before it
//     (12abc34501de35, 12.abc.345/01de-35): without the label it is
//     indistinguishable from an ordinary lowercase token or a dotted path. That
//     includes a label separated from it by a word (CNPJ do cliente
//     12abc34501de35) or by more than eight gap bytes (doubly escaped JSON,
//     {\\\"cnpj\\\":\\\"...\\\"}, is nine).
//     A value stored under a key that ends in cpf or cnpj (a log field, a
//     span attribute) is read with the key as its label: see
//     ScrubDocumentsUnder, which the zap and tracing scrubs use.
//
// # Cost
//
// ScrubDocuments is a shape scanner without regular expressions: one pass per
// reading, plus a byte loop for labels that reads a bounded value after each
// one. It is linear in the input, allocates nothing when the input carries no
// document (labels included), and skips the alphanumeric pass when the input
// has no uppercase letter.
package redaction
