package helpers

import (
	"io/fs"
	"testing"
	"time"

	"github.com/willscott/go-nfs/file"
)

// dirEntry is a fake directory entry that carries a file ID, like a real stat result does.
type dirEntry struct {
	name   string
	size   int64
	fileid uint64
}

func (e dirEntry) Name() string       { return e.name }
func (e dirEntry) Size() int64        { return e.size }
func (e dirEntry) Mode() fs.FileMode  { return 0o644 }
func (e dirEntry) ModTime() time.Time { return time.Time{} }
func (e dirEntry) IsDir() bool        { return false }
func (e dirEntry) Sys() any           { return file.FileInfo{Fileid: e.fileid} }

// Two filesystems can have a directory with the same path and file names. Each must get
// its own verifier, otherwise one would be shown the other's files.
func TestVerifierForTellsApartFilesystemsWithTheSameNames(t *testing.T) {
	c := NewCachingHandler(nil, 16).(*CachingHandler)

	onA := []fs.FileInfo{dirEntry{name: "main.go", size: 10, fileid: 5001}}
	onB := []fs.FileInfo{dirEntry{name: "main.go", size: 99000, fileid: 7002}}

	verifierA := c.VerifierFor("/src", onA)
	verifierB := c.VerifierFor("/src", onB)
	if verifierA == verifierB {
		t.Fatalf("listings with different file IDs share verifier %x", verifierA)
	}

	got := c.DataForVerifier("/src", verifierA)
	if len(got) != 1 || got[0].Size() != 10 {
		t.Errorf("listing for A = %v, want A's main.go of size 10", got)
	}
}

func TestDataForVerifierChecksThePath(t *testing.T) {
	c := NewCachingHandler(nil, 16).(*CachingHandler)

	verifier := c.VerifierFor("/src", []fs.FileInfo{dirEntry{name: "main.go", fileid: 1}})
	if got := c.DataForVerifier("/other", verifier); got != nil {
		t.Errorf("verifier for /src returned a listing for /other: %v", got)
	}
}
