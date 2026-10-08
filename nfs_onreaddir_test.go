package nfs

import (
	"context"
	"io/fs"
	"testing"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
)

// dirHandler serves the directory "dir" of fs for every handle.
type dirHandler struct {
	Handler
	fs billy.Filesystem
}

func (h *dirHandler) FromHandle(context.Context, []byte) (billy.Filesystem, []string, error) {
	return h.fs, []string{"dir"}, nil
}

// cachingDirHandler answers every verifier lookup with the same cached listing.
type cachingDirHandler struct {
	*dirHandler
	cached  []fs.FileInfo
	lookups int
}

func (h *cachingDirHandler) VerifierFor(string, []fs.FileInfo) uint64 { return 42 }

func (h *cachingDirHandler) DataForVerifier(string, uint64) []fs.FileInfo {
	h.lookups++
	return h.cached
}

// wrappingHandler stands in for middleware such as logging or panic recovery.
type wrappingHandler struct{ Handler }

func (w wrappingHandler) Unwrap() Handler { return w.Handler }

func newCachingDirHandler(t *testing.T) *cachingDirHandler {
	t.Helper()

	mem := memfs.New()
	f, err := mem.Create("dir/on-disk")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	cachedDir := memfs.New()
	f, err = cachedDir.Create("cached")
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	cached, err := cachedDir.ReadDir("/")
	if err != nil {
		t.Fatal(err)
	}

	return &cachingDirHandler{dirHandler: &dirHandler{fs: mem}, cached: cached}
}

func names(entries []fs.FileInfo) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestDirListingFindsCachingHandlerThroughWrappers(t *testing.T) {
	h := newCachingDirHandler(t)

	entries, verifier, err := getDirListingWithVerifier(context.Background(), wrappingHandler{h}, nil, 5, 42)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(entries); len(got) != 1 || got[0] != "cached" {
		t.Errorf("continuation page should come from the wrapped handler's cache, got %v", got)
	}
	if verifier != 42 {
		t.Errorf("verifier = %d, want 42", verifier)
	}
}

func TestDirListingFromCookieZeroReadsTheDirectory(t *testing.T) {
	h := newCachingDirHandler(t)

	entries, _, err := getDirListingWithVerifier(context.Background(), h, nil, 0, 42)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(entries); len(got) != 1 || got[0] != "on-disk" {
		t.Errorf("a listing from cookie zero should read the directory, got %v", got)
	}
	if h.lookups != 0 {
		t.Errorf("verifier cache consulted %d times for a cookie-zero listing", h.lookups)
	}
}
