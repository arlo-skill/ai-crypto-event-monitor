package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

type ProbeResult struct {
	Binary     string            `json:"binary"`
	Version    string            `json:"version"`
	CheckedAt  string            `json:"checked_at"`
	Help       map[string]string `json:"help"`
	Errors     map[string]string `json:"errors"`
	Queue      bool              `json:"queue_supported"`
	ExecResume bool              `json:"exec_resume_supported"`
	Selected   string            `json:"selected_mode"`
}
type limitedBuffer struct {
	bytes.Buffer
	Limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.Limit - b.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}
func runCommand(ctx context.Context, binary string, args []string, stdin string) (string, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdin = strings.NewReader(stdin)
	// No shell: templates, Unicode, quotes and metacharacters are a single argument.
	var out limitedBuffer
	out.Limit = 65536
	cmd.Stdout = &out
	cmd.Stderr = &out
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return out.String(), err
}
func probeCodex(ctx context.Context, c CodexConfig) (ProbeResult, error) {
	p := ProbeResult{Binary: c.Binary, CheckedAt: time.Now().UTC().Format(time.RFC3339Nano), Help: map[string]string{}, Errors: map[string]string{}}
	path, err := exec.LookPath(c.Binary)
	if err != nil {
		return p, err
	}
	p.Binary = path
	for _, a := range [][]string{{"--version"}, {"--help"}, {"queue", "--help"}, {"resume", "--help"}, {"exec", "--help"}, {"exec", "resume", "--help"}} {
		cc, cancel := context.WithTimeout(ctx, 5*time.Second)
		out, e := runCommand(cc, path, a, "")
		cancel()
		key := strings.Join(a, " ")
		p.Help[key] = out
		if e != nil {
			p.Errors[key] = e.Error()
		}
	}
	p.Version = strings.TrimSpace(p.Help["--version"])
	top := p.Help["--help"]
	qh := p.Help["queue --help"]
	eh := p.Help["exec --help"]
	rh := p.Help["exec resume --help"]
	p.Queue = p.Errors["--help"] == "" && p.Errors["queue --help"] == "" && strings.Contains(top, "Queue a message") && strings.Contains(qh, "Usage: codex queue") && strings.Contains(qh, "--thread <THREAD>") && strings.Contains(qh, "--message <TEXT>")
	p.ExecResume = p.Errors["exec --help"] == "" && p.Errors["exec resume --help"] == "" && strings.Contains(rh, "Usage: codex exec resume") && strings.Contains(rh, "[SESSION_ID]") && strings.Contains(rh, "[PROMPT]") && strings.Contains(rh, "stdin") && strings.Contains(eh, "--sandbox") && strings.Contains(eh, "--skip-git-repo-check") && strings.Contains(top, "--ask-for-approval") && strings.Contains(top, "--search")
	switch c.Mode {
	case "auto":
		if p.Queue {
			p.Selected = "queue"
		} else if p.ExecResume {
			p.Selected = "exec-resume"
		}
	case "queue":
		if p.Queue {
			p.Selected = "queue"
		}
	case "exec-resume":
		if p.ExecResume {
			p.Selected = "exec-resume"
		}
	}
	if p.Selected == "" {
		return p, fmt.Errorf("no verified compatible Codex command; inspect probe errors/help and configure working binary")
	}
	return p, nil
}

type Invocation struct {
	Binary string   `json:"binary"`
	Args   []string `json:"args"`
	Stdin  string   `json:"stdin,omitempty"`
	DryRun bool     `json:"dry_run"`
}

func buildInvocation(c Config, p ProbeResult, e Event, dry bool) (Invocation, error) {
	v := Invocation{Binary: p.Binary, DryRun: dry}
	if !uuidPattern.MatchString(c.Codex.ThreadID) {
		return v, fmt.Errorf("explicit local Codex thread UUID required (do not use ChatGPT conversation UUID or --last)")
	}
	switch p.Selected {
	case "queue":
		v.Args = []string{"queue", "--thread", c.Codex.ThreadID, "--message", e.Message}
	case "exec-resume":
		v.Args = []string{"--ask-for-approval", "never", "--search", "exec", "--sandbox", "read-only", "--skip-git-repo-check", "resume", c.Codex.ThreadID, "-"}
		v.Stdin = e.Message
	default:
		return v, fmt.Errorf("unverified Codex mode")
	}
	return v, nil
}

type Delivery struct {
	ID     string
	Status string
	Result string
}

func dispatch(ctx context.Context, c Config, p ProbeResult, e Event, dry bool) Delivery {
	v, err := buildInvocation(c, p, e, dry)
	if err != nil {
		return Delivery{e.ID, "unknown", err.Error()}
	}
	if dry {
		return Delivery{e.ID, "dry_run", "rendered invocation only; no message sent"}
	}
	cc, cancel := context.WithTimeout(ctx, c.Codex.Timeout.D())
	defer cancel()
	out, err := runCommand(cc, v.Binary, v.Args, v.Stdin)
	if err != nil {
		return Delivery{e.ID, "unknown", fmt.Sprintf("%v; delivery may have occurred; no automatic retry\n%s", err, out)}
	}
	status := "completed"
	if p.Selected == "queue" {
		status = "queued"
	}
	return Delivery{e.ID, status, out}
}
