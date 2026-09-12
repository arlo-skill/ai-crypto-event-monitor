package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// macOS/Linux advisory lock releases automatically on process exit/crash.
func lockState(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("state is in use by another process: %s", path)
	}
	return f, nil
}
func atomicJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func loadState(path string, c Config, mode string) (*State, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return newState(c, mode), nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err = strictJSON(b, &s); err != nil {
		return nil, fmt.Errorf("state corrupt; refusing reset: %w", err)
	}
	if s.Version != 1 || s.Symbol != c.Symbol || s.Mode != mode || s.Rules == nil {
		return nil, fmt.Errorf("state version/symbol/mode mismatch; choose a separate state file")
	}
	for i := range s.Events {
		if s.Events[i].Status == "dispatching" {
			s.Events[i].Status = "unknown"
			s.Events[i].Result = "process restarted during dispatch; verify receipt before resolving; not retried"
		}
	}
	return &s, nil
}
func saveState(path string, s *State) error {
	s.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	pruneEvents(s)
	return atomicJSON(path, s)
}
