package bee

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testProvider(t *testing.T, baseURL string) *OpenAICompatibleProvider {
	t.Helper()
	provider, err := NewOpenAICompatibleProvider(ProviderConfig{Type: DefaultProviderType, BaseURL: baseURL, Model: "local"}, "")
	if err != nil {
		t.Fatal(err)
	}
	provider.stall = stallPolicy{probeAfter: 100 * time.Millisecond, retryAfter: 50 * time.Millisecond, healthTimeout: 100 * time.Millisecond, tick: 10 * time.Millisecond}
	return provider
}

func streamOnce(provider *OpenAICompatibleProvider) (Response, error) {
	return provider.Stream(context.Background(), Request{Model: "local", Messages: []Message{{Role: RoleUser, Content: "hi"}}}, func(string) error { return nil })
}

func TestRefusedModelServerIsReportedAsUnreachable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	_, err = streamOnce(testProvider(t, "http://"+address))
	unreachable, ok := AsModelUnreachable(err)
	if !ok || unreachable.Reason != "connection refused" || unreachable.Endpoint != address || !providerRequestFailedBeforeResponse(err) {
		t.Fatalf("err = %v", err)
	}
	if RunErrorKind(err) != ErrorKindModelUnreachable {
		t.Fatal("not classified as model_unreachable")
	}
	if !strings.Contains(err.Error(), "can't reach the model server at "+address+" (connection refused)") {
		t.Fatalf("message = %q", err.Error())
	}
}

func TestHungModelServerFailsAfterHealthChecks(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select { // completions and /health both hang: the process is wedged
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	started := time.Now()
	_, err := streamOnce(testProvider(t, server.URL))
	unreachable, ok := AsModelUnreachable(err)
	if !ok || !strings.Contains(unreachable.Reason, "failed 2 health checks") {
		t.Fatalf("err = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("took %s", elapsed)
	}
}

func TestSlowButHealthyModelServerKeepsWaiting(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			fmt.Fprint(w, `{"status":"ok"}`)
			return
		}
		time.Sleep(400 * time.Millisecond) // a long prompt: silent, but alive
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	response, err := streamOnce(testProvider(t, server.URL))
	if err != nil || response.Message.Content != "done" {
		t.Fatalf("response = %#v, err = %v", response, err)
	}
}

func TestProbeModelServer(t *testing.T) {
	alive := httptest.NewServer(http.NotFoundHandler()) // any HTTP answer counts
	defer alive.Close()
	if err := ProbeModelServer(context.Background(), nil, alive.URL+"/v1", time.Second); err != nil {
		t.Fatalf("alive server: %v", err)
	}
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	address := listener.Addr().String()
	_ = listener.Close()
	err := ProbeModelServer(context.Background(), nil, "http://"+address, time.Second)
	if unreachable, ok := AsModelUnreachable(err); !ok || unreachable.Reason != "connection refused" {
		t.Fatalf("dead server: %v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatal("refusal reported as cancellation")
	}
}

func TestDoctorProviderReachabilityCheck(t *testing.T) {
	t.Setenv("BEE_DOCTOR_SKIP_PROVIDER_PROBE", "")
	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }))
	defer alive.Close()
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := "http://" + listener.Addr().String()
	_ = listener.Close()
	config := DefaultConfig()
	config.Profiles = []ProviderProfile{
		{Name: "main", Role: ProviderRoleMain, Provider: ProviderConfig{Type: DefaultProviderType, BaseURL: alive.URL, Model: "local"}},
		{Name: "child", Role: ProviderRoleChild, Provider: ProviderConfig{Type: DefaultProviderType, BaseURL: dead, Model: "local"}},
		{Name: "cloud", Role: ProviderRoleManual, Provider: ProviderConfig{Type: DefaultProviderType, BaseURL: "https://example.invalid", Model: "x"}},
	}
	config.Routing = ProviderRoutingConfig{MainProfile: "main", ChildProfile: "child", ManualProfiles: []string{"cloud"}}
	check := func() (CheckStatus, string) {
		var status CheckStatus
		var detail string
		addProviderReachabilityCheck(config, func(name string, s CheckStatus, d string) {
			if name == "provider_reachability" {
				status, detail = s, d
			}
		})
		return status, detail
	}
	status, detail := check()
	if status != CheckWarn || !strings.Contains(detail, "child (") || !strings.Contains(detail, "connection refused") || !strings.Contains(detail, "reachable: main") || strings.Contains(detail, "cloud") {
		t.Fatalf("status = %v, detail = %q", status, detail)
	}
	config.Routing.ChildProfile = ""
	if status, detail := check(); status != CheckPass || !strings.Contains(detail, "main (") {
		t.Fatalf("status = %v, detail = %q", status, detail)
	}
	t.Setenv("BEE_DOCTOR_SKIP_PROVIDER_PROBE", "1")
	if status, _ := check(); status != "" {
		t.Fatal("probe ran while skipped")
	}
}

func TestUnreachableRunFailureIsClassifiedEverywhere(t *testing.T) {
	event := RunEvent{Type: RunFailure, Error: "provider request: can't reach the model server at 203.0.113.37:8081 (connection refused)", ErrorKind: ErrorKindModelUnreachable}
	if got := RunFailureText(event); got != "Model server unreachable: 203.0.113.37:8081 (connection refused)" {
		t.Fatalf("text = %q", got)
	}
	err := formatRunError(event)
	if RunErrorKind(err) != ErrorKindModelUnreachable || err.Error() != RunFailureText(event) {
		t.Fatalf("run error = %v (%q)", err, RunErrorKind(err))
	}
	if plain := formatRunError(RunEvent{Error: "boom"}); RunErrorKind(plain) != "" || plain.Error() != "run failed: boom" {
		t.Fatalf("plain error = %v", plain)
	}

	store, _ := NewSessionStore(t.TempDir())
	app, _ := NewApplication(DefaultConfig(), &scriptedToolProvider{}, store)
	registry := newOpenAIWebRunRegistry(app)
	registry.publish("c", RunEvent{Type: RunStart, RunID: "r"})
	failed, ok := registry.publish("c", RunEvent{Type: RunFailure, RunID: "r", Error: event.Error, ErrorKind: ErrorKindModelUnreachable})
	if !ok || failed.State != openAIWebRunFailed || failed.Reason != ErrorKindModelUnreachable {
		t.Fatalf("lifecycle failure = %#v", failed)
	}
	end, ok := registry.publish("c", RunEvent{Type: RunEnd, RunID: "r", Error: event.Error, ErrorKind: ErrorKindModelUnreachable})
	if !ok || !end.Terminal || end.Reason != ErrorKindModelUnreachable {
		t.Fatalf("lifecycle end = %#v", end)
	}
}
