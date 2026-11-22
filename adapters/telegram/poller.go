package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/seth-el/bee"
)

const (
	DefaultPollAttempts = 3
	DefaultPollDelay    = 250 * time.Millisecond
)

type PollerOptions struct {
	Sleep         SleepFunc
	MaxAttempts   int
	RetryDelay    time.Duration
	StateLockPath string
}

type Poller struct {
	Client        *Client
	Config        Config
	Checkpoint    *CheckpointStore
	Handler       bee.ChannelHandler
	UpdateHandler UpdateHandler
	// ShowTyping sends Telegram's "typing…" indicator while a reply runs.
	ShowTyping bool
	// Logf receives bounded operational lines (never message content).
	Logf          func(format string, args ...any)
	now           func() time.Time
	sleep         SleepFunc
	maxAttempts   int
	retryDelay    time.Duration
	stateLockPath string
}

func NewPoller(client *Client, config Config, checkpoint *CheckpointStore, handler bee.ChannelHandler) (*Poller, error) {
	return NewPollerWithOptions(client, config, checkpoint, handler, PollerOptions{})
}

func NewPollerWithOptions(client *Client, config Config, checkpoint *CheckpointStore, handler bee.ChannelHandler, options PollerOptions) (*Poller, error) {
	if client == nil || checkpoint == nil || handler == nil {
		return nil, errors.New("telegram poller client, checkpoint, and handler are required")
	}
	config = config.Normalized()
	if err := validateChannelConfig(config); err != nil {
		return nil, err
	}
	if options.Sleep == nil {
		options.Sleep = sleepContext
	}
	if options.MaxAttempts <= 0 || options.MaxAttempts > 5 {
		options.MaxAttempts = DefaultPollAttempts
	}
	if options.RetryDelay < 0 || options.RetryDelay > 10*time.Second {
		options.RetryDelay = DefaultPollDelay
	}
	lockPath := options.StateLockPath
	if lockPath == "" {
		lockPath = config.CheckpointPath + ".lock"
	}
	return &Poller{Client: client, Config: config, Checkpoint: checkpoint, Handler: handler, sleep: options.Sleep, maxAttempts: options.MaxAttempts, retryDelay: options.RetryDelay, stateLockPath: lockPath}, nil
}

