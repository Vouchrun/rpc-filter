package main

import (
	"fmt"
	"net/http"
	"sync"
)

// stats is a deliberately tiny counter set exposed in Prometheus text format.
// No client library: the filter should stay dependency-free so the image can be
// built reproducibly and audited in one sitting.
type stats struct {
	mu             sync.Mutex
	requests       uint64
	batches        uint64
	denied         uint64
	debugCalls     uint64
	upstreamErrors uint64
}

func newStats() *stats { return &stats{} }

func (s *stats) incRequest(batch bool) {
	s.mu.Lock()
	s.requests++
	if batch {
		s.batches++
	}
	s.mu.Unlock()
}

func (s *stats) incDenied(n int) {
	s.mu.Lock()
	s.denied += uint64(n)
	s.mu.Unlock()
}

func (s *stats) incDebug() {
	s.mu.Lock()
	s.debugCalls++
	s.mu.Unlock()
}

func (s *stats) incUpstreamError() {
	s.mu.Lock()
	s.upstreamErrors++
	s.mu.Unlock()
}

func (s *stats) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	requests, batches := s.requests, s.batches
	denied, debugCalls := s.denied, s.debugCalls
	upstreamErrors := s.upstreamErrors
	s.mu.Unlock()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "# HELP rpc_filter_requests_total JSON-RPC requests received.\n")
	fmt.Fprintf(w, "# TYPE rpc_filter_requests_total counter\n")
	fmt.Fprintf(w, "rpc_filter_requests_total %d\n", requests)
	fmt.Fprintf(w, "# HELP rpc_filter_batches_total Batch requests received.\n")
	fmt.Fprintf(w, "# TYPE rpc_filter_batches_total counter\n")
	fmt.Fprintf(w, "rpc_filter_batches_total %d\n", batches)
	fmt.Fprintf(w, "# HELP rpc_filter_denied_total Methods rejected by policy.\n")
	fmt.Fprintf(w, "# TYPE rpc_filter_denied_total counter\n")
	fmt.Fprintf(w, "rpc_filter_denied_total %d\n", denied)
	fmt.Fprintf(w, "# HELP rpc_filter_debug_calls_total debug_ calls forwarded.\n")
	fmt.Fprintf(w, "# TYPE rpc_filter_debug_calls_total counter\n")
	fmt.Fprintf(w, "rpc_filter_debug_calls_total %d\n", debugCalls)
	fmt.Fprintf(w, "# HELP rpc_filter_upstream_errors_total Upstream failures.\n")
	fmt.Fprintf(w, "# TYPE rpc_filter_upstream_errors_total counter\n")
	fmt.Fprintf(w, "rpc_filter_upstream_errors_total %d\n", upstreamErrors)
}
