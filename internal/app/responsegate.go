package app

// This file implements input parameter parsing for response.normalize_before.
//
// The host calls this hook for every SSE line, while the RPC parameters contain the full client request
// (OriginalRequest) and full upstream request (TranslatedRequest) -- under long contexts,
// each can reach several MBs and both appear in JSON as base64. The plugin only actually needs
// Stream / FromFormat / ToFormat / Body, so we use a top-level-object-only
// scanner to extract values: the two large fields only undergo string skipping, with no base64 decoding or allocation.
// A full json.Unmarshal would base64-decode these two large fields, which was the source of
// 73% CPU usage in issue #9.
//
// The scanner relies solely on JSON syntax itself, assuming no field order or outer wrapper presence;
// Any structural anomalies are delegated to decodeResponseTransform using encoding/json as a fallback,
// preferring a slow single run over misjudging as "no processing needed" -- the latter causes Claude's streaming
// converter to drop frames due to missing data: prefixes (issue #5 / #8).

import (
	"bytes"
	"encoding/base64"
)

// responseTransformFields are the fields actually used by normalize_before.
type responseTransformFields struct {
	fromFormat string
	toFormat   string
	stream     bool
	body       []byte
}

// normalizeResponseNeeded is the sole criterion for whether to add a "data:" envelope to the chunk.
//
// Same-format OpenAI responses have envelopes added by the host handler itself; pre-wrapping would result in data: data:;
// Only during cross-protocol streaming conversion does the converter need an SSE frame with a data: prefix.
func normalizeResponseNeeded(stream bool, fromFormat, toFormat string) bool {
	return stream && isChatFormat(fromFormat) && toFormat != "" && !isChatFormat(toFormat)
}

// normalizeResponseChunk returns the chunk Body when normalization is required.
//
// ok=false means "this plugin does not process": the host's semantic for this result is to keep the original body
// (callResponseNormalizer treats it as a no-op when err != nil or Body is empty).
func normalizeResponseChunk(request []byte) ([]byte, bool) {
	fields, okScan := scanResponseTransform(request)
	if !okScan {
		fields, okScan = decodeResponseTransform(request)
		if !okScan {
			return nil, false
		}
	}
	if !normalizeResponseNeeded(fields.stream, fields.fromFormat, fields.toFormat) {
		return nil, false
	}
	return fields.body, len(fields.body) > 0
}

// decodeResponseTransform is the scanner's fallback: uses a narrow struct for encoding/json decoding.
//
// The narrow struct excludes OriginalRequest / TranslatedRequest, so encoding/json skips
// undeclared fields without base64-decoding them, avoiding large allocations here, only requiring an extra
// full-text tokenize.
func decodeResponseTransform(request []byte) (responseTransformFields, bool) {
	var decoded struct {
		FromFormat string
		ToFormat   string
		Stream     bool
		Body       []byte
	}
	if errDecode := decodeRequest(request, &decoded); errDecode != nil {
		return responseTransformFields{}, false
	}
	return responseTransformFields{
		fromFormat: decoded.FromFormat,
		toFormat:   decoded.ToFormat,
		stream:     decoded.Stream,
		body:       decoded.Body,
	}, true
}

// scanResponseTransform performs a single linear scan of the top-level JSON object, extracting only the needed fields.
//
// Once all three scalar values are present and deemed "no processing needed", it returns immediately -- without scanning the two large fields further,
// so the common path only reads the first hundred bytes or so. When deemed necessary, it continues scanning to Body; skipped large
// strings use bytes.IndexByte to locate the closing quote (memchr speed), and the byte-by-byte state machine
// never touches them.
//
// ok=false means the structure does not match expectations (non-object, truncated, escaped characters, etc.), and the caller must use
// decodeResponseTransform as a fallback.
func scanResponseTransform(raw []byte) (responseTransformFields, bool) {
	cursor := jsonCursor{raw: raw}
	if !cursor.consume('{') {
		return responseTransformFields{}, false
	}
	var fields responseTransformFields
	var seen uint8
	for {
		if cursor.consume('}') {
			return fields, true
		}
		key, okKey := cursor.readString()
		if !okKey || !cursor.consume(':') {
			return responseTransformFields{}, false
		}
		if okField := fields.assign(key, &cursor, &seen); !okField {
			return responseTransformFields{}, false
		}

		// Having all three scalars is sufficient to determine: no need to continue scanning subsequent large fields when processing is not required.
		if seen&seenScalars == seenScalars {
			if !normalizeResponseNeeded(fields.stream, fields.fromFormat, fields.toFormat) {
				return fields, true
			}
			if seen&seenBody != 0 {
				return fields, true
			}
		}

		if cursor.consume('}') {
			return fields, true
		}
		if !cursor.consume(',') {
			return responseTransformFields{}, false
		}
	}
}

const (
	seenFromFormat uint8 = 1 << iota
	seenToFormat
	seenStream
	seenBody
	seenScalars = seenFromFormat | seenToFormat | seenStream
)

