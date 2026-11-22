package bee

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestArtifactAndAttachmentIndexesRebuildAndRecover(t *testing.T) {
	root := t.TempDir()
	sessions, err := NewSessionStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := sessions.Create(SessionMetadata{Title: "indexed"})
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := NewArtifactStore(filepath.Join(root, "artifacts"), sessions)
	if err != nil {
		t.Fatal(err)
	}
	attachments, err := NewAttachmentStore(filepath.Join(root, "attachments"), artifacts)
	if err != nil {
		t.Fatal(err)
	}
	first, err := artifacts.Create(context.Background(), session.ID, "", ArtifactInput{Filename: "one.txt", MediaType: "text/plain", Content: []byte("one")})
	if err != nil {
		t.Fatal(err)
	}
	second, err := artifacts.Create(context.Background(), session.ID, "", ArtifactInput{Filename: "two.txt", MediaType: "text/plain", Content: []byte("two")})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := artifacts.List(session.ID); err != nil || len(got) != 2 || got[0].ID != first.ID || got[1].ID != second.ID {
		t.Fatalf("artifact indexed list = %#v, %v", got, err)
	}
	attachment, err := attachments.Create(context.Background(), session.ID, "upload.txt", "text/plain", []byte("upload"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attachments.UpdateStatus(attachment.ID, "ready", "updated"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(scopedIndexPath(artifacts.Root(), session.ID)); err != nil {
		t.Fatal(err)
	}
	if got, err := artifacts.List(session.ID); err != nil || len(got) != 3 {
		t.Fatalf("artifact legacy rebuild = %#v, %v", got, err)
	}
	if err := os.WriteFile(scopedIndexPath(attachments.Root(), session.ID), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	gotAttachments, err := attachments.List(session.ID)
	if err != nil || len(gotAttachments) != 1 || gotAttachments[0].Status != "ready" {
		t.Fatalf("attachment recovery = %#v, %v", gotAttachments, err)
	}
	if err := os.WriteFile(scopedIndexPath(attachments.Root(), session.ID), []byte(`{"version":1,"session_id":"wrong","source_mod_time_ns":0,"ids":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if gotAttachments, err = attachments.List(session.ID); err != nil || len(gotAttachments) != 1 {
		t.Fatalf("stale attachment recovery = %#v, %v", gotAttachments, err)
	}
}

func TestArtifactAttachmentIndexesCrossInstance(t *testing.T) {
	root := t.TempDir()
	sessions, err := NewSessionStore(filepath.Join(root, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := sessions.Create(SessionMetadata{})
	if err != nil {
		t.Fatal(err)
	}
	artifactsA, err := NewArtifactStore(filepath.Join(root, "artifacts"), sessions)
	if err != nil {
		t.Fatal(err)
	}
	artifactsB, err := NewArtifactStore(filepath.Join(root, "artifacts"), sessions)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := artifactsA.Create(context.Background(), session.ID, "", ArtifactInput{Filename: "a.txt", MediaType: "text/plain", Content: []byte("a")}); err != nil {
		t.Fatal(err)
	}
	if _, err := artifactsB.Create(context.Background(), session.ID, "", ArtifactInput{Filename: "b.txt", MediaType: "text/plain", Content: []byte("b")}); err != nil {
		t.Fatal(err)
	}
	got, err := artifactsA.List(session.ID)
	if err != nil || len(got) != 2 {
		t.Fatalf("cross-instance list = %#v, %v", got, err)
	}
}

func TestScopedIndexSnapshotStaysStaleAfterConcurrentSourceMutation(t *testing.T) {
	root := t.TempDir()
	if err := ensureScopedIndexDir(root); err != nil {
		t.Fatal(err)
	}
	sessionID := SessionID("snapshot-race")
	before, err := scopedSourceModTime(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "new-authoritative-record"), 0o700); err != nil {
		t.Fatal(err)
	}
	after, err := scopedSourceModTime(root)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		stamp := time.Now().Add(time.Second)
		if err := os.Chtimes(root, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeScopedIndexAt(root, sessionID, []string{"old"}, before); err != nil {
		t.Fatal(err)
	}
	if index, ok, err := readScopedIndex(root, sessionID); err != nil || ok {
		t.Fatalf("stale snapshot accepted: index=%#v ok=%v err=%v", index, ok, err)
	}
}

func BenchmarkArtifactStoreListUsesSessionIndex(b *testing.B) {
	root := b.TempDir()
	sessions, err := NewSessionStore(filepath.Join(root, "sessions"))
	if err != nil {
		b.Fatal(err)
	}
	session, err := sessions.Create(SessionMetadata{})
	if err != nil {
		b.Fatal(err)
	}
	artifacts, err := NewArtifactStore(filepath.Join(root, "artifacts"), sessions)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if _, err := artifacts.Create(context.Background(), session.ID, "", ArtifactInput{Filename: "item.txt", MediaType: "text/plain", Content: []byte("payload")}); err != nil {
			b.Fatal(err)
		}
	}
	if _, err := artifacts.List(session.ID); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := artifacts.List(session.ID); err != nil {
			b.Fatal(err)
		}
	}
}
