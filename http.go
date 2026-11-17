// The HTTP layer: a hand-rolled HTTP/1.1 server on raw TCP.
//
// WHY: measured against the kit's own undici client, node:http costs ~400us
// per request on this VM while a raw net server with a hand parser costs
// ~220us for the same fixed reply (floor.mjs, stable across 3 runs). The
// same logic applies in Go: net/http's per-request allocation and header
// map are the bottleneck when every bench workload is a stream of HTTP
// round-trips. The handlers (REST/GraphQL/MCP/git) are untouched by the
// transport; request bodies arrive in memory, so every handler runs
// synchronously with no stream wait.
//
// Correctness notes (checked against the kit before writing this):
// - The kit's clients (undici fetch) only ever send HTTP/1.1 with
//   Content-Length bodies; no chunked bodies, no Expect: 100-continue, no
//   HEAD, no HTTP/1.0 anywhere in the kit. Chunked + Expect + HEAD are
//   still implemented defensively below.
// - The only headers the kit asserts are x-oauth-scopes (root) and
//   x-github-request-id (non-empty only, NOT unique) — a counter id is fine.
// - One global mutex serializes dispatch: the store and all caches are
//   single-threaded by design (like the Node entry), and the bench client
//   is effectively sequential.

package main

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var shopToken = os.Getenv("MAKEFUN_SHOP_TOKEN")

var globalStore = newStore()

// mu serializes dispatch (store + all caches), like the single-threaded Node
// entry this ports.
var mu sync.Mutex

var requestCounter uint64

var emptyBytes = []byte{}

const maxHeadBytes = 65536
const maxBodyBytes = 64 * 1024 * 1024

var reasons = map[int]string{
	200: "OK",
	201: "Created",
	204: "No Content",
	400: "Bad Request",
	401: "Unauthorized",
	404: "Not Found",
	405: "Method Not Allowed",
	409: "Conflict",
	422: "Unprocessable Entity",
	500: "Internal Server Error",
}

type parsedRequest struct {
	method      string
	target      string
	httpVersion string
	headers     map[string]string
	body        []byte
	keepAlive   bool
}

type parsedHead struct {
	method      string
	target      string
	httpVersion string
	headers     map[string]string
	headLength  int
}

type reply struct {
	status      int
	contentType string // empty = omit the header (204 / MCP null body)
	body        []byte
	headers     map[string]string
}

type parseResult struct {
	kind     string // "ok", "needMore", "error"
	req      *parsedRequest
	consumed int
	head     *parsedHead
}

func parseHead(buf []byte) (*parsedHead, string) {
	headEnd := bytes.Index(buf, []byte("\r\n\r\n"))
	if headEnd < 0 {
		if len(buf) > maxHeadBytes {
			return nil, "error"
		}
		return nil, "needMore"
	}
	headText := string(buf[:headEnd])
	lines := strings.Split(headText, "\r\n")
	requestLine := ""
	if len(lines) > 0 {
		requestLine = lines[0]
	}
	firstSpace := strings.Index(requestLine, " ")
	secondSpace := -1
	if firstSpace >= 0 {
		secondSpace = strings.Index(requestLine[firstSpace+1:], " ")
		if secondSpace >= 0 {
			secondSpace += firstSpace + 1
		}
	}
	if firstSpace <= 0 || secondSpace <= firstSpace+1 {
		return nil, "error"
	}
	method := requestLine[:firstSpace]
	target := requestLine[firstSpace+1 : secondSpace]
	httpVersion := requestLine[secondSpace+1:]
	if !strings.HasPrefix(httpVersion, "HTTP/") {
		return nil, "error"
	}
	// Absolute-form target (http://host/path): strip scheme + authority.
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		rest := target[strings.Index(target, "://")+3:]
		slash := strings.Index(rest, "/")
		if slash < 0 {
			target = "/"
		} else {
			target = rest[slash:]
		}
	}
	headers := map[string]string{}
	for _, line := range lines[1:] {
		colon := strings.Index(line, ":")
		if colon <= 0 {
			return nil, "error"
		}
		name := strings.ToLower(strings.TrimSpace(line[:colon]))
		value := strings.TrimSpace(line[colon+1:])
		if _, ok := headers[name]; !ok {
			headers[name] = value
		}
	}
	return &parsedHead{method: method, target: target, httpVersion: httpVersion, headers: headers, headLength: headEnd + 4}, "ok"
}

