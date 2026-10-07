package main

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// CacheEntry 是一条已完成的本地缓存记录。
type CacheEntry struct {
	Key      string
	Path     string
	Size     int64
	ModTime  time.Time // 下载完成时间
	Accessed time.Time // 内存态 LRU 依据
	refs     int       // 正在被服务的人数，>0 时不允许淘汰
}

func (e *CacheEntry) addRef() { e.refs++ }
func (e *CacheEntry) unRef()  { e.refs-- }

// flight 是同 key 的一次回源下载。leader 负责真正下载，follower 等它完成。
type flight struct {
	done  chan struct{}
	once  sync.Once
	err   error
	entry *CacheEntry
}

func (f *flight) finish(e *CacheEntry, err error) {
	f.once.Do(func() {
		f.entry, f.err = e, err
		close(f.done)
	})
}

// DiskCache：本地磁盘 LRU + single-flight。
type DiskCache struct {
	dir          string
	maxBytes     int64
	minFreeBytes int64
	maxDownloads int

	mu      sync.Mutex
	entries map[string]*CacheEntry
	total   int64
	flights map[string]*flight

	sem chan struct{} // 并发回源闸门
}

func NewDiskCache(cfg *Config) (*DiskCache, error) {
	if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
		return nil, err
	}
	c := &DiskCache{
		dir:          cfg.CacheDir,
		maxBytes:     cfg.CacheMaxBytes,
		minFreeBytes: cfg.CacheMinFreeBytes,
		maxDownloads: cfg.CacheMaxDownloads,
		entries:      make(map[string]*CacheEntry),
		flights:      make(map[string]*flight),
		sem:          make(chan struct{}, cfg.CacheMaxDownloads),
	}
	if err := c.scan(); err != nil {
		return nil, err
	}
	log.Printf("cache: dir=%s entries=%d used=%.1fMB max=%.1fMB",
		c.dir, len(c.entries), float64(c.total)/1e6, float64(c.maxBytes)/1e6)
	return c, nil
}

// Key 由完整上游 URL 派生。
func Key(upstream string) string {
	sum := sha1.Sum([]byte(upstream))
	return hex.EncodeToString(sum[:])
}

func (c *DiskCache) entryPath(key string) string {
	return filepath.Join(c.dir, key+".mp4")
}

func (c *DiskCache) partPath(key string) string {
	return filepath.Join(c.dir, key+".part")
}

// scan 启动时重建索引；顺手清掉上次崩溃残留的 .part。
func (c *DiskCache) scan() error {
	items, err := os.ReadDir(c.dir)
	if err != nil {
		return err
	}
	for _, it := range items {
		name := it.Name()
		if filepath.Ext(name) == ".part" {
			// 上次进程被杀留下的半成品，直接删
			_ = os.Remove(filepath.Join(c.dir, name))
			continue
		}
		if filepath.Ext(name) != ".mp4" {
			continue
		}
		fi, err := it.Info()
		if err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
			continue
		}
		key := name[:len(name)-len(".mp4")]
		c.entries[key] = &CacheEntry{
			Key: key, Path: filepath.Join(c.dir, name),
			Size: fi.Size(), ModTime: fi.ModTime(), Accessed: fi.ModTime(),
		}
		c.total += fi.Size()
	}
	return nil
}

// Lookup 命中本地完整文件。返回的 entry 已经 +ref，调用方必须 release。
func (c *DiskCache) Lookup(key string) (*CacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	// 文件可能被外部删了
	if fi, err := os.Stat(e.Path); err != nil || fi.Size() != e.Size {
		delete(c.entries, key)
		c.total -= e.Size
		return nil, false
	}
	e.Accessed = time.Now()
	e.addRef()
	return e, true
}

func (c *DiskCache) release(e *CacheEntry) {
	c.mu.Lock()
	e.unRef()
	c.mu.Unlock()
}

// JoinOrStart：命中已有缓存直接返回；否则加入/创建一次回源。
// isLeader=true 时调用方必须调用 flight.finish()。
func (c *DiskCache) JoinOrStart(key string) (f *flight, isLeader bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[key]; ok {
		// 极短窗口内刚被别人下完
		f := &flight{done: make(chan struct{})}
		e := c.entries[key]
		f.finish(e, nil)
		return f, false
	}
	if existing, ok := c.flights[key]; ok {
		return existing, false
	}
	f = &flight{done: make(chan struct{})}
	c.flights[key] = f
	return f, true
}

