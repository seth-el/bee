package bee

import "context"

// brokeredProvider scopes capacity to one actual provider operation. Child
// execution and tool waits do not hold a lease.
type brokeredProvider struct {
	provider Provider
	broker   *LocalChildCapacityBroker
	purpose  LocalChildPurpose
	priority LocalChildPriority
}

func (provider *brokeredProvider) Complete(ctx context.Context, request Request) (Response, error) {
	lease, err := provider.broker.Acquire(ctx, provider.purpose, provider.priority)
	if err != nil {
		return Response{}, err
	}
	response, callErr := provider.provider.Complete(ctx, request)
	lease.ReleaseError(callErr)
	return response, callErr
}

func (provider *brokeredProvider) Stream(ctx context.Context, request Request, onDelta StreamFunc) (Response, error) {
	lease, err := provider.broker.Acquire(ctx, provider.purpose, provider.priority)
	if err != nil {
		return Response{}, err
	}
	response, callErr := provider.provider.Stream(ctx, request, onDelta)
	lease.ReleaseError(callErr)
	return response, callErr
}

// SupportsImageInput forwards the optional capability without making the
// broker wrapper claim capabilities the underlying provider does not have.
func (provider *brokeredProvider) SupportsImageInput() bool {
	if provider == nil || provider.provider == nil {
		return false
	}
	capable, ok := provider.provider.(interface{ SupportsImageInput() bool })
	return ok && capable.SupportsImageInput()
}

// SetAttachmentResolver forwards the narrow composition-owned resolver seam.
// It does not acquire capacity; only Complete/Stream own provider leases.
func (provider *brokeredProvider) SetAttachmentResolver(resolver AttachmentArtifactResolver) {
	if provider == nil || provider.provider == nil {
		return
	}
	if configurable, ok := provider.provider.(interface {
		SetAttachmentResolver(AttachmentArtifactResolver)
	}); ok {
		configurable.SetAttachmentResolver(resolver)
	}
}

func brokerChildProvider(provider Provider, broker *LocalChildCapacityBroker) Provider {
	return brokeredProviderForPurpose(provider, broker, LocalChildPurposeChild)
}

func brokeredProviderForPurpose(provider Provider, broker *LocalChildCapacityBroker, purpose LocalChildPurpose) Provider {
	if provider == nil || broker == nil || !broker.config.Enabled {
		return provider
	}
	if _, already := provider.(*brokeredProvider); already {
		return provider
	}
	return &brokeredProvider{provider: provider, broker: broker, purpose: purpose, priority: LocalChildPriorityNormal}
}
