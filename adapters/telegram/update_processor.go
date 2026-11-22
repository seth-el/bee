package telegram

import (
	"context"
	"errors"
	"fmt"

	"github.com/seth-el/bee"
)

type ProcessResult struct {
	Ignored    bool
	Duplicate  bool
	Delivered  bool
	Checkpoint int64
	Route      bee.ChannelRouteResult
}

type UpdateProcessor struct {
	Config        Config
	Checkpoint    *CheckpointStore
	Handler       bee.ChannelHandler
	UpdateHandler UpdateHandler
	// Commit records that the update is finished. Nil saves update+1 to
	// Checkpoint directly (the sequential behavior); the concurrent poller
	// supplies a tracker that only advances past contiguous finished updates.
	Commit func(context.Context, int64) error
}

func (processor *UpdateProcessor) commit(ctx context.Context, updateID int64) error {
	if processor.Commit != nil {
		return processor.Commit(ctx, updateID)
	}
	return processor.Checkpoint.Save(ctx, updateID+1)
}

func (processor *UpdateProcessor) Process(ctx context.Context, update Update) (ProcessResult, error) {
	if processor == nil || processor.Checkpoint == nil || processor.Handler == nil {
		return ProcessResult{}, errors.New("update processor is not configured")
	}
	if ctx == nil {
		return ProcessResult{}, errors.New("update processor context is nil")
	}
	if update.UpdateID < 0 || update.UpdateID >= MaxCheckpointOffset {
		return ProcessResult{}, errors.New("telegram update id cannot advance checkpoint")
	}
	var result bee.ChannelRouteResult
	var err error
	if processor.UpdateHandler != nil {
		result, err = processor.UpdateHandler.HandleUpdate(ctx, update)
	} else {
		normalized, normalizeErr := NormalizeUpdate(processor.Config, update)
		if normalizeErr != nil {
			return ProcessResult{}, normalizeErr
		}
		if normalized.Ignored {
			if err := processor.commit(ctx, update.UpdateID); err != nil {
				return ProcessResult{}, err
			}
			return ProcessResult{Ignored: true, Checkpoint: update.UpdateID + 1}, nil
		}
		result, err = processor.Handler.Handle(ctx, normalized.Message)
	}
	if err != nil {
		return ProcessResult{}, fmt.Errorf("process Telegram update: %w", err)
	}
	if err := processor.commit(ctx, update.UpdateID); err != nil {
		return ProcessResult{}, err
	}
	return ProcessResult{Delivered: result.Outbound != nil || processor.UpdateHandler != nil, Checkpoint: update.UpdateID + 1, Route: result, Duplicate: result.Duplicate}, nil
}
