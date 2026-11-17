// JSON helpers: marshal without HTML escaping (matching Node's
// JSON.stringify, which does not escape <, > or &), and a lenient base64
// decoder matching Node's Buffer.from(str, 'base64').

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
)

// marshalJSON serializes v to JSON the way Node's JSON.stringify does: no
// HTML escaping, no trailing newline. Maps marshal with sorted keys, which
// keeps repeated reads byte-identical.
func marshalJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// Encoder appends a newline; strip it.
	if err := enc.Encode(v); err != nil {
		return []byte("null")
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// parseJSONBody parses a request body the way JSON.parse does: an empty body
// is null without error; anything else must be exactly one JSON value
// (json.Unmarshal rejects trailing data, like JSON.parse).
func parseJSONBody(body []byte) (parsed any, parseError bool) {
	if len(body) == 0 {
		return nil, false
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, true
	}
	return v, false
}

// decodeBase64Lenient decodes base64 the way Node's Buffer.from(str,
// 'base64') does: whitespace and non-alphabet characters are skipped, and
// missing padding is tolerated.
func decodeBase64Lenient(s string) []byte {
	var clean []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '/' || c == '=' {
			clean = append(clean, c)
		}
	}
	for len(clean)%4 != 0 {
		clean = append(clean, '=')
	}
	decoded, err := base64.StdEncoding.DecodeString(string(clean))
	if err != nil {
		return []byte{}
	}
	return decoded
}

func stripWhitespace(s string) string {
	var out []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' && c != '\v' && c != '\f' {
			out = append(out, c)
		}
	}
	return string(out)
}

// b64Encode is standard base64 with padding (Node's Buffer.toString('base64')).
func b64Encode(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}
