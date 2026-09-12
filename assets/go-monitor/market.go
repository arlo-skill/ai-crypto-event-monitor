package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Market struct {
	C          Config
	Client     *http.Client
	mu         sync.Mutex
	retryAfter time.Time
}
type SymbolInfo struct {
	Symbol     string `json:"symbol"`
	Status     string `json:"status"`
	BaseAsset  string `json:"baseAsset"`
	QuoteAsset string `json:"quoteAsset"`
	Spot       bool   `json:"isSpotTradingAllowed"`
}

func newMarket(c Config) *Market {
	return &Market{C: c, Client: &http.Client{Timeout: c.Market.HTTPTimeout.D()}}
}
func (m *Market) get(ctx context.Context, path string, q url.Values, v any) error {
	m.mu.Lock()
	delay := time.Until(m.retryAfter)
	m.mu.Unlock()
	if delay > 0 {
		return fmt.Errorf("Binance REST backoff active for %s", delay.Round(time.Second))
	}
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(m.C.Market.RESTURL, "/")+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	r, err := m.Client.Do(req)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		wait := time.Minute
		if s, e := strconv.Atoi(r.Header.Get("Retry-After")); e == nil && s > 0 {
			wait = time.Duration(s) * time.Second
		} else if date, e := http.ParseTime(r.Header.Get("Retry-After")); e == nil && time.Until(date) > 0 {
			wait = time.Until(date)
		}
		if r.StatusCode == 418 && wait < 5*time.Minute {
			wait = 5 * time.Minute
		}
		if r.StatusCode == 429 || r.StatusCode == 418 || r.StatusCode >= 500 {
			m.mu.Lock()
			m.retryAfter = time.Now().Add(wait)
			m.mu.Unlock()
		}
		b, _ := io.ReadAll(io.LimitReader(r.Body, 2048))
		return fmt.Errorf("Binance HTTP %d: %s", r.StatusCode, b)
	}
	return json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(v)
}
func (m *Market) Validate(ctx context.Context) (SymbolInfo, error) {
	var response struct {
		Symbols []SymbolInfo `json:"symbols"`
	}
	q := url.Values{"symbol": {m.C.Symbol}}
	if err := m.get(ctx, "/api/v3/exchangeInfo", q, &response); err != nil {
		return SymbolInfo{}, err
	}
	if len(response.Symbols) != 1 {
		return SymbolInfo{}, fmt.Errorf("exchangeInfo did not return exactly one symbol")
	}
	s := response.Symbols[0]
	if s.Symbol != m.C.Symbol || s.BaseAsset != m.C.BaseAsset || s.QuoteAsset != m.C.QuoteAsset || s.Status != "TRADING" || !s.Spot {
		return s, fmt.Errorf("symbol identity/status is not valid spot TRADING: %+v", s)
	}
	return s, nil
}

type aggTrade struct {
	Event       string `json:"e"`
	EventTimeMS int64  `json:"E"`
	Symbol      string `json:"s"`
	ID          int64  `json:"a"`
	Price       string `json:"p"`
	TimeMS      int64  `json:"T"`
}

func (a aggTrade) tick(symbol, source string) Tick {
	return Tick{Symbol: symbol, Price: a.Price, TradeID: a.ID, TimeMS: a.TimeMS, Source: source}
}
func (m *Market) Latest(ctx context.Context) (Tick, error) {
	var trades []aggTrade
	err := m.get(ctx, "/api/v3/aggTrades", url.Values{"symbol": {m.C.Symbol}, "limit": {"1"}}, &trades)
	if err != nil {
		return Tick{}, err
	}
	if len(trades) != 1 {
		return Tick{}, fmt.Errorf("no recent aggregate trade")
	}
	return trades[0].tick(m.C.Symbol, "binance_rest"), nil
}
func (m *Market) streamURL() string {
	u, _ := url.Parse(m.C.Market.WSURL)
	u.Path = strings.TrimRight(u.Path, "/") + "/" + strings.ToLower(m.C.Symbol) + "@aggTrade"
	return u.String()
}

type FeedNotice struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

func notice(ch chan<- FeedNotice, kind, msg string) {
	select {
	case ch <- FeedNotice{kind, msg}:
	default:
	}
}
func waitContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
func (m *Market) Stream(ctx context.Context, ticks chan<- Tick, notices chan<- FeedNotice) {
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := m.connection(ctx, ticks, notices)
		if ctx.Err() != nil {
			return
		}
		notice(notices, "ws_disconnected", fmt.Sprint(err))
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		if !waitContext(ctx, backoff+time.Duration(rand.Int64N(int64(backoff/2)))) {
			return
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}
func (m *Market) connection(ctx context.Context, ticks chan<- Tick, notices chan<- FeedNotice) error {
	d := *websocket.DefaultDialer
	d.HandshakeTimeout = m.C.Market.HTTPTimeout.D()
	ws, response, err := d.DialContext(ctx, m.streamURL(), nil)
	if response != nil && response.Body != nil {
		response.Body.Close()
	}
	if err != nil {
		return err
	}
	defer ws.Close()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			ws.Close()
		case <-stop:
		}
	}()
	ws.SetReadLimit(65536)
	_ = ws.SetReadDeadline(time.Now().Add(70 * time.Second))
	ws.SetPingHandler(func(data string) error {
		_ = ws.SetReadDeadline(time.Now().Add(70 * time.Second))
		return ws.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(5*time.Second))
	})
	notice(notices, "ws_connected", m.streamURL())
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		_ = ws.SetReadDeadline(time.Now().Add(70 * time.Second))
		var a aggTrade
		if err = json.Unmarshal(data, &a); err != nil {
			return err
		}
		if a.Event == "serverShutdown" {
			return fmt.Errorf("Binance requested reconnect")
		}
		if a.Event != "aggTrade" {
			continue
		}
		if a.Symbol != m.C.Symbol {
			return fmt.Errorf("unexpected WS symbol %q", a.Symbol)
		}
		select {
		case ticks <- a.tick(a.Symbol, "binance_ws"):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
