// Raw TCP HTTP/1.1 transport: request parsing, keep-alive, single-write
// responses. The dispatcher (main.go) owns routing, the request counter and
// the global store mutex; this file only reads bytes off the wire and writes
// bytes back. Handlers receive a fully-read Request and return a Response.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

var (
	errBadRequestLine = errors.New("bad request line")
	errBadHeader      = errors.New("bad header line")
	errBadChunkSize   = errors.New("bad chunk size")
	errHeadersTooBig  = errors.New("headers too large")
	errBodyTooBig     = errors.New("body too large")
)

// Caps: generous enough for real git pushes, small enough that a broken
// client cannot grow memory without bound.
const (
	maxHeaderLines = 200
	maxHeaderBytes = 64 * 1024
	maxBodyBytes   = 512 * 1024 * 1024
)

// bufPool recycles the scratch buffers used to assemble a response and to
// accumulate a chunked body.
var bufPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

func getBuf() *bytes.Buffer {
	b := bufPool.Get().(*bytes.Buffer)
	b.Reset()
	return b
}

func putBuf(b *bytes.Buffer) {
	if b.Cap() > 1<<20 {
		return // do not pool huge buffers
	}
	bufPool.Put(b)
}

// serveListener accepts connections forever (until the listener closes),
// enables TCP_NODELAY on each, and hands it to its own goroutine.
func serveListener(ln net.Listener, dispatch func(*Request) *Response) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				continue
			}
			return
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetNoDelay(true)
		}
		go serveConn(conn, dispatch)
	}
}

// serveConn reads requests sequentially on one connection until the client
// goes away, the connection is not keep-alive, or a read fails.
func serveConn(conn net.Conn, dispatch func(*Request) *Response) {
	defer conn.Close()
	br := bufio.NewReaderSize(conn, 8192)
	for {
		req, proto, keepAlive, err := readRequest(br, conn)
		if err != nil {
			return
		}
		resp := dispatch(req)
		if resp == nil {
			return
		}
		if err := writeResponse(conn, resp, proto, keepAlive); err != nil {
			return
		}
		if !keepAlive {
			return
		}
	}
}

// readRequest parses one request line + headers + body. It returns the
// Request (ID still empty — the dispatcher mints it), whether the
// connection stays open afterwards, and any read error.
func readRequest(br *bufio.Reader, conn net.Conn) (*Request, string, bool, error) {
	// Tolerate leading blank lines (a stray CRLF before the request line).
	var line string
	var err error
	for {
		line, err = br.ReadString('\n')
		if err != nil {
			return nil, "", false, err
		}
		if line != "\r\n" && line != "\n" {
			break
		}
	}
	line = strings.TrimRight(line, "\r\n")
	method, target, proto, ok := splitRequestLine(line)
	if !ok {
		return nil, "", false, errBadRequestLine
	}

	headers := make(map[string]string)
	headerBytes := len(line)
	for i := 0; i < maxHeaderLines; i++ {
		hline, err := br.ReadString('\n')
		if err != nil {
			return nil, "", false, err
		}
		headerBytes += len(hline)
		if headerBytes > maxHeaderBytes {
			return nil, "", false, errHeadersTooBig
		}
		if hline == "\r\n" || hline == "\n" {
			goto headersDone
		}
		key, value, ok := splitHeader(strings.TrimRight(hline, "\r\n"))
		if !ok {
			return nil, "", false, errBadHeader
		}
		key = strings.ToLower(key)
		if prev, dup := headers[key]; dup {
			headers[key] = prev + ", " + value
		} else {
			headers[key] = value
		}
	}
	return nil, "", false, errHeadersTooBig

headersDone:
	// 100-continue: answer before the client sends the body.
	if strings.EqualFold(headers["expect"], "100-continue") {
		_, _ = conn.Write([]byte(proto + " 100 Continue\r\n\r\n"))
	}

	body, err := readBody(br, headers)
	if err != nil {
		return nil, "", false, err
	}

	path, rawQuery := splitTarget(target)
	query, _ := url.ParseQuery(rawQuery)
	req := &Request{
		Method:   method,
		Path:     path,
		RawQuery: rawQuery,
		Query:    query,
		Headers:  headers,
		Body:     body,
	}
	return req, proto, wantsKeepAlive(proto, headers), nil
}

// splitRequestLine splits "METHOD SP TARGET SP PROTO" (exactly three fields).
func splitRequestLine(line string) (method, target, proto string, ok bool) {
	first := strings.IndexByte(line, ' ')
	if first < 0 {
		return "", "", "", false
	}
	second := strings.IndexByte(line[first+1:], ' ')
	if second < 0 {
		return "", "", "", false
	}
	second += first + 1
	method, target, proto = line[:first], line[first+1:second], line[second+1:]
	if method == "" || target == "" || !strings.HasPrefix(proto, "HTTP/") {
		return "", "", "", false
	}
	return method, target, proto, true
}

// splitHeader splits "Name: value" at the first colon.
func splitHeader(line string) (key, value string, ok bool) {
	i := strings.IndexByte(line, ':')
	if i <= 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:]), true
}

