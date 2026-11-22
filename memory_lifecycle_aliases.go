package bee

import "context"

// Compatibility aliases for callers that describe the lifecycle layer rather
// than the concrete local implementation.
type MemoryLifecycleService = MemoryService
type MemoryRecordView = MemoryDisplayRecord
type MemoryListRequest = ListMemoryRequest
type MemoryShowRequest = ShowMemoryRequest
type MemorySearchRequest = SearchMemoryRequest

func NewLocalMemoryLifecycle(root string, policy MemoryPolicy) (*LocalMemoryService, error) {
	return NewLocalMemoryService(root, policy)
}

func (service *LocalMemoryService) ExportJSON(ctx context.Context, request MemoryExportRequest) ([]byte, error) {
	export, err := service.Export(ctx, request)
	if err != nil {
		return nil, err
	}
	return MemoryExportJSON(export)
}

func (service *LocalMemoryService) ImportJSON(ctx context.Context, request MemoryImportRequest) (MemoryImportResult, error) {
	return service.Import(ctx, request)
}

func RedactMemoryRecord(policy MemoryPolicy, actor MemoryActor, record MemoryRecord, bounds MemoryDisplayBounds, includeSensitive bool) (MemoryDisplayRecord, error) {
	if err := policy.Validate(); err != nil {
		return MemoryDisplayRecord{}, err
	}
	if err := actor.Validate(); err != nil {
		return MemoryDisplayRecord{}, err
	}
	if err := record.Validate(policy); err != nil {
		return MemoryDisplayRecord{}, err
	}
	effective := bounds.effective(policy)
	service := &LocalMemoryService{policy: policy}
	return service.displayRecord(record, actor, includeSensitive, effective), nil
}
