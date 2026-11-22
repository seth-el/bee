package bee

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	// maxJobOwnerActions bounds the owner-action history kept on one job, and
	// so bounds how many times one occurrence can be retried.
	maxJobOwnerActions      = 8
	maxJobOwnerReasonBytes  = 256
	JobOwnerActionRetry     = "retry"
	JobOwnerActionDismiss   = "dismiss"
	jobRefreshedFromConfig  = "prompt and contract refreshed from config"
	jobRetryKeptPersisted   = "persisted prompt and contract"
	errJobRecoveryWrongKind = "only recurring job occurrences can be retried or dismissed"
)

// JobOwnerAction records an explicit owner decision about one job (a retry or
// a dismissal) so the job's history shows why it ran again or stopped.
type JobOwnerAction struct {
	Action string    `json:"action"`
	At     time.Time `json:"at"`
	Detail string    `json:"detail,omitempty"`
}

// JobRetryOptions controls RetryRecurring.
type JobRetryOptions struct {
	// Refresh, when non-nil, replaces the occurrence's persisted prompt and
	// contract with this definition from the current operator config. It is
	// for the case where the owner fixed the prompt or contract after the
	// pause. The definition must be the occurrence's own recurring job.
	Refresh *RecurringJobConfig
}

// RetryRecurring returns a paused or failed recurring occurrence to the
// approved state so the next `run-due` runs it again, as the same occurrence.
// It never changes the contract unless the owner passes a refreshed
// definition from config, and the scheduler never calls it on its own.
//
// It refuses when a later occurrence of the same job already exists: rerunning
// an older one would run work out of order (a nightly job reading the clock
// would consolidate the wrong day). Dismiss the old one instead.
func (store *JobStore) RetryRecurring(id string, options JobRetryOptions, now time.Time) (Job, error) {
	var out Job
	err := store.mutateRecurring(id, func(jobs []Job, job *Job) error {
		if job.State != JobPaused && job.State != JobFailed {
			return fmt.Errorf("job is %s; only paused or failed occurrences can be retried", job.State)
		}
		for _, other := range jobs {
			if other.ID != job.ID && other.RecurringID == job.RecurringID && other.Occurrence.After(job.Occurrence) {
				return fmt.Errorf("a later occurrence (%s) already exists; retrying this one would run it out of order, so dismiss it instead", other.ID)
			}
		}
		if len(job.OwnerActions) >= maxJobOwnerActions {
			return errors.New("job has reached its owner-action limit; dismiss it instead")
		}
		detail := jobRetryKeptPersisted
		if options.Refresh != nil {
			definition := *options.Refresh
			if definition.ID != job.RecurringID {
				return errors.New("refreshed definition belongs to a different recurring job")
			}
			if err := definition.Validate(); err != nil {
				return fmt.Errorf("refreshed definition is invalid: %w", err)
			}
			contract := cloneJobContract(definition.Contract)
			job.Contract = &contract
			job.Request.Prompt = definition.Prompt
			job.Request.ProjectRoot = contract.WorkspacePaths[0]
			detail = jobRefreshedFromConfig
		}
		job.State = JobApproved
		job.Approval = ApprovalGranted
		job.MaxAttempts = job.Attempts + 1
		job.FinishedAt = nil
		job.LastError = ""
		job.OwnerActions = append(job.OwnerActions, JobOwnerAction{Action: JobOwnerActionRetry, At: now.UTC(), Detail: detail})
		job.UpdatedAt = now.UTC()
		out = *job
		return nil
	})
	return out, err
}

// DismissRecurring makes a paused or failed recurring occurrence terminal
// without running it again. Materialize never recreates a dismissed
// occurrence.
func (store *JobStore) DismissRecurring(id, reason string, now time.Time) (Job, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "dismissed by owner"
	}
	if len(reason) > maxJobOwnerReasonBytes || strings.ContainsAny(reason, "\x00\r\n") {
		return Job{}, fmt.Errorf("dismiss reason must be one line of at most %d bytes", maxJobOwnerReasonBytes)
	}
	var out Job
	err := store.mutateRecurring(id, func(_ []Job, job *Job) error {
		if job.State != JobPaused && job.State != JobFailed {
			return fmt.Errorf("job is %s; only paused or failed occurrences can be dismissed", job.State)
		}
		job.State = JobCanceled
		job.LastError = "dismissed by owner: " + reason
		if len(job.OwnerActions) < maxJobOwnerActions {
			job.OwnerActions = append(job.OwnerActions, JobOwnerAction{Action: JobOwnerActionDismiss, At: now.UTC(), Detail: reason})
		}
		job.UpdatedAt = now.UTC()
		out = *job
		return nil
	})
	return out, err
}

func (store *JobStore) mutateRecurring(id string, mutate func([]Job, *Job) error) error {
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
		if jobs[i].ID != id {
			continue
		}
		if jobs[i].RecurringID == "" || jobs[i].Contract == nil {
			return errors.New(errJobRecoveryWrongKind)
		}
		if err := mutate(jobs, &jobs[i]); err != nil {
			return err
		}
		return store.saveLocked(jobs)
	}
	return os.ErrNotExist
}
