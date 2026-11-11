// dmm-mirror —— javtrailers 镜像的反向代理，专为「让 Cloudflare 缓存」设计
//
// 为什么需要它
// ------------
// javtrailers 的 media 域名被 Cloudflare 的 Bot Fight 保护，普通客户端（curl、nginx、
// Go 默认 TLS）一律吃 `403 cf-mitigated: challenge`。实测：
//
//	HelloChrome_Auto  → 403（Auto 推断的版本和 UA 对不上，被判异常）
//	HelloChrome_120   → 206
//	HelloFirefox_Auto → 206（连测 6 次全过）
//	HelloSafari_Auto  → 206
//	HelloEdge_Auto    → 206
//
// 所以回源必须用 uTLS 伪造浏览器指纹，nginx 的 OpenSSL 指纹过不去。
//
// 设计要点：把缓存压力全部推给 CF
// ------------------------------
// VPS 不落盘（磁盘只剩 9.7G，装不下视频），所以这里只做**纯流式透传**，
// 靠两个手段让 CF 边缘尽量命中：
//
//  1. `Cache-Control: public, max-age=31536000, immutable` —— 直链内容永不变化，
//     给足 TTL，CF 才会长期持有。
//  2. `Range: bytes=0-` 归一化成**无 Range 的完整请求**。播放器起播时普遍发
//     `bytes=0-`，如果原样透传，CF 只会缓存这一小段（206 分片），后面拖动仍要回源。
//     改成回源 200 全量，CF 就能缓存整个对象，后续 Range 从缓存里切。
//
// 其余 Range（拖动、断点续传）照常透传 206，CF 会按分片缓存。
package main

