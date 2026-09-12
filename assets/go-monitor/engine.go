package main

import (
	"fmt"
	"math/big"
	"strings"
	"time"
)

type Tick struct {
	Symbol    string `json:"symbol"`
	Price     string `json:"price"`
	TradeID   int64  `json:"trade_id"`
	TimeMS    int64  `json:"time_ms"`
	Source    string `json:"source"`
	Synthetic bool   `json:"synthetic"`
}
type RuleState struct {
	Fingerprint    string `json:"fingerprint"`
	Initialized    bool   `json:"initialized"`
	Latched        bool   `json:"latched"`
	Armed          bool   `json:"armed"`
	CandidateMS    int64  `json:"candidate_ms"`
	CandidateTicks int    `json:"candidate_ticks"`
	LastFiredMS    int64  `json:"last_fired_ms"`
	FireCount      int    `json:"fire_count"`
}
type Event struct {
	ID            string `json:"id"`
	RuleID        string `json:"rule_id"`
	Symbol        string `json:"symbol"`
	Price         string `json:"price"`
	PreviousPrice string `json:"previous_price"`
	Threshold     string `json:"threshold"`
	ChangePct     string `json:"change_pct,omitempty"`
	TradeID       int64  `json:"trade_id"`
	TimeMS        int64  `json:"time_ms"`
	TimeUTC       string `json:"time_utc"`
	Source        string `json:"source"`
	Synthetic     bool   `json:"synthetic"`
	Message       string `json:"message"`
	Status        string `json:"status"`
	AttemptMS     int64  `json:"attempt_ms,omitempty"`
	FinishedMS    int64  `json:"finished_ms,omitempty"`
	Result        string `json:"result,omitempty"`
	Resolved      bool   `json:"resolved,omitempty"`
	Resolution    string `json:"resolution,omitempty"`
}
type State struct {
	Version        int                   `json:"version"`
	Symbol         string                `json:"symbol"`
	Mode           string                `json:"mode"`
	UpdatedAt      string                `json:"updated_at"`
	ConfigHash     string                `json:"config_hash"`
	Rules          map[string]*RuleState `json:"rules"`
	LastTick       Tick                  `json:"last_tick"`
	Events         []Event               `json:"events"`
	LastDispatchMS int64                 `json:"last_dispatch_ms"`
	BudgetDay      string                `json:"budget_day_utc"`
	BudgetUsed     int                   `json:"budget_used"`
	AcceptedTicks  int64                 `json:"accepted_ticks"`
	SourceCounts   map[string]int64      `json:"source_counts"`
	RejectedTicks  int64                 `json:"rejected_ticks"`
	LastRejection  string                `json:"last_rejection,omitempty"`
	LastError      string                `json:"last_error,omitempty"`
}
type Engine struct {
	C        Config
	S        *State
	warm     bool
	previous Tick
	history  []Tick
}

