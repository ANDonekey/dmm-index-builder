package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Server struct {
	cfg     *Config
	cache   *DiskCache
	up      *http.Client
	quota   *Quota
	limiter *rateLimiter
}

func main() {
	cfg, err := LoadConfig()
	if err != nil {
		log.Fatalf("配置错误: %v", err)
	}
	cache, err := NewDiskCache(cfg)
	if err != nil {
		log.Fatalf("缓存初始化失败: %v", err)
	}
	quota, err := NewQuota(cfg)
	if err != nil {
		log.Fatalf("配额初始化失败: %v", err)
	}
	s := &Server{cfg: cfg, cache: cache, up: newUpstreamClient(cfg),
		quota: quota, limiter: newRateLimiter(cfg.RateLimitPerMin)}

	// 配额每 30 秒落盘一次；收到终止信号时立即落盘再退出
	stop := make(chan struct{})
	go quota.Run(stop)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Print("收到终止信号，保存配额后退出")
		close(stop)
		quota.Flush()
		os.Exit(0)
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/_stats", s.handleStats)
	mux.Handle("/", s)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
		// 41MB 流式传输期间不能设 WriteTimeout
	}
	log.Printf("dmm-proxy 监听 %s | 上游 %s%s | 代理 %s | 缓存 %s (上限 %.1fGB)",
		cfg.Listen, "<cdn>", cfg.UpstreamSuffix, cfg.ProxyURL.String(),
		cfg.CacheDir, float64(cfg.CacheMaxBytes)/1e9)
	log.Fatal(srv.ListenAndServe())
}

func newUpstreamClient(cfg *Config) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(cfg.ProxyURL),
			DialContext: (&net.Dialer{
				Timeout:   15 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			DisableCompression:    true, // 视频不需要 gzip，且避免解压开销
			MaxIdleConnsPerHost:   8,
			ForceAttemptHTTP2:     true,
		},
		// 不跟随重定向：跨站跳转可能指向非 DMM，自己接管校验
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 0, // 下载超时由每个请求自己的 context 控制
	}
}

// ─────────────────────────── 主流程 ───────────────────────────

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	upstream, ok := s.resolve(r)
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	key := Key(upstream)

	// ① 本地磁盘命中 —— 零代理流量
	if e, hit := s.cache.Lookup(key); hit {
		defer s.cache.release(e)
		f, err := os.Open(e.Path)
		if err == nil {
			defer f.Close()
			s.setCommonHeaders(w.Header())
			w.Header().Set("X-Cache", "disk")
			http.ServeContent(w, r, e.Key+".mp4", e.ModTime, f)
			return
		}
	}

	// ② HEAD 且不命中：只探上游头，绝不为一个 HEAD 触发 41MB 下载
	if r.Method == http.MethodHead {
		s.passthrough(w, r, upstream, "head")
		return
	}

	// ③ GET 且未命中：single-flight 回源 + 落盘
	f, isLeader := s.cache.JoinOrStart(key)
	if !isLeader {
		s.follow(w, r, upstream, key, f)
		return
	}
	s.lead(w, r, upstream, key)
}

