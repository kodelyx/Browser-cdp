package main

// Wiring: the mux, the two response helpers, and the server type every handler
// hangs off.
//
// It is separate from main so a test can exercise the handlers without starting
// a bridge or binding a port.

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/kodelyx/Browser-cdp/cdp-control/src/bridge"
	"github.com/kodelyx/Browser-cdp/cdp-control/src/cdp"
)

type server struct {
	bridge *bridge.Bridge
}

// client resolves the attached extension, or reports why there is none.
func (s *server) client() (*cdp.Client, error) {
	client := s.bridge.Current()
	if client == nil || !client.Connected() {
		return nil, fmt.Errorf("no extension attached — load ../extension in Chrome " +
			"and point it at this bridge's address")
	}
	return client, nil
}

func routes(srv *server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", srv.health)
	mux.HandleFunc("/status", srv.status)
	mux.HandleFunc("/tabs", srv.tabs)
	mux.HandleFunc("/eval", srv.eval)
	mux.HandleFunc("/cdp", srv.cdp)
	mux.HandleFunc("/click", srv.click)
	mux.HandleFunc("/events", srv.events)
	mux.HandleFunc("/requests", srv.requests)
	mux.HandleFunc("/cookies", srv.cookies)
	mux.HandleFunc("/agent/act", srv.agentAct)
	mux.HandleFunc("/agent/extract", srv.agentExtract)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"error": err.Error()})
}
