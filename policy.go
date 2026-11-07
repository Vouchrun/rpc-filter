package main

import "strings"

// allowedPrefixes are the JSON-RPC namespaces forwarded to the public edge
// unmodified. geth's --http.api is namespace-granular, so anything reachable
// here is reachable in full; the filter's job is to keep everything outside
// this list and the debug allowlist below away from the internet.
var allowedPrefixes = []string{
	"eth_",
	"net_",
	"web3_",
	"txpool_",
	"rpc_",
}

// allowedDebug is the exhaustive set of debug_* methods permitted through the
// filter. Every entry must be read-only: no file writes, no process control,
// no chain mutation. debug_writeMemProfile and friends are what allowed an
// attacker to write ~60 files as root on 2026-11-06.
//
// Do not add debug_writeMemProfile, debug_writeBlockProfile,
// debug_writeMutexProfile, debug_startCPUProfile, debug_stopCPUProfile,
// debug_goTrace, debug_setHead, debug_chaindbCompact, debug_freeOSMemory,
// debug_setGCPercent, debug_setMemoryLimit, debug_verbosity, debug_vmodule,
// debug_stacks, debug_dbGet or debug_dumpBlock to this map.
var allowedDebug = map[string]struct{}{
	// Tracing (read-only, state queries).
	"debug_traceBlockByNumber": {},
	"debug_traceBlockByHash":   {},
	"debug_traceTransaction":   {},
	"debug_traceCall":          {},

	// Raw data already available through the eth namespace in decoded form.
	"debug_getRawHeader":      {},
	"debug_getRawBlock":       {},
	"debug_getRawTransaction": {},
	"debug_getRawReceipts":    {},
	"debug_getBadBlocks":      {},

	// Historical state enumeration (read-only).
	"debug_accountRange":   {},
	"debug_storageRangeAt": {},
}

// methodAllowed reports whether a single JSON-RPC method may be forwarded.
// Unknown methods are denied rather than forwarded: a typo should not become
// whatever geth decides to serve under that name.
func methodAllowed(method string) bool {
	if _, ok := allowedDebug[method]; ok {
		return true
	}
	for _, prefix := range allowedPrefixes {
		if strings.HasPrefix(method, prefix) {
			return true
		}
	}
	return false
}

// isDebugMethod reports whether a method belongs to the debug namespace. Used
// to apply the concurrency limit, since traces are far more expensive than
// ordinary calls.
func isDebugMethod(method string) bool {
	return strings.HasPrefix(method, "debug_")
}