// Run polls Telegram until ctx ends. Polling continues while a reply is
// being produced: ordinary updates queue for one worker that handles them in
// order, while /stop, /kill, /cancel and /steer are acted on immediately so
// they can interrupt the running reply. The durable checkpoint only advances
// past updates that have finished, so a crash replays unfinished ones.
func (p *Poller) Run(ctx context.Context) error {
	if p == nil || p.Client == nil || p.Checkpoint == nil || p.Handler == nil {
		return errors.New("telegram poller is not configured")
	}
	if ctx == nil {
		return errors.New("telegram poller context is nil")
	}
	lock, err := AcquireStateLock(p.stateLockPath)
	if err != nil {
		return err
	}
	defer lock.Close()
	if _, err := VerifyIdentity(ctx, p.Client, p.Config); err != nil {
		return err
	}
	offset, err := p.Checkpoint.Load(ctx)
	if err != nil {
		return err
	}
	runCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	tracker := newCheckpointTracker(p.Checkpoint, offset)
	queue := make(chan Update, MaxPollLimit)
	workerDone := make(chan struct{})
	active := &activeTurn{}
	go func() {
		defer close(workerDone)
		p.work(runCtx, stop, queue, tracker, active)
	}()
	finish := func(err error) error {
		stop(err)
		<-workerDone
		if cause := context.Cause(runCtx); cause != nil && !errors.Is(cause, context.Canceled) {
			return cause
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	fetchOffset := offset
	outage := &pollOutage{logf: p.Logf, now: p.now}
	for {
		if runCtx.Err() != nil {
			return finish(nil)
		}
		updates, err := p.getUpdatesWithRetry(runCtx, fetchOffset)
		if err != nil {
			if runCtx.Err() != nil {
				return finish(nil)
			}
			delay, retry := outage.failed(err)
			if !retry {
				return finish(fmt.Errorf("telegram poll: %w", err))
			}
			// Network trouble or a Telegram-side outage: keep the process,
			// and any reply in progress, alive and try again. Pooled
			// connections may be dead too, so the retry dials fresh.
			if p.Client.HTTPClient != nil {
				p.Client.HTTPClient.CloseIdleConnections()
			}
			if err := p.sleep(runCtx, delay); err != nil {
				return finish(nil)
			}
			continue
		}
		outage.recovered()
		for _, update := range updates {
			if update.UpdateID < fetchOffset {
				continue
			}
			tracker.dispatch(update.UpdateID)
			fetchOffset = update.UpdateID + 1
			queued, handled := p.interrupt(runCtx, update, active)
			if handled {
				if err := tracker.done(runCtx, update.UpdateID); err != nil {
					return finish(err)
				}
				continue
			}
			select {
			case queue <- queued:
			case <-runCtx.Done():
				return finish(nil)
			}
		}
	}
}

// work handles queued updates one at a time.
func (p *Poller) work(ctx context.Context, stop context.CancelCauseFunc, queue <-chan Update, tracker *checkpointTracker, active *activeTurn) {
	processor := &UpdateProcessor{Config: p.Config, Checkpoint: p.Checkpoint, Handler: p.Handler, UpdateHandler: p.UpdateHandler, Commit: tracker.done}
	for {
		var update Update
		select {
		case <-ctx.Done():
			return
		case update = <-queue:
		}
		turnCtx, cancelTurn := context.WithCancel(ctx)
		chatID := int64(0)
		if message := updateMessage(update); message != nil {
			chatID = message.Chat.ID
		}
		active.begin(chatID, cancelTurn)
		if p.ShowTyping && chatID != 0 {
			go p.typing(turnCtx, chatID)
		}
		_, err := processor.Process(turnCtx, update)
		interrupted := active.end()
		cancelTurn()
		if err != nil && interrupted && ctx.Err() == nil {
			// The owner stopped or steered this reply; the interrupt already
			// answered, so this update is finished.
			err = tracker.done(ctx, update.UpdateID)
		}
		if err != nil {
			if ctx.Err() == nil {
				stop(err)
			}
			return
		}
	}
}

// typing shows "typing…" in the chat until ctx ends. Best effort.
func (p *Poller) typing(ctx context.Context, chatID int64) {
	for {
		_ = p.Client.SendChatAction(ctx, chatID, "typing")
		select {
		case <-ctx.Done():
			return
		case <-time.After(4500 * time.Millisecond):
		}
	}
}

// interrupt acts on /stop, /kill, /cancel and /steer immediately. It returns
// handled=true when the update needs no further processing; otherwise the
// (possibly rewritten) update is queued.
func (p *Poller) interrupt(ctx context.Context, update Update, active *activeTurn) (Update, bool) {
	message := updateMessage(update)
	if message == nil || update.Message == nil {
		return update, false
	}
	name, args := interruptCommand(message.Text)
	if name == "" {
		return update, false
	}
	// NormalizeUpdate applies the chat and sender allowlists.
	if normalized, err := NormalizeUpdate(p.Config, update); err != nil || normalized.Ignored {
		return update, false
	}
	chatID := message.Chat.ID
	running := active.interrupt(chatID)
	reply := func(text string) {
		sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		_, _ = p.Client.SendMessage(sendCtx, SendMessageRequest{ChatID: chatID, Text: text, ReplyToMessageID: int64(message.MessageID), MessageThreadID: int64(message.MessageThreadID)})
	}
	if name == "steer" {
		if args == "" {
			reply("Usage: /steer <correction>")
			return update, true
		}
		if running {
			reply("Steering…")
		}
		copy := *update.Message
		copy.Text = args
		update.Message = &copy
		return update, false
	}
	if running {
		reply("Stopped.")
	} else {
		reply("Nothing is running.")
	}
	return update, true
}

// interruptCommand recognizes the commands that act on a running reply.
func interruptCommand(text string) (string, string) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "/") {
		return "", ""
	}
	name, args := trimmed, ""
	if index := strings.IndexAny(trimmed, " \t\r\n"); index >= 0 {
		name, args = trimmed[:index], strings.TrimSpace(trimmed[index:])
	}
	name = strings.ToLower(strings.TrimPrefix(name, "/"))
	if at := strings.IndexByte(name, '@'); at >= 0 {
		name = name[:at]
	}
	switch name {
	case "stop", "kill", "cancel", "steer":
		return name, args
	}
	return "", ""
}

// activeTurn tracks the update the worker is running so an interrupt can
// cancel it.
type activeTurn struct {
	mu          sync.Mutex
	chatID      int64
	cancel      context.CancelFunc
	interrupted bool
}

func (turn *activeTurn) begin(chatID int64, cancel context.CancelFunc) {
	turn.mu.Lock()
	defer turn.mu.Unlock()
	turn.chatID, turn.cancel, turn.interrupted = chatID, cancel, false
}

// end clears the turn and reports whether it was interrupted.
func (turn *activeTurn) end() bool {
	turn.mu.Lock()
	defer turn.mu.Unlock()
	interrupted := turn.interrupted
	turn.chatID, turn.cancel, turn.interrupted = 0, nil, false
	return interrupted
}

// interrupt cancels the running turn if it belongs to chatID.
func (turn *activeTurn) interrupt(chatID int64) bool {
	turn.mu.Lock()
	defer turn.mu.Unlock()
	if turn.cancel == nil || turn.chatID != chatID {
		return false
	}
	turn.interrupted = true
	turn.cancel()
	return true
}

