package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func fakeCodex(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "codex")
	receipt := filepath.Join(dir, "argv")
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "--version" ]; then printf 'codex-cli fixture\n'; exit 0; fi
if [ "$1" = "--help" ]; then printf 'Queue a message\n--ask-for-approval\n--search\n'; exit 0; fi
if [ "$1" = "queue" ] && [ "$2" = "--help" ]; then printf 'Usage: codex queue\n--thread <THREAD>\n--message <TEXT>\n--sandbox\n'; exit 0; fi
if [ "$1" = "queue" ]; then printf '%%s\000' "$@" > '%s'; printf 'fixture queued\n'; exit 0; fi
exit 2
`, receipt)
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path, receipt
}
func TestPriceToChildProcessAndDryRunIsolation(t *testing.T) {
	s := scenarioFor(t, basicRule())
	binary, receipt := fakeCodex(t)
	s.e.C.Codex.Binary = binary
	s.e.C.MessageTemplate = "{{.ID}} $(touch SHOULD_NOT_EXIST) `echo nope` 中文 'quoted'\n{{.Price}}"
	s.tick("0.120", 0)
	v := s.tick("0.117", 1)[0]
	p, err := probeCodex(context.Background(), s.e.C.Codex)
	if err != nil {
		t.Fatal(err)
	}
	if d := dispatch(context.Background(), s.e.C, p, v, true); d.Status != "dry_run" {
		t.Fatal(d)
	}
	if _, err = os.Stat(receipt); !os.IsNotExist(err) {
		t.Fatal("dry run invoked sender")
	}
	if d := dispatch(context.Background(), s.e.C, p, v, false); d.Status != "queued" {
		t.Fatal(d)
	}
	b, err := os.ReadFile(receipt)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(string(b), "\x00")
	if len(args) != 6 || args[4] != v.Message || args[2] != s.e.C.Codex.ThreadID {
		t.Fatalf("argv was split or altered: %#v", args)
	}
}
func TestExecResumeInvocation(t *testing.T) {
	c := testConfig(t)
	v, err := buildInvocation(c, ProbeResult{Binary: "codex", Selected: "exec-resume"}, Event{Message: "test\n中文"}, false)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(v.Args, " ")
	if !strings.Contains(joined, "exec --sandbox read-only --skip-git-repo-check resume "+c.Codex.ThreadID+" -") || v.Stdin != "test\n中文" {
		t.Fatal(v)
	}
}
func TestFailureIsUnknown(t *testing.T) {
	c := testConfig(t)
	binary := filepath.Join(t.TempDir(), "fail")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 7\n"), 0700); err != nil {
		t.Fatal(err)
	}
	d := dispatch(context.Background(), c, ProbeResult{Binary: binary, Selected: "queue"}, Event{ID: "failure", Message: "test"}, false)
	if d.Status != "unknown" {
		t.Fatal(d)
	}
}
func TestUnicodeWebSocketAndPing(t *testing.T) {
	c := testConfig(t)
	pong := make(chan struct{}, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws/牛来usdt@aggTrade" {
			t.Errorf("wrong unicode stream path: %s", r.URL.Path)
		}
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		ws.SetPongHandler(func(p string) error {
			if p == "ping-token" {
				pong <- struct{}{}
			}
			return nil
		})
		_ = ws.WriteControl(websocket.PingMessage, []byte("ping-token"), time.Now().Add(time.Second))
		_ = ws.WriteJSON(aggTrade{Event: "aggTrade", EventTimeMS: time.Now().UnixMilli(), Symbol: c.Symbol, ID: 9, Price: "0.12", TimeMS: time.Now().UnixMilli()})
		_ = ws.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _, _ = ws.ReadMessage()
	}))
	defer server.Close()
	c.Market.WSURL = "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	m := newMarket(c)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ticks := make(chan Tick, 2)
	notices := make(chan FeedNotice, 2)
	done := make(chan error, 1)
	go func() { done <- m.connection(ctx, ticks, notices) }()
	select {
	case tick := <-ticks:
		if tick.Symbol != c.Symbol || tick.TradeID != 9 {
			t.Fatal(tick)
		}
	case <-ctx.Done():
		t.Fatal("no WS tick")
	}
	select {
	case <-pong:
	case <-ctx.Done():
		t.Fatal("no matching pong")
	}
	cancel()
	<-done
}
func TestRESTValidationTradeTimeAndBackoff(t *testing.T) {
	c := testConfig(t)
	rateLimit := false
	requests := 0
	now := time.Now().UnixMilli()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Query().Get("symbol") != c.Symbol {
			t.Error("symbol encoding")
		}
		if rateLimit {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(429)
			return
		}
		if r.URL.Path == "/api/v3/exchangeInfo" {
			json.NewEncoder(w).Encode(map[string]any{"symbols": []SymbolInfo{{c.Symbol, "TRADING", c.BaseAsset, c.QuoteAsset, true}}})
		} else {
			json.NewEncoder(w).Encode([]aggTrade{{ID: 20, Price: "0.13", TimeMS: now}})
		}
	}))
	defer server.Close()
	c.Market.RESTURL = server.URL
	m := newMarket(c)
	ctx := context.Background()
	if _, err := m.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	v, err := m.Latest(ctx)
	if err != nil || v.TimeMS != now || v.Source != "binance_rest" {
		t.Fatal(v, err)
	}
	rateLimit = true
	if _, err = m.Latest(ctx); err == nil {
		t.Fatal("429 ignored")
	}
	before := requests
	if _, err = m.Latest(ctx); err == nil || requests != before {
		t.Fatal("Retry-After not honored")
	}
}
