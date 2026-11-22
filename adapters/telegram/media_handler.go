package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/seth-el/bee"
)

// MaxInboundImageBytes bounds a photo or image file read for the model.
const MaxInboundImageBytes = 20 << 20

// MediaHandler is the poller's update handler when photos and files are
// enabled. Photos and documents are staged privately and handed to Bee as
// opaque attachment references (the router reads them back through
// ReadAttachment); voice notes go to Voice when it is set; everything else is
// ordinary text.
type MediaHandler struct {
	Config      Config
	Handler     bee.ChannelHandler
	Client      *Client
	Channel     *TelegramChannel
	Attachments *AttachmentArtifactStore
	// Voice handles voice notes and text when voice is enabled.
	Voice *VoiceAwareHandler
}

func (handler *MediaHandler) HandleUpdate(ctx context.Context, update Update) (bee.ChannelRouteResult, error) {
	if handler == nil || handler.Handler == nil || handler.Client == nil || handler.Attachments == nil {
		return bee.ChannelRouteResult{}, errors.New("media handler is not configured")
	}
	if raw := updateMessage(update); raw != nil && (len(raw.Photo) > 0 || raw.Document != nil) {
		return handler.handleFiles(ctx, update)
	}
	if handler.Voice != nil {
		return handler.Voice.HandleUpdate(ctx, update)
	}
	normalized, err := NormalizeUpdate(handler.Config, update)
	if err != nil || normalized.Ignored {
		return bee.ChannelRouteResult{}, err
	}
	return handler.Handler.Handle(ctx, normalized.Message)
}

func (handler *MediaHandler) handleFiles(ctx context.Context, update Update) (result bee.ChannelRouteResult, err error) {
	normalized, err := NormalizeAttachmentUpdate(handler.Config, update)
	if err != nil || normalized.Ignored {
		return bee.ChannelRouteResult{}, err
	}
	message := normalized.Message
	message.Attachments = nil
	var artifacts []*AttachmentArtifact
	defer func() {
		for _, artifact := range artifacts {
			_ = artifact.Cleanup()
		}
	}()
	for _, raw := range normalized.RawAttachments {
		if raw.Kind != AttachmentKindPhoto && raw.Kind != AttachmentKindDocument {
			continue
		}
		artifact, stageErr := handler.Attachments.Stage(ctx, handler.Client, raw)
		if stageErr != nil {
			if ctx.Err() != nil {
				return bee.ChannelRouteResult{}, ctx.Err()
			}
			if handler.Channel != nil {
				return bee.ChannelRouteResult{}, handler.Channel.SendText(ctx, message, "Couldn't download that file: "+stageErr.Error())
			}
			return bee.ChannelRouteResult{}, fmt.Errorf("stage Telegram attachment: %w", stageErr)
		}
		artifacts = append(artifacts, artifact)
		message.Attachments = append(message.Attachments, artifact.Reference())
	}
	if len(message.Attachments) == 0 && strings.TrimSpace(message.Text) == "" {
		return bee.ChannelRouteResult{}, nil
	}
	if err := message.Validate(); err != nil {
		return bee.ChannelRouteResult{}, err
	}
	return handler.Handler.Handle(ctx, message)
}

// ReadAttachment loads a staged attachment for the router. Only references
// this adapter staged resolve.
func (handler *MediaHandler) ReadAttachment(_ context.Context, ref bee.AttachmentRef) (bee.ChannelFile, error) {
	file := bee.ChannelFile{Filename: ref.Filename, MediaType: ref.ContentType}
	artifact, err := handler.Attachments.Resolve(ref)
	if err != nil {
		return file, err
	}
	reader, err := artifact.Open()
	if err != nil {
		return file, err
	}
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, MaxInboundImageBytes+1))
	if err != nil {
		return file, errors.New("read attachment")
	}
	if len(data) > MaxInboundImageBytes {
		return file, fmt.Errorf("file is larger than %d MB", MaxInboundImageBytes>>20)
	}
	file.Data = data
	return file, nil
}
