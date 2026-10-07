package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// fakeUpstream 起一个本地 HTTP 代理，冒充“日本节点出去后的 DMM 源站”。
// Go 的 Transport 走 http 代理访问 http:// 目标时发的是绝对 URL，正好被它接住。
type fakeUpstream struct {
	*httptest.Server
	data []byte

	mu      sync.Mutex
	hits    int
	methods []string
	absURLs []string
}

func newFakeUpstream(t *testing.T, size int) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{data: make([]byte, size)}
	for i := range f.data {
		f.data[i] = byte(i % 251)
	}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits++
		f.methods = append(f.methods, r.Method)
		f.absURLs = append(f.absURLs, r.URL.String())
		f.mu.Unlock()

		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Cache-Control", "no-store, no-cache")
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(f.data)))
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(f.data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(f.data)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeUpstream) URL() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Server.URL
}

func (f *fakeUpstream) hitsCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

func (f *fakeUpstream) methodList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.methods...)
	return out
}

func (f *fakeUpstream) reset() {
	f.mu.Lock()
	f.hits, f.methods, f.absURLs = 0, nil, nil
	f.mu.Unlock()
}

const testPath = "/litevideo/freepv/1/118/118abp888/118abp888_mhb_w.mp4"

func newTestServer(t *testing.T, up *fakeUpstream, maxBytes int64) (*Server, *httptest.Server, string) {
	t.Helper()
	u, _ := url.Parse(up.URL())
	cfg := &Config{
		HostSuffix:        "example.com",
		UpstreamSuffix:    ".dmm.co.jp",
		UpstreamScheme:    "http",
		PathPrefixes:      []string{"/litevideo/freepv/", "/pv/"},
		AllowedCDN:        map[string]bool{"cc3001": true},
		ProxyURL:          u,
		CacheDir:          t.TempDir(),
		CacheMaxBytes:     maxBytes,
		CacheMinFreeBytes: 0,
		CacheMaxDownloads: 2,
		MinBodyBytes:      16,
		UA:                "test",
		Referer:           "https://www.dmm.co.jp/",
		Origin:            "https://www.dmm.co.jp",
		AcceptLanguage:    "ja-JP",
		CacheControl:      "public, max-age=31536000, immutable",
		UpstreamTimeout:   30 * time.Second,
		FlightWait:        10 * time.Second,
		QuotaFactor:       1, // 测试里按真实字节计量，方便断言
		QuotaDailyBytes:   0, // 默认不限
		QuotaMonthlyBytes: 0,
		RateLimitPerMin:   0,
	}
	cache, err := NewDiskCache(cfg)
	if err != nil {
		t.Fatalf("NewDiskCache: %v", err)
	}
	quota, err := NewQuota(cfg)
	if err != nil {
		t.Fatalf("NewQuota: %v", err)
	}
	s := &Server{cfg: cfg, cache: cache, up: newUpstreamClient(cfg),
		quota: quota, limiter: newRateLimiter(cfg.RateLimitPerMin)}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return s, srv, cfg.CacheDir
}

// mp4Files 列出缓存目录里已提交的缓存文件。
func mp4Files(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读缓存目录 %s: %v", dir, err)
	}
	var out []string
	for _, e := range ents {
		if filepath.Ext(e.Name()) == ".mp4" {
			out = append(out, e.Name())
		}
	}
	return out
}

// waitFiles 等到缓存目录里有 n 个 .mp4。
// 客户端读完 body 时服务端的 fsync+rename 可能还没落定，必须轮询。
func waitFiles(t *testing.T, dir string, n int) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		files := mp4Files(t, dir)
		if len(files) >= n {
			return files
		}
		if time.Now().After(deadline) {
			return files
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func do(t *testing.T, srv *httptest.Server, method, host, path string, hdr http.Header) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, nil)
	req.Host = host
	for k, v := range hdr {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func readAll(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("读 body: %v", err)
	}
	return b
}

