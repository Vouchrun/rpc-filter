// rpc-filter is a method-aware JSON-RPC proxy that sits between Caddy and geth.
//
// geth's --http.api is namespace-granular and all-or-nothing, so exposing
// debug_traceBlockByNumber to the public edge would also expose
// debug_writeMemProfile, which accepts an arbitrary path and writes it as root.
// Caddy cannot inspect JSON-RPC bodies, so the split has to happen here.
//
// The filter forwards the eth/net/web3/txpool/rpc namespaces and an explicit
// allowlist of read-only debug methods, and rejects everything else, including
// disallowed methods hidden inside batch requests.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	var (
		listen      = flag.String("listen", "127.0.0.1:8547", "address to listen on for JSON-RPC")
		upstream    = flag.String("upstream", "http://127.0.0.1:8545", "geth JSON-RPC endpoint")
		metricsAddr = flag.String("metrics-listen", "127.0.0.1:8548", "address for /metrics, empty to disable")
		maxBody     = flag.Int64("max-body", 1<<20, "maximum request body size in bytes")
		maxBatch    = flag.Int("max-batch", 100, "maximum number of calls in a batch request")
		debugConc   = flag.Int("debug-concurrency", 4, "maximum concurrent debug_ calls")
		timeout     = flag.Duration("timeout", 120*time.Second, "upstream request timeout")
		maxIdle     = flag.Int("max-idle-conns", 100, "maximum idle upstream connections")
		healthcheck = flag.Bool("healthcheck", false, "probe the local listener and exit 0/1")
	)
	flag.Parse()

	if *healthcheck {
		os.Exit(runHealthcheck(*listen))
	}

	p, err := newProxy(*upstream, *maxBody, *maxBatch, *debugConc, *timeout, *maxIdle)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rpc-filter: %v\n", err)
		os.Exit(2)
	}

	rpcServer := &http.Server{
		Addr:              *listen,
		Handler:           p,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	servers := []*http.Server{rpcServer}
	if *metricsAddr != "" {
		servers = append(servers, &http.Server{
			Addr:              *metricsAddr,
			Handler:           p.stats,
			ReadHeaderTimeout: 10 * time.Second,
		})
	}

	errCh := make(chan error, len(servers))
	go func() {
		log.Printf("rpc-filter listening on %s -> %s", *listen, *upstream)
		errCh <- rpcServer.ListenAndServe()
	}()
	if *metricsAddr != "" {
		go func() {
			log.Printf("rpc-filter metrics on http://%s/metrics", *metricsAddr)
			errCh <- servers[len(servers)-1].ListenAndServe()
		}()
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "rpc-filter: %v\n", err)
			os.Exit(1)
		}
	case <-stop:
		log.Printf("rpc-filter shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		for _, s := range servers {
			_ = s.Shutdown(ctx)
		}
	}
}

// runHealthcheck probes the local listener. Used by the container HEALTHCHECK;
// the image is built from scratch, so there is no shell or curl to lean on.
func runHealthcheck(listen string) int {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s/healthz", listen))
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
