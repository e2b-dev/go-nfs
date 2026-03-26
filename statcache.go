package nfs

import (
	"context"
	"os"
	"sync"

	"github.com/go-git/go-billy/v5"
)

// skipPostOpAttrsKey is the context key for skipping post-op attributes.
type skipPostOpAttrsKey struct{}

// ContextWithSkipPostOpAttrs returns a context that signals handlers to skip
// post-op attribute fetching. This can improve performance when clients don't
// need the post-op attributes and can issue GETATTR calls if needed.
func ContextWithSkipPostOpAttrs(ctx context.Context) context.Context {
	return context.WithValue(ctx, skipPostOpAttrsKey{}, true)
}

// ShouldSkipPostOpAttrs returns true if the context indicates post-op
// attributes should be skipped.
func ShouldSkipPostOpAttrs(ctx context.Context) bool {
	if v, ok := ctx.Value(skipPostOpAttrsKey{}).(bool); ok {
		return v
	}
	return false
}

// statCacheKey is the context key for the stat cache.
type statCacheKey struct{}

// StatCache provides per-request caching of stat results to avoid
// redundant filesystem calls within a single NFS operation.
type StatCache struct {
	mu    sync.RWMutex
	cache map[string]statCacheEntry
}

type statCacheEntry struct {
	info os.FileInfo
	err  error
}

// NewStatCache creates a new stat cache.
func NewStatCache() *StatCache {
	return &StatCache{
		cache: make(map[string]statCacheEntry),
	}
}

// ContextWithStatCache returns a new context with a stat cache attached.
func ContextWithStatCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, statCacheKey{}, NewStatCache())
}

// StatCacheFromContext retrieves the stat cache from the context, or nil if none.
func StatCacheFromContext(ctx context.Context) *StatCache {
	if cache, ok := ctx.Value(statCacheKey{}).(*StatCache); ok {
		return cache
	}
	return nil
}

// Get retrieves a cached stat result, returning (info, err, true) if found,
// or (nil, nil, false) if not cached.
func (c *StatCache) Get(path string) (os.FileInfo, error, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if entry, ok := c.cache[path]; ok {
		return entry.info, entry.err, true
	}
	return nil, nil, false
}

// Put stores a stat result in the cache.
func (c *StatCache) Put(path string, info os.FileInfo, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache[path] = statCacheEntry{info: info, err: err}
}

// Invalidate removes a path from the cache (useful after mutations).
func (c *StatCache) Invalidate(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.cache, path)
}

// CachedLstat performs an Lstat, using the cache if available.
func CachedLstat(ctx context.Context, fs billy.Filesystem, path string) (os.FileInfo, error) {
	if cache := StatCacheFromContext(ctx); cache != nil {
		if info, err, ok := cache.Get(path); ok {
			return info, err
		}
		info, err := fs.Lstat(path)
		cache.Put(path, info, err)
		return info, err
	}
	return fs.Lstat(path)
}

// CachedStat performs a Stat, using the cache if available.
func CachedStat(ctx context.Context, fs billy.Filesystem, path string) (os.FileInfo, error) {
	if cache := StatCacheFromContext(ctx); cache != nil {
		if info, err, ok := cache.Get(path); ok {
			return info, err
		}
		info, err := fs.Stat(path)
		cache.Put(path, info, err)
		return info, err
	}
	return fs.Stat(path)
}

// InvalidatePath removes a path from the stat cache (call after mutations).
func InvalidatePath(ctx context.Context, path string) {
	if cache := StatCacheFromContext(ctx); cache != nil {
		cache.Invalidate(path)
	}
}
