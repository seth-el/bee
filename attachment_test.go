package bee

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAttachmentStoreSafeMetadataTextContextAndImageUnsupported(t *testing.T) {
	sessions, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session, err := sessions.Create(SessionMetadata{Title: "attachments"})
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := NewArtifactStore(filepath.Join(t.TempDir(), "artifacts"), sessions)
	if err != nil {
		t.Fatal(err)
	}
	attachments, err := NewAttachmentStore(filepath.Join(t.TempDir(), "attachments"), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	text := []byte("package main\nfunc main() {}\n")
	att, err := attachments.Create(context.Background(), session.ID, "main.go", "application/octet-stream", text)
	if err != nil {
		t.Fatal(err)
	}
	if att.Kind != "text" || att.Status != "uploaded" || att.ArtifactID == "" {
		t.Fatalf("attachment = %#v", att)
	}
	_, payload, _, err := artifacts.Preview(att.ArtifactID)
	if err != nil || string(payload) != string(text) {
		t.Fatalf("payload = %q err=%v", payload, err)
	}
	if listed, err := artifacts.List(session.ID); err != nil || len(listed) != 1 || listed[0].ID != att.ArtifactID {
		t.Fatalf("artifact list = %#v err=%v", listed, err)
	}
	refs, injected, err := attachments.ResolveForTurn(session.ID, []string{att.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].ID != att.ID || !strings.Contains(injected, "package main") {
		t.Fatalf("refs=%#v context=%q", refs, injected)
	}
	message := Message{Role: RoleUser, Content: "inspect", Attachments: refs}
	encoded, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "package main") {
		t.Fatal("attachment bytes leaked into message JSON")
	}
	image, err := attachments.Create(context.Background(), session.ID, "photo.png", "image/png", []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	if err != nil {
		t.Fatal(err)
	}
	if image.Kind != "image" || image.Interpretation == "" || image.Status != "ready" {
		t.Fatalf("image = %#v", image)
	}
	_, imageContext, err := attachments.ResolveForTurn(session.ID, []string{image.ID})
	if err != nil || !strings.Contains(imageContext, "not interpreted") {
		t.Fatalf("image context=%q err=%v", imageContext, err)
	}
	_, imageContext, err = attachments.ResolveForTurnWithImageCapability(session.ID, []string{image.ID}, true)
	if err != nil || strings.Contains(imageContext, "not interpreted") {
		t.Fatalf("capable image context=%q err=%v", imageContext, err)
	}
}

func TestAttachmentStoreRejectsUnsafeNamesAndSymlinkRoots(t *testing.T) {
	sessions, _ := NewSessionStore(t.TempDir())
	session, _ := sessions.Create(SessionMetadata{Title: "attachments"})
	artifacts, err := NewArtifactStore(filepath.Join(t.TempDir(), "artifacts"), sessions)
	if err != nil {
		t.Fatal(err)
	}
	attachments, err := NewAttachmentStore(filepath.Join(t.TempDir(), "attachments"), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../escape.txt", "nested/file.txt", ""} {
		if _, err := attachments.Create(context.Background(), session.ID, name, "text/plain", []byte("x")); err == nil {
			t.Fatalf("unsafe filename %q accepted", name)
		}
	}
	parent := t.TempDir()
	realRoot := filepath.Join(parent, "real")
	if err := os.Mkdir(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(realRoot, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAttachmentStore(filepath.Join(link, "attachments"), artifacts); err == nil {
		t.Fatal("symlinked attachment root accepted")
	}
}

func TestAttachmentStoreAudioTranscriptDerivedArtifact(t *testing.T) {
	sessions, _ := NewSessionStore(t.TempDir())
	session, _ := sessions.Create(SessionMetadata{Title: "audio"})
	artifacts, err := NewArtifactStore(filepath.Join(t.TempDir(), "artifacts"), sessions)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "transcribe.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'hello from gemma\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	attachments, err := NewAttachmentStore(filepath.Join(t.TempDir(), "attachments"), artifacts, AttachmentStoreOptions{AudioScriptPath: script})
	if err != nil {
		t.Fatal(err)
	}
	audioBytes := append([]byte("RIFF"), make([]byte, 36)...)
	audioBytes = append(audioBytes, []byte("WAVE")...)
	att, err := attachments.Create(context.Background(), session.ID, "voice.wav", "audio/wav", audioBytes)
	if err != nil {
		t.Fatal(err)
	}
	processed, err := attachments.ProcessAudio(context.Background(), att.ID)
	if err != nil {
		t.Fatal(err)
	}
	if processed.Status != "ready" || processed.TranscriptArtifactID == "" {
		t.Fatalf("processed=%#v", processed)
	}
	transcript, preview, _, err := artifacts.Preview(processed.TranscriptArtifactID)
	if err != nil || transcript.Kind != "text" || string(preview) != "hello from gemma" {
		t.Fatalf("transcript=%#v preview=%q err=%v", transcript, preview, err)
	}
	_, contextText, err := attachments.ResolveForTurn(session.ID, []string{att.ID})
	if err != nil || !strings.Contains(contextText, "hello from gemma") {
		t.Fatalf("context=%q err=%v", contextText, err)
	}
}

func TestAttachmentStoreAudioTranscriptionIsDisabledByDefault(t *testing.T) {
	sessions, _ := NewSessionStore(t.TempDir())
	session, _ := sessions.Create(SessionMetadata{Title: "audio disabled"})
	artifacts, err := NewArtifactStore(filepath.Join(t.TempDir(), "artifacts"), sessions)
	if err != nil {
		t.Fatal(err)
	}
	attachments, err := NewAttachmentStore(filepath.Join(t.TempDir(), "attachments"), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	audioBytes := append([]byte("RIFF"), make([]byte, 36)...)
	audioBytes = append(audioBytes, []byte("WAVE")...)
	attachment, err := attachments.Create(context.Background(), session.ID, "voice.wav", "audio/wav", audioBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attachments.ProcessAudio(context.Background(), attachment.ID); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("disabled transcription error=%v", err)
	}
}