import (
	"context"
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

type Config struct {
	Listen   string // 127.0.0.1:8091
	Upstream string // https://media.javtrailers.com
	Profile  string // chrome120 | firefox | safari | edge
	UA       string
	// 只允许反代这两个路径前缀，防止被当成任意 URL 的开放代理（SSRF）
	Allowed []string

	// 链接签名：由 dmm-query 签发，这里校验。
	// 为空 = 不校验（灰度/回滚）；Enforce=false 时只记日志不拦。
	SignSecret  string
	SignEnforce bool
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func loadConfig() Config {
	c := Config{
		Listen:   env("LISTEN", "127.0.0.1:8091"),
		Upstream: strings.TrimRight(env("UPSTREAM", "https://media.javtrailers.com"), "/"),
		Profile:  strings.ToLower(env("TLS_PROFILE", "chrome120")),
		Allowed:  []string{"/litevideo/freepv/", "/hlsvideo/freepv/"},

		SignSecret:  env("DMM_SIGN_SECRET", ""),
		SignEnforce: env("DMM_SIGN_ENFORCE", "1") != "0",
	}
	switch c.Profile {
	case "chrome120":
		c.UA = env("UA", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "+
			"(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	case "firefox":
		c.UA = env("UA", "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:120.0) "+
			"Gecko/20100101 Firefox/120.0")
	default:
		c.UA = env("UA", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "+
			"(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	}
	return c
}

func helloID(name string) utls.ClientHelloID {
	switch name {
	case "firefox":
		return utls.HelloFirefox_Auto
	case "safari":
		return utls.HelloSafari_Auto
	case "edge":
		return utls.HelloEdge_Auto
	default:
		// 显式指定版本，不用 Auto：Auto 推断出的版本与 UA 不一致时会被 CF 判异常
		return utls.HelloChrome_120
	}
}

func newClient(cfg Config) *http.Client {
	host := strings.TrimPrefix(strings.TrimPrefix(cfg.Upstream, "https://"), "http://")
	id := helloID(cfg.Profile)

	tr := &http2.Transport{
		// body 可能几十 MB，不能设整体超时；只控制建连与握手
		ReadIdleTimeout: 45 * time.Second,
		PingTimeout:     15 * time.Second,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
			raw, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			u := utls.UClient(raw, &utls.Config{
				ServerName: host,
				NextProtos: []string{"h2", "http/1.1"},
			}, id)
			if err := u.HandshakeContext(hctx); err != nil {
				raw.Close()
				return nil, err
			}
			return u, nil
		},
	}
	return &http.Client{Transport: tr}
}

type Server struct {
	cfg     Config
	client  *http.Client
	signer  *Signer
	started time.Time

	hits   atomic.Int64 // 回源成功
	misses atomic.Int64 // 上游 4xx/5xx
	errs   atomic.Int64 // 网络错误
	bytes  atomic.Int64
}

// 上游把 m3u8 报成 application/octet-stream，HLS 播放器不认，按扩展名纠正
func fixContentType(p, ct string) string {
	switch {
	case strings.HasSuffix(p, ".m3u8"):
		return "application/vnd.apple.mpegurl"
	case strings.HasSuffix(p, ".ts"):
		return "video/mp2t"
	case strings.HasSuffix(p, ".mp4"):
		return "video/mp4"
	}
	return ct
}

// bytes=0- 归一化：这类请求等价于「要整个文件」，直接回源 200，
// 让 CF 缓存完整对象，而不是只缓存 0-xxx 这一段。
var fullRange = regexp.MustCompile(`^bytes=0-$`)

// 清单上限。真实清单只有几 KB（master 6 行、变体 100 多行），1MB 是防呆上限，
// 免得被一个畸形上游拖成整文件读进内存。
const maxPlaylistBytes = 1 << 20

// 标签行里带 URI="..." 的属性（EXT-X-KEY / EXT-X-MAP / EXT-X-MEDIA）。
var uriAttrRe = regexp.MustCompile(`URI="([^"]+)"`)

// rewritePlaylist 给 m3u8 里的每一条相对 URI 补上签名。
//
// 一行一签（而不是把请求自己的 query 抄下去）：签名绑的是具体路径，抄下去
// 只会让分片请求验签失败。绝对地址（http/https/data:）原样保留 —— 那些不在
// 我们的反代后面，也签不了。
func (s *Server) rewritePlaylist(body []byte, manifestPath string) []byte {
	if !s.signer.Enabled() {
		return body
	}

	now := time.Now()
	base := &url.URL{Path: manifestPath}

	sign := func(raw string) string {
		ref, err := url.Parse(raw)
		if err != nil || ref.IsAbs() || ref.Scheme != "" {
			return raw
		}
		q := s.signer.SignPath(base.ResolveReference(ref).Path, now)
		if q == "" {
			return raw
		}
		sep := "?"
		if strings.Contains(raw, "?") {
			sep = "&"
		}
		return raw + sep + q
	}

	lines := strings.Split(string(body), "\n")
	for i, line := range lines {
		trimmed := strings.TrimRight(line, "\r")
		suffix := line[len(trimmed):] // 保留原来的 \r，别把 CRLF 清单改成 LF

		if trimmed == "" {
			continue
		}

		if strings.HasPrefix(trimmed, "#") {
			if strings.Contains(trimmed, `URI="`) {
				lines[i] = uriAttrRe.ReplaceAllStringFunc(trimmed, func(m string) string {
					inner := strings.TrimSuffix(strings.TrimPrefix(m, `URI="`), `"`)
					return `URI="` + sign(inner) + `"`
				}) + suffix
			}
			continue
		}

		lines[i] = sign(trimmed) + suffix
	}

	return []byte(strings.Join(lines, "\n"))
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if r.URL.Path == "/_stats" {
		s.stats(w)
		return
	}

	// 链接签名（签发方是 dmm-query，这里只验）。放在路径白名单之前：
	// 没签名的请求连「这条路径存不存在」都不该被回答，否则就成了一个免费的
	// 资源探测接口。secret 没配或 enforce 关闭时只记日志放行。
	if verdict := s.signer.Verify(r, time.Now()); verdict != signOK && verdict != signDisabled {
		if s.signer.Enforcing() {
			w.Header().Set("cache-control", "no-store")
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		log.Printf("sign: %s %s", verdict, r.URL.Path)
	}

	p := strings.TrimPrefix(r.URL.Path, "/mirror")
	if p == "" || p == "/" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	ok := false
	for _, pre := range s.cfg.Allowed {
		if strings.HasPrefix(p, pre) {
			ok = true
			break
		}
	}
	if !ok {
		// 不是白名单路径一律拒绝：这个服务不能变成开放代理
		http.Error(w, "path not allowed", http.StatusForbidden)
		return
	}

	// 上游 URL 只拼路径，不带 query（缓存键才稳定）
	upstream := s.cfg.Upstream + p

	req, err := http.NewRequestWithContext(r.Context(), r.Method, upstream, nil)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	req.Header.Set("User-Agent", s.cfg.UA)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	// Range 处理：只有 bytes=0- 才转成全量，其它原样透传
	if rng := r.Header.Get("Range"); rng != "" && !fullRange.MatchString(rng) {
		req.Header.Set("Range", rng)
		if ir := r.Header.Get("If-Range"); ir != "" {
			req.Header.Set("If-Range", ir)
		}
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		req.Header.Set("If-None-Match", inm)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		s.errs.Add(1)
		log.Printf("回源失败 %s: %v", p, err)
		w.Header().Set("cache-control", "no-store")
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	h := w.Header()
	h.Set("access-control-allow-origin", "*")
	h.Set("access-control-expose-headers", "Content-Length, Content-Range, Accept-Ranges, ETag")

	// 只透传这几个：其余（Set-Cookie / Vary / Nel / Report-To / Alt-Svc / Cf-*）
	// 全部丢掉 —— 它们会干扰 CF 的缓存判定，Vary 尤其会让缓存键分裂。
	for _, k := range []string{"Content-Type", "Content-Length", "Content-Range",
		"Accept-Ranges", "ETag", "Last-Modified"} {
		if v := resp.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
	if ct := h.Get("Content-Type"); ct != "" {
		h.Set("Content-Type", fixContentType(p, ct))
	}

	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent:
		// 直链内容永不变化：给满 TTL，CF 才会长期持有
		h.Set("cache-control", "public, max-age=31536000, immutable")
		s.hits.Add(1)
	case resp.StatusCode == http.StatusNotFound:
		// 镜像确实没这部片 —— 别让 CF 缓存这个负结果
		h.Set("cache-control", "no-store")
		s.misses.Add(1)
	default:
		// 403 challenge / 5xx 都是临时性的，不缓存，客户端可以稍后重试
		h.Set("cache-control", "no-store")
		h.Set("x-mirror-status", strconv.Itoa(resp.StatusCode))
		s.misses.Add(1)
		log.Printf("上游 %d %s", resp.StatusCode, p)
	}

	// HLS 清单改写。
	//
	// 清单里的变体清单和 .ts 分片都是**相对路径**（实测），而浏览器/hls.js 解析
	// 相对 URI 时会把 query 整个丢掉 —— 那些后续请求带不上 `?e=&s=`，验签一开
	// 就全 403，HLS 直接播不了。所以必须在返回前把每条相对 URI 换成带签名的地址。
	//
	// 只对 200 的 .m3u8 做：mp4 不需要，也别把二进制当文本读。
	if strings.HasSuffix(p, ".m3u8") && resp.StatusCode == http.StatusOK && r.Method != http.MethodHead {
		if body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxPlaylistBytes)); rerr == nil {
			// ⚠️ 必须传 r.URL.Path（带 /mirror 前缀），不是上面剥过的 p ——
			// 签名是对「反代收到的那条路径」算的，传 p 会签成 /hlsvideo/...，
			// 与验签时用的 r.URL.Path 对不上，表现就是所有清单 403。
			out := s.rewritePlaylist(body, r.URL.Path)
			// 正文变了，这几个头必须作废：留着 ETag/Last-Modified 会让条件请求
			// 拿到 304 + 旧正文（没有签名），播放器照样挂。
			h.Set("Content-Type", "application/vnd.apple.mpegurl")
			h.Set("Content-Length", strconv.Itoa(len(out)))
			h.Del("Content-Range")
			h.Del("ETag")
			h.Del("Last-Modified")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(out)
			return
		}
	}

	w.WriteHeader(resp.StatusCode)

	if r.Method == http.MethodHead {
		return
	}

	// 流式转发：不落盘，边收边发。用 Flush 让 CF 尽早开始收。
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // 客户端断开
			}
			s.bytes.Add(int64(n))
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr == io.EOF {
			return
		}
		if rerr != nil {
			return
		}
	}
}

func (s *Server) stats(w http.ResponseWriter) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.Header().Set("cache-control", "no-store")
	body := "{" +
		`"ok":true,` +
		`"upstream":"` + s.cfg.Upstream + `",` +
		`"profile":"` + s.cfg.Profile + `",` +
		`"served":` + strconv.FormatInt(s.hits.Load(), 10) + `,` +
		`"upstream_4xx5xx":` + strconv.FormatInt(s.misses.Load(), 10) + `,` +
		`"errors":` + strconv.FormatInt(s.errs.Load(), 10) + `,` +
		`"bytes":` + strconv.FormatInt(s.bytes.Load(), 10) + `,` +
		`"uptime_s":` + strconv.Itoa(int(time.Since(s.started).Seconds())) +
		"}"
	_, _ = w.Write([]byte(body))
}

func main() {
	cfg := loadConfig()
	s := &Server{cfg: cfg, client: newClient(cfg), started: time.Now(),
		signer: NewSigner(cfg.SignSecret, signDefaultTTLDays, cfg.SignEnforce)}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.ServeHTTP)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// 写超时不能设：一部片几十 MB，慢客户端会被掐断
		IdleTimeout: 120 * time.Second,
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	log.Printf("dmm-mirror 监听 %s → %s（TLS 指纹 %s）", cfg.Listen, cfg.Upstream, cfg.Profile)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
