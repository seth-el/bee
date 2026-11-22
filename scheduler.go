package bee

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type JobState string

type ApprovalState string

const (
	ApprovalRequired ApprovalState = "required"
	ApprovalGranted  ApprovalState = "approved"
)

const (
	JobPending                      JobState = "pending"
	JobApproved                     JobState = "approved"
	JobRunning                      JobState = "running"
	JobSucceeded                    JobState = "succeeded"
	JobFailed                       JobState = "failed"
	JobPaused                       JobState = "paused"
	JobCanceled                     JobState = "canceled"
	JobRecoverable                  JobState = "recoverable"
	NotificationIntentPending                = "pending"
	NotificationIntentDelivering             = "delivering"
	NotificationIntentDelivered              = "delivered"
	NotificationIntentFailed                 = "failed"
	NotificationMaxDeliveryAttempts          = 3
	NotificationDeliveryLease                = 5 * time.Minute
)

type Schedule struct {
	RunAt time.Time `json:"run_at"`
}
type JobRequest struct {
	Prompt      string    `json:"prompt"`
	SessionID   SessionID `json:"session_id,omitempty"`
	Title       string    `json:"title,omitempty"`
	ProjectRoot string    `json:"project_root,omitempty"`
}
type JobAttempt struct {
	ID         string     `json:"id"`
	JobID      string     `json:"job_id"`
	Number     int        `json:"number"`
	State      JobState   `json:"state"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	LeaseUntil time.Time  `json:"lease_until,omitempty"`
	SessionID  SessionID  `json:"session_id,omitempty"`
	Error      string     `json:"error,omitempty"`
	Summary    string     `json:"summary,omitempty"`
}
type Job struct {
	ID                 string                 `json:"id"`
	Name               string                 `json:"name"`
	Owner              string                 `json:"owner"`
	IdempotencyKey     string                 `json:"idempotency_key"`
	Schedule           Schedule               `json:"schedule"`
	Request            JobRequest             `json:"request"`
	State              JobState               `json:"state"`
	Approval           ApprovalState          `json:"approval"`
	Attempts           int                    `json:"attempts"`
	MaxAttempts        int                    `json:"max_attempts"`
	LastError          string                 `json:"last_error,omitempty"`
	CreatedAt          time.Time              `json:"created_at"`
	UpdatedAt          time.Time              `json:"updated_at"`
	FinishedAt         *time.Time             `json:"finished_at,omitempty"`
	AttemptsLog        []JobAttempt           `json:"attempts_log,omitempty"`
	Notifications      []NotificationAttempt  `json:"notifications,omitempty"`
	NotificationIntent *JobNotificationIntent `json:"notification_intent,omitempty"`
	RecurringID        string                 `json:"recurring_id,omitempty"`
	Occurrence         time.Time              `json:"occurrence,omitempty"`
	Contract           *JobContract           `json:"contract,omitempty"`
	OwnerActions       []JobOwnerAction       `json:"owner_actions,omitempty"`
}

type JobResult struct {
	JobID      string    `json:"job_id"`
	AttemptID  string    `json:"attempt_id"`
	State      JobState  `json:"state"`
	SessionID  SessionID `json:"session_id,omitempty"`
	Summary    string    `json:"summary,omitempty"`
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}
type JobNotifier interface {
	Notify(context.Context, JobResult) error
}
type NotificationAttempt struct {
	ID         string     `json:"id"`
	JobID      string     `json:"job_id"`
	AttemptID  string     `json:"attempt_id"`
	State      string     `json:"state"`
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}
type JobNotificationIntent struct {
	ID              string    `json:"id"`
	DeliveryKey     string    `json:"delivery_key"`
	State           string    `json:"state"`
	Attempts        int       `json:"attempts"`
	ActiveAttemptID string    `json:"active_attempt_id,omitempty"`
	LeaseUntil      time.Time `json:"lease_until,omitempty"`
	LastError       string    `json:"last_error,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}
type JobExecutor func(context.Context, Job) error

// JobExecution records only bounded scheduling metadata. Model output remains
// in the durable session journal and never enters the scheduler snapshot.
type JobExecution struct {
	SessionID SessionID
	Summary   string
}

type JobDetailedExecutor func(context.Context, Job) (JobExecution, error)

var ErrJobApprovalRequired = errors.New("job paused for owner approval")

type JobStore struct {
	path        string
	mu          sync.Mutex
	lock        *SchedulerLock
	retryPolicy RetryPolicy
}

func (store *JobStore) Path() string {
	if store == nil {
		return ""
	}
	return store.path
}

func NewJobStore(path string) (*JobStore, error) {
	if strings.TrimSpace(path) == "" || filepath.Base(path) == "." || filepath.Base(path) == ".." {
		return nil, errors.New("job store path is invalid")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, errors.New("job store path is invalid")
	}
	return &JobStore{path: filepath.Clean(abs), retryPolicy: RetryPolicy{BaseDelay: 100 * time.Millisecond, MaxDelay: 2 * time.Second, Retryable: func(error) bool { return true }}}, nil
}

func (store *JobStore) SetRetryPolicy(policy RetryPolicy) error {
	if policy.BaseDelay < 0 || policy.MaxDelay < 0 {
		return errors.New("retry delays must not be negative")
	}
	store.mu.Lock()
	store.retryPolicy = policy
	store.mu.Unlock()
	return nil
}

func (store *JobStore) retryPolicySnapshot() RetryPolicy {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.retryPolicy
}

func (store *JobStore) acquire() (*SchedulerLock, error) {
	if store == nil {
		return nil, errors.New("job store is nil")
	}
	return acquireSchedulerLock(store.path + ".lock")
}

func (store *JobStore) Recover(id, action string, now time.Time) error {
	lock, err := store.acquire()
	if err != nil {
		return err
	}
	defer lock.Close()
	jobs, err := store.loadLocked()
	if err != nil {
		return err
	}
	for i := range jobs {
		job := &jobs[i]
		if job.ID != id {
			continue
		}
		if job.State != JobRunning || len(job.AttemptsLog) == 0 || job.AttemptsLog[len(job.AttemptsLog)-1].LeaseUntil.After(now) {
			return errors.New("job is not stale and recoverable")
		}
		switch action {
		case "retry":
			job.State = JobApproved
			job.LastError = "stale running attempt recovered for explicit retry"
		case "fail":
			job.State = JobFailed
			job.LastError = "stale running attempt explicitly abandoned"
		default:
			return errors.New("recovery action must be retry or fail")
		}
		job.UpdatedAt = now.UTC()
		return store.saveLocked(jobs)
	}
	return os.ErrNotExist
}
func (store *JobStore) List() ([]Job, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.loadLocked()
}
func (store *JobStore) Get(id string) (Job, error) {
	jobs, err := store.List()
	if err != nil {
		return Job{}, err
	}
	for _, job := range jobs {
		if job.ID == id {
			return job, nil
		}
	}
	return Job{}, os.ErrNotExist
}
func (store *JobStore) saveLocked(jobs []Job) error {
	if err := os.MkdirAll(filepath.Dir(store.path), 0o700); err != nil {
		return errors.New("create job store directory")
	}
	data, err := json.MarshalIndent(jobs, "", "  ")
	if err != nil {
		return errors.New("encode job store")
	}
	tmp, err := os.CreateTemp(filepath.Dir(store.path), ".jobs-*")
	if err != nil {
		return errors.New("create job store temp")
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return errors.New("set job store permissions")
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return errors.New("write job store")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return errors.New("sync job store")
	}
	if err := tmp.Close(); err != nil {
		return errors.New("close job store")
	}
	if err := os.Rename(name, store.path); err != nil {
		return errors.New("commit job store")
	}
	return nil
}
func (store *JobStore) loadLocked() ([]Job, error) {
	data, err := os.ReadFile(store.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("read job store")
	}
	var jobs []Job
	if err := json.Unmarshal(data, &jobs); err != nil {
		return nil, errors.New("decode job store")
	}
	return jobs, nil
}

func (store *JobStore) Create(job Job, now time.Time) error {
	lock, err := store.acquire()
	if err != nil {
		return err
	}
	defer lock.Close()
	if store == nil {
		return errors.New("job store is nil")
	}
	if job.ID == "" || job.Name == "" || job.Owner == "" || job.IdempotencyKey == "" || strings.TrimSpace(job.Request.Prompt) == "" {
		return errors.New("job id, name, owner, and idempotency key are required")
	}
	if job.Schedule.RunAt.IsZero() {
		return errors.New("job run_at is required")
	}
	if job.MaxAttempts <= 0 {
		job.MaxAttempts = 1
	}
	job.State = JobPending
	job.Approval = ApprovalRequired
	job.CreatedAt = now.UTC()
	job.UpdatedAt = job.CreatedAt
	store.mu.Lock()
	defer store.mu.Unlock()
	jobs, err := store.loadLocked()
	if err != nil {
		return err
	}
	for _, existing := range jobs {
		if existing.ID == job.ID || existing.IdempotencyKey == job.IdempotencyKey {
			return errors.New("job idempotency key already exists")
		}
	}
	return store.saveLocked(append(jobs, job))
}
func (store *JobStore) Approve(id string, now time.Time) error {
	return store.transition(id, JobApproved, now, func(job *Job) error {
		if job.State != JobPending {
			return errors.New("job is not pending")
		}
		job.Approval = ApprovalGranted
		return nil
	})
}
func (store *JobStore) Cancel(id string, now time.Time) error {
	return store.transition(id, JobCanceled, now, func(job *Job) error {
		if job.State == JobSucceeded || job.State == JobCanceled {
			return errors.New("job is already terminal")
		}
		return nil
	})
}
func (store *JobStore) transition(id string, state JobState, now time.Time, check func(*Job) error) error {
	lock, err := store.acquire()
	if err != nil {
		return err
	}
	defer lock.Close()
	store.mu.Lock()
	defer store.mu.Unlock()
	jobs, err := store.loadLocked()
	if err != nil {
		return err
	}
	for i := range jobs {
		if jobs[i].ID == id {
			if err := check(&jobs[i]); err != nil {
				return err
			}
			jobs[i].State = state
			jobs[i].UpdatedAt = now.UTC()
			return store.saveLocked(jobs)
		}
	}
	return os.ErrNotExist
}

func (store *JobStore) RunDueWithNotifier(ctx context.Context, now time.Time, execute JobExecutor, notifier JobNotifier) ([]Job, error) {
	if execute == nil {
		return nil, errors.New("scheduler executor is required")
	}
	return store.RunDueDetailedWithNotifier(ctx, now, func(ctx context.Context, job Job) (JobExecution, error) {
		return JobExecution{}, execute(ctx, job)
	}, notifier)
}

func (store *JobStore) RunDueDetailedWithNotifier(ctx context.Context, now time.Time, execute JobDetailedExecutor, notifier JobNotifier) ([]Job, error) {
	results, err := store.runDueDetailed(ctx, now, execute)
	if err != nil || notifier == nil {
		return results, err
	}
	if err := store.deliverPendingNotificationIntents(ctx, time.Now().UTC(), notifier); err != nil {
		return results, err
	}
	return results, nil
}

func (store *JobStore) RunDue(ctx context.Context, now time.Time, execute JobExecutor) ([]Job, error) {
	if execute == nil {
		return nil, errors.New("scheduler executor is required")
	}
	return store.runDueDetailed(ctx, now, func(ctx context.Context, job Job) (JobExecution, error) {
		return JobExecution{}, execute(ctx, job)
	})
}

func (store *JobStore) runDueDetailed(ctx context.Context, now time.Time, execute JobDetailedExecutor) ([]Job, error) {
	if ctx == nil || execute == nil {
		return nil, errors.New("scheduler context and executor are required")
	}
	jobs, err := store.List()
	if err != nil {
		return nil, err
	}
	results := make([]Job, 0)
	for _, candidate := range jobs {
		if candidate.State != JobApproved || candidate.Schedule.RunAt.After(now) || candidate.Attempts >= candidate.MaxAttempts {
			continue
		}
		if err := ctx.Err(); err != nil {
			return results, err
		}
		job, err := store.claim(candidate.ID, now)
		if err != nil {
			if errors.Is(err, errJobChanged) {
				continue
			}
			return results, err
		}
		job, execution, execErr := executeDetailedWithRetries(ctx, store, job, execute)
		finished := time.Now().UTC()
		if execErr == nil {
			job.State = JobSucceeded
			job.LastError = ""
			job.FinishedAt = &finished
		} else if errors.Is(execErr, ErrJobApprovalRequired) {
			job.State = JobPaused
			job.LastError = boundedJobError(execErr)
			job.FinishedAt = &finished
		} else if ctx.Err() != nil {
			job.State = JobCanceled
			job.LastError = boundedJobError(ctx.Err())
			job.FinishedAt = &finished
		} else {
			job.State = JobFailed
			job.LastError = boundedJobError(execErr)
			job.FinishedAt = &finished
		}
		if len(job.AttemptsLog) > 0 {
			last := &job.AttemptsLog[len(job.AttemptsLog)-1]
			last.State = job.State
			last.FinishedAt = &finished
			last.Error = job.LastError
			last.SessionID = execution.SessionID
			last.Summary = boundedJobText(execution.Summary, NotificationMaxTitleBytes)
		}
		job.UpdatedAt = finished
		job, err = store.finalize(job, finished)
		if err != nil {
			return results, err
		}
		results = append(results, job)
	}
	return results, nil
}

func newJobNotificationIntent(job Job, at time.Time) *JobNotificationIntent {
	attemptID := ""
	if len(job.AttemptsLog) > 0 {
		attemptID = job.AttemptsLog[len(job.AttemptsLog)-1].ID
	}
	key := notificationDeliveryKey(JobResult{JobID: job.ID, AttemptID: attemptID, State: job.State})
	return &JobNotificationIntent{ID: notificationID(key), DeliveryKey: key, State: NotificationIntentPending, CreatedAt: at.UTC(), UpdatedAt: at.UTC()}
}

func (store *JobStore) deliverPendingNotificationIntents(ctx context.Context, now time.Time, notifier JobNotifier) error {
	jobs, err := store.List()
	if err != nil {
		return err
	}
	for _, candidate := range jobs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if candidate.NotificationIntent == nil || (candidate.State != JobSucceeded && candidate.State != JobFailed && candidate.State != JobPaused) {
			continue
		}
		if candidate.NotificationIntent.State == NotificationIntentDelivered {
			continue
		}
		if candidate.NotificationIntent.Attempts >= NotificationMaxDeliveryAttempts && !(candidate.NotificationIntent.State == NotificationIntentDelivering && !candidate.NotificationIntent.LeaseUntil.After(now)) {
			continue
		}
		job, attempt, result, ok, err := store.beginNotificationDelivery(candidate.ID, now)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		notifyErr := notifier.Notify(ctx, result)
		if err := store.finishNotificationDelivery(job.ID, attempt.ID, notifyErr, now); err != nil {
			return err
		}
	}
	return nil
}

