package xmlda

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// newCharsetReader is the xml.Decoder.CharsetReader every decoder in this
// package installs.
//
// Without one, encoding/xml refuses any document whose declaration names
// an encoding other than UTF-8 — including UTF-16, which XML 1.0 §4.3.3
// obliges every processor to accept, and ISO-8859-1, which legacy
// industrial clients (and anything speaking Windows code pages) still
// emit for item names carrying umlauts. The refusal surfaced as a
// transport-level fault quoting a Go-internal message, which is neither
// conformant nor diagnosable.
//
// Only the encodings a real OPC XML-DA peer sends are supported, and each
// is transcoded to UTF-8 up front rather than streamed: request bodies are
// already fully buffered by the time they reach a decoder, so there is
// nothing to gain from streaming and a single conversion keeps the
// mapping tables trivial. An unknown label is still an error — this is a
// tolerance for encodings that exist, not a guess at bytes.
func newCharsetReader(label string, input io.Reader) (io.Reader, error) {
	raw, err := io.ReadAll(input)
	if err != nil {
		return nil, fmt.Errorf("xmlda: reading %s-encoded document: %w", label, err)
	}
	switch normalizeCharsetLabel(label) {
	case "utf8":
		return bytes.NewReader(raw), nil
	case "utf16":
		return bytes.NewReader(decodeUTF16(raw, guessUTF16Endianness(raw))), nil
	case "utf16be":
		return bytes.NewReader(decodeUTF16(raw, true)), nil
	case "utf16le":
		return bytes.NewReader(decodeUTF16(raw, false)), nil
	case "iso88591", "windows1252", "usascii":
		// Windows-1252 differs from Latin-1 only in 0x80-0x9F, a range
		// that holds no character any conforming XML document needs; both
		// are decoded as Latin-1, which maps every byte to the code point
		// of the same value.
		return bytes.NewReader(decodeLatin1(raw)), nil
	default:
		return nil, fmt.Errorf("xmlda: unsupported character encoding %q "+
			"(supported: UTF-8, UTF-16, ISO-8859-1, Windows-1252, US-ASCII)", label)
	}
}

// transcodeToUTF8 converts raw to UTF-8 if it is UTF-16, and otherwise
// returns it untouched.
//
// This has to happen BEFORE the bytes reach encoding/xml, not through its
// CharsetReader hook: the decoder discovers the encoding by reading the
// XML declaration out of the stream itself, and in a UTF-16 document that
// declaration is UTF-16 too — so the tokenizer fails on a NUL byte long
// before it has a label to hand to CharsetReader. XML 1.0 §4.3.3
// anticipates exactly this, which is why it requires UTF-16 documents to
// be recognizable from their byte-order mark.
//
// The declaration is rewritten to say UTF-8 afterwards, so the
// CharsetReader is not then asked to decode the same bytes a second time.
func transcodeToUTF8(raw []byte) []byte {
	if !looksLikeUTF16(raw) {
		return raw
	}
	return rewriteEncodingDeclaration(decodeUTF16(raw, guessUTF16Endianness(raw)))
}

// looksLikeUTF16 reports whether raw carries a UTF-16 byte-order mark, or
// — for the BOM-less form — begins with the NUL byte that an ASCII '<'
// takes in either UTF-16 order.
func looksLikeUTF16(raw []byte) bool {
	if len(raw) < 4 {
		return false
	}
	switch {
	case raw[0] == 0xFF && raw[1] == 0xFE:
		return true
	case raw[0] == 0xFE && raw[1] == 0xFF:
		return true
	case raw[0] == 0x00 && raw[1] == '<':
		return true // big-endian, no BOM
	case raw[0] == '<' && raw[1] == 0x00:
		return true // little-endian, no BOM
	}
	return false
}

// rewriteEncodingDeclaration replaces the encoding pseudo-attribute of a
// leading XML declaration with UTF-8, which is what the bytes now are.
func rewriteEncodingDeclaration(raw []byte) []byte {
	end := bytes.Index(raw, []byte("?>"))
	if !bytes.HasPrefix(raw, []byte("<?xml")) || end < 0 {
		return raw
	}
	decl := encodingAttr.ReplaceAll(raw[:end], []byte(`encoding="UTF-8"`))
	return append(decl, raw[end:]...)
}

var encodingAttr = regexp.MustCompile(`encoding\s*=\s*("[^"]*"|'[^']*')`)

// normalizeCharsetLabel folds an encoding label to a comparable form:
// lowercase, with separators removed, so "ISO-8859-1", "iso_8859_1" and
// "iso88591" are one label.
func normalizeCharsetLabel(label string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(label) {
		switch r {
		case '-', '_', ' ', ':':
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// guessUTF16Endianness reads a byte-order mark, defaulting to big-endian
// as XML 1.0 §4.3.3 prescribes for UTF-16 without one.
func guessUTF16Endianness(raw []byte) bool {
	if len(raw) >= 2 {
		switch {
		case raw[0] == 0xFF && raw[1] == 0xFE:
			return false // little-endian BOM
		case raw[0] == 0xFE && raw[1] == 0xFF:
			return true // big-endian BOM
		case raw[0] == 0x00:
			// No BOM, but a leading NUL byte can only be the high half of
			// an ASCII character in big-endian order.
			return true
		case len(raw) >= 2 && raw[1] == 0x00:
			return false
		}
	}
	return true
}

// decodeUTF16 transcodes raw to UTF-8, dropping a leading BOM. An odd
// trailing byte is ignored rather than fatal: it can only be a truncated
// document, which the XML parser then reports in its own terms.
func decodeUTF16(raw []byte, bigEndian bool) []byte {
	if len(raw) >= 2 && ((raw[0] == 0xFF && raw[1] == 0xFE) || (raw[0] == 0xFE && raw[1] == 0xFF)) {
		raw = raw[2:]
	}
	units := make([]uint16, 0, len(raw)/2)
	for i := 0; i+1 < len(raw); i += 2 {
		if bigEndian {
			units = append(units, uint16(raw[i])<<8|uint16(raw[i+1]))
		} else {
			units = append(units, uint16(raw[i+1])<<8|uint16(raw[i]))
		}
	}
	return []byte(string(utf16.Decode(units)))
}

// decodeLatin1 transcodes ISO-8859-1 to UTF-8. Every byte is the code
// point of the same value, which is what makes the mapping table
// unnecessary.
func decodeLatin1(raw []byte) []byte {
	out := make([]byte, 0, len(raw)+len(raw)/4)
	var buf [utf8.UTFMax]byte
	for _, b := range raw {
		n := utf8.EncodeRune(buf[:], rune(b))
		out = append(out, buf[:n]...)
	}
	return out
}
