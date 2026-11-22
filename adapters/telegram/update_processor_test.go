package telegram

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seth-el/bee"
)

func TestUpdateProcessorDoesNotAdvanceOnHandlerFailure(t *testing.T) {
	checkpoint, _ := NewCheckpointStore(filepath.Join(t.TempDir(), "checkpoint.json"))
	processor := &UpdateProcessor{Config: Config{ChannelID: "telegram", AccountID: "test", AllowedChats: []int64{42}, AllowedSenders: []int64{7}}, Checkpoint: checkpoint, Handler: bee.ChannelHandlerFunc(func(context.Context, bee.InboundMessage) (bee.ChannelRouteResult, error) {
		return bee.ChannelRouteResult{}, errors.New("handler failed")
	})}
	_, err := processor.Process(context.Background(), Update{UpdateID: 4, Message: &Message{MessageID: 1, From: &User{ID: 7}, Chat: Chat{ID: 42}, Text: "hello"}})
	if err == nil || !strings.Contains(err.Error(), "handler failed") {
		t.Fatalf("err=%v offset=%d", err, checkpointOffset(t, checkpoint))
	}
	if offset := checkpointOffset(t, checkpoint); offset != 0 {
		t.Fatalf("offset advanced=%d", offset)
	}
}

func checkpointOffset(t *testing.T, checkpoint *CheckpointStore) int64 {
	t.Helper()
	value, err := checkpoint.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return value
}
