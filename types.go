// Shared transport + JSON helpers (contract reference implementation).
// Builders: copy this file as-is into your scratch dir. Do not modify the signatures.
package main

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
)

type ApiError struct {
	Status  int
	Message string
}

func (e *ApiError) Error() string { return e.Message }

func NewApiError(status int, msg string) *ApiError {
	return &ApiError{Status: status, Message: msg}
}

type Request struct {
	Method   string
	Path     string // raw path as sent, no query, no fragment
	RawQuery string // raw query string without '?'
	Query    url.Values
	Headers  map[string]string // keys lower-cased
	Body     []byte
	ID       string // "req-N", minted once per request
}

func (r *Request) Header(name string) string {
	if r.Headers == nil {
		return ""
	}
	return r.Headers[strings.ToLower(name)]
}

type Response struct {
	Status int
	Header [][2]string // ordered pairs, written in this order
	Body   []byte
}

// marshalJSON mirrors TS JSON.stringify for these payloads: no HTML escaping
// (Go's default escapes <, >, & — TS does not), no trailing newline.
func marshalJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// Encoder.Encode appends '\n'; trim it — callers add it where TS does.
	if err := enc.Encode(v); err != nil {
		return []byte("null")
	}
	b := buf.Bytes()
	if len(b) > 0 && b[len(b)-1] == '\n' {
		b = b[:len(b)-1]
	}
	return b
}

func jsonHeaders(status int, bodyLen int, reqID string, extra [][2]string) [][2]string {
	h := [][2]string{
		{"content-type", "application/json; charset=utf-8"},
		{"content-length", itoa(bodyLen)},
		{"x-github-request-id", reqID},
	}
	return append(h, extra...)
}

// SendJSON mirrors TS sendJson: body = JSON + '\n'; status 204 has no
// content-type/length and an empty body.
func SendJSON(status int, v any, reqID string, extra ...[2]string) *Response {
	if status == 204 {
		h := [][2]string{{"x-github-request-id", reqID}}
		return &Response{Status: 204, Header: append(h, extra...)}
	}
	body := append(marshalJSON(v), '\n')
	return &Response{Status: status, Header: jsonHeaders(status, len(body), reqID, extra), Body: body}
}

// SendRaw mirrors TS sendRawJson: body is used exactly as given.
func SendRaw(status int, body []byte, reqID string, extra ...[2]string) *Response {
	return &Response{Status: status, Header: jsonHeaders(status, len(body), reqID, extra), Body: body}
}

func ErrJSON(status int, msg string, reqID string) *Response {
	return SendJSON(status, map[string]string{"message": msg}, reqID)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
