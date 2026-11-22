package bee

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// Model server reachability (G10). Every model Bee talks to runs locally (on
// this machine or the LAN), so a dead or hung server should be obvious within
// a minute or two instead of a raw dial error after 30 s or a silent wait
// until the 10-minute turn timeout. The timings assume local servers: a
// healthy local server can still take a long time to read a large prompt
// before its first token, which is why silence alone never fails a call: it
// only triggers a health check.
const (
	// ProviderConnectTimeout bounds TCP connection setup (Go's default is 30 s).
	ProviderConnectTimeout = 15 * time.Second
	// ProviderStallProbeAfter is how long a call may produce nothing before
	// Bee checks the server's health.
	ProviderStallProbeAfter = 60 * time.Second
	// ProviderHealthTimeout bounds one health check.
	ProviderHealthTimeout = 10 * time.Second
	// ProviderHealthRetryAfter spaces consecutive failing health checks.
	ProviderHealthRetryAfter = 15 * time.Second
	// ProviderHealthFailures is how many health checks in a row must fail
	// before a silent call is abandoned as unreachable.
	ProviderHealthFailures = 2
)

// ErrorKindModelUnreachable marks run failures caused by an unreachable or
// unresponsive model server (RunEvent.ErrorKind).
const ErrorKindModelUnreachable = "model_unreachable"

// ModelUnreachableError says the model server could not be reached, or
// stopped responding and failed its health checks.
type ModelUnreachableError struct {
	// Endpoint is host:port of the model server.
	Endpoint string
	// Reason is short and human: "connection refused", "timed out
	// connecting", "stopped responding and failed 2 health checks".
	Reason string
	cause  error
}

func (err *ModelUnreachableError) Error() string {
	return fmt.Sprintf("can't reach the model server at %s (%s)", err.Endpoint, err.Reason)
}

func (err *ModelUnreachableError) Unwrap() error { return err.cause }

// AsModelUnreachable finds a ModelUnreachableError in err's chain.
func AsModelUnreachable(err error) (*ModelUnreachableError, bool) {
	var unreachable *ModelUnreachableError
	if errors.As(err, &unreachable) {
		return unreachable, true
	}
	return nil, false
}

// RunErrorKind classifies a run error for RunEvent.ErrorKind.
func RunErrorKind(err error) string {
	if _, ok := AsModelUnreachable(err); ok {
		return ErrorKindModelUnreachable
	}
	var failed *RunFailedError
	if errors.As(err, &failed) {
		return failed.Kind
	}
	return ""
}

// RunFailedError is a finished run's failure as seen by a caller that only
// has its events (channels, jobs): the classified kind and the human text.
type RunFailedError struct {
	Kind    string
	Message string
}

func (err *RunFailedError) Error() string { return err.Message }

func providerEndpoint(baseURL string) string {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" {
		return "the configured address"
	}
	return parsed.Host
}

// classifyDialError turns a connection-setup failure into a
// ModelUnreachableError, or returns nil when err is something else (for
// example the caller's own cancellation).
func classifyDialError(baseURL string, err error) *ModelUnreachableError {
	if err == nil || errors.Is(err, context.Canceled) {
		return nil
	}
	reason := ""
	var dnsErr *net.DNSError
	var opErr *net.OpError
	switch {
	case errors.As(err, &dnsErr):
		reason = "host name not found"
	case errors.Is(err, syscall.ECONNREFUSED):
		reason = "connection refused"
	case errors.Is(err, syscall.EHOSTUNREACH):
		reason = "host unreachable"
	case errors.Is(err, syscall.ENETUNREACH):
		reason = "network unreachable"
	case errors.As(err, &opErr) && opErr.Op == "dial" && opErr.Timeout():
		reason = "timed out connecting"
	case errors.As(err, &opErr) && opErr.Op == "dial":
		reason = "connection failed"
	default:
		return nil
	}
	return &ModelUnreachableError{Endpoint: providerEndpoint(baseURL), Reason: reason, cause: err}
}

// newProviderHTTPClient is the model client: no overall timeout (long
// generations), but a bounded connection setup.
func newProviderHTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: ProviderConnectTimeout, KeepAlive: 30 * time.Second}).DialContext
	return &http.Client{Timeout: timeout, Transport: transport}
}

// providerHealthURL is the server's /health, beside /v1.
func providerHealthURL(baseURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return ""
	}
	parsed.Path, parsed.RawQuery, parsed.Fragment = "/health", "", ""
	return parsed.String()
}

