package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// rpcRequest is the subset of a JSON-RPC call the filter needs to inspect.
// Everything else is forwarded untouched.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type proxy struct {
	upstream *url.URL
	client   *http.Client
	maxBody  int64
	maxBatch int
	debugSem chan struct{}
	timeout  time.Duration
	stats    *stats
}

func newProxy(rawUpstream string, maxBody int64, maxBatch, debugConc int, timeout time.Duration, maxIdle int) (*proxy, error) {
	u, err := url.Parse(rawUpstream)
	if err != nil {
		return nil, fmt.Errorf("parse upstream: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported upstream scheme %q", u.Scheme)
	}
	if debugConc < 1 {
		debugConc = 1
	}
	transport := &http.Transport{
		MaxIdleConns:        maxIdle,
		MaxIdleConnsPerHost: maxIdle,
		IdleConnTimeout:     90 * time.Second,
		// Pass compressed responses through untouched.
		DisableCompression: true,
	}
	return &proxy{
		upstream: u,
		client:   &http.Client{Transport: transport},
		maxBody:  maxBody,
		maxBatch: maxBatch,
		debugSem: make(chan struct{}, debugConc),
		timeout:  timeout,
		stats:    newStats(),
	}, nil
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeRPCError(w, http.StatusMethodNotAllowed, nil, -32600, "only POST is supported")
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" &&
		!strings.HasPrefix(strings.ToLower(ct), "application/json") {
		writeRPCError(w, http.StatusUnsupportedMediaType, nil, -32700, "content type must be application/json")
		return
	}

	body, err := readBody(w, r, p.maxBody)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeRPCError(w, http.StatusRequestEntityTooLarge, nil, -32600, "request body too large")
			return
		}
		writeRPCError(w, http.StatusBadRequest, nil, -32700, "cannot read request body")
		return
	}

	reqs, isBatch, err := parseRequests(body, p.maxBatch)
	if err != nil {
		writeRPCError(w, http.StatusBadRequest, nil, -32700, err.Error())
		return
	}

	p.stats.incRequest(isBatch)

	// Policy check first, on every element. A batch is rejected as a whole if
	// any element is disallowed: forwarding the allowed remainder would let a
	// caller pair a harmless call with a forbidden one and observe the side
	// effects of neither, which makes auditing harder for no benefit.
	var denied []string
	debugRequested := false
	for _, req := range reqs {
		if !methodAllowed(req.Method) {
			denied = append(denied, req.Method)
		}
		if isDebugMethod(req.Method) {
			debugRequested = true
		}
	}
	if len(denied) > 0 {
		p.stats.incDenied(len(denied))
		client := clientAddr(r)
		for _, m := range denied {
			log.Printf("DENIED method=%q client=%s batch=%t", m, client, isBatch)
		}
		writeDenied(w, reqs, isBatch)
		return
	}

	if debugRequested {
		select {
		case p.debugSem <- struct{}{}:
			defer func() { <-p.debugSem }()
		case <-r.Context().Done():
			writeRPCError(w, http.StatusServiceUnavailable, nil, -32603, "client cancelled while waiting for a trace slot")
			return
		}
		p.stats.incDebug()
	}

	p.forward(w, r, body)
}

func (p *proxy) forward(w http.ResponseWriter, r *http.Request, body []byte) {
	ctx, cancel := context.WithTimeout(r.Context(), p.timeout)
	defer cancel()

	target := *p.upstream
	target.Path = "/"
	target.RawQuery = ""

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		writeRPCError(w, http.StatusBadGateway, nil, -32603, "cannot build upstream request")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if ua := r.Header.Get("User-Agent"); ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		p.stats.incUpstreamError()
		log.Printf("UPSTREAM error=%v client=%s", err, clientAddr(r))
		writeRPCError(w, http.StatusBadGateway, nil, -32603, "upstream request failed")
		return
	}
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				p.stats.incUpstreamError()
				log.Printf("UPSTREAM stream error=%v client=%s", readErr, clientAddr(r))
			}
			return
		}
	}
}

var errBodyTooLarge = errors.New("request body too large")

// readBody applies the size limit and transparently decodes gzip bodies, which
// some clients send for large batches.
func readBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	limited := http.MaxBytesReader(w, r.Body, limit)
	var reader io.Reader = limited
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		gz, err := gzip.NewReader(limited)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		reader = gz
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, errBodyTooLarge
		}
		return nil, err
	}
	return body, nil
}

func parseRequests(body []byte, maxBatch int) (reqs []rpcRequest, isBatch bool, err error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, false, errors.New("empty request body")
	}
	if trimmed[0] == '[' {
		var raw []json.RawMessage
		if err := json.Unmarshal(trimmed, &raw); err != nil {
			return nil, true, errors.New("malformed JSON-RPC batch")
		}
		if len(raw) == 0 {
			return nil, true, errors.New("empty JSON-RPC batch")
		}
		if len(raw) > maxBatch {
			return nil, true, fmt.Errorf("batch size %d exceeds limit %d", len(raw), maxBatch)
		}
		for _, item := range raw {
			var req rpcRequest
			if err := json.Unmarshal(item, &req); err != nil {
				return nil, true, errors.New("malformed JSON-RPC batch element")
			}
			if req.Method == "" {
				return nil, true, errors.New("JSON-RPC batch element has no method")
			}
			reqs = append(reqs, req)
		}
		return reqs, true, nil
	}

	var req rpcRequest
	if err := json.Unmarshal(trimmed, &req); err != nil {
		return nil, false, errors.New("malformed JSON-RPC request")
	}
	if req.Method == "" {
		return nil, false, errors.New("JSON-RPC request has no method")
	}
	return []rpcRequest{req}, false, nil
}

// writeDenied answers a policy rejection in JSON-RPC shape. For a batch, one
// error object is returned per element so clients that expect an array for a
// batch still parse the response.
func writeDenied(w http.ResponseWriter, reqs []rpcRequest, isBatch bool) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	if !isBatch {
		writeErrorBody(w, reqs[0].ID, -32601, fmt.Sprintf("the method %s does not exist/is not available", reqs[0].Method))
		return
	}

	out := make([]map[string]any, 0, len(reqs))
	for _, req := range reqs {
		out = append(out, errorObject(req.ID, -32601,
			fmt.Sprintf("the method %s does not exist/is not available", req.Method)))
	}
	_ = json.NewEncoder(w).Encode(out)
}

func writeRPCError(w http.ResponseWriter, status int, id json.RawMessage, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	writeErrorBody(w, id, code, message)
}

func writeErrorBody(w io.Writer, id json.RawMessage, code int, message string) {
	_ = json.NewEncoder(w).Encode(errorObject(id, code, message))
}

func errorObject(id json.RawMessage, code int, message string) map[string]any {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	}
}

var hopByHop = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

func copyHeaders(dst, src http.Header) {
	for k, values := range src {
		if _, skip := hopByHop[http.CanonicalHeaderKey(k)]; skip {
			continue
		}
		for _, v := range values {
			dst.Add(k, v)
		}
	}
}

// clientAddr prefers the address Caddy recorded over the direct peer, since
// the proxy only ever sees the local Caddy connection.
func clientAddr(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, found := strings.Cut(xff, ","); found {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
