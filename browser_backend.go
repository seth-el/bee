package bee

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"syscall"
)

// BrowserBackend is Bee's provider-neutral seam for a Camofox control service.
// It deliberately leaves browser policy, identity, target ownership, and
// action validation to Camofox.
type BrowserBackend interface {
	Tabs(context.Context) (BrowserTabsResult, error)
	Open(context.Context, BrowserOpenRequest) (BrowserOpenResult, error)
	Navigate(context.Context, BrowserNavigateRequest) (BrowserNavigateResult, error)
	Snapshot(context.Context, BrowserSnapshotRequest) (BrowserSnapshotResult, error)
	Act(context.Context, BrowserActRequest) (BrowserActResult, error)
}

type BrowserTabsResult struct {
	Running bool
	Count   int
}

type BrowserOpenRequest struct {
	URL        string
	UserID     string
	SessionKey string
}
type BrowserOpenResult struct {
	TargetID string
	URL      string
	Title    string
}
type BrowserNavigateRequest struct {
	TargetID string
	URL      string
	UserID   string
}
type BrowserNavigateResult struct {
	TargetID string
	URL      string
	Title    string
}
type BrowserSnapshotRequest struct {
	TargetID string
	Format   string
	Offset   int
	UserID   string
}
type BrowserSnapshotResult struct {
	TargetID   string
	URL        string
	Title      string
	Format     string
	Text       string
	Offset     int
	HasMore    bool
	NextOffset *int
	TotalChars int
	Truncated  bool
}
type BrowserActRequest struct {
	TargetID  string
	Kind      string
	Ref       string
	Key       string
	Text      string
	UserID    string
	Direction string
	Amount    int
}
type BrowserActResult struct{}

type BrowserCloseRequest struct{ TargetID, UserID string }
type BrowserWaitRequest struct {
	TargetID, UserID string
	Milliseconds     int
}
type BrowserScrollRequest struct {
	TargetID, UserID, Direction string
	Amount                      int
}

type BrowserCloser interface {
	Close(context.Context, BrowserCloseRequest) error
}
type BrowserWaiter interface {
	Wait(context.Context, BrowserWaitRequest) (BrowserSnapshotResult, error)
}
type BrowserScroller interface {
	Scroll(context.Context, BrowserScrollRequest) (BrowserSnapshotResult, error)
}
type BrowserSessionTabs interface {
	TabsForSession(context.Context, string) (BrowserTabsResult, error)
}

func validateCamofoxControlURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("camofox_url must be an absolute http(s) URL")
	}
	return parsed, nil
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(strings.ToLower(host))
	return host == "localhost" || host == "[::1]" || host == "::1" || strings.HasPrefix(host, "127.")
}

func validatePrivateSecretFile(name string) error {
	linkInfo, err := os.Lstat(name)
	if err != nil {
		return err
	}
	if linkInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("must not be a symbolic link")
	}
	info := linkInfo
	if !info.Mode().IsRegular() {
		return errors.New("must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("must not be group/world accessible")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Getuid() {
		return errors.New("must be owned by the current user")
	}
	return nil
}
