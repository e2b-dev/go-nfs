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

// statType distinguishes between Lstat and Stat results in the cache.
// This is important because they return different results for symlinks:
// - Lstat returns info about the symlink itself
// - Stat follows the symlink and returns info about the target
type statType int

const (
	statTypeLstat statType = iota
	statTypeStat
)

// statCacheKeyEntry is the composite key for the stat cache.
type statCacheKeyEntry struct {
	path string
	typ  statType
}

// StatCache provides per-request caching of stat results to avoid
// redundant filesystem calls within a single NFS operation.
// Lstat and Stat results are cached separately since they return
// different results for symlinks.
type StatCache struct {
	mu    sync.RWMutex
	cache map[statCacheKeyEntry]statCacheEntry
}

type statCacheEntry struct {
	info os.FileInfo
	err  error
}

// NewStatCache creates a new stat cache.
func NewStatCache() *StatCache {
	return &StatCache{
		cache: make(map[statCacheKeyEntry]statCacheEntry),
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

// get retrieves a cached stat result, returning (info, err, true) if found,
// or (nil, nil, false) if not cached.
func (c *StatCache) get(path string, typ statType) (os.FileInfo, error, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	key := statCacheKeyEntry{path: path, typ: typ}
	if entry, ok := c.cache[key]; ok {
		return entry.info, entry.err, true
	}
	return nil, nil, false
}

// put stores a stat result in the cache.
func (c *StatCache) put(path string, typ statType, info os.FileInfo, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := statCacheKeyEntry{path: path, typ: typ}
	c.cache[key] = statCacheEntry{info: info, err: err}
}

// invalidate removes a path from the cache for both Lstat and Stat.
func (c *StatCache) invalidate(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.cache, statCacheKeyEntry{path: path, typ: statTypeLstat})
	delete(c.cache, statCacheKeyEntry{path: path, typ: statTypeStat})
}

// CachedLstat performs an Lstat, using the cache if available.
func CachedLstat(ctx context.Context, fs billy.Filesystem, path string) (os.FileInfo, error) {
	if cache := StatCacheFromContext(ctx); cache != nil {
		if info, err, ok := cache.get(path, statTypeLstat); ok {
			return info, err
		}
		info, err := fs.Lstat(path)
		cache.put(path, statTypeLstat, info, err)
		return info, err
	}
	return fs.Lstat(path)
}

// CachedStat performs a Stat, using the cache if available.
func CachedStat(ctx context.Context, fs billy.Filesystem, path string) (os.FileInfo, error) {
	if cache := StatCacheFromContext(ctx); cache != nil {
		if info, err, ok := cache.get(path, statTypeStat); ok {
			return info, err
		}
		info, err := fs.Stat(path)
		cache.put(path, statTypeStat, info, err)
		return info, err
	}
	return fs.Stat(path)
}

// InvalidatePath removes a path from the stat cache (call after mutations).
// This invalidates both Lstat and Stat entries for the path.
func InvalidatePath(ctx context.Context, path string) {
	if cache := StatCacheFromContext(ctx); cache != nil {
		cache.invalidate(path)
	}
}
