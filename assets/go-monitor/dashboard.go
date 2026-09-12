package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

//go:embed web/*
var dashboardFiles embed.FS

type dashboardConfig struct {
	Version       int    `json:"version"`
	PortfolioFile string `json:"portfolio_file"`
	Monitors      []struct {
		ID         string `json:"id"`
		ConfigFile string `json:"config_file"`
	} `json:"monitors"`
}

type dashboardRule struct {
	Rule
	FullMessageTemplate string     `json:"full_message_template"`
	State               *RuleState `json:"state,omitempty"`
}

type dashboardMonitor struct {
	ID            string          `json:"id"`
	Asset         string          `json:"asset"`
	Symbol        string          `json:"symbol"`
	Mode          string          `json:"mode"`
	UpdatedAt     string          `json:"updated_at"`
	Health        string          `json:"health"`
	Error         string          `json:"error,omitempty"`
	LastError     string          `json:"last_error,omitempty"`
	LastTick      Tick            `json:"last_tick"`
	MaxTickAgeMS  int64           `json:"max_tick_age_ms"`
	AcceptedTicks int64           `json:"accepted_ticks"`
	RejectedTicks int64           `json:"rejected_ticks"`
	BudgetUsed    int             `json:"budget_used"`
	BudgetLimit   int             `json:"budget_limit"`
	BudgetDay     string          `json:"budget_day"`
	ConfigApplied bool            `json:"config_applied"`
	Rules         []dashboardRule `json:"rules"`
	Events        []Event         `json:"events"`
	EventCount    int             `json:"event_count"`
}

type dashboardSnapshot struct {
	GeneratedAt    string             `json:"generated_at"`
	Portfolio      json.RawMessage    `json:"portfolio"`
	PortfolioError string             `json:"portfolio_error,omitempty"`
	Monitors       []dashboardMonitor `json:"monitors"`
}

func readDashboardJSON(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (4<<20)+1))
	if err != nil {
		return err
	}
	if len(b) > 4<<20 {
		return fmt.Errorf("dashboard input exceeds 4 MiB")
	}
	return json.Unmarshal(b, v)
}

func loadDashboardConfig(path string) (dashboardConfig, error) {
	var c dashboardConfig
	if err := readDashboardJSON(path, &c); err != nil {
		return c, err
	}
	if c.Version != 1 || c.PortfolioFile == "" || len(c.Monitors) < 1 || len(c.Monitors) > 20 {
		return c, fmt.Errorf("dashboard requires version 1, portfolio_file and 1..20 monitors")
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return c, err
	}
	resolve := func(p string) string {
		if filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}
	c.PortfolioFile = resolve(c.PortfolioFile)
	seen := map[string]bool{}
	for i := range c.Monitors {
		m := &c.Monitors[i]
		if !idPattern.MatchString(m.ID) || seen[m.ID] || m.ConfigFile == "" {
			return c, fmt.Errorf("invalid/duplicate monitor id or missing config")
		}
		seen[m.ID] = true
		m.ConfigFile = resolve(m.ConfigFile)
	}
	return c, nil
}

func collectDashboard(c dashboardConfig, now time.Time) dashboardSnapshot {
	s := dashboardSnapshot{GeneratedAt: now.UTC().Format(time.RFC3339), Portfolio: json.RawMessage("null"), Monitors: []dashboardMonitor{}}
	var portfolio map[string]json.RawMessage
	if err := readDashboardJSON(c.PortfolioFile, &portfolio); err != nil {
		s.PortfolioError = "持仓记录读取失败，请在对话中让 AI 检查；页面不会用旧记录代替。"
	} else if string(portfolio["version"]) != "1" || len(portfolio["positions"]) == 0 || len(portfolio["orders"]) == 0 {
		s.PortfolioError = "持仓记录格式不完整，请在对话中让 AI 检查。"
	} else {
		s.Portfolio, _ = json.Marshal(portfolio)
	}
	for _, entry := range c.Monitors {
		m := dashboardMonitor{ID: entry.ID, Health: "unavailable", Rules: []dashboardRule{}, Events: []Event{}}
		cfg, err := loadConfig(entry.ConfigFile)
		if err != nil {
			m.Error = "监控配置读取或校验失败"
			s.Monitors = append(s.Monitors, m)
			continue
		}
		m.Asset, m.Symbol = cfg.Asset, cfg.Symbol
		m.BudgetLimit, m.MaxTickAgeMS = cfg.Codex.MaxPerDay, cfg.Market.MaxTickAge.D().Milliseconds()
		statePath := cfg.StateFile
		if cfg.Codex.DryRun {
			statePath += ".dry-run.json"
		}
		var state State
		err = readDashboardJSON(statePath, &state)
		if err == nil && (state.Symbol != cfg.Symbol || state.Version != 1 || (state.Mode != "live" && state.Mode != "dry-run")) {
			err = fmt.Errorf("state identity mismatch")
		}
		for _, r := range cfg.Rules {
			m.Rules = append(m.Rules, dashboardRule{r, cfg.rawTemplate(r), state.Rules[r.ID]})
		}
		if err != nil {
			m.Error = "尚无可读取的运行状态，请检查监控服务"
			s.Monitors = append(s.Monitors, m)
			continue
		}
		m.Mode, m.UpdatedAt, m.LastError = state.Mode, state.UpdatedAt, state.LastError
		m.LastTick, m.AcceptedTicks, m.RejectedTicks = state.LastTick, state.AcceptedTicks, state.RejectedTicks
		m.ConfigApplied = state.ConfigHash == fingerprint(cfg)
		m.BudgetUsed, m.BudgetDay = state.BudgetUsed, state.BudgetDay
		m.Health = "state_stale"
		updated, err := time.Parse(time.RFC3339Nano, state.UpdatedAt)
		if err == nil && now.Sub(updated) >= -5*time.Second && now.Sub(updated) < 5*time.Second {
			m.Health = "waiting_trade"
			age := now.UnixMilli() - state.LastTick.TimeMS
			if state.LastTick.TimeMS > 0 && age >= -2000 && age <= m.MaxTickAgeMS {
				m.Health = "receiving"
			}
		}
		m.EventCount = len(state.Events)
		start := len(state.Events) - 30
		if start < 0 {
			start = 0
		}
		for i := len(state.Events) - 1; i >= start; i-- {
			e := state.Events[i]
			if len(e.Result) > 2000 {
				e.Result = e.Result[:2000] + "…"
			}
			m.Events = append(m.Events, e)
		}
		s.Monitors = append(s.Monitors, m)
	}
	return s
}

