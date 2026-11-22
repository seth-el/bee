package bee

import (
	"context"
	"testing"
	"time"
)

func TestJobAttemptLeaseFollowsContractBudget(t *testing.T) {
	cases := []struct {
		name string
		job  Job
		want time.Duration
	}{
		{"one-shot keeps default", Job{}, DefaultJobLease},
		{"contract without budget", Job{Contract: &JobContract{}}, DefaultJobLease},
		{"short budget plus grace", Job{Contract: &JobContract{MaxWallClockSeconds: 30}}, 30*time.Second + JobLeaseGrace},
		{"long budget plus grace", Job{Contract: &JobContract{MaxWallClockSeconds: 3 * 3600}}, 3*time.Hour + JobLeaseGrace},
		{"largest valid budget", Job{Contract: &JobContract{MaxWallClockSeconds: 24 * 3600}}, MaxJobLease},
		{"over the bound is clamped", Job{Contract: &JobContract{MaxWallClockSeconds: 72 * 3600}}, MaxJobLease},
	}
	for _, tc := range cases {
		if got := jobAttemptLease(tc.job); got != tc.want {
			t.Errorf("%s: lease = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A recurring attempt still inside its budget (here an hour, well past the
// old fixed 15 minutes) is never stale; once budget plus grace has passed
// it is recoverable.
func TestRecurringAttemptLeaseCoversItsBudget(t *testing.T) {
	start := time.Date(2026, 3, 6, 5, 0, 0, 0, time.UTC)
	config, store := recurringFixture(t, start)
	config.Jobs[0].MaxCatchUp = 1
	config.Jobs[0].Contract.MaxWallClockSeconds = 3600
	scheduler, err := NewRecurringScheduler(config, store)
	if err != nil {
		t.Fatal(err)
	}
	checked := false
	_, err = scheduler.Tick(context.Background(), start, func(_ context.Context, job Job) (JobExecution, error) {
		current, err := store.Get(job.ID)
		if err != nil {
			t.Fatal(err)
		}
		attempt := current.AttemptsLog[len(current.AttemptsLog)-1]
		if got := attempt.LeaseUntil.Sub(attempt.StartedAt); got != time.Hour+JobLeaseGrace {
			t.Fatalf("lease = %v", got)
		}
		for _, at := range []time.Duration{16 * time.Minute, time.Hour, time.Hour + JobLeaseGrace - time.Second} {
			if err := store.Recover(job.ID, "fail", attempt.StartedAt.Add(at)); err == nil {
				t.Fatalf("attempt reported stale %v into a one-hour budget", at)
			}
		}
		if current, _ := store.Get(job.ID); current.State != JobRunning {
			t.Fatalf("state changed: %s", current.State)
		}
		checked = true
		return JobExecution{}, nil
	}, nil)
	if err != nil || !checked {
		t.Fatalf("tick: %v checked=%v", err, checked)
	}
}

func TestOneShotAttemptKeepsDefaultLease(t *testing.T) {
	store, err := NewJobStore(t.TempDir() + "/jobs.json")
	if err != nil {
		t.Fatal(err)
	}
	job := Job{ID: "one-shot", State: JobApproved, MaxAttempts: 1}
	if err := store.saveLocked([]Job{job}); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.claim("one-shot", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	attempt := claimed.AttemptsLog[0]
	if got := attempt.LeaseUntil.Sub(attempt.StartedAt); got != DefaultJobLease {
		t.Fatalf("one-shot lease = %v", got)
	}
	if err := store.Recover("one-shot", "fail", attempt.StartedAt.Add(DefaultJobLease+time.Second)); err != nil {
		t.Fatalf("expired one-shot lease not recoverable: %v", err)
	}
}