// resolve 校验并把请求映射成上游 URL。任一条件不满足即 false（不回源、不出网）。
func (s *Server) resolve(r *http.Request) (upstream string, ok bool) {
	cfg := s.cfg

	// 路径：前缀 + .mp4 + 字符集白名单（字符集校验天然排除 .. 与 %2e 编码绕过）
	p := r.URL.Path
	if !strings.HasSuffix(p, ".mp4") {
		return "", false
	}
	// 多前缀：/litevideo/freepv/（老体系）与 /pv/（新体系，token 那层）。
	// 命中哪个前缀，上游就原样拼哪个，不要归一成同一个。
	var body string
	matched := false
	for _, pre := range cfg.PathPrefixes {
		if strings.HasPrefix(p, pre) {
			body = p[len(pre) : len(p)-len(".mp4")]
			matched = true
			break
		}
	}
	if !matched {
		return "", false
	}
	// 不允许空段（否则能拼出 // 这种会被上游重新解读的路径）
	if body == "" || body[0] == '/' || strings.HasSuffix(body, "/") || strings.Contains(body, "//") {
		return "", false
	}
	for _, c := range body {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			// '+' 必须放行：/pv/ 的 token 是 base64url 风格，实测 90,619 个里
			// 有 5 个含 '+'。少了这一条这些预览会全部 403。
		case c == '_' || c == '-' || c == '/' || c == '+':
		default:
			return "", false
		}
	}

	// Host：必须是 <cdn>.<HOST_SUFFIX>，且 cdn 在白名单内
	host := strings.ToLower(r.Host)
	if i := strings.Index(host, ":"); i >= 0 {
		host = host[:i]
	}
	suffix := strings.ToLower(cfg.HostSuffix)
	if host != suffix && !strings.HasSuffix(host, "."+suffix) {
		return "", false
	}
	cdn := host
	if i := strings.Index(host, "."); i >= 0 {
		cdn = host[:i]
	}
	if !cfg.AllowedCDN[cdn] {
		return "", false
	}

	// 上游 host 只能由「白名单 cdn + 固定后缀」拼出，杜绝 SSRF
	return cfg.UpstreamScheme + "://" + cdn + cfg.UpstreamSuffix + p, true
}

// ─────────────────────────── leader 路径 ───────────────────────────

