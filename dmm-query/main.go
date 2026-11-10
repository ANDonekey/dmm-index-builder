// dmm-query —— 番号 → DMM 预览视频直链（VPS 本地 SQLite 版）
//
// 为什么存在
// ----------
// 原先查询走 Cloudflare D1 + Worker。D1 免费版每天 500 万行读取额度，
// 一次全表统计就能烧掉一大截，同步还要分片、限流、对账，维护成本远超收益。
// 索引库本身只有 95 MB、79 万行，放在 VPS 的 SQLite 里一条索引查询就够，
// 于是把「查」这一层搬回本地，Worker/D1 整条链路可以退役。
//
// 与 Worker 的接口保持**完全一致**，客户端无需改动：
//
//	GET /?code=ABP-888
//	GET /?code=ABP-888&code=SSIS-095     （批量，上限 50）
//	GET /health                          （索引状态，读 meta，不做 COUNT）
//
// 响应字段沿用 {code, quality, cdn, dmmUrl, proxyUrl}，另加两个可选的镜像兜底：
//
//	fallbackUrl — javtrailers 的 mp4 镜像（与 litevideo 同构，实测约五成命中）
//	hlsUrl      — javtrailers 的 HLS 入口（新片才有，路径用完整 cid 作 stem）
//
// 两者都可能 404，属「试探型」字段，由客户端决定要不要试。
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

/* ---------- 配置 ---------- */

type Config struct {
	Listen      string // 默认 127.0.0.1:8090
	DBPath      string
	PublicHost  string // 固定对外域名（如 cc3001.fastcdn.dpdns.org），优先使用
	ProxyHost   string // 兜底：PUBLIC_HOST 为空时拼 <cdn>.<ProxyHost>
	MirrorProxy bool   // true = 包含「走自己反代」的镜像候选（CF 边缘缓存）
	MirrorFirst bool   // true = urls 里镜像排前面（省日本出口配额），false = 反代优先
	CacheKiB    int    // SQLite page cache，默认 16384 KiB
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func loadConfig() Config {
	c := Config{
		Listen:      env("LISTEN", "127.0.0.1:8090"),
		DBPath:      env("DB_PATH", "/var/lib/dmmpv/dmm-index.db"),
		PublicHost:  env("PUBLIC_HOST", "cc3001.fastcdn.dpdns.org"),
		ProxyHost:   env("PROXY_HOST", "fastcdn.dpdns.org"),
		MirrorProxy: env("MIRROR_PROXY", "1") != "0",
		MirrorFirst: env("MIRROR_FIRST", "1") != "0",
		CacheKiB:    16384,
	}
	if v := env("CACHE_KIB", ""); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.CacheKiB = n
		}
	}
	return c
}

/* ---------- 番号归一化 ---------- */

// 与 worker/index.ts 的 normalizeCode、cidkey.py 的 code_key 保持同一规则。
// ⚠️ 字母段是**非贪婪**的：`118tre00024` 必须拆成 (tre, 24)，贪婪会吃成 (tre, 00024)
// 之外的东西。库侧 cid_key 用的是贪婪版，两边键必须能对上 —— 改这里要同步 cidkey.py。
var codeRe = regexp.MustCompile(`^(\d*?)([a-z]+?)(\d+)$`)

