package bee

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	NotificationStateSucceeded   = "succeeded"
	NotificationStateFailed      = "failed"
	NotificationStatePaused      = "paused"
	NotificationKindJob          = "job"
	NotificationMaxItems         = 1000
	NotificationMaxListLimit     = 50
	NotificationMaxTitleBytes    = 256
	NotificationMaxErrorBytes    = 1024
	NotificationMaxSnapshotBytes = 8 << 20
	NotificationMaxUnreadBadge   = 99
)

var (
	ErrNotificationNotFound = errors.New("notification not found")
)

type NotificationItem struct {
	ID          string     `json:"id"`
	DeliveryKey string     `json:"delivery_key"`
	Kind        string     `json:"kind"`
	JobID       string     `json:"job_id"`
	AttemptID   string     `json:"attempt_id"`
	State       string     `json:"state"`
	Summary     string     `json:"summary,omitempty"`
	Error       string     `json:"error,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	ReadAt      *time.Time `json:"read_at,omitempty"`
	Sequence    uint64     `json:"sequence"`
}

type NotificationOutboxRecord struct {
	ID          string    `json:"id"`
	DeliveryKey string    `json:"delivery_key"`
	Kind        string    `json:"kind"`
	JobID       string    `json:"job_id"`
	AttemptID   string    `json:"attempt_id"`
	State       string    `json:"state"`
	CreatedAt   time.Time `json:"created_at"`
	CommittedAt time.Time `json:"committed_at"`
}

type notificationSnapshotPayload struct {
	Version  uint64                     `json:"version"`
	Sequence uint64                     `json:"sequence"`
	Outbox   []NotificationOutboxRecord `json:"outbox"`
	Items    []NotificationItem         `json:"items"`
}

type notificationSnapshot struct {
	Version  uint64                     `json:"version"`
	Sequence uint64                     `json:"sequence"`
	Outbox   []NotificationOutboxRecord `json:"outbox"`
	Items    []NotificationItem         `json:"items"`
	Checksum string                     `json:"checksum"`
}

type NotificationListResult struct {
	Items           []NotificationItemView `json:"items"`
	UnreadCount     int                    `json:"unread_count"`
	UnreadTruncated bool                   `json:"unread_truncated,omitempty"`
	Sequence        uint64                 `json:"sequence"`
	Truncated       bool                   `json:"truncated,omitempty"`
	NextBefore      *uint64                `json:"next_before,omitempty"`
}

// NotificationItemView is the bounded public projection. Delivery keys and
// attempt identifiers remain private to the job/inbox stores.
type NotificationItemView struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind"`
	JobID     string     `json:"job_id"`
	State     string     `json:"state"`
	Summary   string     `json:"summary,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ReadAt    *time.Time `json:"read_at,omitempty"`
	Sequence  uint64     `json:"sequence"`
}

type NotificationInboxStore struct {
	path  string
	mu    sync.Mutex
	clock func() time.Time
}

func NewNotificationInboxStore(path string) (*NotificationInboxStore, error) {
	if strings.TrimSpace(path) == "" || !filepath.IsAbs(path) || filepath.Clean(path) == string(filepath.Separator) {
		return nil, errors.New("notification store path is invalid")
	}
	path = filepath.Clean(path)
	if err := rejectBeeSymlinkAncestors(path); err != nil {
		return nil, errors.New("notification store path is unsafe")
	}
	if err := ensurePrivateDir(filepath.Dir(path), 0o700); err != nil {
		return nil, errors.New("notification store directory is unavailable")
	}
	return &NotificationInboxStore{path: path, clock: func() time.Time { return time.Now().UTC() }}, nil
}

func NewDefaultNotificationInboxStore() (*NotificationInboxStore, error) {
	path, err := DefaultBeeNotificationStorePath()
	if err != nil {
		return nil, err
	}
	return NewNotificationInboxStore(path)
}

func (s *NotificationInboxStore) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

func (s *NotificationInboxStore) SetClock(clock func() time.Time) {
	if s != nil && clock != nil {
		s.clock = clock
	}
}

func (s *NotificationInboxStore) withLock(ctx context.Context, fn func(*notificationSnapshot) (bool, error)) error {
	if s == nil {
		return errors.New("notification store is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.lockLocal(ctx); err != nil {
		return err
	}
	defer s.mu.Unlock()
	if info, err := os.Lstat(s.path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("notification store is unsafe")
	}
	if info, err := os.Lstat(s.path + ".lock"); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("notification store is unsafe")
	}
	parentInfo, err := os.Lstat(filepath.Dir(s.path))
	if err != nil || parentInfo.Mode()&os.ModeSymlink != 0 || !parentInfo.IsDir() || parentInfo.Mode().Perm()&0o077 != 0 {
		return errors.New("notification store directory is unsafe")
	}
	lock, err := acquireSessionFileLockContext(ctx, s.path+".lock")
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("notification store is busy")
	}
	defer lock.Close()
	if err := rejectBeeSymlinkAncestors(s.path); err != nil {
		return errors.New("notification store is unsafe")
	}
	snap, err := s.load()
	if err != nil {
		return err
	}
	changed, err := fn(&snap)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.save(snap)
}

func (s *NotificationInboxStore) lockLocal(ctx context.Context) error {
	for {
		if s.mu.TryLock() {
			return nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *NotificationInboxStore) load() (notificationSnapshot, error) {
	file, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return notificationSnapshot{Version: 1, Outbox: []NotificationOutboxRecord{}, Items: []NotificationItem{}}, nil
	}
	if err != nil {
		return notificationSnapshot{}, errors.New("notification store cannot be read")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return notificationSnapshot{}, errors.New("notification store permissions are unsafe")
	}
	if info.Size() > NotificationMaxSnapshotBytes {
		return notificationSnapshot{}, errors.New("notification store is too large")
	}
	data, err := io.ReadAll(io.LimitReader(file, NotificationMaxSnapshotBytes+1))
	if err != nil || len(data) > NotificationMaxSnapshotBytes {
		return notificationSnapshot{}, errors.New("notification store cannot be read")
	}
	var snap notificationSnapshot
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&snap); err != nil {
		return notificationSnapshot{}, errors.New("notification store is corrupt")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return notificationSnapshot{}, errors.New("notification store is corrupt")
	}
	if snap.Version != 1 || snap.Checksum == "" {
		return notificationSnapshot{}, errors.New("notification store is corrupt")
	}
	payload := notificationSnapshotPayload{Version: snap.Version, Sequence: snap.Sequence, Outbox: snap.Outbox, Items: snap.Items}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return notificationSnapshot{}, errors.New("notification store is corrupt")
	}
	sum := sha256.Sum256(encoded)
	if !strings.EqualFold(snap.Checksum, hex.EncodeToString(sum[:])) {
		return notificationSnapshot{}, errors.New("notification store checksum mismatch")
	}
	if err := validateNotificationSnapshot(&snap); err != nil {
		return notificationSnapshot{}, errors.New("notification store is corrupt")
	}
	return snap, nil
}

func (s *NotificationInboxStore) save(snap notificationSnapshot) error {
	if err := validateNotificationSnapshot(&snap); err != nil {
		return err
	}
	payload := notificationSnapshotPayload{Version: 1, Sequence: snap.Sequence, Outbox: snap.Outbox, Items: snap.Items}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return errors.New("notification store commit failed")
	}
	sum := sha256.Sum256(encoded)
	snap.Version = 1
	snap.Checksum = hex.EncodeToString(sum[:])
	data, err := json.Marshal(snap)
	if err != nil || len(data) > NotificationMaxSnapshotBytes {
		return errors.New("notification store size limit exceeded")
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".notifications-")
	if err != nil {
		return errors.New("notification store commit failed")
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return errors.New("notification store commit failed")
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return errors.New("notification store commit failed")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return errors.New("notification store commit failed")
	}
	if err := tmp.Close(); err != nil {
		return errors.New("notification store commit failed")
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return errors.New("notification store commit failed")
	}
	if dir, err := os.Open(filepath.Dir(s.path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func validateNotificationSnapshot(snap *notificationSnapshot) error {
	if snap == nil || snap.Version != 1 || len(snap.Items) > NotificationMaxItems || len(snap.Outbox) > NotificationMaxItems || snap.Sequence < uint64(len(snap.Items)) {
		return errors.New("invalid notification snapshot")
	}
	items := make(map[string]NotificationItem, len(snap.Items))
	keys := make(map[string]bool, len(snap.Items))
	for i := range snap.Items {
		item := &snap.Items[i]
		if err := validateNotificationItem(item); err != nil || item.Sequence == 0 || item.Sequence > snap.Sequence || items[item.ID].ID != "" || keys[item.DeliveryKey] {
			return errors.New("invalid notification item")
		}
		items[item.ID] = *item
		keys[item.DeliveryKey] = true
	}
	outboxKeys := make(map[string]bool, len(snap.Outbox))
	for i := range snap.Outbox {
		row := &snap.Outbox[i]
		if err := validateNotificationOutbox(row); err != nil || outboxKeys[row.DeliveryKey] {
			return errors.New("invalid notification outbox")
		}
		outboxKeys[row.DeliveryKey] = true
		itemID := notificationID(row.DeliveryKey)
		item, ok := items[itemID]
		if !ok || item.JobID != row.JobID || item.AttemptID != row.AttemptID || item.State != row.State {
			return errors.New("notification outbox is not committed")
		}
	}
	if len(snap.Items) != len(snap.Outbox) {
		return errors.New("notification inbox/outbox mismatch")
	}
	return nil
}

func validateNotificationItem(item *NotificationItem) error {
	if item == nil || item.ID != notificationID(item.DeliveryKey) || !validNotificationDeliveryKey(item.DeliveryKey) || item.Kind != NotificationKindJob || item.JobID == "" || item.AttemptID == "" || !validNotificationState(item.State) || item.CreatedAt.IsZero() || !timeIsUTC(item.CreatedAt) || len(item.Summary) > NotificationMaxTitleBytes || len(item.Error) > NotificationMaxErrorBytes {
		return errors.New("invalid notification item")
	}
	if item.ReadAt != nil && (item.ReadAt.IsZero() || !timeIsUTC(*item.ReadAt) || item.ReadAt.Before(item.CreatedAt)) {
		return errors.New("invalid notification read timestamp")
	}
	return nil
}

func validateNotificationOutbox(row *NotificationOutboxRecord) error {
	if row == nil || row.ID != notificationID(row.DeliveryKey) || !validNotificationDeliveryKey(row.DeliveryKey) || row.Kind != NotificationKindJob || row.JobID == "" || row.AttemptID == "" || !validNotificationState(row.State) || row.CreatedAt.IsZero() || row.CommittedAt.IsZero() || !timeIsUTC(row.CreatedAt) || !timeIsUTC(row.CommittedAt) || row.CommittedAt.Before(row.CreatedAt) {
		return errors.New("invalid notification outbox")
	}
	return nil
}

func validNotificationState(state string) bool {
	return state == NotificationStateSucceeded || state == NotificationStateFailed || state == NotificationStatePaused
}
func validNotificationDeliveryKey(key string) bool {
	return len(key) > 0 && len(key) <= 512 && !strings.ContainsAny(key, "\x00\r\n")
}
func notificationDeliveryKey(result JobResult) string {
	return fmt.Sprintf("job/%s/attempt/%s/state/%s", result.JobID, result.AttemptID, result.State)
}
func notificationID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "notification-" + hex.EncodeToString(sum[:])
}

func (s *NotificationInboxStore) EnqueueJobResult(ctx context.Context, result JobResult) (NotificationItem, error) {
	if s == nil {
		return NotificationItem{}, errors.New("notification store is not configured")
	}
	if result.State != JobSucceeded && result.State != JobFailed && result.State != JobPaused {
		return NotificationItem{}, errors.New("only succeeded, failed, and paused jobs create notifications")
	}
	if err := validateSafeID("job id", result.JobID, 96); err != nil {
		return NotificationItem{}, err
	}
	if err := validateSafeID("attempt id", result.AttemptID, 96); err != nil {
		return NotificationItem{}, err
	}
	if result.FinishedAt.IsZero() || !timeIsUTC(result.FinishedAt) {
		return NotificationItem{}, errors.New("job finished timestamp is invalid")
	}
	key := notificationDeliveryKey(result)
	id := notificationID(key)
	now := s.clock().UTC()
	var out NotificationItem
	err := s.withLock(ctx, func(snap *notificationSnapshot) (bool, error) {
		for _, item := range snap.Items {
			if item.DeliveryKey == key {
				out = item
				return false, nil
			}
		}
		if len(snap.Items) >= NotificationMaxItems {
			return false, errors.New("notification inbox limit exceeded")
		}
		created := result.FinishedAt.UTC()
		committedAt := now
		if committedAt.Before(created) {
			committedAt = created
		}
		snap.Sequence++
		summary := strings.TrimSpace(result.Summary)
		if len(summary) > NotificationMaxTitleBytes || strings.ContainsAny(summary, "\x00\r\n") {
			return false, errors.New("notification summary is invalid")
		}
		item := NotificationItem{ID: id, DeliveryKey: key, Kind: NotificationKindJob, JobID: result.JobID, AttemptID: result.AttemptID, State: string(result.State), Summary: summary, Error: boundedNotificationError(result.Error), CreatedAt: created, Sequence: snap.Sequence}
		row := NotificationOutboxRecord{ID: id, DeliveryKey: key, Kind: NotificationKindJob, JobID: result.JobID, AttemptID: result.AttemptID, State: string(result.State), CreatedAt: created, CommittedAt: committedAt}
		snap.Outbox = append(snap.Outbox, row)
		snap.Items = append(snap.Items, item)
		out = item
		return true, nil
	})
	return out, err
}

func boundedNotificationError(value string) string {
	if len(value) > NotificationMaxErrorBytes {
		return value[:NotificationMaxErrorBytes]
	}
	return value
}

func (s *NotificationInboxStore) List(ctx context.Context, limit int) (NotificationListResult, error) {
	return s.ListBefore(ctx, limit, 0)
}

func (s *NotificationInboxStore) ListBefore(ctx context.Context, limit int, before uint64) (NotificationListResult, error) {
	if limit == 0 {
		limit = NotificationMaxListLimit
	}
	if limit < 1 || limit > NotificationMaxListLimit {
		return NotificationListResult{}, errors.New("notification list limit is invalid")
	}
	var result NotificationListResult
	err := s.withLock(ctx, func(snap *notificationSnapshot) (bool, error) {
		result.Sequence = snap.Sequence
		result.Items = make([]NotificationItemView, 0, minNotificationInt(limit, len(snap.Items)))
		unread := 0
		for _, item := range snap.Items {
			if item.ReadAt == nil {
				unread++
			}
		}
		result.UnreadTruncated = unread > NotificationMaxUnreadBadge
		if unread > NotificationMaxUnreadBadge {
			unread = NotificationMaxUnreadBadge
		}
		result.UnreadCount = unread
		for i := len(snap.Items) - 1; i >= 0 && len(result.Items) < limit; i-- {
			if before != 0 && snap.Items[i].Sequence >= before {
				continue
			}
			result.Items = append(result.Items, notificationItemView(snap.Items[i]))
		}
		result.Truncated = false
		if len(result.Items) > 0 {
			oldest := result.Items[len(result.Items)-1].Sequence
			for _, item := range snap.Items {
				if item.Sequence < oldest {
					result.Truncated = true
					next := oldest
					result.NextBefore = &next
					break
				}
			}
		}
		if len(result.Items) == 0 && before != 0 {
			for _, item := range snap.Items {
				if item.Sequence < before {
					result.Truncated = true
					next := before
					result.NextBefore = &next
					break
				}
			}
		}
		return false, nil
	})
	return result, err
}

func notificationItemView(item NotificationItem) NotificationItemView {
	return NotificationItemView{ID: item.ID, Kind: item.Kind, JobID: item.JobID, State: item.State, Summary: item.Summary, CreatedAt: item.CreatedAt, ReadAt: item.ReadAt, Sequence: item.Sequence}
}

func (s *NotificationInboxStore) MarkRead(ctx context.Context, id string) (NotificationItem, error) {
	if err := validateSafeID("notification id", id, 96); err != nil {
		return NotificationItem{}, err
	}
	var out NotificationItem
	err := s.withLock(ctx, func(snap *notificationSnapshot) (bool, error) {
		for i := range snap.Items {
			if snap.Items[i].ID != id {
				continue
			}
			if snap.Items[i].ReadAt == nil {
				now := s.clock().UTC()
				if now.Before(snap.Items[i].CreatedAt) {
					now = snap.Items[i].CreatedAt
				}
				snap.Items[i].ReadAt = &now
				snap.Sequence++
				out = snap.Items[i]
				return true, nil
			}
			out = snap.Items[i]
			return false, nil
		}
		return false, ErrNotificationNotFound
	})
	return out, err
}

// MarkReadWhere marks every unread item accepted by match as read in one
// commit and returns how many changed. It is the bulk form of MarkRead used
// by owner acks ("ack all") and by job dismissal/retry, which resolve every
// notification for that job at once.
func (s *NotificationInboxStore) MarkReadWhere(ctx context.Context, match func(NotificationItemView) bool) (int, error) {
	if s == nil {
		return 0, errors.New("notification store is not configured")
	}
	if match == nil {
		return 0, errors.New("notification match is required")
	}
	changed := 0
	err := s.withLock(ctx, func(snap *notificationSnapshot) (bool, error) {
		now := s.clock().UTC()
		for i := range snap.Items {
			if snap.Items[i].ReadAt != nil || !match(notificationItemView(snap.Items[i])) {
				continue
			}
			at := now
			if at.Before(snap.Items[i].CreatedAt) {
				at = snap.Items[i].CreatedAt
			}
			snap.Items[i].ReadAt = &at
			changed++
		}
		if changed == 0 {
			return false, nil
		}
		snap.Sequence++
		return true, nil
	})
	return changed, err
}

// MarkAllRead marks every unread notification read.
func (s *NotificationInboxStore) MarkAllRead(ctx context.Context) (int, error) {
	return s.MarkReadWhere(ctx, func(NotificationItemView) bool { return true })
}

// MarkJobRead marks every unread notification for one job read.
func (s *NotificationInboxStore) MarkJobRead(ctx context.Context, jobID string) (int, error) {
	if err := validateSafeID("job id", jobID, 96); err != nil {
		return 0, err
	}
	return s.MarkReadWhere(ctx, func(item NotificationItemView) bool { return item.JobID == jobID })
}

// NotificationErrorFor returns the bounded private error recorded with one
// notification. The HTTP projection omits it; local owner surfaces (the CLI
// and TUI) show it because the owner needs the reason a job paused.
func (s *NotificationInboxStore) NotificationErrorFor(ctx context.Context, id string) (string, error) {
	if err := validateSafeID("notification id", id, 96); err != nil {
		return "", err
	}
	found := ""
	err := s.withLock(ctx, func(snap *notificationSnapshot) (bool, error) {
		for _, item := range snap.Items {
			if item.ID == id {
				found = item.Error
				return false, nil
			}
		}
		return false, ErrNotificationNotFound
	})
	return found, err
}

type JobNotificationNotifier struct{ Store *NotificationInboxStore }

func NewJobNotificationNotifier(store *NotificationInboxStore) JobNotifier {
	return JobNotificationNotifier{Store: store}
}

func (notifier JobNotificationNotifier) Notify(ctx context.Context, result JobResult) error {
	if notifier.Store == nil {
		return errors.New("notification store is not configured")
	}
	_, err := notifier.Store.EnqueueJobResult(ctx, result)
	return err
}

func minNotificationInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
