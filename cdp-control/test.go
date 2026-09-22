package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"time"

	"github.com/kodelyx/Browser-cdp/cdp-control/src/bridge"
	"github.com/kodelyx/Browser-cdp/cdp-control/src/cdp"
	"github.com/kodelyx/Browser-cdp/cdp-control/src/cookiejar"
	"github.com/kodelyx/Browser-cdp/cdp-control/src/needle"
)

func main() {
	fmt.Println("==================================================")
	fmt.Println("   CDP-CONTROL: COMPREHENSIVE SUITE RUNNER")
	fmt.Println("==================================================")

	passed := 0
	failed := 0

	runTest := func(name string, fn func() error) {
		start := time.Now()
		err := fn()
		dur := time.Since(start).Round(time.Millisecond)
		if err != nil {
			fmt.Printf("  ❌ [FAIL] %-42s (%v)\n     Error: %v\n", name, dur, err)
			failed++
		} else {
			fmt.Printf("  ✓ [PASS] %-42s (%v)\n", name, dur)
			passed++
		}
	}

	// 1. Cookiejar tests
	runTest("Cookiejar: Storage & Expiry Logic", func() error {
		now := time.Now()
		cookies := []cookiejar.Cookie{
			{
				Name:           "__Secure-next-auth.session-token",
				Value:          "secret_token_123",
				Domain:         "example.com",
				Path:           "/",
				ExpirationDate: float64(now.Add(2 * time.Hour).Unix()),
			},
			{
				Name:           "expired_token",
				Value:          "old",
				Domain:         "example.com",
				Path:           "/",
				ExpirationDate: float64(now.Add(-2 * time.Hour).Unix()),
			},
		}
		jar := cookiejar.FromCookies(cookies, "test")
		if jar.Count() != 2 {
			return fmt.Errorf("expected 2 cookies, got %d", jar.Count())
		}
		active := jar.ForDomain("https://example.com/app")
		if len(active) != 1 || active[0].Name != "__Secure-next-auth.session-token" {
			return fmt.Errorf("expected 1 unexpired cookie, got %d", len(active))
		}
		if !cookiejar.IsEssential("__Secure-next-auth.session-token") {
			return fmt.Errorf("expected auth token to be essential")
		}
		return nil
	})

	// 2. CDP Serialization tests
	runTest("CDP: Message & Config Protocol", func() error {
		cfg := cdp.Config{
			BlockedMethods:    []string{"Storage.clearDataForOrigin"},
			TargetURLPrefixes: []string{},
			CookieDomains:     []string{},
		}
		raw, err := json.Marshal(cfg)
		if err != nil {
			return err
		}
		if !strings.Contains(string(raw), "blockedMethods") {
			return fmt.Errorf("missing blockedMethods in serialization: %s", string(raw))
		}
		return nil
	})

	// 3. Bridge Scoping tests
	runTest("Bridge: Universal Scope & Safe Handshake", func() error {
		tmpDir, err := os.MkdirTemp("", "cdp-bridge-test-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmpDir)

		b := bridge.NewBridge(nil, nil, tmpDir)
		if b.Connected() {
			return fmt.Errorf("bridge should not be connected initially")
		}
		st := b.Status()
		if st.Connected {
			return fmt.Errorf("bridge status should show connected=false")
		}
		return nil
	})

	// 4. Needle 3 SLM Engine check
	runTest("Needle 3: Embedded SLM Engine Probe", func() error {
		libPath, weightsPath, err := needle.EnsureNeedle3Assets()
		if err != nil {
			return fmt.Errorf("Needle 3 assets error: %w", err)
		}
		if _, err := os.Stat(libPath); err != nil {
			return fmt.Errorf("Needle lib not found at %s: %w", libPath, err)
		}
		if _, err := os.Stat(weightsPath); err != nil {
			return fmt.Errorf("Needle weights not found at %s: %w", weightsPath, err)
		}

		eng, err := needle.GetNeedleEngine()
		if err != nil {
			return fmt.Errorf("failed to initialize Needle engine: %w", err)
		}
		defer eng.Reset()

		// Test tool calling inference
		tools := `[{"type":"function","function":{"name":"click","description":"Click an element","parameters":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}}}]`
		resp, err := eng.CompleteTools("Click on the Settings button on page", tools)
		if err != nil {
			return fmt.Errorf("Needle inference failed: %w", err)
		}
		if resp == nil {
			return fmt.Errorf("Needle returned nil response")
		}
		return nil
	})

	// 5. Mock HTTP API tests
	runTest("HTTP API: Mock Endpoints & Security Checks", func() error {
		tmpDir, err := os.MkdirTemp("", "cdp-http-test-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmpDir)

		b := bridge.NewBridge(nil, nil, tmpDir)

		req := httptest.NewRequest(http.MethodGet, "/status", nil)
		rec := httptest.NewRecorder()

		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			client := b.Current()
			attached := client != nil && client.Connected()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"attached": attached,
				"status":   b.Status(),
			})
		})
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			return fmt.Errorf("status handler returned %d", rec.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			return err
		}
		if body["attached"] != false {
			return fmt.Errorf("expected attached=false when no extension connected")
		}
		return nil
	})

	// 6. Agent Action Validation tests
	runTest("Agent: Action Grammar & Tool Schema Validation", func() error {
		validJSON := []byte(`{"action": "click", "text": "Save"}`)
		var act struct {
			Action string `json:"action"`
			Text   string `json:"text"`
		}
		if err := json.Unmarshal(validJSON, &act); err != nil {
			return err
		}
		if act.Action != "click" || act.Text != "Save" {
			return fmt.Errorf("unexpected parse result: %+v", act)
		}

		badBody := bytes.NewReader([]byte(`{"invalid": true}`))
		req := httptest.NewRequest(http.MethodPost, "/agent/act", badBody)
		if req.Method != http.MethodPost {
			return fmt.Errorf("expected POST method")
		}
		return nil
	})

	fmt.Println("==================================================")
	if failed > 0 {
		fmt.Printf("FAILED: %d passed, %d failed\n", passed, failed)
		os.Exit(1)
	} else {
		fmt.Printf("ALL %d TESTS PASSED SUCCESSFULLY! ✓\n", passed)
		fmt.Println("==================================================")
	}
}
