package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func printJSON(v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return
	}
	fmt.Println(string(b))
}
func logEvent(kind string, v any) {
	b, _ := json.Marshal(map[string]any{"at": time.Now().UTC().Format(time.RFC3339Nano), "kind": kind, "data": v})
	fmt.Println(string(b))
}
func main() {
	if err := cli(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
func usage() {
	fmt.Print(`niu-monitor — Binance spot price → deterministic rules → existing Codex session

  niu-monitor rules    --config config/monitor.json [--rule ID]
  niu-monitor template --config config/monitor.json --rule ID
  niu-monitor validate --config config/monitor.json
  niu-monitor doctor   --config config/monitor.json [--offline] [--out evidence/doctor.json]
  niu-monitor price    --config config/monitor.json
  niu-monitor status   --config config/monitor.json [--send | --state PATH]
  niu-monitor run      --config config/monitor.json [--dry-run | --send] [--duration 30s] [--state PATH]
  niu-monitor test     --config config/monitor.json --rule ID --prices 0.12,0.118,0.117 [--step 1s] [--send] [--state PATH]
  niu-monitor resolve  --config config/monitor.json --send --event ID --note "receipt checked" [--state PATH]
  niu-monitor dashboard --config config/dashboard.json [--listen 127.0.0.1:8788]

Rules/template/status are local reads. Doctor runs only CLI help and public market reads.
Test uses synthetic prices and an isolated state; --send submits an ACK-only test message.
Default dry-run run state: <state_file>.dry-run.json. --send selects live state.
Config loads once; after edits validate, stop and restart. Ctrl-C stops cleanly.
`)
}
func cli(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		usage()
		return nil
	}
	command := args[0]
	if command == "dashboard" {
		return dashboardCLI(args[1:])
	}
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	configPath := f.String("config", "config/monitor.json", "config JSON path")
	ruleID := f.String("rule", "", "rule id")
	prices := f.String("prices", "", "comma-separated synthetic prices")
	step := f.Duration("step", time.Second, "virtual time between test ticks")
	send := f.Bool("send", false, "actually invoke Codex")
	dry := f.Bool("dry-run", false, "never send messages")
	statePath := f.String("state", "", "separate state path")
	duration := f.Duration("duration", 0, "stop run after duration; 0 means continuous")
	outPath := f.String("out", "", "write doctor report JSON")
	offline := f.Bool("offline", false, "skip doctor market API read")
	eventID := f.String("event", "", "event id to resolve")
	note := f.String("note", "", "receipt verification note")
	if err := f.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if *send && *dry {
		return fmt.Errorf("--send and --dry-run are mutually exclusive")
	}
	if *duration < 0 {
		return fmt.Errorf("duration must be >=0")
	}
	c, err := loadConfig(*configPath)
	if err != nil {
		return err
	}
	switch command {
	case "validate":
		printJSON(map[string]any{"valid": true, "symbol": c.Symbol, "rules": len(c.Rules), "config": c.Path})
		return nil
	case "rules":
		type entry struct {
			Rule
			FullMessageTemplate string `json:"full_message_template"`
		}
		entries := []entry{}
		for _, r := range c.Rules {
			if *ruleID == "" || *ruleID == r.ID {
				entries = append(entries, entry{r, c.rawTemplate(r)})
			}
		}
		if len(entries) == 0 {
			return fmt.Errorf("rule not found")
		}
		printJSON(map[string]any{"symbol": c.Symbol, "codex": c.Codex, "rules": entries})
		return nil
	case "template":
		r, err := c.rule(*ruleID)
		if err != nil {
			return err
		}
		fmt.Println(c.rawTemplate(r))
		return nil
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if command == "price" {
		m := newMarket(c)
		info, err := m.Validate(ctx)
		if err != nil {
			return err
		}
		tick, err := m.Latest(ctx)
		if err != nil {
			return err
		}
		printJSON(map[string]any{"symbol_info": info, "tick": tick})
		return nil
	}
	if command == "doctor" {
		p, pe := probeCodex(ctx, c.Codex)
		report := map[string]any{"codex": p, "config_valid": true, "thread_id": c.Codex.ThreadID, "thread_delivery_verified": false}
		if pe != nil {
			report["codex_error"] = pe.Error()
		}
		var me error
		if !*offline {
			m := newMarket(c)
			info, e := m.Validate(ctx)
			me = e
			report["market"] = info
			if e != nil {
				report["market_error"] = e.Error()
			}
		}
		if *outPath != "" {
			if err = atomicJSON(*outPath, report); err != nil {
				return err
			}
		}
		printJSON(report)
		return errors.Join(pe, me)
	}
	dryRun := c.Codex.DryRun
	if *send {
		dryRun = false
	}
	if *dry {
		dryRun = true
	}
	// Manual tests never inherit a live-send default.
	if command == "test" {
		dryRun = !*send
	}
	mode := "live"
	path := c.StateFile
	if dryRun {
		mode = "dry-run"
		path += ".dry-run.json"
	}
	if *statePath != "" {
		path, err = filepath.Abs(*statePath)
		if err != nil {
			return err
		}
	}
	if command == "test" {
		if *step < time.Millisecond {
			return fmt.Errorf("test step must be >=1ms")
		}
		if *statePath == "" {
			path = c.StateFile + ".test.json"
		}
		if path == c.StateFile || path == c.StateFile+".dry-run.json" {
			return fmt.Errorf("test requires separate state file")
		}
		mode = "test-dry-run"
		if *send {
			mode = "test-live"
		}
	}
	if dryRun && path == c.StateFile {
		return fmt.Errorf("dry-run cannot use live state_file")
	}
	if command == "status" {
		b, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			printJSON(map[string]any{"state_file": path, "initialized": false})
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}
	if command != "run" && command != "test" && command != "resolve" {
		return fmt.Errorf("unknown command %q", command)
	}
	lock, err := lockState(path)
	if err != nil {
		return err
	}
	defer lock.Close()
	if command == "test" {
		return testRule(ctx, c, path, mode, *ruleID, *prices, *step, dryRun)
	}
	s, err := loadState(path, c, mode)
	if err != nil {
		return err
	}
	if command == "resolve" {
		if *eventID == "" || strings.TrimSpace(*note) == "" {
			return fmt.Errorf("--event and receipt-verification --note required")
		}
		for i := range s.Events {
			v := &s.Events[i]
			if v.ID == *eventID {
				if v.Status != "unknown" {
					return fmt.Errorf("only unknown delivery can be resolved")
				}
				v.Resolved = true
				v.Resolution = *note
				return saveState(path, s)
			}
		}
		return fmt.Errorf("event not found")
	}
	p, err := probeCodex(ctx, c.Codex)
	if err != nil {
		return err
	}
	if _, err = buildInvocation(c, p, Event{}, dryRun); err != nil {
		return err
	}
	if *duration > 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, *duration)
		defer stop()
	}
	return monitor(ctx, c, s, path, p, dryRun)
}
func testRule(ctx context.Context, c Config, path, mode, id, prices string, step time.Duration, dry bool) error {
	c.StateFile = path
	r, err := c.rule(id)
	if err != nil {
		return err
	}
	if prices == "" {
		return fmt.Errorf("--prices required; same rule engine is used, no forced trigger")
	}
	r.Enabled = true
	c.Rules = []Rule{r}
	p, err := probeCodex(ctx, c.Codex)
	if err != nil {
		return err
	}
	// Each manual test is a fresh synthetic scenario; all test events are archived in stdout.
	s := newState(c, mode)
	e := newEngine(c, s)
	parts := strings.Split(prices, ",")
	if len(parts) > 10000 {
		return fmt.Errorf("at most 10000 test prices")
	}
	start := time.Now().Add(-time.Duration(len(parts)-1) * step)
	for i, value := range parts {
		at := start.Add(time.Duration(i) * step)
		t := Tick{Symbol: c.Symbol, Price: strings.TrimSpace(value), TradeID: int64(i + 1), TimeMS: at.UnixMilli(), Source: "synthetic", Synthetic: true}
		if _, err = decimal(t.Price); err != nil {
			return err
		}
		if _, err = e.Process(t, at); err != nil {
			return err
		}
	}
	if s.RejectedTicks > 0 {
		return fmt.Errorf("test rejected ticks: %s", s.LastRejection)
	}
	if !dry && len(s.Events) > 1 {
		return fmt.Errorf("live test is limited to one ACK message; shorten price sequence")
	}
	if err = saveState(path, s); err != nil {
		return err
	}
	for i := range s.Events {
		v := &s.Events[i]
		inv, err := buildInvocation(c, p, *v, dry)
		if err != nil {
			return err
		}
		logEvent("invocation", inv)
		v.Status = "dispatching"
		v.AttemptMS = time.Now().UnixMilli()
		if err = saveState(path, s); err != nil {
			return err
		}
		result := dispatch(ctx, c, p, *v, dry)
		v.Status = result.Status
		v.Result = result.Result
		v.FinishedMS = time.Now().UnixMilli()
		if err = saveState(path, s); err != nil {
			return err
		}
		logEvent("delivery", *v)
		if result.Status == "unknown" {
			return fmt.Errorf("delivery outcome unknown; inspect test state: %s", path)
		}
	}
	printJSON(map[string]any{"synthetic": true, "dry_run": dry, "events": len(s.Events), "accepted_ticks": s.AcceptedTicks, "state_file": path})
	if len(s.Events) == 0 {
		return fmt.Errorf("test sequence did not trigger this rule; inspect its condition/confirmation")
	}
	return nil
}
