// Command cdp-control drives a browser through the generic CDP extension, on its
// own ports.
//
// It is an AI-first debugging engine: a calling agent uses it as its eyes and
// hands on a live tab. It drives the UI, reads the network traffic the page
// actually sent, and turns either into typed JSON with a local Needle 3 model —
// so the caller gets ground truth instead of a guess.
//
// It exists so debugging does not have to go through any product's own bridge.
// Finding out what an app sends means driving its UI and reading its network
// traffic, and doing that from inside a product means every experiment runs
// against that product's bridge, its scope and its state. This is the same
// extension on a separate port with nothing else attached, scoped to nothing in
// particular.
//
//	ws   127.0.0.1:9223   the extension dials in here
//	http 127.0.0.1:8201   this is what you talk to
//
// 9223 rather than the extension's own 9222 default, so this can run alongside
// something else driving the same extension.
//
// The package is split by role, so a reader knows where to look:
//
//	main.go               flags, signal handling, shutdown — and nothing else
//	routes.go             the mux, the response helpers, the server type
//	handlers_browser.go   the endpoints that talk to the tab
//	handlers_agent.go     the endpoints an AI agent drives
//	agent.go              the model-facing core: tools, dispatch, capture
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kodelyx/Browser-cdp/cdp-control/src/bridge"
)

func main() {
	// 9223, not the extension's 9222 default, because this tool is meant to run
	// *alongside* a product that also drives the same extension. Both listen for
	// a WebSocket and both extensions dial 9222 by default, so sharing the port
	// means whichever connects first wins and the other silently gets the wrong
	// bridge — with errors that name the missing operation rather than the port.
	//
	// Keeping this one off 9222 makes the two coexist. Point the extension you use
	// for debugging at this address (one line in its config.js) and leave the
	// other where it is.
	wsAddr := flag.String("ws", "127.0.0.1:9223", "address the extension dials")
	httpAddr := flag.String("http", "127.0.0.1:8201", "address this API listens on")
	dataDir := flag.String("data", "", "where to keep the pairing token (default: a temp dir)")
	targets := flag.String("targets", defaultTargets, "comma-separated URLs the extension may attach to (empty = every tab)")
	domains := flag.String("domains", defaultDomains, "comma-separated cookie scopes (empty = every domain, cookie mirroring off)")
	flag.Parse()

	dir := *dataDir
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "cdp-control")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Fatalf("cdp-control: could not create %s: %v", dir, err)
	}

	br := bridge.NewBridge(splitList(*targets), splitList(*domains), dir)
	br.ListenAddr = *wsAddr

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := br.Listen(ctx); err != nil && ctx.Err() == nil {
			log.Fatalf("cdp-control: bridge stopped: %v", err)
		}
	}()

	srv := &server{bridge: br}
	mux := routes(srv)

	httpSrv := &http.Server{Addr: *httpAddr, Handler: mux}
	go func() {
		log.Printf("cdp-control: API on http://%s", *httpAddr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("cdp-control: http stopped: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("cdp-control: shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

// The defaults are empty, which the extension reads as full access: every tab is
// attachable and every cookie is in scope, exactly like a raw
// --remote-debugging-port. That is the right posture for a debugging tool whose
// job is to be pointed at whatever the caller is looking at, and it is the only
// posture that works without knowing the site in advance.
//
// Scoping is still available — pass -targets/-domains — but it is a decision the
// caller makes, not one this tool assumes on their behalf.
const (
	defaultTargets = ""
	defaultDomains = ""
)

func splitList(raw string) []string {
	out := make([]string, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