func (store *JobStore) beginNotificationDelivery(id string, now time.Time) (Job, NotificationAttempt, JobResult, bool, error) {
	lock, err := store.acquire()
	if err != nil {
		return Job{}, NotificationAttempt{}, JobResult{}, false, err
	}
	defer lock.Close()
	store.mu.Lock()
	defer store.mu.Unlock()
	jobs, err := store.loadLocked()
	if err != nil {
		return Job{}, NotificationAttempt{}, JobResult{}, false, err
	}
	for i := range jobs {
		job := &jobs[i]
		if job.ID != id || job.NotificationIntent == nil || job.NotificationIntent.State == NotificationIntentDelivered {
			continue
		}
		intent := job.NotificationIntent
		if intent.State == NotificationIntentDelivering {
			if intent.LeaseUntil.After(now) {
				return Job{}, NotificationAttempt{}, JobResult{}, false, nil
			}
			for index := range job.Notifications {
				if job.Notifications[index].ID != intent.ActiveAttemptID || job.Notifications[index].State != "sending" {
					continue
				}
				finished := now.UTC()
				if finished.Before(job.Notifications[index].StartedAt) {
					finished = job.Notifications[index].StartedAt
				}
				job.Notifications[index].State = "failed"
				job.Notifications[index].Error = "notification delivery interrupted"
				job.Notifications[index].FinishedAt = &finished
				break
			}
			intent.State = NotificationIntentFailed
			intent.LastError = "notification delivery interrupted"
			intent.ActiveAttemptID = ""
			intent.LeaseUntil = time.Time{}
			intent.UpdatedAt = now.UTC()
		}
		if intent.Attempts >= NotificationMaxDeliveryAttempts {
			if err := store.saveLocked(jobs); err != nil {
				return Job{}, NotificationAttempt{}, JobResult{}, false, err
			}
			return Job{}, NotificationAttempt{}, JobResult{}, false, nil
		}
		attemptID := ""
		startedAt := job.CreatedAt
		if len(job.AttemptsLog) > 0 {
			attemptID = job.AttemptsLog[len(job.AttemptsLog)-1].ID
			startedAt = job.AttemptsLog[len(job.AttemptsLog)-1].StartedAt
		}
		intent.Attempts++
		intent.State = NotificationIntentDelivering
		intent.UpdatedAt = now.UTC()
		attempt := NotificationAttempt{ID: fmt.Sprintf("%s-notify-%d", job.ID, intent.Attempts), JobID: job.ID, AttemptID: attemptID, State: "sending", StartedAt: now.UTC()}
		intent.ActiveAttemptID = attempt.ID
		intent.LeaseUntil = now.UTC().Add(NotificationDeliveryLease)
		job.Notifications = append(job.Notifications, attempt)
		if err := store.saveLocked(jobs); err != nil {
			return Job{}, NotificationAttempt{}, JobResult{}, false, err
		}
		return *job, attempt, JobResult{JobID: job.ID, AttemptID: attemptID, State: job.State, Summary: job.Request.Title, Error: job.LastError, StartedAt: startedAt, FinishedAt: job.UpdatedAt}, true, nil
	}
	return Job{}, NotificationAttempt{}, JobResult{}, false, nil
}