// checkpointTracker advances the durable offset only past updates that have
// finished, in order.
type checkpointTracker struct {
	mu        sync.Mutex
	store     *CheckpointStore
	pending   map[int64]bool
	next      int64
	committed int64
}

func newCheckpointTracker(store *CheckpointStore, offset int64) *checkpointTracker {
	return &checkpointTracker{store: store, pending: map[int64]bool{}, next: offset, committed: offset}
}

func (tracker *checkpointTracker) dispatch(updateID int64) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.pending[updateID] = true
	if updateID+1 > tracker.next {
		tracker.next = updateID + 1
	}
}

func (tracker *checkpointTracker) done(ctx context.Context, updateID int64) error {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	delete(tracker.pending, updateID)
	offset := tracker.next
	for id := range tracker.pending {
		if id < offset {
			offset = id
		}
	}
	if offset <= tracker.committed {
		return nil
	}
	if err := tracker.store.Save(ctx, offset); err != nil {
		return err
	}
	tracker.committed = offset
	return nil
}

const (
	pollOutageBaseDelay = time.Second
	pollOutageMaxDelay  = 30 * time.Second
	// pollOutageWarnAfter is how long polling must keep failing before a
	// second, louder log line says so.
	pollOutageWarnAfter = 5 * time.Minute
)

// pollOutage tracks a run of failed getUpdates calls. Transport failures and
// temporary Telegram errors are retried without limit, with a capped backoff;
// anything else (a revoked token, a bad request) still ends the run.
type pollOutage struct {
	logf     func(format string, args ...any)
	now      func() time.Time
	since    time.Time
	failures int
	delay    time.Duration
	warned   bool
}

func pollFailureReason(err error) (string, bool) {
	var transport *TransportError
	if errors.As(err, &transport) {
		return transport.Reason, true
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Temporary() {
		if apiErr.HTTPStatus != 0 {
			return fmt.Sprintf("HTTP %d", apiErr.HTTPStatus), true
		}
		return fmt.Sprintf("API error %d", apiErr.Code), true
	}
	return "", false
}

func (o *pollOutage) clock() time.Time {
	if o.now != nil {
		return o.now()
	}
	return time.Now()
}

func (o *pollOutage) log(format string, args ...any) {
	if o.logf != nil {
		o.logf(format, args...)
	}
}

func (o *pollOutage) failed(err error) (time.Duration, bool) {
	reason, ok := pollFailureReason(err)
	if !ok {
		return 0, false
	}
	now := o.clock()
	if o.failures == 0 {
		o.since = now
		o.delay = pollOutageBaseDelay
	} else {
		o.delay = min(o.delay*2, pollOutageMaxDelay)
	}
	o.failures++
	delay := o.delay
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.RetryAfter > delay {
		delay = min(apiErr.RetryAfter, 5*time.Minute)
	}
	switch {
	case o.failures == 1:
		o.log("telegram poll failed (%s); retrying in %s", reason, delay)
	case !o.warned && now.Sub(o.since) >= pollOutageWarnAfter:
		o.warned = true
		o.log("telegram polling has failed for %s (%d attempts, last: %s); still retrying", now.Sub(o.since).Round(time.Second), o.failures, reason)
	}
	return delay, true
}

func (o *pollOutage) recovered() {
	if o.failures == 0 {
		return
	}
	o.log("telegram polling recovered after %s (%d failed attempts)", o.clock().Sub(o.since).Round(time.Second), o.failures)
	*o = pollOutage{logf: o.logf, now: o.now}
}

func (p *Poller) getUpdatesWithRetry(ctx context.Context, offset int64) ([]Update, error) {
	var updates []Update
	var last error
	policy := bee.RetryPolicy{MaxAttempts: p.maxAttempts, BaseDelay: p.retryDelay, MaxDelay: 10 * time.Second, Sleep: bee.RetrySleep(p.sleep), Retryable: retryableTelegram}
	err := policy.Run(ctx, func(ctx context.Context) error {
		var err error
		updates, err = p.Client.GetUpdates(ctx, offset, p.Config.PollTimeoutSeconds, p.Config.PollLimit)
		last = err
		return err
	})
	if err != nil {
		return nil, err
	}
	if last != nil {
		return nil, last
	}
	return updates, nil
}

func VerifyIdentity(ctx context.Context, client *Client, config Config) (User, error) {
	if client == nil {
		return User{}, errors.New("telegram identity client is not configured")
	}
	config = config.Normalized()
	if err := validateChannelConfig(config); err != nil {
		return User{}, err
	}
	user, err := client.GetMe(ctx)
	if err != nil {
		return User{}, fmt.Errorf("telegram bot identity check: %w", err)
	}
	if !user.IsBot || user.ID != config.ExpectedBotID || user.Username != config.ExpectedBotUsername {
		return User{}, errors.New("telegram bot identity does not match configured test bot")
	}
	return user, nil
}