// One collector serves every browser. Slow clients retain only the newest snapshot.
type dashboardHub struct {
	mu      sync.Mutex
	latest  []byte
	clients map[chan []byte]struct{}
	limit   int
}

func (h *dashboardHub) publish(b []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.latest = b
	for ch := range h.clients {
		select {
		case ch <- b:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- b:
			default:
			}
		}
	}
}

func (h *dashboardHub) subscribe() (chan []byte, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.clients) >= h.limit {
		return nil, false
	}
	ch := make(chan []byte, 1)
	h.clients[ch] = struct{}{}
	ch <- h.latest
	return ch, true
}

func (h *dashboardHub) unsubscribe(ch chan []byte) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
}

func dashboardHandler(h *dashboardHub, allowedHost string) http.Handler {
	assets, _ := fs.Sub(dashboardFiles, "web")
	mux := http.NewServeMux()
	mux.HandleFunc("/api/snapshot", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		h.mu.Lock()
		b := h.latest
		h.mu.Unlock()
		w.Write(b)
	})
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		ch, ok := h.subscribe()
		if !ok {
			http.Error(w, "Too many open dashboards", http.StatusServiceUnavailable)
			return
		}
		defer h.unsubscribe(ch)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Accel-Buffering", "no")
		control := http.NewResponseController(w)
		for {
			select {
			case <-r.Context().Done():
				return
			case b := <-ch:
				control.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if _, err := fmt.Fprintf(w, "retry: 5000\nevent: snapshot\ndata: %s\n\n", b); err != nil {
					return
				}
				if err := control.Flush(); err != nil {
					return
				}
			}
		}
	})
	mux.Handle("/", http.FileServer(http.FS(assets)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Read-only dashboard", http.StatusMethodNotAllowed)
			return
		}
		if r.Host != allowedHost {
			http.Error(w, "Local host only", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || u.Scheme != "http" || u.Host != allowedHost {
				http.Error(w, "Same origin only", http.StatusForbidden)
				return
			}
		}
		if r.Method == http.MethodHead && r.URL.Path == "/api/events" {
			w.WriteHeader(http.StatusOK)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func dashboardCLI(args []string) error {
	f := flag.NewFlagSet("dashboard", flag.ContinueOnError)
	configPath := f.String("config", "config/dashboard.json", "dashboard registry")
	listen := f.String("listen", "127.0.0.1:8788", "loopback IP:port only")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	host, port, err := net.SplitHostPort(*listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return fmt.Errorf("dashboard must bind a loopback IP, for example 127.0.0.1:8788")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("invalid port")
	}
	c, err := loadDashboardConfig(*configPath)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	h := &dashboardHub{clients: map[chan []byte]struct{}{}, limit: 100}
	collect := func() {
		b, _ := json.Marshal(collectDashboard(c, time.Now()))
		h.publish(bytes.TrimSpace(b))
	}
	collect()
	server := &http.Server{Addr: *listen, Handler: dashboardHandler(h, *listen), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10, BaseContext: func(net.Listener) context.Context { return ctx }}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				stop, done := context.WithTimeout(context.Background(), 5*time.Second)
				server.Shutdown(stop)
				done()
				return
			case <-ticker.C:
				collect()
			}
		}
	}()
	logEvent("dashboard_started", map[string]any{"url": "http://" + *listen, "read_only": true, "max_clients": h.limit})
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