// splitTarget returns the raw path (no query, no fragment) and the raw
// query string. Absolute-form targets (http://host/path?...) are reduced to
// their path; a target that will not parse is used verbatim as the path.
func splitTarget(target string) (path, rawQuery string) {
	if target == "*" {
		return "*", ""
	}
	u, err := url.Parse(target)
	if err != nil {
		return target, ""
	}
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	return p, u.RawQuery
}

// wantsKeepAlive mirrors HTTP/1.1 defaults: keep-alive unless
// "Connection: close", or HTTP/1.0 without an explicit keep-alive.
func wantsKeepAlive(proto string, headers map[string]string) bool {
	conn := strings.ToLower(headers["connection"])
	if strings.Contains(conn, "close") {
		return false
	}
	if proto == "HTTP/1.0" {
		return strings.Contains(conn, "keep-alive")
	}
	return true
}

// readBody reads a Content-Length body, or decodes a chunked body. No
// length and no chunked encoding means an empty body.
func readBody(br *bufio.Reader, headers map[string]string) ([]byte, error) {
	te := strings.ToLower(headers["transfer-encoding"])
	if strings.Contains(te, "chunked") {
		return readChunkedBody(br)
	}
	cl := strings.TrimSpace(headers["content-length"])
	if cl == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(cl)
	if err != nil || n < 0 {
		return nil, errBadRequestLine
	}
	if n == 0 {
		return nil, nil
	}
	if n > maxBodyBytes {
		return nil, errBodyTooBig
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, err
	}
	return body, nil
}

// readChunkedBody decodes chunked transfer encoding, including chunk
// extensions and trailing headers. Never crashes on chunked input: any
// malformed chunk is an error, not a panic.
func readChunkedBody(br *bufio.Reader) ([]byte, error) {
	buf := getBuf()
	defer putBuf(buf)
	for {
		sizeLine, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		sizeLine = strings.TrimRight(sizeLine, "\r\n")
		if i := strings.IndexByte(sizeLine, ';'); i >= 0 {
			sizeLine = sizeLine[:i] // chunk extension
		}
		sizeLine = strings.TrimSpace(sizeLine)
		n, err := strconv.ParseInt(sizeLine, 16, 64)
		if err != nil || n < 0 || n > maxBodyBytes {
			return nil, errBadChunkSize
		}
		if n == 0 {
			// Trailers run to a blank line.
			for {
				t, err := br.ReadString('\n')
				if err != nil {
					return nil, err
				}
				if t == "\r\n" || t == "\n" {
					out := make([]byte, buf.Len())
					copy(out, buf.Bytes())
					return out, nil
				}
			}
		}
		if int64(buf.Len())+n > maxBodyBytes {
			return nil, errBodyTooBig
		}
		if _, err := io.CopyN(buf, br, n); err != nil {
			return nil, err
		}
		// Chunk data is followed by CRLF (tolerate a bare LF).
		c, err := br.ReadByte()
		if err != nil {
			return nil, err
		}
		if c == '\r' {
			if c2, err := br.ReadByte(); err != nil || c2 != '\n' {
				return nil, errBadChunkSize
			}
		} else if c != '\n' {
			// The byte is the start of the next size line; unread it.
			if err := br.UnreadByte(); err != nil {
				return nil, err
			}
		}
	}
}

// writeResponse serializes the status line, headers and body into one
// buffer and issues a single Write to the connection.
func writeResponse(conn net.Conn, resp *Response, proto string, keepAlive bool) error {
	buf := getBuf()
	defer putBuf(buf)

	buf.WriteString(proto)
	buf.WriteByte(' ')
	buf.WriteString(itoa(resp.Status))
	buf.WriteByte(' ')
	buf.WriteString(reasonPhrase(resp.Status))
	buf.WriteString("\r\n")

	seenContentLength := false
	for _, h := range resp.Header {
		// Header values come from our own code; strip CR/LF defensively so
		// a value can never inject a forged header line.
		buf.WriteString(h[0])
		buf.WriteString(": ")
		buf.WriteString(strings.ReplaceAll(strings.ReplaceAll(h[1], "\r", ""), "\n", ""))
		buf.WriteString("\r\n")
		if strings.EqualFold(h[0], "content-length") {
			seenContentLength = true
		}
	}
	// 204/304 must carry no content-length (contract: the 204 form has no
	// content-type/length headers); every other response gets one if the
	// handler did not supply it.
	if !seenContentLength && resp.Status != 204 && resp.Status != 304 {
		buf.WriteString("content-length: ")
		buf.WriteString(itoa(len(resp.Body)))
		buf.WriteString("\r\n")
	}
	if keepAlive {
		buf.WriteString("connection: keep-alive\r\n")
	} else {
		buf.WriteString("connection: close\r\n")
	}
	buf.WriteString("\r\n")
	buf.Write(resp.Body)

	_, err := conn.Write(buf.Bytes())
	return err
}

func reasonPhrase(status int) string {
	switch status {
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 204:
		return "No Content"
	case 301:
		return "Moved Permanently"
	case 302:
		return "Found"
	case 304:
		return "Not Modified"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 403:
		return "Forbidden"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 409:
		return "Conflict"
	case 422:
		return "Unprocessable Entity"
	case 500:
		return "Internal Server Error"
	case 502:
		return "Bad Gateway"
	case 503:
		return "Service Unavailable"
	default:
		return "Status"
	}
}