// assign extracts values by key; unknown keys simply skip their values.
func (f *responseTransformFields) assign(key []byte, cursor *jsonCursor, seen *uint8) bool {
	switch {
	case bytes.Equal(key, []byte("FromFormat")):
		value, ok := cursor.readString()
		if !ok {
			return false
		}
		f.fromFormat = string(value)
		*seen |= seenFromFormat
		return true
	case bytes.Equal(key, []byte("ToFormat")):
		value, ok := cursor.readString()
		if !ok {
			return false
		}
		f.toFormat = string(value)
		*seen |= seenToFormat
		return true
	case bytes.Equal(key, []byte("Stream")):
		value, ok := cursor.readBool()
		if !ok {
			return false
		}
		f.stream = value
		*seen |= seenStream
		return true
	case bytes.Equal(key, []byte("Body")):
		encoded, ok := cursor.readString()
		if !ok {
			return false
		}
		decoded, okDecode := decodeBase64(encoded)
		if !okDecode {
			return false
		}
		f.body = decoded
		*seen |= seenBody
		return true
	default:
		return cursor.skipValue()
	}
}

// decodeBase64 decodes the Body field on the wire.
//
// It is only reached after determining that "envelope padding is needed", so the cost scales with chunk size rather than request body
// size growth.
func decodeBase64(encoded []byte) ([]byte, bool) {
	decoded := make([]byte, base64.StdEncoding.DecodedLen(len(encoded)))
	n, errDecode := base64.StdEncoding.Decode(decoded, encoded)
	if errDecode != nil {
		return nil, false
	}
	return decoded[:n], true
}

// jsonCursor is a minimal cursor just sufficient to parse this protocol payload.
type jsonCursor struct {
	raw []byte
	pos int
}

func (c *jsonCursor) skipSpace() {
	for c.pos < len(c.raw) {
		switch c.raw[c.pos] {
		case ' ', '\t', '\n', '\r':
			c.pos++
		default:
			return
		}
	}
}

func (c *jsonCursor) peek() (byte, bool) {
	c.skipSpace()
	if c.pos < len(c.raw) {
		return c.raw[c.pos], true
	}
	return 0, false
}

func (c *jsonCursor) consume(want byte) bool {
	ch, ok := c.peek()
	if !ok || ch != want {
		return false
	}
	c.pos++
	return true
}

// readString reads an unescaped string, returning the unquoted raw bytes.
//
// Fields requiring values (format name, base64 Body) are pure ASCII. Any escape character is considered
// a structural anomaly, falling back to encoding/json rather than partially decoding here.
func (c *jsonCursor) readString() ([]byte, bool) {
	if !c.consume('"') {
		return nil, false
	}
	start := c.pos
	offset := bytes.IndexByte(c.raw[start:], '"')
	if offset < 0 {
		return nil, false
	}
	end := start + offset
	if bytes.IndexByte(c.raw[start:end], '\\') >= 0 {
		return nil, false
	}
	c.pos = end + 1
	return c.raw[start:end], true
}

func (c *jsonCursor) readBool() (bool, bool) {
	if c.hasPrefix("true") {
		c.pos += len("true")
		return true, true
	}
	if c.hasPrefix("false") {
		c.pos += len("false")
		return false, true
	}
	return false, false
}

func (c *jsonCursor) hasPrefix(literal string) bool {
	c.skipSpace()
	if len(c.raw)-c.pos < len(literal) {
		return false
	}
	return bytes.Equal(c.raw[c.pos:c.pos+len(literal)], []byte(literal))
}

// skipValue skips a value without parsing its content.
func (c *jsonCursor) skipValue() bool {
	ch, ok := c.peek()
	if !ok {
		return false
	}
	switch ch {
	case '"':
		return c.skipString()
	case '{', '[':
		return c.skipContainer()
	default:
		return c.skipLiteral()
	}
}

// skipString skips a string value (allowing escapes).
func (c *jsonCursor) skipString() bool {
	if !c.consume('"') {
		return false
	}
	for c.pos < len(c.raw) {
		offset := bytes.IndexByte(c.raw[c.pos:], '"')
		if offset < 0 {
			return false
		}
		index := c.pos + offset
		// Count leading backslashes: an odd number means the quote is escaped, continue searching.
		escapes := 0
		for i := index - 1; i >= 0 && c.raw[i] == '\\'; i-- {
			escapes++
		}
		c.pos = index + 1
		if escapes%2 == 0 {
			return true
		}
	}
	return false
}

// skipContainer skips an object or array by bracket depth.
func (c *jsonCursor) skipContainer() bool {
	depth := 0
	for c.pos < len(c.raw) {
		switch c.raw[c.pos] {
		case '"':
			if !c.skipString() {
				return false
			}
		case '{', '[':
			depth++
			c.pos++
		case '}', ']':
			depth--
			c.pos++
			if depth == 0 {
				return true
			}
		default:
			c.pos++
		}
	}
	return false
}

// skipLiteral skips a number / true / false / null.
func (c *jsonCursor) skipLiteral() bool {
	start := c.pos
	for c.pos < len(c.raw) {
		switch c.raw[c.pos] {
		case ',', '}', ']', ' ', '\t', '\n', '\r':
			return c.pos > start
		}
		c.pos++
	}
	return c.pos > start
}
