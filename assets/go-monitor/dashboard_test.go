package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestDashboardFanoutBounded(t *testing.T) {
	h := &dashboardHub{clients: map[chan []byte]struct{}{}, limit: 100}
	h.publish([]byte("initial"))
	clients := []chan []byte{}
	for i := 0; i < 100; i++ {
		ch, ok := h.subscribe()
		if !ok {
			t.Fatal("premature client limit")
		}
		clients = append(clients, ch)
	}
	if _, ok := h.subscribe(); ok {
		t.Fatal("client limit not enforced")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				h.publish([]byte("update"))
			}
		}()
	}
	wg.Wait()
	h.publish([]byte("newest"))
	for _, ch := range clients {
		if len(ch) != 1 || string(<-ch) != "newest" {
			t.Fatal("slow client retained stale queue")
		}
		h.unsubscribe(ch)
	}
	if len(h.clients) != 0 {
		t.Fatal("clients leaked")
	}
}

func TestDashboardReadOnlyAndOrigin(t *testing.T) {
	h := &dashboardHub{clients: map[chan []byte]struct{}{}, limit: 1}
	h.publish([]byte(`{"portfolio":null}`))
	handler := dashboardHandler(h, "127.0.0.1:8788")
	for _, tc := range []struct {
		method, path, host, origin string
		want                       int
	}{
		{"GET", "/", "127.0.0.1:8788", "", 200},
		{"GET", "/api/snapshot", "127.0.0.1:8788", "", 200},
		{"POST", "/api/snapshot", "127.0.0.1:8788", "", 405},
		{"PUT", "/api/portfolio", "127.0.0.1:8788", "", 405},
		{"DELETE", "/api/events", "127.0.0.1:8788", "", 405},
		{"GET", "/api/snapshot", "attacker.example", "", 403},
		{"GET", "/api/snapshot", "127.0.0.1:8788", "https://attacker.example", 403},
		{"GET", "/api/snapshot", "127.0.0.1:8788", "http://127.0.0.1:8788", 200},
		{"GET", "/api/portfolio", "127.0.0.1:8788", "", 404},
	} {
		r := httptest.NewRequest(tc.method, "http://"+tc.host+tc.path, nil)
		r.Host = tc.host
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("%s %s: got %d want %d", tc.method, tc.path, w.Code, tc.want)
		}
		if w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("unexpected cross-origin access")
		}
	}
	if err := dashboardCLI([]string{"--listen", "0.0.0.0:8788"}); err == nil {
		t.Fatal("public bind allowed")
	}
}

func TestDashboardSnapshotFreshnessAndLedger(t *testing.T) {
	dir := t.TempDir()
	c := testConfig(t)
	c.Codex.DryRun = false
	configPath := filepath.Join(dir, "monitor.json")
	b, _ := json.Marshal(c)
	if err := os.WriteFile(configPath, b, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	portfolioPath := filepath.Join(dir, "portfolio.json")
	ledger := []byte(`{"version":1,"positions":[{"allocation_fraction":"0.25"}],"orders":[{"status":"open","planned_allocation_fraction":"0.1"}]}`)
	if err := os.WriteFile(portfolioPath, ledger, 0600); err != nil {
		t.Fatal(err)
	}
	registryPath := filepath.Join(dir, "dashboard.json")
	registry := map[string]any{"version": 1, "portfolio_file": "portfolio.json", "monitors": []map[string]string{{"id": "coin", "config_file": "monitor.json"}}}
	b, _ = json.Marshal(registry)
	os.WriteFile(registryPath, b, 0600)
	dc, err := loadDashboardConfig(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s := newState(c, "live")
	s.LastTick = Tick{Symbol: c.Symbol, Price: "0.12", TimeMS: now.UnixMilli(), Source: "binance_ws"}
	if err := saveState(c.StateFile, s); err != nil {
		t.Fatal(err)
	}
	view := collectDashboard(dc, now)
	if view.Monitors[0].Health != "receiving" || !view.Monitors[0].ConfigApplied {
		t.Fatal("fresh state not recognized")
	}
	view = collectDashboard(dc, now.Add(time.Minute))
	if view.Monitors[0].Health != "state_stale" {
		t.Fatal("stopped monitor shown as live")
	}
	after, _ := os.ReadFile(portfolioPath)
	if string(after) != string(ledger) {
		t.Fatal("dashboard mutated ledger")
	}
	s.LastTick.TimeMS = now.Add(-time.Hour).UnixMilli()
	saveState(c.StateFile, s)
	view = collectDashboard(dc, now)
	if view.Monitors[0].Health != "waiting_trade" {
		t.Fatal("old quote shown as fresh")
	}
	os.WriteFile(portfolioPath, []byte("{bad json"), 0600)
	view = collectDashboard(dc, now)
	if view.PortfolioError == "" || string(view.Portfolio) != "null" {
		t.Fatal("corrupt ledger silently reused")
	}
}