// ── 冒烟 1：首次回源落盘，第二次命中磁盘且零回源 ──
func TestSmoke_CacheHitOnSecondRequest(t *testing.T) {
	up := newFakeUpstream(t, 1<<20)
	_, srv, dir := newTestServer(t, up, 8<<20)

	r1 := do(t, srv, http.MethodGet, "cc3001.example.com", testPath, nil)
	body1 := readAll(t, r1)
	if r1.StatusCode != 200 {
		t.Fatalf("首次状态码 %d", r1.StatusCode)
	}
	if !bytes.Equal(body1, up.data) {
		t.Fatal("首次 body 与源站不一致")
	}
	if got := r1.Header.Get("X-Cache"); got != "upstream" {
		t.Errorf("首次 X-Cache=%q want upstream", got)
	}
	if got := r1.Header.Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control 未被覆盖: %q", got)
	}
	if got := r1.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Errorf("Accept-Ranges=%q want bytes", got)
	}

	if files := waitFiles(t, dir, 1); len(files) != 1 {
		t.Fatalf("落盘文件数 %d want 1", len(files))
	}

	r2 := do(t, srv, http.MethodGet, "cc3001.example.com", testPath, nil)
	body2 := readAll(t, r2)
	if got := r2.Header.Get("X-Cache"); got != "disk" {
		t.Errorf("二次 X-Cache=%q want disk", got)
	}
	if !bytes.Equal(body2, up.data) {
		t.Fatal("命中路径 body 不一致")
	}
	if up.hitsCount() != 1 {
		t.Errorf("上游被请求 %d 次，应为 1（核心指标）", up.hitsCount())
	}
}

// ── 冒烟 2：首次就带 Range，返回 206 且仍整份落盘 ──
func TestSmoke_RangeOnFirstRequest(t *testing.T) {
	up := newFakeUpstream(t, 1<<20)
	_, srv, dir := newTestServer(t, up, 8<<20)

	r := do(t, srv, http.MethodGet, "cc3001.example.com", testPath,
		http.Header{"Range": []string{"bytes=100-199"}})
	body := readAll(t, r)
	if r.StatusCode != 206 {
		t.Fatalf("状态码 %d want 206", r.StatusCode)
	}
	if want := "bytes 100-199/1048576"; r.Header.Get("Content-Range") != want {
		t.Errorf("Content-Range=%q want %q", r.Header.Get("Content-Range"), want)
	}
	if !bytes.Equal(body, up.data[100:200]) {
		t.Errorf("Range body 长度 %d want 100", len(body))
	}
	// 虽然只给了 100 字节，整份仍应落盘
	if files := waitFiles(t, dir, 1); len(files) != 1 {
		t.Fatalf("整份文件未落盘（当前 %d 个）", len(files))
	}

	// 命中后再要另一段 Range
	r2 := do(t, srv, http.MethodGet, "cc3001.example.com", testPath,
		http.Header{"Range": []string{"bytes=0-9"}})
	b2 := readAll(t, r2)
	if r2.StatusCode != 206 || !bytes.Equal(b2, up.data[:10]) {
		t.Errorf("命中后 Range 失败: %d %d 字节", r2.StatusCode, len(b2))
	}
	if up.hitsCount() != 1 {
		t.Errorf("上游被请求 %d 次 want 1", up.hitsCount())
	}
}

// ── 冒烟 3：HEAD 不落盘、不触发整份下载 ──
func TestSmoke_HeadDoesNotCache(t *testing.T) {
	up := newFakeUpstream(t, 1<<20)
	_, srv, dir := newTestServer(t, up, 8<<20)

	r := do(t, srv, http.MethodHead, "cc3001.example.com", testPath, nil)
	readAll(t, r)
	if r.StatusCode != 200 {
		t.Fatalf("HEAD 状态码 %d", r.StatusCode)
	}
	if r.Header.Get("Content-Length") != "1048576" {
		t.Errorf("HEAD Content-Length=%q", r.Header.Get("Content-Length"))
	}
	if got := r.Header.Get("X-Cache"); got != "passthrough:head" {
		t.Errorf("HEAD X-Cache=%q want passthrough:head", got)
	}
	if m := up.methodList(); len(m) != 1 || m[0] != http.MethodHead {
		t.Errorf("上游收到 %v，应该只有一次 HEAD", m)
	}
	files := mp4Files(t, dir)
	if len(files) != 0 {
		t.Errorf("HEAD 不应落盘，实际 %d 个文件", len(files))
	}
}

// ── 冒烟 4：非法 Host / 路径 → 403，且完全不出网 ──
func TestSmoke_ForbiddenNoEgress(t *testing.T) {
	up := newFakeUpstream(t, 1<<20)
	_, srv, _ := newTestServer(t, up, 8<<20)

	for _, c := range []struct{ host, path string }{
		{"cc9999.example.com", testPath},           // cdn 不在白名单
		{"cc3001.evil.com", testPath},              // 非本域
		{"cc3001.example.com", "/other/a_b_c.mp4"}, // 路径前缀错
		{"cc3001.example.com", "/litevideo/freepv/a.m3u8"},
		{"cc3001.example.com", "/litevideo/freepv/../../etc/x.mp4"},
	} {
		r := do(t, srv, http.MethodGet, c.host, c.path, nil)
		readAll(t, r)
		if r.StatusCode != 403 {
			t.Errorf("%s%s 状态码 %d want 403", c.host, c.path, r.StatusCode)
		}
	}
	if up.hitsCount() != 0 {
		t.Errorf("非法请求产生了 %d 次出网", up.hitsCount())
	}
}

