package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var (
	store       = NewStore()
	authToken   = os.Getenv("MAKEFUN_SHOP_TOKEN")
	viewerLogin = "adaiversen"
	viewerID    = 1000
)

func main() {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	srv := &http.Server{Handler: handler(), ReadHeaderTimeout: 10 * time.Second}

	envURL := os.Getenv("MAKEFUN_ENV_URL")
	if envURL == "" {
		// Local / test mode: serve forever.
		fmt.Fprintf(os.Stderr, "forge listening on :%d (no MAKEFUN_ENV_URL)\n", port)
		if rawHTTPEnabled() {
			serveRaw(ln, handler())
			return
		}
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(os.Stderr, "serve:", err)
			os.Exit(1)
		}
		return
	}
	if rawHTTPEnabled() {
		go serveRaw(ln, handler())
	} else {
		go func() {
			_ = srv.Serve(ln)
		}()
	}
	// Register with the harness, retrying until it answers.
	regURL := strings.TrimRight(envURL, "/") + "/register"
	body, _ := json.Marshal(map[string]int{"port": port})
	client := &http.Client{Timeout: 5 * time.Second}
	for {
		req, _ := http.NewRequest("POST", regURL, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if authToken != "" {
			req.Header.Set("Authorization", "Bearer "+authToken)
		}
		resp, err := client.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 || resp.StatusCode == 201 || resp.StatusCode == 409 {
				break
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	// Poll /done until the harness says so.
	doneURL := strings.TrimRight(envURL, "/") + "/done"
	for {
		resp, err := client.Get(doneURL)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			var d struct {
				Done bool `json:"done"`
			}
			if json.Unmarshal(b, &d) == nil && d.Done {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	srv.Close()
	os.Exit(0)
}

// ---------------------------------------------------------------------------
// HTTP plumbing
// ---------------------------------------------------------------------------

func handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// request id for every response
		id := atomic.AddInt64(&store.reqID, 1)
		reqID := "req-" + strconv.FormatInt(id, 10)
		w.Header().Set("X-GitHub-Request-Id", reqID)
		// Normalize: the harness mounts us at the root.
		p := r.URL.Path
		switch {
		case p == "/" || p == "/api/v3" || p == "/api/v3/":
			if !auth(w, r) {
				return
			}
			w.Header().Set("X-OAuth-Scopes", "repo, read:org")
			writeJSON(w, 200, map[string]any{
				"current_user_url": "/api/v3/user",
				"rate_limit_url":   "/api/v3/rate_limit",
			})
			return
		case p == "/api/v3/meta":
			if !auth(w, r) {
				return
			}
			writeJSON(w, 200, map[string]any{"installed_version": "1.0.0"})
			return
		case strings.HasPrefix(p, "/api/v3/"):
			restHandler(w, r, strings.TrimPrefix(p, "/api/v3/"))
			return
		case p == "/api/graphql":
			graphqlHandler(w, r)
			return
		case p == "/mcp":
			mcpHandler(w, r)
			return
		case strings.Contains(p, "/info/refs"):
			gitInfoRefs(w, r)
			return
		case strings.Contains(p, "/git-upload-pack"):
			gitUploadPack(w, r)
			return
		case strings.Contains(p, "/git-receive-pack"):
			gitReceivePack(w, r)
			return
		case p == "/healthz":
			w.WriteHeader(200)
			w.Write([]byte("ok"))
			return
		default:
			writeJSON(w, 404, map[string]any{"message": "Not Found"})
			return
		}
	})
}

// auth checks the Bearer token. When no token is configured (local dev) any
// non-empty Bearer is accepted; missing Bearer is always 401.
func auth(w http.ResponseWriter, r *http.Request) bool {
	tok := bearer(r)
	if tok == "" {
		writeJSON(w, 401, map[string]any{"message": "Requires authentication"})
		return false
	}
	if authToken != "" && tok != authToken {
		writeJSON(w, 401, map[string]any{"message": "Bad credentials"})
		return false
	}
	return true
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	if strings.HasPrefix(h, "token ") {
		return strings.TrimSpace(h[6:])
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	w.Write(b)
}

func writeJSONBytes(w http.ResponseWriter, status int, b []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(b)
}

func errJSON(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"message": msg})
}

// readJSONBody parses a request body as a generic object; returns nil, nil
// for an empty body; on malformed JSON writes 400 and returns err.
func readJSONBody(w http.ResponseWriter, r *http.Request) (map[string]any, error) {
	b, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		errJSON(w, 400, "cannot read body")
		return nil, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		errJSON(w, 400, "Problems parsing JSON")
		return nil, err
	}
	return m, nil
}

func strField(m map[string]any, k string) string {
	if v, ok := m[k]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func boolField(m map[string]any, k string) bool {
	if v, ok := m[k]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

func intField(m map[string]any, k string) (int, bool) {
	if v, ok := m[k]; ok {
		switch n := v.(type) {
		case float64:
			return int(n), true
		case int:
			return n, true
		}
	}
	return 0, false
}

// paginate slices items per per_page/page and returns (page items, hasNext).
func paginate[T any](items []T, r *http.Request) ([]T, bool) {
	perPage := 30
	if v := r.URL.Query().Get("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			perPage = n
		}
	}
	page := 1
	if v := r.URL.Query().Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	start := (page - 1) * perPage
	if start >= len(items) {
		return []T{}, false
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	return items[start:end], end < len(items)
}

// linkHeader builds a GitHub-style Link header for the next page, as an
// absolute URL on this entry's origin.
func linkHeader(w http.ResponseWriter, r *http.Request, page int) {
	q := r.URL.Query()
	q.Set("page", strconv.Itoa(page))
	u := *r.URL
	u.RawQuery = q.Encode()
	w.Header().Set("Link", "<http://"+r.Host+u.String()+">; rel=\"next\"")
}