func (store *JobStore) finishNotificationDelivery(id, attemptID string, notifyErr error, now time.Time) error {
	lock, err := store.acquire()
	if err != nil {
		return err
	}
	defer lock.Close()
	store.mu.Lock()
	defer store.mu.Unlock()
	jobs, err := store.loadLocked()
	if err != nil {
		return err
	}
	for i := range jobs {
		job := &jobs[i]
		if job.ID != id || job.NotificationIntent == nil {
			continue
		}
		for j := range job.Notifications {
			if job.Notifications[j].ID != attemptID {
				continue
			}
			finished := now.UTC()
			if finished.Before(job.Notifications[j].StartedAt) {
				finished = job.Notifications[j].StartedAt
			}
			job.Notifications[j].FinishedAt = &finished
			if notifyErr != nil {
				job.Notifications[j].State = "failed"
				job.Notifications[j].Error = boundedJobError(notifyErr)
				if job.NotificationIntent.State != NotificationIntentDelivered && job.NotificationIntent.ActiveAttemptID == attemptID {
					job.NotificationIntent.LastError = boundedJobError(notifyErr)
					job.NotificationIntent.State = NotificationIntentFailed
					job.NotificationIntent.ActiveAttemptID = ""
					job.NotificationIntent.LeaseUntil = time.Time{}
				}
			} else {
				job.Notifications[j].State = "sent"
				job.NotificationIntent.LastError = ""
				job.NotificationIntent.State = NotificationIntentDelivered
				job.NotificationIntent.ActiveAttemptID = ""
				job.NotificationIntent.LeaseUntil = time.Time{}
			}
			job.NotificationIntent.UpdatedAt = finished
			return store.saveLocked(jobs)
		}
	}
	return os.ErrNotExist
}

