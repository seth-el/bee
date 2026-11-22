package bee

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type brokerProviderTestProvider struct {
	err      error
	resolver AttachmentArtifactResolver
}

func (provider *brokerProviderTestProvider) SupportsImageInput() bool { return true }
func (provider *brokerProviderTestProvider) SetAttachmentResolver(resolver AttachmentArtifactResolver) {
	provider.resolver = resolver
}
func (provider *brokerProviderTestProvider) Complete(context.Context, Request) (Response, error) {
	return Response{}, provider.err
}
func (provider *brokerProviderTestProvider) Stream(context.Context, Request, StreamFunc) (Response, error) {
	return Response{}, provider.err
}

func TestBrokeredProviderLeasesOnlyProviderCall(t *testing.T) {
	broker, err := NewLocalChildCapacityBroker(LocalChildBrokerConfig{Enabled: true, MaxConcurrency: 1, MaxQueue: 2})
	if err != nil {
		t.Fatal(err)
	}
	provider := brokerChildProvider(&brokerProviderTestProvider{err: errors.New("failed")}, broker)
	if snapshot := broker.Snapshot(); snapshot.InFlight != 0 {
		t.Fatalf("before call snapshot=%#v", snapshot)
	}
	_, _ = provider.Complete(context.Background(), Request{})
	snapshot := broker.Snapshot()
	if snapshot.Acquisitions != 1 || snapshot.Completed != 1 || snapshot.FailedCalls != 1 || snapshot.InFlight != 0 {
		t.Fatalf("after call snapshot=%#v", snapshot)
	}
}

func TestBrokeredProviderForwardsOptionalCapabilitiesWithoutAcquiring(t *testing.T) {
	broker, err := NewLocalChildCapacityBroker(LocalChildBrokerConfig{Enabled: true, MaxConcurrency: 1, MaxQueue: 2})
	if err != nil {
		t.Fatal(err)
	}
	raw := &brokerProviderTestProvider{}
	wrapped := brokerChildProvider(raw, broker)
	capable, ok := wrapped.(interface{ SupportsImageInput() bool })
	if !ok || !capable.SupportsImageInput() {
		t.Fatal("image capability was erased by broker wrapper")
	}
	configurable, ok := wrapped.(interface {
		SetAttachmentResolver(AttachmentArtifactResolver)
	})
	if !ok {
		t.Fatal("attachment resolver capability was erased by broker wrapper")
	}
	configurable.SetAttachmentResolver(brokerTestResolver{})
	if raw.resolver == nil {
		t.Fatal("attachment resolver was not forwarded")
	}
	if snapshot := broker.Snapshot(); snapshot.Acquisitions != 0 || snapshot.InFlight != 0 {
		t.Fatalf("optional capability forwarding acquired broker lease: %#v", snapshot)
	}
}

type brokerTestResolver struct{}

func (brokerTestResolver) OpenAttachmentPayload(context.Context, AttachmentRef) (Artifact, io.ReadCloser, error) {
	return Artifact{}, io.NopCloser(strings.NewReader("")), nil
}