func (s *Server) lead(w http.ResponseWriter, r *http.Request, upstream, key string) {
	// 并发闸门
	if err := s.cache.acquireSlot(r.Context()); err != nil {
		s.cache.finish(key, nil, err)
		s.passthrough(w, r, upstream, "busy")
		return
	}
	defer s.cache.releaseSlot()

	// 回源频次限流：防的是「刷不同 URL」——每部片都只回源一次，缓存层挡不住
	if !s.limiter.Allow() {
		s.cache.finish(key, nil, errors.New("rate limited"))
		s.writeRateLimited(w)
		return
	}

	// 只有明确允许缓存的档位 / 磁盘有余量时才落盘
	var cw *Writer
	if s.allowCache(upstream) {
		var err error
		if cw, err = s.cache.NewWriter(key); err != nil {
			log.Printf("cache: 创建落盘文件失败 %s: %v", key, err)
		}
	}
	committed := false
	if cw != nil {
		defer func() {
			if !committed {
				cw.Abort() // 未 Commit 说明不完整，删掉半成品
			}
		}()
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.UpstreamTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream, nil)
	if err != nil {
		s.cache.finish(key, nil, err)
		http.Error(w, "bad upstream request", http.StatusBadGateway)
		return
	}
	s.setUpstreamHeaders(req.Header)

	resp, err := s.up.Do(req)
	if err != nil {
		s.cache.finish(key, nil, err)
		log.Printf("upstream: %s 失败: %v", upstream, err)
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// 非 200 直接透传（404/403 等），不落盘、不缓存负结果
	if resp.StatusCode != http.StatusOK {
		s.cache.finish(key, nil, fmt.Errorf("upstream %d", resp.StatusCode))
		s.writeUpstream(w, r, resp, "upstream-error")
		return
	}

	total := resp.ContentLength
	// 假 200 / 长度未知：不落盘，改成直通（直播式 chunked 也走这条）
	if total < s.cfg.MinBodyBytes {
		s.cache.finish(key, nil, fmt.Errorf("body too small or unknown: %d", total))
		s.writeUpstream(w, r, resp, "uncacheable")
		return
	}

	// ── 流量配额：整个服务里唯一会消耗节点流量的地方 ──
	billed := s.quota.Bill(total)
	if ok, scope := s.quota.Check(billed); !ok {
		resp.Body.Close()
		s.cache.finish(key, nil, fmt.Errorf("quota exceeded: %s", scope))
		s.writeQuotaExceeded(w, scope)
		return
	}
	snap := s.quota.Snap()
	s.quota.Reserve(billed, total)

	// 长度已知但不想缓存（档位/磁盘）：仍要完整读，但只转发给客户端
	if cw == nil {
		s.streamOnly(w, r, resp, total, snap)
		s.cache.finish(key, nil, errors.New("not cached"))
		return
	}

	// ── 正常路径：边下边落盘边返回 ──
	start, length, hasRange := int64(0), total, false
	if rh := r.Header.Get("Range"); rh != "" {
		if a, b, ok := parseRange(rh, total); ok {
			start, length, hasRange = a, b, true
		}
	}

	s.setCommonHeaders(w.Header())
	h := w.Header()
	h.Set("Content-Type", resp.Header.Get("Content-Type"))
	if v := resp.Header.Get("Last-Modified"); v != "" {
		h.Set("Last-Modified", v)
	}
	if v := resp.Header.Get("ETag"); v != "" {
		h.Set("ETag", v)
	}
	h.Set("X-Cache", "upstream")
	h.Del("Set-Cookie")

	if hasRange {
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+length-1, total))
		h.Set("Content-Length", strconv.FormatInt(length, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		h.Set("Content-Length", strconv.FormatInt(total, 10))
		w.WriteHeader(http.StatusOK)
	}
	if r.Method == http.MethodHead {
		return
	}

	sw := &switchWriter{w: w}
	var client io.Writer = sw
	if hasRange {
		client = &windowWriter{dst: sw, skip: int64(start), remain: length}
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-r.Context().Done():
			sw.disable() // 客户端断开 → 后续字节丢弃，但下载继续
		case <-stop:
		}
	}()

	// 边读边计数；单次回源字节超硬上限时由 quotaReader 直接掐断
	qr := &quotaReader{q: s.quota, r: resp.Body, maxBytes: s.cfg.QuotaMaxPerRequest}
	_, copyErr := io.Copy(io.MultiWriter(client, cw), qr)
	s.quota.Settle(snap, qr.n) // 按真实读到的字节回算，不用预扣值

	if copyErr != nil {
		s.cache.finish(key, nil, copyErr)
		if errors.Is(copyErr, errQuotaExceeded) {
			log.Printf("quota: %s 单次回源超过 %s 上限，已中断", upstream, humanBytes(s.cfg.QuotaMaxPerRequest, 0))
		} else {
			log.Printf("upstream: %s 下载中断: %v", upstream, copyErr)
		}
		return
	}
	entry, err := cw.Commit(total)
	if err != nil {
		s.cache.finish(key, nil, err)
		log.Printf("cache: %s 提交失败: %v", key, err)
		return
	}
	committed = true
	s.cache.finish(key, entry, nil)
	log.Printf("cache: 已缓存 %s (%.1fMB) | 本次回源 真实 %.1fMB / 计费 %.1fMB | 今日 %s",
		key, float64(entry.Size)/1e6, float64(qr.n)/1e6, float64(s.quota.Bill(qr.n))/1e6,
		s.quotaStatus())
}

// ─────────────────────────── follower 路径 ───────────────────────────

func (s *Server) follow(w http.ResponseWriter, r *http.Request, upstream, key string, f *flight) {
	select {
	case <-f.done:
	case <-time.After(s.cfg.FlightWait):
		s.passthrough(w, r, upstream, "wait-timeout")
		return
	case <-r.Context().Done():
		return
	}
	if f.err == nil {
		if e, ok := s.cache.Lookup(key); ok {
			defer s.cache.release(e)
			fh, err := os.Open(e.Path)
			if err == nil {
				defer fh.Close()
				s.setCommonHeaders(w.Header())
				w.Header().Set("X-Cache", "disk-follow")
				http.ServeContent(w, r, e.Key+".mp4", e.ModTime, fh)
				return
			}
		}
	}
	s.passthrough(w, r, upstream, "flight-failed")
}

