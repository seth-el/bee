package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const maxTTSLockPathBytes = MaxSessionRootPathByte

func validateTTSLockPath(path string) error {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" || len(trimmed) > maxTTSLockPathBytes || !utf8.ValidString(trimmed) || strings.IndexByte(trimmed, 0) >= 0 {
		return errors.New("TTS lock path is required and bounded")
	}
	if !filepath.IsAbs(trimmed) {
		return errors.New("TTS lock path must be absolute")
	}
	return nil
}

func withTTSRequestLock(ctx context.Context, path string, wait, stale time.Duration, fn func() error) error {
	if ctx == nil {
		return errors.New("TTS lock context is nil")
	}
	if fn == nil {
		return errors.New("TTS lock callback is nil")
	}
	if err := validateTTSLockPath(path); err != nil {
		return err
	}
	if wait <= 0 || stale < time.Second {
		return errors.New("TTS lock bounds are invalid")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return errors.New("create TTS lock parent")
	}
	deadline := time.Now().Add(wait)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.Mkdir(path, 0o700); err == nil {
			stop := make(chan struct{})
			var heartbeat sync.WaitGroup
			ownerPath := filepath.Join(path, "owner.json")
			owner, marshalErr := json.Marshal(map[string]any{"pid": os.Getpid(), "acquired_at": time.Now().UTC().Format(time.RFC3339)})
			if marshalErr == nil {
				_ = os.WriteFile(ownerPath, owner, 0o600)
			}
			heartbeat.Add(1)
			go func() {
				defer heartbeat.Done()
				ticker := time.NewTicker(minTTSHeartbeat(stale))
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						_ = os.Chtimes(path, time.Now(), time.Now())
					case <-stop:
						return
					}
				}
			}()
			defer func() {
				close(stop)
				heartbeat.Wait()
				_ = os.RemoveAll(path)
			}()
			return fn()
		} else if !errors.Is(err, os.ErrExist) {
			return errors.New("acquire TTS request lock")
		}
		info, statErr := os.Stat(path)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return errors.New("inspect TTS request lock")
		}
		if time.Since(info.ModTime()) > stale {
			if removeErr := os.RemoveAll(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return errors.New("reclaim stale TTS request lock")
			}
			continue
		}
		if !time.Now().Before(deadline) {
			return errors.New("timed out waiting for TTS request lock")
		}
		delay := 250 * time.Millisecond
		remaining := time.Until(deadline)
		if remaining < delay {
			delay = remaining
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func minTTSHeartbeat(stale time.Duration) time.Duration {
	interval := stale / 3
	if interval < time.Second {
		return time.Second
	}
	if interval > 5*time.Second {
		return 5 * time.Second
	}
	return interval
}
