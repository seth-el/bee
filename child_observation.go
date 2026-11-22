package bee

import (
	"context"
	"time"
)

// ChildObservation is a bounded, provider-neutral observation of one child.
// It is metadata for tracing and monitoring, not an instruction channel.
type ChildObservation struct {
	TaskID     string                 `json:"task_id"`
	ParentID   string                 `json:"parent_id,omitempty"`
	Type       string                 `json:"type"`
	Profile    string                 `json:"profile,omitempty"`
	Role       ProviderRole           `json:"role,omitempty"`
	At         time.Time              `json:"at"`
	Delta      string                 `json:"delta,omitempty"`
	Info       string                 `json:"info,omitempty"`
	Message    *Message               `json:"message,omitempty"`
	ToolCall   *ToolCall              `json:"tool_call,omitempty"`
	ToolResult *ToolResultEvent       `json:"tool_result,omitempty"`
	Approval   *ApprovalEvent         `json:"approval,omitempty"`
	Usage      *UsageEvent            `json:"usage,omitempty"`
	Completion *ChildCompletionPacket `json:"completion,omitempty"`
}

// ChildObserver receives optional child lifecycle/provider/tool/approval
// observations. Observers must not be able to alter child execution.
type ChildObserver interface {
	ObserveChildEvent(ChildObservation)
}

type childObserverFunc func(ChildObservation)

func (fn childObserverFunc) ObserveChildEvent(event ChildObservation) { fn(event) }

// ChildObservableExecutor is an optional extension of ChildExecutor. Existing
// callers can continue using ChildExecutor.Run unchanged.
type ChildObservableExecutor interface {
	ChildExecutor
	RunObserved(context.Context, ChildTask, ChildObserver) (ChildCompletionPacket, error)
}

func notifyChild(observer ChildObserver, event ChildObservation) {
	if observer == nil {
		return
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	observer.ObserveChildEvent(event)
}
