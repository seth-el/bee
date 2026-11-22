package bee

import "context"

// ACPChildExecutor adapts a provider-neutral ACPBackend to Bee's existing
// ChildExecutor/WorktreeOrchestrator contracts. It does not add authority or
// create a second assistant surface.
type ACPChildExecutor struct {
	// Backend preserves the original ACP-specific field for compatibility.
	Backend *ACPBackend
	// CodingBackend lets the parent route any provider-neutral coding backend
	// (including fake/local implementations) through the same child seam.
	CodingBackend    CodingBackend
	Plan             []string
	Role             CodingBackendRole
	PermissionPolicy ACPPermissionPolicy
}

var _ ChildExecutor = (*ACPChildExecutor)(nil)
var _ ChildObservableExecutor = (*ACPChildExecutor)(nil)

func (executor *ACPChildExecutor) Run(ctx context.Context, task ChildTask) (ChildCompletionPacket, error) {
	return executor.RunObserved(ctx, task, nil)
}

func (executor *ACPChildExecutor) RunObserved(ctx context.Context, task ChildTask, observer ChildObserver) (ChildCompletionPacket, error) {
	if executor == nil {
		return ChildCompletionPacket{TaskID: task.TaskID, ParentID: task.ParentID, Status: ChildMalformed, Summary: "ACP child executor backend is not configured"}, nil
	}
	backend := executor.CodingBackend
	if backend == nil {
		backend = executor.Backend
	}
	if backend == nil {
		return ChildCompletionPacket{TaskID: task.TaskID, ParentID: task.ParentID, Status: ChildMalformed, Summary: "ACP child executor backend is not configured"}, nil
	}
	codingObserver := codingChildObserver{observer: observer}
	permissionPolicy := executor.PermissionPolicy
	if permissionPolicy.Role == "" {
		permissionPolicy.Role = executor.Role
	}
	completion, err := backend.Run(ctx, CodingBackendTask{
		Task: task, Plan: append([]string(nil), executor.Plan...), Role: executor.Role,
		PermissionPolicy: permissionPolicy, Workspace: task.Workspace, ParentScopeRoot: task.ParentScopeRoot,
	}, codingObserver)
	packet := completion.ChildCompletionPacket
	if packet.RequestedBackend == "" {
		packet.RequestedBackend = completion.Backend
	}
	if packet.EffectiveBackend == "" {
		packet.EffectiveBackend = completion.Backend
	}
	if err != nil && packet.Status == "" {
		packet.Status = ChildFailed
	}
	return packet, err
}

type codingChildObserver struct {
	observer ChildObserver
}

func (observer codingChildObserver) ObserveCodingBackendEvent(event CodingBackendEvent) {
	if observer.observer == nil {
		return
	}
	notifyChild(observer.observer, ChildObservation{TaskID: event.TaskID, ParentID: event.ParentID, Type: "acp_" + event.Type, Delta: event.Text, Info: event.Info, Completion: event.Completion})
}