// acquireSlot 占用一个回源并发名额。拿到后调用方必须 releaseSlot。
func (c *DiskCache) acquireSlot(ctx context.Context) error {
	select {
	case c.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *DiskCache) releaseSlot() { <-c.sem }

// Writer 是一个“边下边落盘”的目标文件。只有 Commit 之后才对后续请求可见。
type Writer struct {
	c      *DiskCache
	key    string
	part   string
	file   *os.File
	writen int64
	closed bool
}

// NewWriter 创建 .part 文件。磁盘剩余空间不足时返回 (nil, nil)，表示“本次不落盘”。
func (c *DiskCache) NewWriter(key string) (*Writer, error) {
	if free, err := freeBytes(c.dir); err == nil && free < c.minFreeBytes {
		log.Printf("cache: 磁盘剩余 %.1fGB < 阈值 %.1fGB，本次不落盘",
			float64(free)/1e9, float64(c.minFreeBytes)/1e9)
		return nil, nil
	}
	part := c.partPath(key)
	f, err := os.Create(part)
	if err != nil {
		return nil, err
	}
	return &Writer{c: c, key: key, part: part, file: f}, nil
}

func (w *Writer) Write(p []byte) (int, error) {
	n, err := w.file.Write(p)
	w.writen += int64(n)
	return n, err
}

// Commit 校验字节数后原子 rename 入索引。expect>0 时会比对 content-length。
func (w *Writer) Commit(expect int64) (*CacheEntry, error) {
	if w.closed {
		return nil, errors.New("writer 已关闭")
	}
	w.closed = true
	if err := w.file.Sync(); err != nil {
		w.abort()
		return nil, err
	}
	if err := w.file.Close(); err != nil {
		_ = os.Remove(w.part)
		return nil, err
	}
	if expect > 0 && w.writen != expect {
		_ = os.Remove(w.part)
		return nil, fmt.Errorf("落盘不完整: got %d want %d", w.writen, expect)
	}
	if w.writen == 0 {
		_ = os.Remove(w.part)
		return nil, errors.New("空文件")
	}

	dst := w.c.entryPath(w.key)
	if err := os.Rename(w.part, dst); err != nil {
		_ = os.Remove(w.part)
		return nil, err
	}
	return w.c.admit(w.key, dst, w.writen)
}

func (w *Writer) abort() {
	if w.closed {
		return
	}
	w.closed = true
	_ = w.file.Close()
	_ = os.Remove(w.part)
}

// Abort 丢弃半成品。
func (w *Writer) Abort() { w.abort() }

// admit 把新文件纳入索引，并在超限时按 LRU 淘汰到 80%。
func (c *DiskCache) admit(key, path string, size int64) (*CacheEntry, error) {
	now := time.Now()
	e := &CacheEntry{Key: key, Path: path, Size: size, ModTime: now, Accessed: now}

	c.mu.Lock()
	if old, ok := c.entries[key]; ok { // 理论上不会
		c.total -= old.Size
	}
	c.entries[key] = e
	c.total += size
	c.mu.Unlock()

	c.evict()
	return e, nil
}

func (c *DiskCache) evict() {
	c.mu.Lock()
	if c.total <= c.maxBytes {
		c.mu.Unlock()
		return
	}
	target := int64(float64(c.maxBytes) * 0.8)
	type kv struct {
		k string
		e *CacheEntry
	}
	all := make([]kv, 0, len(c.entries))
	for k, e := range c.entries {
		all = append(all, kv{k, e})
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].e.Accessed.Before(all[j].e.Accessed)
	})
	var freed int64
	var n int
	for _, it := range all {
		if c.total <= target {
			break
		}
		if it.e.refs > 0 { // 正在服务，跳过
			continue
		}
		if err := os.Remove(it.e.Path); err != nil && !os.IsNotExist(err) {
			continue
		}
		delete(c.entries, it.k)
		c.total -= it.e.Size
		freed += it.e.Size
		n++
	}
	c.mu.Unlock()
	if n > 0 {
		log.Printf("cache: LRU 淘汰 %d 个文件，释放 %.1fMB，当前 %.1fMB",
			n, float64(freed)/1e6, float64(c.total)/1e6)
	}
}

// Stats 供 /_stats 之类的内部接口用。
func (c *DiskCache) Stats() (count int, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries), c.total
}

// finish 结束一次回源并唤醒所有 follower。leader 必须调用。
func (c *DiskCache) finish(key string, e *CacheEntry, err error) {
	c.mu.Lock()
	f := c.flights[key]
	delete(c.flights, key)
	c.mu.Unlock()
	if f != nil {
		f.finish(e, err)
	}
}
