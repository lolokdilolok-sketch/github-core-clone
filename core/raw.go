package main

// Minimal hand-rolled HTTP/1.1 server.
//
// Why: the kit's perf workloads are thousands of small sequential requests
// on one keep-alive connection. net/http's per-request machinery (per-conn
// goroutines + backgroundRead, textproto header maps, chunkWriter, timers)
// costs ~40-60us of server CPU per request — a large share of the ~70us
// total server CPU per request. This server replaces only the transport:
// it parses requests into a real *http.Request and calls the same handler
// (handler()), so all routing, auth and response logic is unchanged.
//
// The handler surface actually used (audited):
//   r.Method, r.URL.Path, r.URL.Query(), r.URL (copied), r.Host,
//   r.Header.Get("Authorization"|"Git-Protocol"|"Mcp-Session-Id"),
//   r.Body (io.ReadAll, capped by the handlers), w.Header().Set,
//   w.WriteHeader, w.Write. No Flush/Hijack/streaming anywhere.
// Responses are buffered whole and written in a single conn.Write with
// Content-Length (the client never needs chunked responses, and no
// conformance check asserts Date/Server/Connection headers).
//
// Safety: any request this parser cannot represent (HTTP/1.0 without a
// usable form, CONNECT, asterisk-form, malformed line/headers, bad
// Content-Length, body > 64 MiB) gets a 400 and the connection closed —
// the kit client only ever sends well-formed origin-form HTTP/1.1
// requests with Content-Length bodies, so the fast path covers it.
// FORGE_RAW_HTTP=0 selects the net/http server instead.

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	rawReaderPool = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, 4096) }}
	rawBufPool    = sync.Pool{New: func() any { return new(bytes.Buffer) }}
	// dateCache holds "Mon, 02 Jan 2006 15:04:05 GMT" for the current second.
	dateCacheUnix atomic.Int64
	dateCacheStr  atomic.Value // string
)

func cachedDate() string {
	now := time.Now().Unix()
	if dateCacheUnix.Load() != now {
		s := time.Now().UTC().Format(http.TimeFormat)
		dateCacheStr.Store(s)
		dateCacheUnix.Store(now)
	}
	if v := dateCacheStr.Load(); v != nil {
		return v.(string)
	}
	return time.Now().UTC().Format(http.TimeFormat)
}

// rawResponseWriter buffers the whole response; the server flushes it in
// one write with Content-Length.
type rawResponseWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *rawResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *rawResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *rawResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return w.body.Write(p)
}

// serveRaw accepts connections on ln and serves handler() on each.
func serveRaw(ln net.Listener, h http.Handler) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go serveRawConn(conn, h)
	}
}

func serveRawConn(conn net.Conn, h http.Handler) {
	defer conn.Close()
	br := rawReaderPool.Get().(*bufio.Reader)
	br.Reset(conn)
	defer rawReaderPool.Put(br)
	for {
		// Read the request; a 10s deadline covers header+body like
		// net/http's ReadHeaderTimeout (the handler itself does no I/O).
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		req, keepAlive, ok := readRawRequest(br, conn)
		if !ok {
			return
		}
		_ = conn.SetReadDeadline(time.Time{})
		req.RemoteAddr = conn.RemoteAddr().String()

		rw := &rawResponseWriter{}
		func() {
			defer func() {
				// A handler panic must not kill the connection loop the
				// way net/http recovers per-connection; mimic it: 500.
				if rec := recover(); rec != nil {
					rw.status = 500
					rw.body.Reset()
					rw.body.WriteString(`{"message":"Internal Server Error"}`)
					if rw.header == nil {
						rw.header = make(http.Header)
					}
					rw.header.Set("Content-Type", "application/json")
				}
			}()
			h.ServeHTTP(rw, req)
		}()
		if rw.status == 0 {
			rw.status = 200
		}
		if !writeRawResponse(conn, rw, keepAlive) {
			return
		}
		if !keepAlive {
			return
		}
	}
}