// ── 冒烟 5：并发同 URL 只回源一次 ──
func TestSmoke_SingleFlight(t *testing.T) {
	up := newFakeUpstream(t, 1<<20)
	_, srv, _ := newTestServer(t, up, 8<<20)

	const n = 20
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := do(t, srv, http.MethodGet, "cc3001.example.com", testPath, nil)
			b := readAll(t, r)
			codes[i] = r.StatusCode
			if !bytes.Equal(b, up.data) {
				t.Errorf("第 %d 个请求 body 不完整 (%d 字节)", i, len(b))
			}
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != 200 && c != 206 {
			t.Errorf("第 %d 个请求状态码 %d", i, c)
		}
	}
	if got := up.hitsCount(); got != 1 {
		t.Errorf("并发 %d 请求产生 %d 次回源，应为 1", n, got)
	}
}

// ── 冒烟 6：档位白名单外不落盘，但仍能正常播放 ──
func TestSmoke_QualityFilter(t *testing.T) {
	up := newFakeUpstream(t, 1<<20)
	s, srv, dir := newTestServer(t, up, 8<<20)
	s.cfg.CacheQualityAllow = map[string]bool{"sm": true, "dm": true} // mhb 不在内

	r := do(t, srv, http.MethodGet, "cc3001.example.com", testPath, nil)
	b := readAll(t, r)
	if r.StatusCode != 200 || !bytes.Equal(b, up.data) {
		t.Fatalf("非缓存档位也应正常返回: %d", r.StatusCode)
	}
	if got := r.Header.Get("X-Cache"); got != "upstream-nocache" {
		t.Errorf("X-Cache=%q want upstream-nocache", got)
	}
	files := mp4Files(t, dir)
	if len(files) != 0 {
		t.Errorf("档位外不应落盘，实际 %d 个", len(files))
	}
}

// ── 冒烟 7：源站 404 透传且不缓存负结果 ──
func TestSmoke_UpstreamErrorPassthrough(t *testing.T) {
	up := newFakeUpstream(t, 1<<20)
	_, srv, dir := newTestServer(t, up, 8<<20)
	// 改成永远 404
	up.Server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.mu.Lock()
		up.hits++
		up.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	})

	r := do(t, srv, http.MethodGet, "cc3001.example.com", testPath, nil)
	readAll(t, r)
	if r.StatusCode != 404 {
		t.Errorf("状态码 %d want 404", r.StatusCode)
	}
	if got := r.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("错误响应 Cache-Control=%q want no-store", got)
	}
	if files := mp4Files(t, dir); len(files) != 0 {
		t.Error("错误响应不应落盘")
	}
}

// ── 冒烟 8：LRU 淘汰 ──
func TestSmoke_Eviction(t *testing.T) {
	up := newFakeUpstream(t, 1<<20)
	s, srv, _ := newTestServer(t, up, 3<<20) // 3MB，约能放 3 个 1MB 文件

	for i := 0; i < 6; i++ {
		p := fmt.Sprintf("/litevideo/freepv/v%d/video_%d_sm_w.mp4", i, i)
		r := do(t, srv, http.MethodGet, "cc3001.example.com", p, nil)
		readAll(t, r)
		if r.StatusCode != 200 {
			t.Fatalf("第 %d 个请求状态码 %d", i, r.StatusCode)
		}
		time.Sleep(10 * time.Millisecond) // 保证 Accessed 有序
	}
	n, total := s.cache.Stats()
	if total > 3<<20 {
		t.Errorf("淘汰后占用 %.2fMB 超过 3MB", float64(total)/1e6)
	}
	if n > 3 {
		t.Errorf("淘汰后仍有 %d 个文件", n)
	}
	ents, _ := os.ReadDir(s.cfg.CacheDir)
	var parts int
	for _, e := range ents {
		if filepath.Ext(e.Name()) == ".part" {
			parts++
		}
	}
	if parts != 0 {
		t.Errorf("残留 %d 个 .part", parts)
	}
}