// Attempt leases. A running attempt whose lease has passed is stale and may
// be recovered explicitly (bee jobs recover). A recurring job's run is cut
// off at its contract's wall-clock budget, so its lease is that budget plus
// JobLeaseGrace, which covers composing the run before the budget clock
// starts and finalizing it afterwards. One-shot jobs have no budget and keep
// DefaultJobLease. No lease exceeds MaxJobLease.
const (
	DefaultJobLease = 15 * time.Minute
	JobLeaseGrace   = 5 * time.Minute
	MaxJobLease     = 24*time.Hour + JobLeaseGrace
)

// jobAttemptLease is how long one attempt of job may run before it counts
// as stale.
func jobAttemptLease(job Job) time.Duration {
	if job.Contract == nil || job.Contract.MaxWallClockSeconds <= 0 {
		return DefaultJobLease
	}
	lease := time.Duration(job.Contract.MaxWallClockSeconds)*time.Second + JobLeaseGrace
	if lease > MaxJobLease {
		return MaxJobLease
	}
	return lease
}

var errJobChanged = errors.New("job changed before claim")
var errJobCanceled = errors.New("job canceled by operator")

func (store *JobStore) claim(id string, now time.Time) (Job, error) {
	lock, err := store.acquire()
	if err != nil {
		return Job{}, err
	}
	defer lock.Close()
	store.mu.Lock()
	defer store.mu.Unlock()
	jobs, err := store.loadLocked()
	if err != nil {
		return Job{}, err
	}
	for i := range jobs {
		job := &jobs[i]
		if job.ID != id {
			continue
		}
		if job.State != JobApproved {
			return Job{}, errJobChanged
		}
		if job.RecurringID != "" {
			for _, other := range jobs {
				if other.ID != job.ID && other.RecurringID == job.RecurringID && other.State == JobRunning {
					return Job{}, errJobChanged
				}
			}
		}
		job.State = JobRunning
		job.Attempts++
		started := time.Now().UTC()
		job.AttemptsLog = append(job.AttemptsLog, JobAttempt{ID: fmt.Sprintf("%s-%d", job.ID, job.Attempts), JobID: job.ID, Number: job.Attempts, State: JobRunning, StartedAt: started, LeaseUntil: started.Add(jobAttemptLease(*job))})
		job.UpdatedAt = started
		if err := store.saveLocked(jobs); err != nil {
			return Job{}, err
		}
		return *job, nil
	}
	return Job{}, os.ErrNotExist
}

