package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testRig starts a fake geth and a proxy pointed at it.
type testRig struct {
	proxy     *proxy
	upstream  *httptest.Server
	callCount int64
	lastBody  atomic.Value // []byte
}

func newRig(t *testing.T, maxBody int64, maxBatch int) *testRig {
	t.Helper()

	rig := &testRig{}
	rig.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&rig.callCount, 1)
		body, _ := io.ReadAll(r.Body)
		rig.lastBody.Store(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":"0x1"}`)
	}))
	t.Cleanup(rig.upstream.Close)

	p, err := newProxy(rig.upstream.URL, maxBody, maxBatch, 2, 5*time.Second, 4)
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	rig.proxy = p
	return rig
}

func post(t *testing.T, p *proxy, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

func rpcCall(method string) string {
	return `{"jsonrpc":"2.0","id":1,"method":"` + method + `","params":[]}`
}

func TestForwardsAllowedMethod(t *testing.T) {
	rig := newRig(t, 1<<20, 10)

	rec := post(t, rig.proxy, rpcCall("eth_call"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := atomic.LoadInt64(&rig.callCount); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
	if body, _ := rig.lastBody.Load().([]byte); !bytes.Contains(body, []byte("eth_call")) {
		t.Fatalf("upstream body did not contain the original method: %s", body)
	}
}

func TestForwardsTraceMethod(t *testing.T) {
	rig := newRig(t, 1<<20, 10)

	rec := post(t, rig.proxy, rpcCall("debug_traceBlockByNumber"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := atomic.LoadInt64(&rig.callCount); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}

func TestDeniesDangerousMethodsWithoutCallingUpstream(t *testing.T) {
	dangerous := []string{
		"debug_writeMemProfile",
		"debug_startCPUProfile",
		"debug_setHead",
		"debug_chaindbCompact",
		"debug_stacks",
		"admin_nodeInfo",
		"personal_unlockAccount",
		"engine_forkchoiceUpdatedV2",
	}
	for _, method := range dangerous {
		t.Run(method, func(t *testing.T) {
			rig := newRig(t, 1<<20, 10)

			rec := post(t, rig.proxy, rpcCall(method))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (JSON-RPC error)", rec.Code)
			}
			var resp struct {
				Error *struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v (body %s)", err, rec.Body.String())
			}
			if resp.Error == nil || resp.Error.Code != -32601 {
				t.Fatalf("expected -32601 error, got %s", rec.Body.String())
			}
			if got := atomic.LoadInt64(&rig.callCount); got != 0 {
				t.Fatalf("upstream was called %d times for a denied method", got)
			}
		})
	}
}

func TestBatchSmugglingRejected(t *testing.T) {
	rig := newRig(t, 1<<20, 10)

	batch := `[` + rpcCall("eth_call") + `,` + rpcCall("debug_writeMemProfile") + `]`
	rec := post(t, rig.proxy, batch)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := atomic.LoadInt64(&rig.callCount); got != 0 {
		t.Fatalf("upstream was called %d times for a batch containing a denied method", got)
	}

	var resp []struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode batch response: %v (body %s)", err, rec.Body.String())
	}
	if len(resp) != 2 {
		t.Fatalf("expected 2 error objects, got %d", len(resp))
	}
	for i, item := range resp {
		if item.Error == nil || item.Error.Code != -32601 {
			t.Errorf("element %d: expected -32601, got %+v", i, item)
		}
	}
}

func TestBatchAllAllowedForwarded(t *testing.T) {
	rig := newRig(t, 1<<20, 10)

	batch := `[` + rpcCall("eth_call") + `,` + rpcCall("eth_getLogs") + `]`
	rec := post(t, rig.proxy, batch)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := atomic.LoadInt64(&rig.callCount); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}

func TestBodyTooLarge(t *testing.T) {
	rig := newRig(t, 64, 10)

	big := `{"jsonrpc":"2.0","id":1,"method":"eth_call","params":["` + strings.Repeat("x", 200) + `"]}`
	rec := post(t, rig.proxy, big)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if got := atomic.LoadInt64(&rig.callCount); got != 0 {
		t.Fatalf("upstream called for an oversized body")
	}
}

func TestBatchTooLarge(t *testing.T) {
	rig := newRig(t, 1<<20, 2)

	batch := `[` + rpcCall("eth_call") + `,` + rpcCall("eth_call") + `,` + rpcCall("eth_call") + `]`
	rec := post(t, rig.proxy, batch)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := atomic.LoadInt64(&rig.callCount); got != 0 {
		t.Fatalf("upstream called for an oversized batch")
	}
}

func TestGzipBody(t *testing.T) {
	rig := newRig(t, 1<<20, 10)

	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	_, _ = io.WriteString(gz, rpcCall("debug_traceBlockByNumber"))
	_ = gz.Close()

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(compressed.Bytes()))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()
	rig.proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := atomic.LoadInt64(&rig.callCount); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}

func TestMalformedJSON(t *testing.T) {
	rig := newRig(t, 1<<20, 10)

	rec := post(t, rig.proxy, `{"jsonrpc":`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestMissingMethod(t *testing.T) {
	rig := newRig(t, 1<<20, 10)

	rec := post(t, rig.proxy, `{"jsonrpc":"2.0","id":1,"params":[]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestNonPostRejected(t *testing.T) {
	rig := newRig(t, 1<<20, 10)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	rig.proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestHealthz(t *testing.T) {
	rig := newRig(t, 1<<20, 10)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	rig.proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
