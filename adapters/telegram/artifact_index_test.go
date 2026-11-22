package telegram

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestArtifactIndexUpsertGetAndExpire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "artifacts.json")
	index, err := NewArtifactIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	record := ArtifactRecord{Reference: "tg-attachment-v1-" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Kind: "document", Filename: "a.pdf", ContentType: "application/pdf", SizeBytes: 4, SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(-time.Minute), State: ArtifactStaged}
	if err := index.Upsert(record); err != nil {
		t.Fatal(err)
	}
	got, err := index.Get(record.Reference)
	if err != nil || got.State != ArtifactStaged {
		t.Fatalf("got=%#v err=%v", got, err)
	}
	expired, err := index.Expire(time.Now().UTC())
	if err != nil || len(expired) != 1 {
		t.Fatalf("expired=%#v err=%v", expired, err)
	}
	got, err = index.Get(record.Reference)
	if err != nil || got.State != ArtifactExpired {
		t.Fatalf("expired record=%#v err=%v", got, err)
	}
	records, err := index.Records(time.Now().UTC())
	if err != nil || len(records) != 0 {
		t.Fatalf("records=%#v err=%v", records, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("index mode/info=%v %#v", err, info)
	}
}