func executeDetailedWithRetries(ctx context.Context, store *JobStore, job Job, execute JobDetailedExecutor) (Job, JobExecution, error) {
	policy := store.retryPolicySnapshot()
	for {
		execution, err := execute(ctx, job)
		if err == nil || ctx.Err() != nil || job.Attempts >= job.MaxAttempts || policy.Retryable == nil || !policy.Retryable(err) {
			return job, execution, err
		}
		var retryErr error
		job, retryErr = store.prepareRetry(job, execution, err)
		if retryErr != nil {
			if errors.Is(retryErr, errJobCanceled) {
				return job, execution, context.Canceled
			}
			return job, execution, retryErr
		}
		delay := retryDelay(policy, job.Attempts)
		sleep := policy.Sleep
		if sleep == nil {
			sleep = retrySleep
		}
		if err := sleep(ctx, delay); err != nil {
			return job, execution, err
		}
		claimed, claimErr := store.claim(job.ID, time.Now().UTC())
		if claimErr != nil {
			return job, execution, claimErr
		}
		job = claimed
	}
}

func retryDelay(policy RetryPolicy, attempt int) time.Duration {
	if attempt <= 0 {
		return 0
	}
	delay := policy.BaseDelay
	if delay < 0 {
		delay = 0
	}
	for i := 1; i < attempt && delay < policy.MaxDelay; i++ {
		if delay == 0 {
			break
		}
		delay *= 2
	}
	if policy.MaxDelay > 0 && delay > policy.MaxDelay {
		return policy.MaxDelay
	}
	return delay
}
func (store *JobStore) prepareRetry(job Job, execution JobExecution, execErr error) (Job, error) {
	lock, err := store.acquire()
	if err != nil {
		return job, err
	}
	defer lock.Close()
	store.mu.Lock()
	defer store.mu.Unlock()
	jobs, err := store.loadLocked()
	if err != nil {
		return job, err
	}
	for i := range jobs {
		current := &jobs[i]
		if current.ID != job.ID {
			continue
		}
		if current.State == JobCanceled {
			return *current, errJobCanceled
		}
		if current.State != JobRunning {
			return *current, errJobChanged
		}
		current.LastError = boundedJobError(execErr)
		current.State = JobApproved
		current.UpdatedAt = time.Now().UTC()
		if len(current.AttemptsLog) > 0 {
			last := &current.AttemptsLog[len(current.AttemptsLog)-1]
			last.State = JobFailed
			last.FinishedAt = &current.UpdatedAt
			last.Error = current.LastError
			last.SessionID = execution.SessionID
			last.Summary = boundedJobText(execution.Summary, NotificationMaxTitleBytes)
		}
		if err := store.saveLocked(jobs); err != nil {
			return *current, err
		}
		return *current, nil
	}
	return job, os.ErrNotExist
}