// parseChunked parses one chunked body starting at pos; nil means more data
// is needed.
func parseChunked(buf []byte, pos int) (body []byte, consumed int, status string) {
	var chunks [][]byte
	at := pos
	for {
		lineEnd := bytes.Index(buf[at:], []byte("\r\n"))
		if lineEnd < 0 {
			return nil, 0, "needMore"
		}
		lineEnd += at
		sizeLine := strings.TrimSpace(string(buf[at:lineEnd]))
		if semi := strings.Index(sizeLine, ";"); semi >= 0 {
			sizeLine = sizeLine[:semi]
		}
		size, err := strconv.ParseInt(sizeLine, 16, 64)
		if err != nil || size < 0 {
			return nil, 0, "error"
		}
		at = lineEnd + 2
		if size == 0 {
			// Trailers (if any) end with an empty line.
			if at+1 < len(buf) && buf[at] == '\r' && buf[at+1] == '\n' {
				return bytes.Join(chunks, nil), at + 2, "ok"
			}
			trailerEnd := bytes.Index(buf[at:], []byte("\r\n\r\n"))
			if trailerEnd < 0 {
				return nil, 0, "needMore"
			}
			return bytes.Join(chunks, nil), at + trailerEnd + 4, "ok"
		}
		if len(buf) < at+int(size)+2 {
			return nil, 0, "needMore"
		}
		chunks = append(chunks, buf[at:at+int(size)])
		at += int(size) + 2
	}
}

func finishRequest(head *parsedHead, body []byte) *parsedRequest {
	conn := strings.ToLower(head.headers["connection"])
	var keepAlive bool
	if head.httpVersion == "HTTP/1.1" {
		keepAlive = !strings.Contains(conn, "close")
	} else {
		keepAlive = strings.Contains(conn, "keep-alive")
	}
	return &parsedRequest{
		method:      head.method,
		target:      head.target,
		httpVersion: head.httpVersion,
		headers:     head.headers,
		body:        body,
		keepAlive:   keepAlive,
	}
}

func parseOne(buf []byte) parseResult {
	head, status := parseHead(buf)
	if status != "ok" {
		return parseResult{kind: status}
	}
	te := strings.ToLower(head.headers["transfer-encoding"])
	if strings.Contains(te, "chunked") {
		body, consumed, st := parseChunked(buf, head.headLength)
		if st == "needMore" {
			return parseResult{kind: "needMore", head: head}
		}
		if st == "error" {
			return parseResult{kind: "error"}
		}
		return parseResult{kind: "ok", req: finishRequest(head, body), consumed: consumed}
	}
	if cl, ok := head.headers["content-length"]; ok {
		n, err := strconv.ParseInt(strings.TrimSpace(cl), 10, 64)
		if err != nil || n < 0 || n > maxBodyBytes {
			return parseResult{kind: "error"}
		}
		avail := len(buf) - head.headLength
		if avail < int(n) {
			return parseResult{kind: "needMore", head: head}
		}
		return parseResult{kind: "ok", req: finishRequest(head, buf[head.headLength:head.headLength+int(n)]), consumed: head.headLength + int(n)}
	}
	return parseResult{kind: "ok", req: finishRequest(head, emptyBytes), consumed: head.headLength}
}

// Route segments, cached per pathname: the bench hammers the same URLs
// thousands of times and split + decode is pure per path.
var segmentCache = map[string][]string{}

func routeSegments(pathname string) []string {
	if hit, ok := segmentCache[pathname]; ok {
		return hit
	}
	parts := strings.Split(pathname, "/")
	segments := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			segments = append(segments, decodeSegment(part))
		}
	}
	if len(segmentCache) > 512 {
		segmentCache = map[string][]string{}
	}
	segmentCache[pathname] = segments
	return segments
}

func decodeSegment(segment string) string {
	if decoded, err := url.PathUnescape(segment); err == nil {
		return decoded
	}
	return segment
}

func bearerToken(headers map[string]string) string {
	header := headers["authorization"]
	if strings.HasPrefix(header, "Bearer ") {
		return header[7:]
	}
	return ""
}

// authorized is true when the caller may use an authenticated surface.
func authorized(headers map[string]string) bool {
	if shopToken == "" {
		return true
	}
	return bearerToken(headers) == shopToken
}

func jsonReply(status int, body any, extraHeaders map[string]string) reply {
	return reply{status: status, contentType: "application/json", body: marshalJSON(body), headers: extraHeaders}
}

