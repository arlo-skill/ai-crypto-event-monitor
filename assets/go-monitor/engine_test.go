package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	c, err := loadConfig("config/monitor.json")
	if err != nil {
		t.Fatal(err)
	}
	c.StateFile = filepath.Join(t.TempDir(), "state.json")
	c.Codex.ThreadID = "00000000-0000-4000-8000-000000000001" // Synthetic test target; never a real task.
	return c
}
func basicRule() Rule {
	return Rule{ID: "r", Enabled: true, Type: "cross_below", Threshold: "0.118", Hysteresis: "0.001", ConfirmTicks: 1, Cooldown: Duration(10 * time.Second)}
}

type scenario struct {
	e  *Engine
	t  *testing.T
	at time.Time
	id int64
}

func scenarioFor(t *testing.T, r Rule) *scenario {
	c := testConfig(t)
	c.Rules = []Rule{r}
	c.MessageTemplate = "{{.ID}} {{.Price}} {{.RuleID}} {{.RuleType}}"
	return &scenario{e: newEngine(c, newState(c, "test")), t: t, at: time.Now()}
}
func (s *scenario) tick(price string, seconds int) []Event {
	s.t.Helper()
	s.at = s.at.Add(time.Duration(seconds) * time.Second)
	s.id++
	v, err := s.e.Process(Tick{Symbol: s.e.C.Symbol, Price: price, TimeMS: s.at.UnixMilli(), TradeID: s.id, Source: "binance_ws"}, s.at)
	if err != nil {
		s.t.Fatal(err)
	}
	return v
}
func TestCrossingCooldownHysteresis(t *testing.T) {
	s := scenarioFor(t, basicRule())
	if len(s.tick("0.120", 0)) != 0 || len(s.tick("0.118", 1)) != 1 {
		t.Fatal("boundary crossing not detected")
	}
	s.e.S.Events[0].Status = "queued"
	if len(s.tick("0.117", 1)) != 0 {
		t.Fatal("repeated condition")
	}
	s.tick("0.119", 1)
	if len(s.tick("0.118", 1)) != 0 {
		t.Fatal("hysteresis equality must not rearm")
	}
	s.tick("0.120", 1)
	if len(s.tick("0.118", 1)) != 0 {
		t.Fatal("cooldown bypass")
	}
	if len(s.tick("0.117", 5)) != 1 {
		t.Fatal("rearmed condition after cooldown should fire")
	}
}
func TestStartupAndRestart(t *testing.T) {
	r := basicRule()
	r.Type = "below"
	s := scenarioFor(t, r)
	if len(s.tick("0.117", 0)) != 0 {
		t.Fatal("startup alert flood")
	}
	s.tick("0.120", 1)
	if len(s.tick("0.118", 1)) != 1 {
		t.Fatal("missing level event")
	}
	s.e = newEngine(s.e.C, s.e.S)
	if len(s.tick("0.117", 1)) != 0 {
		t.Fatal("restart duplicated latched event")
	}
}
func TestCrossingDoesNotBridgeGapOrRestart(t *testing.T) {
	for _, restart := range []bool{false, true} {
		s := scenarioFor(t, basicRule())
		s.tick("0.120", 0)
		if restart {
			s.e = newEngine(s.e.C, s.e.S)
		}
		if len(s.tick("0.117", 20)) != 0 {
			t.Fatal("invented crossing across gap")
		}
		s.tick("0.120", 1)
		if len(s.tick("0.117", 1)) != 1 {
			t.Fatal("valid new crossing missing")
		}
	}
}
func TestHoldContinuity(t *testing.T) {
	r := basicRule()
	r.Type = "hold_above"
	r.ConfirmFor = Duration(3 * time.Second)
	r.ConfirmTicks = 2
	s := scenarioFor(t, r)
	s.tick("0.110", 0)
	s.tick("0.120", 1)
	s.tick("0.120", 1)
	if len(s.tick("0.120", 20)) != 0 {
		t.Fatal("hold bridged outage")
	}
	s.tick("0.120", 1)
	if len(s.tick("0.120", 2)) != 1 {
		t.Fatal("hold not confirmed")
	}
}
func TestHoldRejectsRESTAndTradeGaps(t *testing.T) {
	r := basicRule()
	r.Type = "hold_above"
	r.ConfirmFor = Duration(2 * time.Second)
	s := scenarioFor(t, r)
	s.tick("0.110", 0)
	s.tick("0.120", 1)
	s.id += 9
	if len(s.tick("0.120", 2)) != 0 {
		t.Fatal("hold bridged missing trades")
	}
	s.at = s.at.Add(3 * time.Second)
	s.id++
	v, err := s.e.Process(Tick{Symbol: s.e.C.Symbol, Price: "0.120", TimeMS: s.at.UnixMilli(), TradeID: s.id, Source: "binance_rest"}, s.at)
	if err != nil || len(v) > 0 {
		t.Fatal("hold triggered with REST samples")
	}
}
func TestPercentageWarmup(t *testing.T) {
	r := basicRule()
	r.Type = "pct_drop"
	r.Threshold = "8"
	r.Hysteresis = "2"
	r.Window = Duration(3 * time.Second)
	r.FireOnStart = true
	s := scenarioFor(t, r)
	s.tick("0.100", 0)
	s.tick("0.097", 1)
	if len(s.tick("0.094", 1)) != 0 {
		t.Fatal("insufficient history")
	}
	if len(s.tick("0.092", 1)) != 1 {
		t.Fatal("exact 8% threshold missing")
	}
}
func TestDecimalPrecision(t *testing.T) {
	r := basicRule()
	r.Threshold = "0.100000000000000001"
	r.Hysteresis = "0"
	s := scenarioFor(t, r)
	s.tick("0.100000000000000002", 0)
	if len(s.tick("0.100000000000000001", 1)) != 1 {
		t.Fatal("decimal rounded at boundary")
	}
}
func TestBadTicks(t *testing.T) {
	s := scenarioFor(t, basicRule())
	s.tick("0.120", 0)
	base := Tick{Symbol: s.e.C.Symbol, Price: "0.117", TradeID: 2, TimeMS: s.at.Add(time.Second).UnixMilli(), Source: "binance_ws"}
	cases := []Tick{}
	for _, price := range []string{"NaN", "Inf", "0", "-1", "1e9"} {
		v := base
		v.Price = price
		cases = append(cases, v)
	}
	v := base
	v.Symbol = "NIULAIUSDT"
	cases = append(cases, v)
	v = base
	v.TradeID = 1
	cases = append(cases, v)
	v = base
	v.TimeMS = s.at.Add(-time.Minute).UnixMilli()
	cases = append(cases, v)
	v = base
	v.TimeMS = s.at.Add(time.Minute).UnixMilli()
	cases = append(cases, v)
	for _, tick := range cases {
		events, err := s.e.Process(tick, s.at)
		if err != nil || len(events) > 0 {
			t.Fatal("bad tick accepted", tick, err)
		}
	}
	if s.e.S.AcceptedTicks != 1 || s.e.S.RejectedTicks != int64(len(cases)) {
		t.Fatal("rejection accounting")
	}
}
func TestUnknownDeliveryBlocksRule(t *testing.T) {
	s := scenarioFor(t, basicRule())
	s.tick("0.120", 0)
	s.tick("0.117", 1)
	s.e.S.Events[0].Status = "unknown"
	s.tick("0.120", 5)
	if len(s.tick("0.117", 6)) != 0 {
		t.Fatal("unknown auto-retried")
	}
	s.e.S.Events[0].Resolved = true
	if len(s.tick("0.117", 1)) != 1 {
		t.Fatal("resolved rule did not recover")
	}
}
func TestConfigValidation(t *testing.T) {
	c := testConfig(t)
	b, _ := json.Marshal(c)
	var x Config
	if err := strictJSON(b, &x); err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(string(b), `"version":1`, `"version":1,"typo":true`, 1)
	if strictJSON([]byte(bad), &x) == nil {
		t.Fatal("unknown config key accepted")
	}
	c.Rules[0].MessageTemplate = "{{.DoesNotExist}}"
	if c.validate() == nil {
		t.Fatal("unknown template variable accepted")
	}
}
func TestStateLockRecoveryAndCorruption(t *testing.T) {
	c := testConfig(t)
	f, err := lockState(c.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := lockState(c.StateFile); err == nil {
		second.Close()
		t.Fatal("second writer acquired lock")
	}
	f.Close()
	f, err = lockState(c.StateFile)
	if err != nil {
		t.Fatal("lock not released")
	}
	defer f.Close()
	s := newState(c, "live")
	s.Events = []Event{{ID: "x", Status: "dispatching"}}
	if err = saveState(c.StateFile, s); err != nil {
		t.Fatal(err)
	}
	s, err = loadState(c.StateFile, c, "live")
	if err != nil || s.Events[0].Status != "unknown" {
		t.Fatal("ambiguous delivery replay risk", err)
	}
	if _, err = loadState(c.StateFile, c, "dry-run"); err == nil {
		t.Fatal("mode mixing")
	}
	if err = os.WriteFile(c.StateFile, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = loadState(c.StateFile, c, "live"); err == nil {
		t.Fatal("corrupt state silently reset")
	}
}

func TestAllRuleDirections(t *testing.T) {
	for _, kind := range []string{"below", "above", "cross_below", "cross_above", "hold_below", "hold_above", "pct_drop", "pct_rise"} {
		t.Run(kind, func(t *testing.T) {
			r := basicRule()
			r.Type = kind
			r.Threshold = "0.100"
			r.Hysteresis = "0.001"
			if strings.HasPrefix(kind, "hold_") {
				r.ConfirmFor = Duration(2 * time.Second)
			}
			if strings.HasPrefix(kind, "pct_") {
				r.Threshold = "5"
				r.Hysteresis = "1"
				r.Window = Duration(2 * time.Second)
				r.FireOnStart = true
			}
			s := scenarioFor(t, r)
			prices := []string{"0.110", "0.100", "0.099", "0.098"}
			if strings.Contains(kind, "above") {
				prices = []string{"0.090", "0.100", "0.101", "0.102"}
			}
			if kind == "pct_drop" {
				prices = []string{"0.100", "0.097", "0.094", "0.091"}
			}
			if kind == "pct_rise" {
				prices = []string{"0.100", "0.103", "0.106", "0.109"}
			}
			count := 0
			for _, p := range prices {
				count += len(s.tick(p, 1))
			}
			if count != 1 {
				t.Fatalf("%s produced %d events", kind, count)
			}
		})
	}
}

func TestOutboxCapacityFailsClosed(t *testing.T) {
	r := basicRule()
	s := scenarioFor(t, r)
	second := r
	second.ID = "r2"
	s.e.C.Rules = append(s.e.C.Rules, second)
	s.e.C.Codex.MaxPending = 1
	s.e = newEngine(s.e.C, s.e.S)
	s.tick("0.120", 0)
	s.at = s.at.Add(time.Second)
	_, err := s.e.Process(Tick{Symbol: s.e.C.Symbol, Price: "0.117", TimeMS: s.at.UnixMilli(), TradeID: 2, Source: "binance_ws"}, s.at)
	if err == nil || !strings.Contains(err.Error(), "outbox full") {
		t.Fatal("queue overflow was silently ignored")
	}
}