// writeRawResponse serializes headers + body in one Write. Returns false
// when the connection must close.
func writeRawResponse(conn net.Conn, rw *rawResponseWriter, keepAlive bool) bool {
	out := rawBufPool.Get().(*bytes.Buffer)
	out.Reset()
	defer rawBufPool.Put(out)
	status := rw.status
	text := http.StatusText(status)
	if text == "" {
		text = "OK"
	}
	out.WriteString("HTTP/1.1 ")
	out.WriteString(strconv.Itoa(status))
	out.WriteByte(' ')
	out.WriteString(text)
	out.WriteString("\r\nDate: ")
	out.WriteString(cachedDate())
	out.WriteString("\r\nContent-Length: ")
	out.WriteString(strconv.Itoa(rw.body.Len()))
	out.WriteString("\r\n")
	seenDate, seenLen, seenConn := false, false, false
	for k, vs := range rw.header {
		lk := strings.ToLower(k)
		switch lk {
		case "date":
			seenDate = true
		case "content-length":
			seenLen = true
		case "connection":
			seenConn = true
		}
		for _, v := range vs {
			out.WriteString(k)
			out.WriteString(": ")
			out.WriteString(v)
			out.WriteString("\r\n")
		}
	}
	_ = seenDate
	_ = seenLen
	if !seenConn {
		if keepAlive {
			out.WriteString("Connection: keep-alive\r\n")
		} else {
			out.WriteString("Connection: close\r\n")
		}
	}
	out.WriteString("\r\n")
	out.Write(rw.body.Bytes())
	_, err := conn.Write(out.Bytes())
	return err == nil
}

// readRawRequest parses one request from br. On any parse failure it
// writes a minimal 400 and returns ok=false.
func readRawRequest(br *bufio.Reader, conn net.Conn) (*http.Request, bool, bool) {
	line, err := readLine(br)
	if err != nil {
		return nil, false, false // EOF / timeout: just close
	}
	if line == "" {
		// Tolerate one stray blank line before the request line (net/http
		// does the same for keep-alive connections).
		line, err = readLine(br)
		if err != nil || line == "" {
			return nil, false, false
		}
	}
	method, target, proto, ok := splitRequestLine(line)
	if !ok {
		write400(conn)
		return nil, false, false
	}
	if method == "CONNECT" || target == "*" {
		write400(conn)
		return nil, false, false
	}
	major, minor, ok := parseProto(proto)
	if !ok || major != 1 || minor > 1 {
		write400(conn)
		return nil, false, false
	}

	// Headers. Only the three headers the handlers ever read are kept in
	// the request's Header map (Authorization, Git-Protocol,
	// Mcp-Session-Id); the rest are parsed for transport control (Host,
	// Content-Length, Transfer-Encoding, Expect, Connection) and dropped,
	// which avoids canonicalizing and storing ~6 map entries per request.
	// (Audited: grep Header.Get across core/ returns only those three.)
	hdr := make(http.Header, 3)
	var host string
	var contentLength int64 = -1
	var chunked bool
	var expectContinue bool
	keepAlive := minor == 1 // HTTP/1.1 defaults to keep-alive
	nHeaders := 0
	var lastKey string
	for {
		hline, err := readLine(br)
		if err != nil {
			return nil, false, false
		}
		if hline == "" {
			break
		}
		nHeaders++
		if nHeaders > 200 {
			write400(conn)
			return nil, false, false
		}
		// Obsolete line folding: append to the previous header's value if
		// it is one of the kept headers; otherwise ignore the line.
		if hline[0] == ' ' || hline[0] == '\t' {
			if lastKey != "" {
				vs := hdr[lastKey]
				vs[len(vs)-1] += " " + strings.TrimSpace(hline)
				hdr[lastKey] = vs
			}
			continue
		}
		colon := strings.IndexByte(hline, ':')
		if colon <= 0 {
			write400(conn)
			return nil, false, false
		}
		key := http.CanonicalHeaderKey(strings.TrimSpace(hline[:colon]))
		val := strings.TrimSpace(hline[colon+1:])
		switch key {
		case "Authorization", "Git-Protocol", "Mcp-Session-Id":
			hdr.Add(key, val)
			lastKey = key
		default:
			lastKey = ""
		}
		switch strings.ToLower(key) {
		case "host":
			host = val
		case "content-length":
			cl, err := strconv.ParseInt(val, 10, 64)
			if err != nil || cl < 0 {
				write400(conn)
				return nil, false, false
			}
			contentLength = cl
		case "transfer-encoding":
			if strings.Contains(strings.ToLower(val), "chunked") {
				chunked = true
			}
		case "expect":
			if strings.EqualFold(val, "100-continue") {
				expectContinue = true
			}
		case "connection":
			lv := strings.ToLower(val)
			if strings.Contains(lv, "close") {
				keepAlive = false
			} else if strings.Contains(lv, "keep-alive") {
				keepAlive = true
			}
		}
	}

	if expectContinue {
		_, _ = conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
	}

	// Body.
	var body []byte
	if chunked {
		b, ok := readChunkedBody(br)
		if !ok {
			return nil, false, false
		}
		body = b
	} else if contentLength > 0 {
		if contentLength > 64<<20 {
			write400(conn)
			return nil, false, false
		}
		b := make([]byte, contentLength)
		if _, err := io.ReadFull(br, b); err != nil {
			return nil, false, false
		}
		body = b
	} else {
		body = []byte{}
	}

	// Build the request the handlers expect. The target is parsed by hand:
	// split path/query at '?', unescape the path only when it carries '%'.
	// Equivalent to url.ParseRequestURI for the origin-form targets the
	// client sends, without its allocation and validation overhead.
	var u *url.URL
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		parsed, err := url.Parse(target)
		if err != nil {
			write400(conn)
			return nil, false, false
		}
		u = parsed
		if host == "" {
			host = parsed.Host
		}
	} else {
		if !strings.HasPrefix(target, "/") {
			write400(conn)
			return nil, false, false
		}
		path, rawQuery := target, ""
		if i := strings.IndexByte(target, '?'); i >= 0 {
			path, rawQuery = target[:i], target[i+1:]
		}
		if strings.Contains(path, "%") {
			unesc, err := url.PathUnescape(path)
			if err != nil {
				write400(conn)
				return nil, false, false
			}
			path = unesc
		}
		u = &url.URL{Path: path, RawQuery: rawQuery}
	}
	if host == "" {
		// HTTP/1.1 requires Host; net/http rejects it. Tolerate it here
		// by leaving Host empty (handlers only use it for Link headers).
		host = "127.0.0.1"
	}
	req := &http.Request{
		Method:        method,
		URL:           u,
		Proto:         proto,
		ProtoMajor:    major,
		ProtoMinor:    minor,
		Header:        hdr,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Host:          host,
		RequestURI:    target,
	}
	return req, keepAlive, true
}