func newState(c Config, mode string) *State {
	return &State{Version: 1, Symbol: c.Symbol, Mode: mode, ConfigHash: fingerprint(c), Rules: map[string]*RuleState{}, Events: []Event{}}
}
func newEngine(c Config, s *State) *Engine {
	if s.SourceCounts == nil {
		s.SourceCounts = map[string]int64{}
	}
	for _, r := range c.Rules {
		st := s.Rules[r.ID]
		if st == nil {
			st = &RuleState{}
			s.Rules[r.ID] = st
		}
		fp := fingerprint(r)
		if st.Fingerprint != fp { // Keep prior cooldown when editing a rule under the same id.
			*st = RuleState{Fingerprint: fp, LastFiredMS: st.LastFiredMS, FireCount: st.FireCount}
		}
		st.CandidateMS = 0
		st.CandidateTicks = 0
	}
	s.ConfigHash = fingerprint(c)
	return &Engine{C: c, S: s}
}
func (e *Engine) reject(reason string) []Event {
	e.S.RejectedTicks++
	e.S.LastRejection = reason
	return nil
}
func (e *Engine) Process(t Tick, now time.Time) ([]Event, error) {
	p, err := decimal(t.Price)
	if err != nil || p.Sign() <= 0 {
		return e.reject("invalid price"), nil
	}
	if t.Symbol != e.C.Symbol || t.TimeMS <= 0 || t.TradeID < 0 {
		return e.reject("wrong symbol or invalid timestamp/trade id"), nil
	}
	if now.UnixMilli()-t.TimeMS > e.C.Market.MaxTickAge.D().Milliseconds() || t.TimeMS > now.Add(5*time.Second).UnixMilli() {
		return e.reject("stale/future tick"), nil
	}
	if e.S.AcceptedTicks > 0 && (t.TradeID <= e.S.LastTick.TradeID || t.TimeMS < e.S.LastTick.TimeMS) {
		return e.reject("duplicate/out-of-order tick"), nil
	}
	gap := !e.warm || t.TimeMS-e.previous.TimeMS > e.C.Market.MaxGap.D().Milliseconds()
	coverageGap := gap || t.Source != e.previous.Source || (e.warm && t.TradeID != e.previous.TradeID+1)
	if coverageGap {
		e.history = nil
	}
	previousPrice := e.previous.Price
	// One last-trade sample per second; bound percentage-rule memory to one hour.
	if len(e.history) > 0 && e.history[len(e.history)-1].TimeMS/1000 == t.TimeMS/1000 {
		e.history[len(e.history)-1] = t
	} else {
		e.history = append(e.history, t)
	}
	cutoff := t.TimeMS - time.Hour.Milliseconds() - 2000
	i := 0
	for i < len(e.history) && e.history[i].TimeMS < cutoff {
		i++
	}
	if i > 0 {
		e.history = append([]Tick(nil), e.history[i:]...)
	}
	var emitted []Event
	for _, r := range e.C.Rules {
		if !r.Enabled {
			continue
		}
		st := e.S.Rules[r.ID]
		metric := p
		threshold := rat(r.Threshold)
		hyst := rat(r.Hysteresis)
		change := ""
		pct := strings.HasPrefix(r.Type, "pct_")
		if pct {
			if t.Source != "binance_ws" && !t.Synthetic {
				st.CandidateMS = 0
				st.CandidateTicks = 0
				continue
			}
			base := e.baseline(t.TimeMS - r.Window.D().Milliseconds())
			if base == nil {
				st.CandidateMS = 0
				st.CandidateTicks = 0
				continue
			}
			bp := rat(base.Price)
			metric = new(big.Rat).Mul(new(big.Rat).Quo(new(big.Rat).Sub(p, bp), bp), big.NewRat(100, 1))
			change = metric.FloatString(6)
			if r.Type == "pct_drop" {
				metric.Neg(metric)
			}
		}
		below := strings.HasSuffix(r.Type, "below") || r.Type == "below"
		condition := metric.Cmp(threshold) >= 0
		rearm := metric.Cmp(new(big.Rat).Sub(threshold, hyst)) < 0
		if below {
			condition = metric.Cmp(threshold) <= 0
			rearm = metric.Cmp(new(big.Rat).Add(threshold, hyst)) > 0
		}
		cross := strings.HasPrefix(r.Type, "cross_")
		continuous := r.ConfirmFor.D() > 0 || pct
		if !st.Initialized {
			st.Initialized = true
			st.Armed = !condition
			if condition && !r.FireOnStart {
				st.Latched = true
			}
		}
		if gap {
			st.CandidateMS = 0
			st.CandidateTicks = 0
			// Never infer a crossing across restart/disconnection.
			if cross {
				st.Armed = !condition
			}
		}
		if continuous && coverageGap {
			st.CandidateMS = 0
			st.CandidateTicks = 0
		}
		if continuous && t.Source != "binance_ws" && !t.Synthetic {
			st.CandidateMS = 0
			st.CandidateTicks = 0
			continue
		}
		if rearm {
			st.Latched = false
			st.Armed = true
		}
		if !condition {
			st.CandidateMS = 0
			st.CandidateTicks = 0
			continue
		}
		if st.Latched || (cross && !st.Armed) {
			continue
		}
		if st.CandidateTicks == 0 {
			st.CandidateMS = t.TimeMS
		}
		st.CandidateTicks++
		if st.CandidateTicks < r.ConfirmTicks || t.TimeMS-st.CandidateMS < r.ConfirmFor.D().Milliseconds() {
			continue
		}
		if now.UnixMilli()-st.LastFiredMS < r.Cooldown.D().Milliseconds() {
			continue
		}
		blocked := false
		pending := 0
		for _, v := range e.S.Events {
			if v.Status == "pending" || v.Status == "dispatching" {
				pending++
			}
			if v.RuleID == r.ID && ((v.Status == "unknown" && !v.Resolved) || v.Status == "pending" || v.Status == "dispatching") {
				blocked = true
			}
		}
		if blocked {
			continue
		}
		if pending >= e.C.Codex.MaxPending {
			return nil, fmt.Errorf("outbox full (%d); stopping instead of silently losing a trigger", pending)
		}
		id := fingerprint([]any{e.C.Symbol, r.ID, st.Fingerprint, t.TradeID, t.TimeMS, st.FireCount + 1})[:24]
		v := Event{ID: id, RuleID: r.ID, Symbol: t.Symbol, Price: t.Price, PreviousPrice: previousPrice, Threshold: r.Threshold, ChangePct: change, TradeID: t.TradeID, TimeMS: t.TimeMS, TimeUTC: time.UnixMilli(t.TimeMS).UTC().Format(time.RFC3339Nano), Source: t.Source, Synthetic: t.Synthetic, Status: "pending"}
		v.Message, err = e.C.render(r, v)
		if err != nil {
			return nil, err
		}
		st.Latched = true
		st.Armed = false
		st.LastFiredMS = now.UnixMilli()
		st.FireCount++
		st.CandidateMS = 0
		st.CandidateTicks = 0
		e.S.Events = append(e.S.Events, v)
		emitted = append(emitted, v)
	}
	e.warm = true
	e.previous = t
	e.S.LastTick = t
	e.S.AcceptedTicks++
	e.S.SourceCounts[t.Source]++
	return emitted, nil
}
func (e *Engine) baseline(cutoff int64) *Tick {
	for i := len(e.history) - 1; i >= 0; i-- {
		if e.history[i].TimeMS <= cutoff {
			if cutoff-e.history[i].TimeMS > e.C.Market.MaxGap.D().Milliseconds() {
				return nil
			}
			return &e.history[i]
		}
	}
	return nil
}
func pruneEvents(s *State) {
	if len(s.Events) <= 200 {
		return
	}
	excess := len(s.Events) - 200
	out := make([]Event, 0, len(s.Events))
	for _, v := range s.Events {
		terminal := v.Status != "pending" && v.Status != "dispatching" && (v.Status != "unknown" || v.Resolved)
		if excess > 0 && terminal {
			excess--
			continue
		}
		out = append(out, v)
	}
	s.Events = out
}