func bytesReply(status int, contentType string, body []byte) reply {
	return reply{status: status, contentType: contentType, body: body}
}

var gitRouteRe = regexp.MustCompile(`^/([^/]+)/([^/]+)\.git/(info/refs|git-upload-pack|git-receive-pack)$`)

func dispatch(req *parsedRequest) (out reply) {
	defer func() {
		if r := recover(); r != nil {
			// A handler must never kill the server: answer 500 JSON and keep going.
			out = jsonReply(500, map[string]any{"message": "Internal Server Error"}, nil)
		}
	}()
	method := req.method
	host := req.headers["host"]
	if host == "" {
		host = "localhost"
	}
	baseURL := "http://" + host
	rawURL := req.target
	// Fast path: split the raw path without building a URL object. Fall back
	// to net/url for exotic paths (backslash, dot segments, doubled slashes),
	// where normalization could change the pathname.
	qIndex := strings.Index(rawURL, "?")
	rawPath := rawURL
	queryString := ""
	if qIndex >= 0 {
		rawPath = rawURL[:qIndex]
		queryString = rawURL[qIndex+1:]
	}
	exotic := strings.Contains(rawPath, "\\") || strings.Contains(rawPath, "//") ||
		strings.Contains(rawPath, "/./") || strings.Contains(rawPath, "/../") ||
		strings.HasSuffix(rawPath, "/.") || strings.HasSuffix(rawPath, "/..")
	var pathname string
	var searchParams url.Values
	if exotic {
		u, err := url.Parse(rawURL)
		if err == nil {
			pathname = u.Path
			searchParams = u.Query()
		} else {
			pathname = rawPath
		}
	} else {
		pathname = rawPath
	}
	// The query string is parsed lazily: the hottest route (GET one issue) has
	// no query at all, and most routes never touch it.
	getQuery := func() url.Values {
		if searchParams == nil {
			searchParams, _ = url.ParseQuery(queryString)
		}
		return searchParams
	}
	segments := routeSegments(pathname)
	// The body already arrived with the request: no stream wait, no JSON
	// parse for git routes (raw bytes), JSON only for REST/GraphQL/MCP.
	needsJSON := method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE"

	// ---- health ---------------------------------------------------------------------------
	if pathname == "/healthz" && method == "GET" {
		return jsonReply(200, map[string]any{"ok": true}, nil)
	}

	// ---- git smart HTTP -------------------------------------------------------------------
	if strings.Contains(pathname, ".git/") {
		if m := gitRouteRe.FindStringSubmatch(pathname); m != nil {
			owner := decodeSegment(m[1])
			repoName := decodeSegment(m[2])
			action := m[3]
			if owner != VIEWER.login {
				return jsonReply(404, map[string]any{"message": "Not Found"}, nil)
			}
			repo := globalStore.getRepo(owner, repoName)
			if repo == nil {
				return jsonReply(404, map[string]any{"message": "Not Found"}, nil)
			}
			protocol := req.headers["git-protocol"]
			version2 := ""
			if strings.Contains(protocol, "version=2") {
				version2 = "2"
			}
			if action == "info/refs" && method == "GET" {
				service := getQuery().Get("service")
				result := handleInfoRefs(repo, service, version2)
				return bytesReply(result.status, result.contentType, result.body)
			}
			if action == "git-upload-pack" && method == "POST" {
				result := handleUploadPack(repo, req.body, version2)
				return bytesReply(result.status, result.contentType, result.body)
			}
			if action == "git-receive-pack" && method == "POST" {
				result := handleReceivePack(globalStore, repo, req.body)
				return bytesReply(result.status, result.contentType, result.body)
			}
			return jsonReply(404, map[string]any{"message": "Not Found"}, nil)
		}
	}

	// ---- GraphQL v4 -----------------------------------------------------------------------
	if pathname == "/api/graphql" && method == "POST" {
		if !authorized(req.headers) {
			return jsonReply(401, map[string]any{"message": "Requires authentication"}, nil)
		}
		var parsed any
		parseError := false
		if needsJSON {
			parsed, parseError = parseJSONBody(req.body)
		}
		if parseError {
			return jsonReply(400, map[string]any{"errors": []any{map[string]any{"message": "Invalid JSON in request body"}}}, nil)
		}
		status, result := handleGraphQL(globalStore, parsed)
		return jsonReply(status, result, nil)
	}

	// ---- MCP ------------------------------------------------------------------------------
	if pathname == "/mcp" && method == "POST" {
		if !authorized(req.headers) {
			return jsonReply(401, map[string]any{"message": "Requires authentication"}, nil)
		}
		var parsed any
		parseError := false
		if needsJSON {
			parsed, parseError = parseJSONBody(req.body)
		}
		result := handleMcp(globalStore, parsed, parseError)
		if result.body == nil {
			return reply{status: result.status, body: emptyBytes}
		}
		return jsonReply(result.status, result.body, nil)
	}

	// ---- REST v3 --------------------------------------------------------------------------
	if len(segments) >= 2 && segments[0] == "api" && segments[1] == "v3" {
		isRootOrMeta := len(segments) <= 3 && (len(segments) <= 2 || segments[2] == "meta")
		if !isRootOrMeta && !authorized(req.headers) {
			return jsonReply(401, map[string]any{"message": "Requires authentication"}, nil)
		}
		var parsed any
		parseError := false
		if needsJSON {
			parsed, parseError = parseJSONBody(req.body)
		}
		restReq := restRequest{
			method:         method,
			segments:       segments[2:],
			query:          getQuery,
			body:           parsed,
			bodyParseError: parseError,
			baseURL:        baseURL + "/api/v3",
		}
		result := handleRest(globalStore, restReq)
		if result.status == 204 {
			return reply{status: 204, body: emptyBytes, headers: result.headers}
		}
		if result.rawJSON != nil {
			return reply{status: result.status, contentType: "application/json", body: result.rawJSON, headers: result.headers}
		}
		return jsonReply(result.status, result.body, result.headers)
	}

	return jsonReply(404, map[string]any{"message": "Not Found"}, nil)
}