// ─────────────────────────── 直通（不落盘） ───────────────────────────

func (s *Server) passthrough(w http.ResponseWriter, r *http.Request, upstream, reason string) {
	// 回源频次限流（HEAD 太轻，不计）
	if r.Method != http.MethodHead && !s.limiter.Allow() {
		s.writeRateLimited(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.UpstreamTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, r.Method, upstream, nil)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.setUpstreamHeaders(req.Header)
	// 直通时尊重客户端的 Range，不再强取整份
	if v := r.Header.Get("Range"); v != "" {
		req.Header.Set("Range", v)
	}
	if v := r.Header.Get("If-Range"); v != "" {
		req.Header.Set("If-Range", v)
	}
	resp, err := s.up.Do(req)
	if err != nil {
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	s.writeUpstream(w, r, resp, reason)
}

// streamOnly：不落盘（档位/磁盘原因），但仍完整读一遍并服务客户端 Range。
func (s *Server) streamOnly(w http.ResponseWriter, r *http.Request, resp *http.Response, total int64, snap QuotaSnap) {
	start, length, hasRange := int64(0), total, false
	if rh := r.Header.Get("Range"); rh != "" {
		if a, b, ok := parseRange(rh, total); ok {
			start, length, hasRange = a, b, true
		}
	}
	s.setCommonHeaders(w.Header())
	h := w.Header()
	h.Set("Content-Type", resp.Header.Get("Content-Type"))
	h.Set("X-Cache", "upstream-nocache")
	if hasRange {
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+length-1, total))
		h.Set("Content-Length", strconv.FormatInt(length, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		h.Set("Content-Length", strconv.FormatInt(total, 10))
		w.WriteHeader(http.StatusOK)
	}
	if r.Method == http.MethodHead {
		return
	}
	sw := &switchWriter{w: w}
	var client io.Writer = sw
	if hasRange {
		client = &windowWriter{dst: sw, skip: start, remain: length}
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-r.Context().Done():
			sw.disable()
		case <-stop:
		}
	}()
	qr := &quotaReader{q: s.quota, r: resp.Body, maxBytes: s.cfg.QuotaMaxPerRequest}
	_, _ = io.Copy(client, qr)
	s.quota.Settle(snap, qr.n)
}

// writeUpstream 原样透传上游响应（4xx/5xx/HEAD/不可缓存）。
func (s *Server) writeUpstream(w http.ResponseWriter, r *http.Request, resp *http.Response, reason string) {
	h := w.Header()
	for _, k := range []string{"Content-Type", "Content-Length", "Content-Range", "ETag", "Last-Modified"} {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	s.setCommonHeaders(h)
	if resp.StatusCode == http.StatusOK {
		h.Set("Cache-Control", s.cfg.CacheControl)
	} else {
		// 错误响应绝不能进任何一层缓存
		h.Set("Cache-Control", "no-store")
	}
	h.Set("X-Cache", "passthrough:"+reason)
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead && resp.Body != nil {
		// 直通也要记账（长度未知的 chunked 响应尤其需要 hard 熔断）
		qr := &quotaReader{q: s.quota, r: resp.Body, maxBytes: s.cfg.QuotaMaxPerRequest, hard: true}
		_, _ = io.Copy(w, qr)
		if qr.n > 0 {
			s.quota.Add(qr.n)
		}
	}
}

// ─────────────────────────── 限额响应 ───────────────────────────

// writeQuotaExceeded：节点流量配额用尽，拒绝这次回源。
// 已经缓存的内容照常能看（命中路径不走这里），只有新片会失败。
func (s *Server) writeQuotaExceeded(w http.ResponseWriter, scope string) {
	du, dl, mu, ml := s.quota.Usage()
	h := w.Header()
	h.Set("Cache-Control", "no-store") // 绝不能让 CF 缓存 429
	h.Set("X-Cache", "quota-exceeded:"+scope)
	h.Set("X-Quota-Daily", fmt.Sprintf("%d/%d", du, dl))
	h.Set("X-Quota-Monthly", fmt.Sprintf("%d/%d", mu, ml))
	h.Set("X-Quota-Factor", strconv.FormatFloat(s.cfg.QuotaFactor, 'f', 2, 64))
	h.Set("Access-Control-Allow-Origin", "*")
	if scope == "daily" {
		h.Set("Retry-After", "3600")
	}
	code := s.cfg.QuotaExceededCode
	if code < 400 || code > 599 {
		code = http.StatusTooManyRequests
	}
	log.Printf("quota: 配额用尽（%s）今日 %s / 本月 %s，拒绝回源",
		scope, humanBytes(du, dl), humanBytes(mu, ml))
	http.Error(w, "proxy traffic quota exceeded", code)
}

func (s *Server) writeRateLimited(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Cache", "rate-limited")
	h.Set("Retry-After", "60")
	log.Printf("quota: 回源频次达到 %d 次/分钟，拒绝本次回源", s.cfg.RateLimitPerMin)
	http.Error(w, "too many upstream requests", http.StatusTooManyRequests)
}

// quotaStatus 给日志用的一行摘要。
func (s *Server) quotaStatus() string {
	du, dl, mu, ml := s.quota.Usage()
	return fmt.Sprintf("%s | 本月 %s", humanBytes(du, dl), humanBytes(mu, ml))
}

// ─────────────────────────── 头处理 ───────────────────────────

func (s *Server) setUpstreamHeaders(h http.Header) {
	h.Set("User-Agent", s.cfg.UA)
	h.Set("Referer", s.cfg.Referer)
	h.Set("Origin", s.cfg.Origin)
	h.Set("Accept-Language", s.cfg.AcceptLanguage)
	h.Set("Accept", "*/*")
	// 其它客户端头一律不带（包括 Range —— 我们要完整文件）
	h.Del("Range")
	h.Del("If-Range")
	h.Del("Cookie")
}

// setCommonHeaders 覆盖源站的 no-store/no-cache，让 CF 边缘愿意缓存。
func (s *Server) setCommonHeaders(h http.Header) {
	h.Set("Cache-Control", s.cfg.CacheControl)
	h.Set("Accept-Ranges", "bytes")
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Cross-Origin-Resource-Policy", "cross-origin")
	h.Del("Set-Cookie") // CF 见到 Set-Cookie 就不缓存
}

// allowCache 档位白名单判断。空集合 = 全缓存。
func (s *Server) allowCache(upstream string) bool {
	if len(s.cfg.CacheQualityAllow) == 0 {
		return true
	}
	return s.cfg.CacheQualityAllow[fileQuality(upstream)]
}

// qualityInFile 匹配文件名尾部的「档位 + 可选变体 + .mp4」。
// 档位清单必须与 build-index.py 的 QUALITIES_DESC 保持一致（否则档位白名单会漏判）。
var qualityInFile = regexp.MustCompile(`(4k|hhb|hmb|mhb|mmb|dmb|dm|sm)[ws]?\.mp4$`)

// fileQuality 从文件名里取 quality，A/B 两种命名都要认：
//
//	A: {stem}_{quality}_{variant}.mp4   例 1sdjs206_mhb_w.mp4
//	B: {stem}{quality}{variant}.mp4     例 1sdjs00383mhb.mp4（variant 可缺省）
//
// ⚠️ 旧实现只会 split("_")，B 形态没有下划线 → 一律返回 ""。
//
//	一旦部署时设了 CACHE_QUALITY_ALLOW 白名单，所有 B 形态（含 pv 新体系的一部分）
//	都会被判成「不可缓存」→ 每次都回源，直接烧节点流量（审查报告 P1-5）。
func fileQuality(p string) string {
	base := p
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.ToLower(base)
	if m := qualityInFile.FindStringSubmatch(base); m != nil {
		return m[1]
	}
	// 兜底：A 形态（下划线分隔）里可能出现清单外的写法，取倒数第二段
	parts := strings.Split(strings.TrimSuffix(base, ".mp4"), "_")
	if len(parts) >= 3 {
		return parts[len(parts)-2]
	}
	return ""
}

// ─────────────────────────── 辅助 writer ───────────────────────────

// switchWriter 客户端断开后变成 /dev/null，保证落盘仍能继续。
type switchWriter struct {
	mu   sync.Mutex
	w    io.Writer
	dead bool
}

func (s *switchWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	w := s.w
	s.mu.Unlock()
	if w == nil {
		return len(p), nil
	}
	n, err := w.Write(p)
	if err != nil {
		s.disable()
		return len(p), nil // 吞掉错误，别中断落盘
	}
	return n, nil
}

func (s *switchWriter) disable() {
	s.mu.Lock()
	s.w = nil
	s.dead = true
	s.mu.Unlock()
}

// windowWriter 只把 [skip, skip+remain) 这段写给 dst，其余丢弃。
// 永远返回“写成功”，保证 io.Copy 不会因为客户端问题中断上游读取。
type windowWriter struct {
	dst    io.Writer
	skip   int64
	remain int64
}

func (w *windowWriter) Write(p []byte) (int, error) {
	// 必须返回「入参原始长度」，否则 io.Copy 判定 short write 并中断下载
	orig := len(p)
	if w.remain <= 0 {
		return orig, nil
	}
	if w.skip > 0 {
		if w.skip >= int64(len(p)) {
			w.skip -= int64(len(p))
			return orig, nil
		}
		p = p[w.skip:]
		w.skip = 0
	}
	if int64(len(p)) > w.remain {
		p = p[:w.remain]
	}
	if len(p) > 0 {
		n, _ := w.dst.Write(p)
		w.remain -= int64(n)
	}
	return orig, nil
}

// ─────────────────────────── Range 解析 ───────────────────────────

// parseRange 只支持单区间；多区间或非法一律 ok=false（退化为 200 全量）。
func parseRange(s string, size int64) (start, length int64, ok bool) {
	if size <= 0 {
		return
	}
	s = strings.TrimSpace(s)
	const prefix = "bytes="
	if !strings.HasPrefix(s, prefix) {
		return
	}
	s = s[len(prefix):]
	if strings.Contains(s, ",") {
		return
	}
	i := strings.Index(s, "-")
	if i < 0 {
		return
	}
	var err error
	if i == 0 { // bytes=-N
		var n int64
		if n, err = strconv.ParseInt(s[1:], 10, 64); err != nil || n <= 0 {
			return
		}
		if n > size {
			n = size
		}
		return size - n, n, true
	}
	if start, err = strconv.ParseInt(s[:i], 10, 64); err != nil || start < 0 || start >= size {
		start = 0
		return
	}
	if i == len(s)-1 { // bytes=N-
		return start, size - start, true
	}
	var end int64
	if end, err = strconv.ParseInt(s[i+1:], 10, 64); err != nil || end < start {
		return
	}
	if end >= size {
		end = size - 1
	}
	return start, end - start + 1, true
}

// ─────────────────────────── 内部接口 ───────────────────────────

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	n, b := s.cache.Stats()
	du, dl, mu, ml := s.quota.Usage()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w,
		`{"files":%d,"bytes":%d,"max_bytes":%d,`+
			`"quota":{"daily_used":%d,"daily_limit":%d,"monthly_used":%d,"monthly_limit":%d,"factor":%.2f}}`+"\n",
		n, b, s.cfg.CacheMaxBytes, du, dl, mu, ml, s.cfg.QuotaFactor)
}