// ProbeModelServer checks that the server at baseURL answers HTTP at all:
// any response, even 404 or 503 (llama.cpp still loading), means the server
// process is alive. It never sends a completion.
func ProbeModelServer(ctx context.Context, client *http.Client, baseURL string, timeout time.Duration) error {
	target := providerHealthURL(baseURL)
	if target == "" {
		return &ModelUnreachableError{Endpoint: providerEndpoint(baseURL), Reason: "invalid address"}
	}
	if client == nil {
		client = newProviderHTTPClient(0)
	}
	if timeout <= 0 {
		timeout = ProviderHealthTimeout
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		if classified := classifyDialError(baseURL, err); classified != nil {
			return classified
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &ModelUnreachableError{Endpoint: providerEndpoint(baseURL), Reason: "health check timed out", cause: err}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	_ = response.Body.Close()
	return nil
}

// stallPolicy holds the silence/health-check timings (defaults above;
// tests shorten them).
type stallPolicy struct {
	probeAfter, retryAfter, healthTimeout, tick time.Duration
	failures                                    int
}

func (policy stallPolicy) effective() stallPolicy {
	if policy.probeAfter <= 0 {
		policy.probeAfter = ProviderStallProbeAfter
	}
	if policy.retryAfter <= 0 {
		policy.retryAfter = ProviderHealthRetryAfter
	}
	if policy.healthTimeout <= 0 {
		policy.healthTimeout = ProviderHealthTimeout
	}
	if policy.tick <= 0 {
		policy.tick = time.Second
	}
	if policy.failures <= 0 {
		policy.failures = ProviderHealthFailures
	}
	return policy
}

// watchStall returns a context for one model call that is cancelled with a
// ModelUnreachableError when the call has been silent past probeAfter and
// the server then fails consecutive health checks. touch records activity
// (response headers, every byte read); stop ends the watch.
func (provider *OpenAICompatibleProvider) watchStall(parent context.Context) (ctx context.Context, touch func(), stop func()) {
	policy := provider.stall.effective()
	ctx, cancel := context.WithCancelCause(parent)
	activity := make(chan struct{}, 1)
	touch = func() {
		select {
		case activity <- struct{}{}:
		default:
		}
	}
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(policy.tick)
		defer ticker.Stop()
		last := time.Now()
		nextProbe := last.Add(policy.probeAfter)
		failures := 0
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-activity:
				last = time.Now()
				nextProbe = last.Add(policy.probeAfter)
				failures = 0
			case now := <-ticker.C:
				if now.Before(nextProbe) {
					continue
				}
				if err := ProbeModelServer(ctx, provider.client, provider.baseURL, policy.healthTimeout); err != nil {
					if ctx.Err() != nil {
						return
					}
					failures++
					if failures >= policy.failures {
						cancel(&ModelUnreachableError{Endpoint: providerEndpoint(provider.baseURL), Reason: fmt.Sprintf("stopped responding for %s and failed %d health checks", time.Since(last).Round(time.Second), failures), cause: err})
						return
					}
					nextProbe = time.Now().Add(policy.retryAfter)
				} else {
					// Alive, just slow (a long prompt, say): keep waiting.
					failures = 0
					nextProbe = time.Now().Add(policy.probeAfter)
				}
			}
		}
	}()
	var stopped bool
	stop = func() {
		if !stopped {
			stopped = true
			close(done)
			cancel(nil)
		}
	}
	return ctx, touch, stop
}

// stallCause returns the watch's ModelUnreachableError when it cancelled
// the call, so the caller reports that instead of a bare cancellation.
func stallCause(ctx context.Context) *ModelUnreachableError {
	if unreachable, ok := AsModelUnreachable(context.Cause(ctx)); ok {
		return unreachable
	}
	return nil
}

// activityReader reports every successful read as activity.
type activityReader struct {
	io.ReadCloser
	touch func()
}

func (reader activityReader) Read(buffer []byte) (int, error) {
	n, err := reader.ReadCloser.Read(buffer)
	if n > 0 {
		reader.touch()
	}
	return n, err
}

// RunFailureText is the human line for a failed run: for an unreachable
// model server, the plain reason without transport prefixes.
func RunFailureText(event RunEvent) string {
	if event.ErrorKind != ErrorKindModelUnreachable {
		return event.Error
	}
	const marker = "can't reach the model server at "
	if index := strings.Index(event.Error, marker); index >= 0 {
		return "Model server unreachable: " + event.Error[index+len(marker):]
	}
	return "Model server unreachable: " + event.Error
}

// ProviderReachabilityTargets are the profiles Bee depends on: main, child
// and the main fallback, skipping manual (cloud) profiles, which are not
// probed. With no profiles, the legacy provider.
func ProviderReachabilityTargets(config Config) []ProviderProfile {
	if len(config.Profiles) == 0 {
		if strings.TrimSpace(config.Provider.BaseURL) == "" {
			return nil
		}
		return []ProviderProfile{{Name: "provider", Role: ProviderRoleMain, Provider: config.Provider}}
	}
	var targets []ProviderProfile
	seen := map[string]bool{}
	for _, name := range []string{config.Routing.MainProfile, config.Routing.ChildProfile, config.Routing.FallbackMainProfile} {
		if name == "" || seen[name] {
			continue
		}
		for _, profile := range config.Profiles {
			if profile.Name == name && profile.Role != ProviderRoleManual {
				seen[name] = true
				targets = append(targets, profile)
			}
		}
	}
	return targets
}

// addProviderReachabilityCheck is doctor's provider_reachability: one
// health probe per target, in parallel, no completion sent.
func addProviderReachabilityCheck(config Config, add func(string, CheckStatus, string)) {
	// BEE_DOCTOR_SKIP_PROVIDER_PROBE=1 keeps doctor offline (CI, tests).
	if os.Getenv("BEE_DOCTOR_SKIP_PROVIDER_PROBE") == "1" {
		return
	}
	targets := ProviderReachabilityTargets(config)
	if len(targets) == 0 {
		return
	}
	results := make([]error, len(targets))
	done := make(chan int, len(targets))
	for index, target := range targets {
		go func(index int, baseURL string) {
			results[index] = ProbeModelServer(context.Background(), nil, baseURL, ProviderHealthTimeout)
			done <- index
		}(index, target.Provider.BaseURL)
	}
	for range targets {
		<-done
	}
	var ok, failed []string
	for index, target := range targets {
		label := fmt.Sprintf("%s (%s)", target.Name, providerEndpoint(target.Provider.BaseURL))
		if results[index] != nil {
			failed = append(failed, label+": "+truncateUTF8(redactString(results[index].Error()), 160))
		} else {
			ok = append(ok, label)
		}
	}
	if len(failed) > 0 {
		detail := strings.Join(failed, "; ")
		if len(ok) > 0 {
			detail += "; reachable: " + strings.Join(ok, ", ")
		}
		add("provider_reachability", CheckWarn, detail)
		return
	}
	add("provider_reachability", CheckPass, "reachable: "+strings.Join(ok, ", "))
}