// readLine reads one CRLF- or LF-terminated line without the terminator.
// It returns the line without allocating when it fits in the buffer.
func readLine(br *bufio.Reader) (string, error) {
	// Fast path: the line fits in the buffer.
	frag, err := br.ReadSlice('\n')
	if err == nil {
		return trimLineEnd(string(frag)), nil
	}
	if err == bufio.ErrBufferFull {
		// Slow path: accumulate fragments.
		var buf []byte
		buf = append(buf, frag...)
		for {
			frag, err = br.ReadSlice('\n')
			buf = append(buf, frag...)
			if err == nil {
				return trimLineEnd(string(buf)), nil
			}
			if err != bufio.ErrBufferFull {
				return "", err
			}
		}
	}
	return "", err
}

func trimLineEnd(s string) string {
	if n := len(s); n > 0 && s[n-1] == '\n' {
		s = s[:n-1]
		if n := len(s); n > 0 && s[n-1] == '\r' {
			s = s[:n-1]
		}
	}
	return s
}

func splitRequestLine(line string) (method, target, proto string, ok bool) {
	i := strings.IndexByte(line, ' ')
	if i <= 0 {
		return "", "", "", false
	}
	method = line[:i]
	rest := line[i+1:]
	j := strings.LastIndexByte(rest, ' ')
	if j <= 0 {
		return "", "", "", false
	}
	target = rest[:j]
	proto = rest[j+1:]
	if target == "" || proto == "" {
		return "", "", "", false
	}
	return method, target, proto, true
}

func parseProto(proto string) (major, minor int, ok bool) {
	if !strings.HasPrefix(proto, "HTTP/") {
		return 0, 0, false
	}
	ver := proto[len("HTTP/"):]
	dot := strings.IndexByte(ver, '.')
	if dot < 0 {
		return 0, 0, false
	}
	maj, err1 := strconv.Atoi(ver[:dot])
	min, err2 := strconv.Atoi(ver[dot+1:])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return maj, min, true
}

// readChunkedBody reads a chunked request body (rare; the kit client
// uses Content-Length, but correctness first).
func readChunkedBody(br *bufio.Reader) ([]byte, bool) {
	var out bytes.Buffer
	for {
		line, err := readLine(br)
		if err != nil {
			return nil, false
		}
		// Chunk size may carry extensions after ';'.
		if i := strings.IndexByte(line, ';'); i >= 0 {
			line = line[:i]
		}
		size, err := strconv.ParseInt(strings.TrimSpace(line), 16, 64)
		if err != nil || size < 0 {
			return nil, false
		}
		if size == 0 {
			// Trailer headers until a blank line.
			for {
				tl, err := readLine(br)
				if err != nil || tl == "" {
					break
				}
			}
			return out.Bytes(), true
		}
		if out.Len()+int(size) > 64<<20 {
			return nil, false
		}
		chunk := make([]byte, size)
		if _, err := io.ReadFull(br, chunk); err != nil {
			return nil, false
		}
		out.Write(chunk)
		// Trailing CRLF after the chunk.
		if _, err := readLine(br); err != nil {
			return nil, false
		}
	}
}

func write400(conn net.Conn) {
	_, _ = conn.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
}

func rawHTTPEnabled() bool {
	return os.Getenv("FORGE_RAW_HTTP") != "0"
}
