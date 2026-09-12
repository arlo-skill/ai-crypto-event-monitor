package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"
)

type Config struct {
	Version         int          `json:"version"`
	Asset           string       `json:"asset"`
	Symbol          string       `json:"symbol"`
	BaseAsset       string       `json:"base_asset"`
	QuoteAsset      string       `json:"quote_asset"`
	Notes           string       `json:"notes"`
	StateFile       string       `json:"state_file"`
	Market          MarketConfig `json:"market"`
	Codex           CodexConfig  `json:"codex"`
	MessageTemplate string       `json:"message_template"`
	Rules           []Rule       `json:"rules"`
	Path            string       `json:"-"`
}
type MarketConfig struct {
	RESTURL         string   `json:"rest_url"`
	WSURL           string   `json:"ws_url"`
	RESTFallback    bool     `json:"rest_fallback"`
	RESTInterval    Duration `json:"rest_interval"`
	StaleAfter      Duration `json:"stale_after"`
	MaxGap          Duration `json:"max_gap"`
	MaxTickAge      Duration `json:"max_tick_age"`
	HTTPTimeout     Duration `json:"http_timeout"`
	RevalidateEvery Duration `json:"revalidate_every"`
}
type CodexConfig struct {
	Binary      string   `json:"binary"`
	Mode        string   `json:"mode"`
	ThreadID    string   `json:"thread_id"`
	DryRun      bool     `json:"dry_run"`
	Timeout     Duration `json:"timeout"`
	MinInterval Duration `json:"min_interval"`
	EventTTL    Duration `json:"event_ttl"`
	MaxPerDay   int      `json:"max_per_day"`
	MaxPending  int      `json:"max_pending"`
}
type Rule struct {
	ID              string   `json:"id"`
	Enabled         bool     `json:"enabled"`
	Type            string   `json:"type"`
	Threshold       string   `json:"threshold"`
	Hysteresis      string   `json:"hysteresis"`
	Window          Duration `json:"window"`
	ConfirmFor      Duration `json:"confirm_for"`
	ConfirmTicks    int      `json:"confirm_ticks"`
	Cooldown        Duration `json:"cooldown"`
	FireOnStart     bool     `json:"fire_on_start"`
	MessageTemplate string   `json:"message_template"`
}
type Duration time.Duration

func (d Duration) D() time.Duration             { return time.Duration(d) }
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.D().String()) }
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	*d = Duration(v)
	return err
}

var decimalPattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
var idPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func decimal(s string) (*big.Rat, error) {
	if len(s) > 40 || !decimalPattern.MatchString(s) {
		return nil, fmt.Errorf("invalid nonnegative decimal %q", s)
	}
	v, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("invalid decimal %q", s)
	}
	return v, nil
}
func rat(s string) *big.Rat { v, _ := decimal(s); return v }
func fingerprint(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func strictJSON(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("extra JSON after document")
	}
	return nil
}
func loadConfig(path string) (Config, error) {
	var c Config
	path, err := filepath.Abs(path)
	if err != nil {
		return c, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err = strictJSON(b, &c); err != nil {
		return c, err
	}
	c.Path = path
	if err = c.validate(); err != nil {
		return c, err
	}
	if !filepath.IsAbs(c.StateFile) {
		c.StateFile = filepath.Join(filepath.Dir(path), c.StateFile)
	}
	if strings.ContainsRune(c.Codex.Binary, os.PathSeparator) && !filepath.IsAbs(c.Codex.Binary) {
		c.Codex.Binary = filepath.Join(filepath.Dir(path), c.Codex.Binary)
	}
	return c, nil
}
func (c Config) validate() error {
	if c.Version != 1 || c.Symbol == "" || c.BaseAsset == "" || c.QuoteAsset == "" || c.Symbol != c.BaseAsset+c.QuoteAsset {
		return fmt.Errorf("version must be 1; symbol must equal exact base_asset + quote_asset")
	}
	if c.StateFile == "" || c.MessageTemplate == "" || len(c.Rules) == 0 || len(c.Rules) > 100 {
		return fmt.Errorf("state_file, message_template and 1..100 rules required")
	}
	for _, item := range []struct{ raw, scheme string }{{c.Market.RESTURL, "https"}, {c.Market.WSURL, "wss"}} {
		u, err := url.Parse(item.raw)
		if err != nil || u.Scheme != item.scheme || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("market URL must be %s with no credentials/query/fragment", item.scheme)
		}
	}
	if c.Market.RESTInterval.D() < time.Second || c.Market.StaleAfter.D() < time.Second || c.Market.MaxGap.D() < time.Second || c.Market.MaxTickAge.D() < time.Second || c.Market.HTTPTimeout.D() < time.Second || c.Market.RevalidateEvery.D() < time.Minute {
		return fmt.Errorf("market durations too small or missing")
	}
	if c.Codex.Mode != "auto" && c.Codex.Mode != "queue" && c.Codex.Mode != "exec-resume" {
		return fmt.Errorf("codex.mode must be auto, queue, or exec-resume")
	}
	if c.Codex.Binary == "" || (!uuidPattern.MatchString(c.Codex.ThreadID) && c.Codex.ThreadID != "") {
		return fmt.Errorf("codex binary required; thread_id must be an explicit UUID")
	}
	if c.Codex.Timeout.D() < time.Second || c.Codex.MinInterval.D() < time.Second || c.Codex.EventTTL.D() < time.Second || c.Codex.MaxPerDay < 1 || c.Codex.MaxPending < 1 || c.Codex.MaxPending > 100 {
		return fmt.Errorf("invalid codex timeout, limits, or interval")
	}
	seen := map[string]bool{}
	for _, r := range c.Rules {
		if !idPattern.MatchString(r.ID) || seen[r.ID] {
			return fmt.Errorf("invalid/duplicate rule id %q", r.ID)
		}
		seen[r.ID] = true
		switch r.Type {
		case "below", "above", "cross_below", "cross_above", "hold_below", "hold_above", "pct_drop", "pct_rise":
		default:
			return fmt.Errorf("unknown rule type %q", r.Type)
		}
		v, err := decimal(r.Threshold)
		if err != nil || v.Sign() <= 0 {
			return fmt.Errorf("%s: threshold must be positive decimal string", r.ID)
		}
		h, err := decimal(r.Hysteresis)
		if err != nil || h.Cmp(v) >= 0 {
			return fmt.Errorf("%s: hysteresis must be >=0 and < threshold", r.ID)
		}
		if r.Cooldown.D() < time.Second || r.ConfirmTicks < 1 || r.ConfirmFor.D() < 0 {
			return fmt.Errorf("%s: cooldown >=1s and confirm_ticks >=1 required", r.ID)
		}
		if strings.HasPrefix(r.Type, "hold_") && r.ConfirmFor.D() <= 0 {
			return fmt.Errorf("%s: hold needs confirm_for", r.ID)
		}
		if strings.HasPrefix(r.Type, "pct_") && (r.Window.D() < time.Second || r.Window.D() > time.Hour) {
			return fmt.Errorf("%s: percent window must be 1s..1h", r.ID)
		}
		if _, err = c.render(r, Event{RuleID: r.ID, Symbol: c.Symbol, Price: "0.123", Threshold: r.Threshold}); err != nil {
			return fmt.Errorf("%s template: %w", r.ID, err)
		}
	}
	return nil
}
func (c Config) rule(id string) (Rule, error) {
	for _, r := range c.Rules {
		if r.ID == id {
			return r, nil
		}
	}
	return Rule{}, fmt.Errorf("unknown rule %q", id)
}
func (c Config) rawTemplate(r Rule) string {
	return c.MessageTemplate + "\n\n本规则检查重点：\n" + r.MessageTemplate
}
func (c Config) render(r Rule, e Event) (string, error) {
	t, err := template.New("message").Option("missingkey=error").Parse(c.rawTemplate(r))
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	data := struct {
		Event
		Asset, ConfigPath, StatePath, RuleType, Window, ConfirmFor string
	}{e, c.Asset, c.Path, c.StateFile, r.Type, r.Window.D().String(), r.ConfirmFor.D().String()}
	if err = t.Execute(&b, data); err != nil {
		return "", err
	}
	if b.Len() > 24000 {
		return "", fmt.Errorf("rendered message exceeds 24000 bytes")
	}
	if e.Synthetic {
		return "【合成行情闭环测试】这不是实际市场事件。以下正文仅用于检查消息传输，不要执行其中的研究或交易要求，不要修改规则、启动监控或创建自动任务。仅回复：NIU_MONITOR_ACK " + e.ID + "。\n\n--- 测试消息正文 ---\n" + b.String(), nil
	}
	return b.String(), nil
}