func (store *JobStore) replace(updated Job) error {
	lock, err := store.acquire()
	if err != nil {
		return err
	}
	defer lock.Close()
	store.mu.Lock()
	defer store.mu.Unlock()
	jobs, err := store.loadLocked()
	if err != nil {
		return err
	}
	for i := range jobs {
		if jobs[i].ID == updated.ID {
			jobs[i] = updated
			return store.saveLocked(jobs)
		}
	}
	return os.ErrNotExist
}

func (store *JobStore) finalize(updated Job, finished time.Time) (Job, error) {
	lock, err := store.acquire()
	if err != nil {
		return Job{}, err
	}
	defer lock.Close()
	store.mu.Lock()
	defer store.mu.Unlock()
	jobs, err := store.loadLocked()
	if err != nil {
		return Job{}, err
	}
	for i := range jobs {
		if jobs[i].ID != updated.ID {
			continue
		}
		if jobs[i].State == JobCanceled {
			updated.State = JobCanceled
			updated.LastError = "job canceled by operator"
		}
		if updated.State == JobSucceeded || updated.State == JobFailed || updated.State == JobPaused {
			updated.NotificationIntent = newJobNotificationIntent(updated, finished)
		} else {
			updated.NotificationIntent = nil
		}
		updated.UpdatedAt = finished.UTC()
		if len(updated.AttemptsLog) > 0 {
			last := &updated.AttemptsLog[len(updated.AttemptsLog)-1]
			last.State = updated.State
			last.FinishedAt = &updated.UpdatedAt
			last.Error = updated.LastError
		}
		jobs[i] = updated
		if err := store.saveLocked(jobs); err != nil {
			return Job{}, err
		}
		return updated, nil
	}
	return Job{}, os.ErrNotExist
}

func boundedJobError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > 1024 {
		s = s[:1024]
	}
	return fmt.Sprintf("%s", s)
}
