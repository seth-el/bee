package telegram

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/seth-el/bee"
)

// AttachmentAwareHandler explicitly opts a poller into documents, selected
// photos, audio, and voice staging. It never places local paths in Bee.
type AttachmentAwareHandler struct {
	Config  Config
	Handler bee.ChannelHandler
	Client  *Client
	Store   *AttachmentArtifactStore

	mu   sync.Mutex
	seen map[string]struct{}
}

func NewAttachmentAwareHandler(config Config, handler bee.ChannelHandler, client *Client, store *AttachmentArtifactStore) (*AttachmentAwareHandler, error) {
	if handler == nil || client == nil || store == nil {
		return nil, errors.New("attachment-aware handler requires handler, client, and store")
	}
	config = config.Normalized()
	if err := validateChannelConfig(config); err != nil {
		return nil, err
	}
	return &AttachmentAwareHandler{Config: config, Handler: handler, Client: client, Store: store, seen: make(map[string]struct{})}, nil
}

func (handler *AttachmentAwareHandler) HandleUpdate(ctx context.Context, update Update) (result bee.ChannelRouteResult, err error) {
	if handler == nil || handler.Handler == nil || handler.Client == nil || handler.Store == nil {
		return bee.ChannelRouteResult{}, errors.New("attachment-aware handler is not configured")
	}
	if ctx == nil {
		return bee.ChannelRouteResult{}, errors.New("attachment-aware handler context is nil")
	}
	if update.UpdateID < 0 || update.UpdateID > MaxCheckpointOffset {
		return bee.ChannelRouteResult{}, errors.New("telegram update id is out of bounds")
	}
	key := opaqueUpdateID(update.UpdateID)
	if handler.isSeen(key) {
		return bee.ChannelRouteResult{Duplicate: true}, nil
	}
	normalized, err := NormalizeAttachmentUpdate(handler.Config, update)
	if err != nil {
		return bee.ChannelRouteResult{}, err
	}
	if normalized.Ignored {
		return bee.ChannelRouteResult{}, nil
	}
	artifacts := make([]*AttachmentArtifact, 0, len(normalized.RawAttachments))
	defer func() {
		for _, artifact := range artifacts {
			if cleanupErr := artifact.Cleanup(); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("cleanup Telegram attachment: %w", cleanupErr))
			}
		}
	}()
	if len(normalized.RawAttachments) > 0 {
		normalized.Message.Attachments = make([]bee.AttachmentRef, 0, len(normalized.RawAttachments))
		for _, raw := range normalized.RawAttachments {
			artifact, stageErr := handler.Store.Stage(ctx, handler.Client, raw)
			if stageErr != nil {
				return bee.ChannelRouteResult{}, fmt.Errorf("stage Telegram attachment: %w", stageErr)
			}
			artifacts = append(artifacts, artifact)
			normalized.Message.Attachments = append(normalized.Message.Attachments, artifact.Reference())
		}
		if err := normalized.Message.Validate(); err != nil {
			return bee.ChannelRouteResult{}, err
		}
	}
	result, err = handler.Handler.Handle(ctx, normalized.Message)
	if err != nil {
		return bee.ChannelRouteResult{}, err
	}
	handler.markSeen(key)
	return result, nil
}

func NewAttachmentAwarePoller(client *Client, config Config, checkpoint *CheckpointStore, handler bee.ChannelHandler, store *AttachmentArtifactStore) (*Poller, error) {
	poller, err := NewPoller(client, config, checkpoint, handler)
	if err != nil {
		return nil, err
	}
	attachmentHandler, err := NewAttachmentAwareHandler(config, handler, client, store)
	if err != nil {
		return nil, err
	}
	poller.UpdateHandler = attachmentHandler
	return poller, nil
}

func (handler *AttachmentAwareHandler) isSeen(key string) bool {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	_, seen := handler.seen[key]
	return seen
}
func (handler *AttachmentAwareHandler) markSeen(key string) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	handler.seen[key] = struct{}{}
	if len(handler.seen) > 4096 {
		delete(handler.seen, key)
	}
}
