package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Push delivery of inbox notifications to an external channel (Telegram
// first). The inbox store stays the source of truth and its on-disk format is
// unchanged; each push target keeps its own small ledger next to it recording
// what was delivered. Delivery is at-least-once: the ledger is written after a
// successful send, so a crash between the two can repeat one message, and a
// restart never repeats a delivered one.

const (
	NotificationPushMaxAttempts  = 5
	NotificationPushMaxAge       = 24 * time.Hour
	NotificationPushBatch        = NotificationMaxListLimit
	notificationPushLedgerMax    = NotificationMaxItems
	notificationPushLedgerBytes  = 1 << 20
	notificationPushStatusSent   = "delivered"
	notificationPushStatusSkip   = "skipped"
	notificationPushStatusFailed = "failed"
)

var notificationPushTargetPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}:[A-Za-z0-9_-]{1,64}$`)

// NotificationPushSender delivers one notification. It receives only the
// public projection (no private error text) and the rendered message.
type NotificationPushSender func(ctx context.Context, item NotificationItemView, text string) error

type NotificationPushOptions struct {
	// Target names the destination, e.g. "telegram:7590644052". It selects
	// the ledger, so changing the target starts a fresh delivery history.
	Target string
	// States limits which notification states are pushed. Empty means
	// paused and failed: the states that need the owner.
	States []string
	Clock  func() time.Time
}

type NotificationPushResult struct {
	Delivered int
	Skipped   int
	Failed    int
}

type notificationPushEntry struct {
	ID        string    `json:"id"`
	Status    string    `json:"status"`
	Attempts  int       `json:"attempts"`
	UpdatedAt time.Time `json:"updated_at"`
}

type notificationPushLedger struct {
	Version int                     `json:"version"`
	Target  string                  `json:"target"`
	Entries []notificationPushEntry `json:"entries"`
}

// DefaultNotificationPushStates are pushed when no states are configured.
var DefaultNotificationPushStates = []string{NotificationStatePaused, NotificationStateFailed}

// ValidateNotificationPushStates checks a configured state list.
func ValidateNotificationPushStates(states []string) error {
	seen := map[string]bool{}
	for _, state := range states {
		if !validNotificationState(state) {
			return fmt.Errorf("notification state %q is not one of succeeded, failed, paused", state)
		}
		if seen[state] {
			return fmt.Errorf("notification state %q is listed twice", state)
		}
		seen[state] = true
	}
	return nil
}

// NotificationPushLedgerPath is the ledger file for one target, next to the
// inbox store.
func NotificationPushLedgerPath(store *NotificationInboxStore, target string) string {
	name := strings.NewReplacer(":", "-").Replace(target)
	return filepath.Join(filepath.Dir(store.Path()), "notification-push-"+name+".json")
}

// FormatNotificationPush renders the plain-text push message. It carries the
// state, the job and its bounded summary only; the reason stays in the local
// inbox (`bee inbox`), which is where the owner acts on it.
func FormatNotificationPush(item NotificationItemView) string {
	name := item.Summary
	if name == "" {
		name = item.JobID
	}
	var line string
	switch item.State {
	case NotificationStatePaused:
		line = fmt.Sprintf("Bee job %q paused and needs you.", name)
	case NotificationStateFailed:
		line = fmt.Sprintf("Bee job %q failed.", name)
	default:
		line = fmt.Sprintf("Bee job %q %s.", name, item.State)
	}
	return line + "\n" + item.JobID + " · " + item.CreatedAt.UTC().Format("2006-01-02 15:04 UTC") + "\nDetails: bee inbox"
}

// DeliverNotificationPushes sends every unread, recent notification in the
// configured states that the target's ledger has not delivered. Items that
// are already read, or older than NotificationPushMaxAge when first seen, are
// recorded as skipped so enabling push never floods the channel with a
// backlog. A failed send is retried on later calls up to
// NotificationPushMaxAttempts and never marks the inbox item read.
func DeliverNotificationPushes(ctx context.Context, store *NotificationInboxStore, options NotificationPushOptions, send NotificationPushSender) (NotificationPushResult, error) {
	var result NotificationPushResult
	if store == nil || send == nil {
		return result, errors.New("notification push is not configured")
	}
	if !notificationPushTargetPattern.MatchString(options.Target) {
		return result, errors.New("notification push target is invalid")
	}
	states := options.States
	if len(states) == 0 {
		states = DefaultNotificationPushStates
	}
	if err := ValidateNotificationPushStates(states); err != nil {
		return result, err
	}
	wanted := map[string]bool{}
	for _, state := range states {
		wanted[state] = true
	}
	clock := options.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	listing, err := store.List(ctx, NotificationPushBatch)
	if err != nil {
		return result, err
	}
	path := NotificationPushLedgerPath(store, options.Target)
	lock, err := acquireSessionFileLockContext(ctx, path+".lock")
	if err != nil {
		return result, errors.New("notification push ledger is busy")
	}
	defer lock.Close()
	ledger, err := loadNotificationPushLedger(path, options.Target)
	if err != nil {
		return result, err
	}
	index := map[string]int{}
	for i, entry := range ledger.Entries {
		index[entry.ID] = i
	}
	record := func(id, status string, attempts int) {
		entry := notificationPushEntry{ID: id, Status: status, Attempts: attempts, UpdatedAt: clock().UTC()}
		if i, ok := index[id]; ok {
			ledger.Entries[i] = entry
			return
		}
		index[id] = len(ledger.Entries)
		ledger.Entries = append(ledger.Entries, entry)
	}
	changed := false
	// Oldest first, so a burst arrives in the order it happened.
	for i := len(listing.Items) - 1; i >= 0; i-- {
		item := listing.Items[i]
		if err := ctx.Err(); err != nil {
			break
		}
		previous, seen := notificationPushEntry{}, false
		if at, ok := index[item.ID]; ok {
			previous, seen = ledger.Entries[at], true
		}
		if seen && previous.Status != notificationPushStatusFailed {
			continue
		}
		if seen && previous.Attempts >= NotificationPushMaxAttempts {
			continue
		}
		if !wanted[item.State] || item.ReadAt != nil || (!seen && clock().Sub(item.CreatedAt) > NotificationPushMaxAge) {
			record(item.ID, notificationPushStatusSkip, previous.Attempts)
			result.Skipped++
			changed = true
			continue
		}
		attempts := previous.Attempts + 1
		if err := send(ctx, item, FormatNotificationPush(item)); err != nil {
			record(item.ID, notificationPushStatusFailed, attempts)
			result.Failed++
			changed = true
			continue
		}
		record(item.ID, notificationPushStatusSent, attempts)
		result.Delivered++
		changed = true
		// Persist after each send to keep the duplicate window to one message.
		if err := saveNotificationPushLedger(path, ledger); err != nil {
			return result, err
		}
		changed = false
	}
	if len(ledger.Entries) > notificationPushLedgerMax {
		ledger.Entries = ledger.Entries[len(ledger.Entries)-notificationPushLedgerMax:]
		changed = true
	}
	if changed {
		if err := saveNotificationPushLedger(path, ledger); err != nil {
			return result, err
		}
	}
	return result, nil
}

func loadNotificationPushLedger(path, target string) (notificationPushLedger, error) {
	empty := notificationPushLedger{Version: 1, Target: target, Entries: []notificationPushEntry{}}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return empty, errors.New("notification push ledger cannot be read")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return empty, errors.New("notification push ledger permissions are unsafe")
	}
	data, err := io.ReadAll(io.LimitReader(file, notificationPushLedgerBytes+1))
	if err != nil || len(data) > notificationPushLedgerBytes {
		return empty, errors.New("notification push ledger is too large")
	}
	var ledger notificationPushLedger
	if err := json.Unmarshal(data, &ledger); err != nil || ledger.Version != 1 || ledger.Target != target {
		return empty, errors.New("notification push ledger is corrupt")
	}
	if ledger.Entries == nil {
		ledger.Entries = []notificationPushEntry{}
	}
	return ledger, nil
}

func saveNotificationPushLedger(path string, ledger notificationPushLedger) error {
	data, err := json.Marshal(ledger)
	if err != nil {
		return errors.New("notification push ledger commit failed")
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".notification-push-")
	if err != nil {
		return errors.New("notification push ledger commit failed")
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return errors.New("notification push ledger commit failed")
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return errors.New("notification push ledger commit failed")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return errors.New("notification push ledger commit failed")
	}
	if err := tmp.Close(); err != nil {
		return errors.New("notification push ledger commit failed")
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return errors.New("notification push ledger commit failed")
	}
	return nil
}