func normalizeCode(code string) (letters string, num int64, ok bool) {
	s := strings.ToLower(strings.TrimSpace(code))
	s = strings.NewReplacer("-", "", "_", "", " ", "", "\t", "").Replace(s)
	m := codeRe.FindStringSubmatch(s)
	if m == nil {
		return "", 0, false
	}
	n, err := strconv.ParseInt(m[3], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return m[2], n, true
}

/* ---------- 数据库 ---------- */

type Row struct {
	cid     string
	code    string
	cdn     string
	dirpath string
	stem    string
	quality string
	variant string
	fmt     int
	source  string
}

// 择优：code 非空 > trailer 源 > cc3001 > 字母段更短。
// 一个键平均对应 1.3 个 cid（79 万里 19.5 万个键是一键多 cid），
// 顺序必须确定，否则同一番号两次查询可能给不同清晰度。
const selCols = `cid, COALESCE(code,''), cdn, dirpath, stem, quality, variant, fmt, COALESCE(source,'')`
const selOrder = ` ORDER BY (code IS NULL), (source <> 'trailer'), (cdn <> 'cc3001'), length(k_letters) LIMIT 1`

func openDB(cfg Config) (*sql.DB, error) {
	// mode=ro：服务只读，避免误写污染索引库
	// _pragma：page cache 限死，VPS 上这点内存要省着用
	dsn := "file:" + cfg.DBPath + "?mode=ro&_pragma=busy_timeout(5000)" +
		"&_pragma=cache_size(-" + strconv.Itoa(cfg.CacheKiB) + ")" +
		"&_pragma=mmap_size(0)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(0)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// 返回 (响应体, HTTP 状态)。状态与 Worker 对齐：
// 非法番号 400，未收录 404，命中 200 —— 客户端靠状态码区分「参数错」和「没这片」。
func (s *Server) lookup(code string) (map[string]any, int) {
	s.lookups.Add(1)
	letters, num, ok := normalizeCode(code)
	if !ok {
		s.misses.Add(1)
		return map[string]any{"code": code, "error": "invalid code: " + code}, http.StatusBadRequest
	}

	s.mu.RLock()
	db := s.db
	s.mu.RUnlock()

	var r Row
	q := `SELECT ` + selCols + ` FROM video WHERE k_letters = ?1 AND k_num = ?2` + selOrder
	err := db.QueryRow(q, letters, num).Scan(&r.cid, &r.code, &r.cdn, &r.dirpath,
		&r.stem, &r.quality, &r.variant, &r.fmt, &r.source)
	if err == sql.ErrNoRows {
		// 字母段互为前缀时互通（STAR-359 ↔ STARS）。只做「库里的更长」这一个方向：
		// 反向（库里的更短）需要 k_num 单列索引，否则是 79 万行全表扫，得不偿失。
		q2 := `SELECT ` + selCols + ` FROM video WHERE k_num = ?1 AND k_letters GLOB ?2` +
			` ORDER BY length(k_letters), (code IS NULL), (source <> 'trailer') LIMIT 1`
		err = db.QueryRow(q2, num, letters+"*").Scan(&r.cid, &r.code, &r.cdn, &r.dirpath,
			&r.stem, &r.quality, &r.variant, &r.fmt, &r.source)
	}
	if err != nil {
		s.misses.Add(1)
		return map[string]any{"code": code, "error": "not found"}, http.StatusNotFound
	}
	return s.build(r, code), http.StatusOK
}

// publicBase 返回对外域名。
//
// 纯透传之后，URL 里再按 cdn 变子域只是维护负担（每多一个 cdn 就要加 DNS、
// 加 nginx server_name、扩证书 SAN）。所以统一固定成一个域名，
// 真实 cdn 改由 query 参数传递（见 cdnQuery）。
func (s *Server) publicBase(cdn string) string {
	if s.cfg.PublicHost != "" {
		return "https://" + s.cfg.PublicHost
	}
	return "https://" + cdn + "." + s.cfg.ProxyHost
}

// cdnQuery 只有非默认 cdn 才带参数，让 99.99% 的 URL 保持干净。
// 注意它会进 CF 的缓存键 —— 不同 cdn 天然分开缓存，不会串。
func cdnQuery(cdn string) string {
	if cdn == "" || cdn == "cc3001" {
		return ""
	}
	return "?cdn=" + cdn
}

func (s *Server) build(r Row, code string) map[string]any {
	// fmt：0/1 → /litevideo/freepv，2/3 → /pv；偶数带下划线，奇数不带
	file := r.stem + "_" + r.quality + "_" + r.variant + ".mp4"
	prefix := "/litevideo/freepv/"
	if r.fmt >= 2 {
		prefix = "/pv/"
	}
	if r.fmt%2 != 0 {
		file = r.stem + r.quality + r.variant + ".mp4"
	}
	path := prefix + r.dirpath + "/" + file

	dmmURL := "https://" + r.cdn + ".dmm.co.jp" + path
	base := s.publicBase(r.cdn)
	proxyURL := base + path + cdnQuery(r.cdn)

	out := map[string]any{
		"code":     code,
		"cid":      r.cid,
		"quality":  r.quality + "_" + r.variant,
		"cdn":      r.cdn,
		"dmmUrl":   dmmURL,
		"proxyUrl": proxyURL,
	}

	// 走自己反代的镜像地址：CF 边缘缓存一年，且回源由 dmm-mirror 用 uTLS 指纹完成，
	// 不受 Bot Fight 影响。
	//
	// ⚠️ 不生成「客户端直连 javtrailers」的候选：前端去拉 media.javatrailers.com
	//    会被 CF 的 Bot Fight 直接拒绝（浏览器能过，但 fetch/XHR + 跨域 Referer 过不去）。
	//    所以镜像只有「经我们反代」这一条路。
	//
	// ⚠️ 只对 litevideo 体系生成 mp4：/pv/ 新体系的片在镜像站上基本没有对应资源，
	//    拼出来也是 404，不如不生成，让客户端直接落到 proxy。
	var mirrorProxyMP4, mirrorProxyHLS string
	if s.cfg.MirrorProxy {
		if strings.HasPrefix(path, "/litevideo/freepv/") {
			// mirror 的上游固定是 javtrailers，不需要 cdn 参数
			mirrorProxyMP4 = base + "/mirror" + path
		}
		// HLS 体系的目录用**完整 cid** 作 stem，不是 litevideo 那个去零 stem。
		// 例：ksbj141 的 mp4 在 k/ksb/ksbj141/，HLS 在 k/ksb/ksbj00141/playlist.m3u8
		// DMM 路径全小写，而库里 cid 可能带大写（1STARS00359），必须转小写
		if i := strings.LastIndex(r.dirpath, "/"); i > 0 {
			mirrorProxyHLS = base + "/mirror/hlsvideo/freepv/" +
				r.dirpath[:i] + "/" + strings.ToLower(r.cid) + "/playlist.m3u8"
		}
	}
	if mirrorProxyMP4 != "" {
		out["mirrorProxyUrl"] = mirrorProxyMP4
	}

	// urls：按优先级排好的候选，客户端从 0 开始试，第一个 2xx 的拿来播。
	//
	// 为什么不在这里替客户端探测：服务端要判断镜像可用性就得回源，而回源必须伪造
	// 浏览器 TLS 指纹（见 dmm-mirror），探测一次 0.8 秒且结果要缓存，成本远高于
	// 让客户端试一次。所以「优先镜像」做成**排序优先**。
	//
	// via 的取值：
	//   mirror_proxy — 走自己的反代，CF 边缘缓存，不消耗日本出口配额
	//   proxy        — 自建 DMM 反代，走日本出口，吃日/月配额
	//   direct       — DMM 源站，仅日本 IP 可用
	var urls []map[string]string
	add := func(u, typ, via string) {
		if u != "" {
			urls = append(urls, map[string]string{"url": u, "type": typ, "via": via})
		}
	}
	if s.cfg.MirrorFirst {
		add(mirrorProxyMP4, "mp4", "mirror_proxy")
		add(mirrorProxyHLS, "hls", "mirror_proxy")
	}
	add(proxyURL, "mp4", "proxy")
	add(dmmURL, "mp4", "direct")
	if !s.cfg.MirrorFirst {
		add(mirrorProxyMP4, "mp4", "mirror_proxy")
		add(mirrorProxyHLS, "hls", "mirror_proxy")
	}
	out["urls"] = urls
	return out
}

/* ---------- HTTP ---------- */

type Server struct {
	cfg Config
	mu  sync.RWMutex
	db  *sql.DB

	started time.Time
	lookups atomic.Int64
	misses  atomic.Int64
}

func writeJSON(w http.ResponseWriter, status int, body any, extra map[string]string) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.Header().Set("access-control-allow-origin", "*")
	for k, v := range extra {
		w.Header().Set(k, v)
	}
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(body)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.Header().Set("access-control-allow-origin", "*")
		w.Header().Set("access-control-allow-methods", "GET, OPTIONS")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// 允许挂在 /api 前缀下（nginx 复用现有域名，不必再签一张证书）。
	// /api?code=x 与 /?code=x 等价；/api/health 与 /health 等价。
	path := r.URL.Path
	if path == "/api" {
		path = "/"
	} else if strings.HasPrefix(path, "/api/") {
		path = strings.TrimPrefix(path, "/api")
	}

	switch path {
	case "/health":
		s.handleHealth(w, r)
		return
	case "/_stats":
		s.handleStats(w, r)
		return
	}

	codes := r.URL.Query()["code"]
	if len(codes) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing code"}, nil)
		return
	}

	if len(codes) > 1 {
		if len(codes) > 50 {
			codes = codes[:50]
		}
		out := make(map[string]any, len(codes))
		for _, c := range codes {
			v, _ := s.lookup(strings.TrimSpace(c))
			out[c] = v
		}
		writeJSON(w, http.StatusOK, out, map[string]string{
			"cache-control": "public, max-age=3600",
		})
		return
	}

	code := strings.TrimSpace(codes[0])
	if code == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing code"}, nil)
		return
	}
	body, status := s.lookup(code)
	if status != http.StatusOK {
		extra := map[string]string{}
		if status == http.StatusNotFound {
			// x-dmm-miss 让调用方区分「未收录」和「参数错」，沿用 Worker 的约定
			extra["x-dmm-miss"] = "1"
		}
		writeJSON(w, status, body, extra)
		return
	}
	// 直链永不变，可长缓存；客户端/CF 都能省一次回源
	writeJSON(w, http.StatusOK, body, map[string]string{
		"cache-control": "public, max-age=86400",
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	db := s.db
	s.mu.RUnlock()

	var total, builtAt string
	_ = db.QueryRow(`SELECT COALESCE(value,'') FROM meta WHERE key='video_count'`).Scan(&total)
	_ = db.QueryRow(`SELECT COALESCE(value,'') FROM meta WHERE key='built_at'`).Scan(&builtAt)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"db":       s.cfg.DBPath,
		"rows":     total,
		"built_at": builtAt,
		"uptime_s": int(time.Since(s.started).Seconds()),
	}, nil)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	// ⚠️ 不要在这里 COUNT(*) 全表：79 万行会把 page cache 冲干净，
	// 健康检查变慢，还会干扰正常查询的缓存命中。
	s.mu.RLock()
	db := s.db
	s.mu.RUnlock()

	var keyed int
	_ = db.QueryRow(`SELECT COUNT(*) FROM video WHERE k_letters <> ''`).Scan(&keyed)

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	writeJSON(w, http.StatusOK, map[string]any{
		"keyed_rows": keyed,
		"lookups":    s.lookups.Load(),
		"misses":     s.misses.Load(),
		"heap_mb":    float64(ms.HeapAlloc) / 1048576,
		"sys_mb":     float64(ms.Sys) / 1048576,
		"goroutines": runtime.NumGoroutine(),
	}, nil)
}

/* ---------- 启动 ---------- */

func main() {
	cfg := loadConfig()
	db, err := openDB(cfg)
	if err != nil {
		log.Fatalf("打开索引库失败 %s: %v", cfg.DBPath, err)
	}

	s := &Server{cfg: cfg, db: db, started: time.Now()}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// SIGHUP → 重开索引库。索引每周更新，替换文件后 reload 即可，不用重启进程。
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for v := range sig {
			if v == syscall.SIGHUP {
				nd, err := openDB(cfg)
				if err != nil {
					log.Printf("reload 失败，继续用旧库: %v", err)
					continue
				}
				s.mu.Lock()
				old := s.db
				s.db = nd
				s.mu.Unlock()
				log.Printf("索引库已重新加载")
				// 等一会儿再关，避免正在处理的请求拿到已关闭的连接
				go func() { time.Sleep(30 * time.Second); old.Close() }()
				continue
			}
			log.Printf("收到 %v，退出", v)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
			return
		}
	}()

	log.Printf("dmm-query 监听 %s，索引 %s", cfg.Listen, cfg.DBPath)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