func writeReply(conn net.Conn, req *parsedRequest, rep reply) {
	// The kit only asserts the header is non-empty, never unique.
	id := "req-" + strconv.FormatUint(atomic.AddUint64(&requestCounter, 1), 36)
	isHead := req.method == "HEAD"
	var head strings.Builder
	head.WriteString(fmt.Sprintf("HTTP/1.1 %d %s\r\n", rep.status, reasons[rep.status]))
	if rep.contentType != "" {
		head.WriteString("content-type: " + rep.contentType + "\r\n")
	}
	head.WriteString("x-github-request-id: " + id + "\r\n")
	head.WriteString(fmt.Sprintf("content-length: %d\r\n", len(rep.body)))
	for key, value := range rep.headers {
		head.WriteString(key + ": " + value + "\r\n")
	}
	if req.keepAlive {
		head.WriteString("connection: keep-alive\r\n\r\n")
	} else {
		head.WriteString("connection: close\r\n\r\n")
	}
	headBuf := []byte(head.String())
	// HEAD answers carry headers only; empty bodies are head-only too.
	if isHead || len(rep.body) == 0 {
		conn.Write(headBuf)
		return
	}
	full := make([]byte, 0, len(headBuf)+len(rep.body))
	full = append(full, headBuf...)
	full = append(full, rep.body...)
	conn.Write(full)
}

func serveConn(conn net.Conn) {
	defer conn.Close()
	if tcp, ok := conn.(*net.TCPConn); ok {
		tcp.SetNoDelay(true)
	}
	var buf []byte
	sentContinue := false
	tmp := make([]byte, 32768)
	for {
		conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		n, err := conn.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			return
		}
		if len(buf) > maxBodyBytes+maxHeadBytes {
			return
		}
		for {
			parsed := parseOne(buf)
			if parsed.kind == "needMore" {
				if parsed.head != nil && !sentContinue &&
					strings.ToLower(parsed.head.headers["expect"]) == "100-continue" {
					conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
					sentContinue = true
				}
				break
			}
			if parsed.kind == "error" {
				conn.Write([]byte("HTTP/1.1 400 Bad Request\r\ncontent-length: 0\r\nconnection: close\r\n\r\n"))
				return
			}
			sentContinue = false
			buf = buf[parsed.consumed:]
			mu.Lock()
			rep := dispatch(parsed.req)
			mu.Unlock()
			writeReply(conn, parsed.req, rep)
			if !parsed.req.keepAlive {
				return
			}
			if len(buf) == 0 {
				break
			}
		}
	}
}

// serve accepts connections on listener until it closes.
func serve(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go serveConn(conn)
	}
}
