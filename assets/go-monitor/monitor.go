package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

func monitor(ctx context.Context, c Config, s *State, path string, p ProbeResult, dry bool) error {
	c.StateFile = path
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := newMarket(c)
	info, err := m.Validate(ctx)
	if err != nil {
		return err
	}
	logEvent("market_validated", info)
	e := newEngine(c, s)
	if err = saveState(path, s); err != nil {
		return err
	}
	ticks := make(chan Tick, 1024)
	notices := make(chan FeedNotice, 16)
	deliveries := make(chan Delivery, 1)
	type restResult struct {
		Tick Tick
		Err  error
	}
	restResults := make(chan restResult, 1)
	type validationResult struct{ Err error }
	validations := make(chan validationResult, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); m.Stream(ctx, ticks, notices) }()
	defer func() { cancel(); wg.Wait() }()
	pulse := time.NewTicker(time.Second)
	defer pulse.Stop()
	lastWS := time.Time{}
	nextREST := time.Now()
	nextValidation := time.Now().Add(c.Market.RevalidateEvery.D())
	restBusy := false
	validationBusy := false
	deliveryBusy := false
	marketOK := true
	logEvent("started", map[string]any{"mode": s.Mode, "state_file": path, "codex_mode": p.Selected, "thread_id": c.Codex.ThreadID, "symbol": c.Symbol})
	accept := func(t Tick) error {
		if !marketOK {
			return nil
		}
		before := s.AcceptedTicks
		events, err := e.Process(t, time.Now())
		if err != nil {
			return err
		}
		if t.Source == "binance_ws" && s.AcceptedTicks > before {
			lastWS = time.Now()
		}
		if len(events) > 0 {
			if err = saveState(path, s); err != nil {
				return err
			}
			for _, v := range events {
				logEvent("trigger", v)
			}
		}
		return nil
	}
	startDispatch := func(now time.Time) error {
		if deliveryBusy || !marketOK {
			return nil
		}
		day := now.UTC().Format("2006-01-02")
		if s.BudgetDay != day {
			s.BudgetDay = day
			s.BudgetUsed = 0
		}
		for i := range s.Events {
			v := &s.Events[i]
			if v.Status != "pending" {
				continue
			}
			if now.UnixMilli()-v.TimeMS > c.Codex.EventTTL.D().Milliseconds() {
				v.Status = "expired"
				v.Result = "event too old; not sent"
				logEvent("expired", v.ID)
				continue
			}
			if s.BudgetUsed >= c.Codex.MaxPerDay || now.UnixMilli()-s.LastDispatchMS < c.Codex.MinInterval.D().Milliseconds() {
				return nil
			}
			inv, err := buildInvocation(c, p, *v, dry)
			if err != nil {
				return err
			}
			// Persist intent before starting any child process. Ambiguous attempts are never replayed.
			v.Status = "dispatching"
			v.AttemptMS = now.UnixMilli()
			s.LastDispatchMS = now.UnixMilli()
			s.BudgetUsed++
			if err = saveState(path, s); err != nil {
				return err
			}
			logEvent("invocation", inv)
			copyEvent := *v
			deliveryBusy = true
			wg.Add(1)
			go func() { defer wg.Done(); result := dispatch(ctx, c, p, copyEvent, dry); deliveries <- result }()
			return nil
		}
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			if deliveryBusy {
				result := <-deliveries
				applyDelivery(s, result)
			}
			if err = saveState(path, s); err != nil {
				return err
			}
			logEvent("stopped", map[string]any{"accepted_ticks": s.AcceptedTicks, "rejected_ticks": s.RejectedTicks, "last_price": s.LastTick.Price, "state_file": path})
			return nil
		case t := <-ticks:
			if err = accept(t); err != nil {
				return err
			}
		case n := <-notices:
			logEvent(n.Kind, n.Message)
		case r := <-restResults:
			restBusy = false
			if r.Err != nil {
				s.LastError = r.Err.Error()
				logEvent("rest_error", r.Err.Error())
			} else {
				logEvent("rest_fallback", r.Tick)
				if err = accept(r.Tick); err != nil {
					return err
				}
			}
		case r := <-validations:
			validationBusy = false
			marketOK = r.Err == nil
			if r.Err != nil {
				s.LastError = r.Err.Error()
				logEvent("market_paused", r.Err.Error())
				e.warm = false
			} else {
				logEvent("market_revalidated", c.Symbol)
				s.LastError = ""
			}
		case result := <-deliveries:
			deliveryBusy = false
			applyDelivery(s, result)
			if err = saveState(path, s); err != nil {
				return err
			}
			logEvent("delivery", result)
		case now := <-pulse.C:
			if c.Market.RESTFallback && !restBusy && !now.Before(nextREST) && now.Sub(lastWS) > c.Market.StaleAfter.D() {
				restBusy = true
				nextREST = now.Add(c.Market.RESTInterval.D())
				wg.Add(1)
				go func() { defer wg.Done(); t, err := m.Latest(ctx); restResults <- restResult{t, err} }()
			}
			if !validationBusy && !now.Before(nextValidation) {
				validationBusy = true
				nextValidation = now.Add(c.Market.RevalidateEvery.D())
				wg.Add(1)
				go func() { defer wg.Done(); _, err := m.Validate(ctx); validations <- validationResult{err} }()
			}
			if err = startDispatch(now); err != nil {
				return err
			}
			if err = saveState(path, s); err != nil {
				return fmt.Errorf("checkpoint failed; monitoring stopped: %w", err)
			}
		}
	}
}
func applyDelivery(s *State, d Delivery) {
	for i := range s.Events {
		if s.Events[i].ID == d.ID {
			s.Events[i].Status = d.Status
			s.Events[i].Result = d.Result
			s.Events[i].FinishedMS = time.Now().UnixMilli()
			return
		}
	}
}
