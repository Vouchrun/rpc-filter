package main

import "testing"

func TestMethodAllowed(t *testing.T) {
	allowed := []string{
		"eth_call",
		"eth_getLogs",
		"eth_getBlockByNumber",
		"eth_sendRawTransaction",
		"net_version",
		"web3_clientVersion",
		"txpool_status",
		"rpc_modules",
		"debug_traceBlockByNumber",
		"debug_traceBlockByHash",
		"debug_traceTransaction",
		"debug_traceCall",
		"debug_getRawBlock",
		"debug_getRawReceipts",
		"debug_accountRange",
		"debug_storageRangeAt",
	}
	for _, m := range allowed {
		if !methodAllowed(m) {
			t.Errorf("expected %q to be allowed", m)
		}
	}

	denied := []string{
		// The file-write primitives used on 2026-11-06.
		"debug_writeMemProfile",
		"debug_writeBlockProfile",
		"debug_writeMutexProfile",
		"debug_startCPUProfile",
		"debug_stopCPUProfile",
		"debug_goTrace",
		// Chain and process control.
		"debug_setHead",
		"debug_chaindbCompact",
		"debug_freeOSMemory",
		"debug_setGCPercent",
		"debug_setMemoryLimit",
		"debug_verbosity",
		"debug_vmodule",
		"debug_stacks",
		"debug_dbGet",
		"debug_dumpBlock",
		// Other namespaces that must never reach the public edge.
		"admin_nodeInfo",
		"admin_addPeer",
		"personal_listAccounts",
		"personal_unlockAccount",
		"engine_newPayloadV2",
		"engine_forkchoiceUpdatedV2",
		"miner_start",
		"clique_getSigners",
		// Prefix lookalikes must not slip through.
		"debug_traceBlockByNumberExtra",
		"debug_writeMemProfile2",
		"",
	}
	for _, m := range denied {
		if methodAllowed(m) {
			t.Errorf("expected %q to be denied", m)
		}
	}
}

func TestIsDebugMethod(t *testing.T) {
	if !isDebugMethod("debug_traceCall") {
		t.Fatal("debug_traceCall should be a debug method")
	}
	if isDebugMethod("eth_call") {
		t.Fatal("eth_call should not be a debug method")
	}
}
