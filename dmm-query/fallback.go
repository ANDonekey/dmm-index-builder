// 库未命中时的实时回退：番号 → DMM 搜索 → cid → 预览地址，结果落独立 SQLite 永久复用。
//
// 为什么存在
// ----------
// 主索引库（dmm-index.db）是从 r18.dev dump 抽出来的，天生滞后：新片、冷门片、
// 或者 dump 里没收录的，查库就是 404。以前这种情况前端只能显示「未收录」。
//
// 但 DMM 自己的搜索页能查到，而且**一次请求就够**：搜索结果的 HTML 里同时内嵌了
// `content_id` 和 `/pv/<token>/<file>.mp4` 预览直链（实测）。所以回退链路是：
//
//	https://www.dmm.co.jp/search/=/searchstr=FNS-272/   （走日本出口）
//	  → 从内嵌的 /pv/ 直链里取出完整 cid（1fns00272）
//	  → 拼镜像 HLS：/mirror/hlsvideo/freepv/1/1fn/1fns00272/playlist.m3u8
//
// 为什么不抓 javtrailers 的页面：它前面有 CF 的 bot 挑战，纯 HTTP 客户端进不去；
// 而 media.javatrailers.com 的媒体路径由 dmm-mirror 用 uTLS 指纹回源，一直好用。
// 所以「拿 cid」用 DMM、「出流」用镜像，各走各的强项。
//
// 为什么只存 cid
// -------------
// 预览地址是 cid 的纯函数（见 mirrorHLSPath），存 cid 就等于存了地址，还不会因为
// 以后换域名/换路径而失效。这也是「永久复用」能成立的前提。
package main

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

/* ---------- 配置 ---------- */

// FallbackConfig 是回退解析的配置。默认值指向本机 sing-box 的 socks 入站
// （dmm.co.jp 已分流到 jp-group），没有它 DMM 会对非日本 IP 直接拒绝。
type FallbackConfig struct {
	Enabled     bool
	DBPath      string
	SearchBase  string // https://www.dmm.co.jp
	ProxyURL    string // socks5://127.0.0.1:1080
	Timeout     time.Duration
	MissTTLDays int
	MinInterval time.Duration // 两次回源之间的最小间隔，防止把搜索页打爆
}

/* ---------- 存储 ---------- */

// Entry 是一条回退结果。CID 是唯一真正需要持久化的东西。
type Entry struct {
	Code      string
	CID       string
	PVURL     string // DMM 的 /pv/<token>/<file>.mp4，拿不到时为空
	FetchedAt time.Time
}

const fallbackSchema = `
CREATE TABLE IF NOT EXISTS fallback (
  k_letters TEXT    NOT NULL,
  k_num     INTEGER NOT NULL,
  code      TEXT    NOT NULL,
  cid       TEXT    NOT NULL DEFAULT '',
  pv_url    TEXT    NOT NULL DEFAULT '',
  fetched_at INTEGER NOT NULL,
  PRIMARY KEY (k_letters, k_num)
)`

/* ---------- 解析器 ---------- */

type Resolver struct {
	cfg    FallbackConfig
	db     *sql.DB
	client *http.Client

	// 同一时刻只允许一个回源：搜索页很重，并发打过去只会被 DMM 掐。
	fetchMu sync.Mutex
	lastAt  time.Time

	// 同一番号并发请求合并成一次解析（前端一次播放可能连发几个探测）。
	inflightMu sync.Mutex
	inflight   map[string]*inflightCall
}

type inflightCall struct {
	done  chan struct{}
	entry *Entry
}

func NewResolver(cfg FallbackConfig) (*Resolver, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if cfg.DBPath == "" {
		return nil, errors.New("fallback: 缺少 DBPath")
	}

	// mode=rwc：这个库是可写的（主索引库是只读的，两个分开）。
	// WAL 让读写不互相阻塞，进程被 kill 也不会留半条写。
	dsn := "file:" + cfg.DBPath + "?mode=rwc&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(2)
	if _, err := db.Exec(fallbackSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("建表失败: %w", err)
	}

	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: cfg.Timeout,
		MaxIdleConns:          4,
		IdleConnTimeout:       60 * time.Second,
	}
	if cfg.ProxyURL != "" {
		pu, err := url.Parse(cfg.ProxyURL)
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("代理地址非法 %q: %w", cfg.ProxyURL, err)
		}
		// net/http 原生支持 socks5:// 与 socks5h://，不需要额外依赖
		transport.Proxy = http.ProxyURL(pu)
	}

	return &Resolver{
		cfg:      cfg,
		db:       db,
		client:   &http.Client{Transport: transport, Timeout: cfg.Timeout},
		inflight: make(map[string]*inflightCall),
	}, nil
}

// Resolve 返回番号对应的回退结果；没有就返回 nil。
//
// 顺序：内存合并 → 库 → 回源。库里的正结果**永不过期**，负结果按 MissTTL 过期
// （否则新片一旦被判过「没有」，以后永远查不到）。
func (r *Resolver) Resolve(code string) *Entry {
	if r == nil {
		return nil
	}
	letters, num, ok := normalizeCode(code)
	if !ok || letters == "" {
		return nil
	}
	key := fmt.Sprintf("%s:%d", letters, num)

	r.inflightMu.Lock()
	if call, exists := r.inflight[key]; exists {
		r.inflightMu.Unlock()
		<-call.done
		return call.entry
	}
	call := &inflightCall{done: make(chan struct{})}
	r.inflight[key] = call
	r.inflightMu.Unlock()

	defer func() {
		r.inflightMu.Lock()
		delete(r.inflight, key)
		r.inflightMu.Unlock()
		close(call.done)
	}()

	call.entry = r.resolveLocked(code, letters, num)
	return call.entry
}

func (r *Resolver) resolveLocked(code, letters string, num int64) *Entry {
	if entry, fresh := r.load(letters, num); entry != nil && fresh {
		return entry
	}

	entry, err := r.fetch(code)
	if err != nil {
		// 网络/解析失败**不写负结果**：那是一次故障，不是「这部片不存在」。
		// 写进去就等于把故障固化成一个 TTL 那么长的错误答案。
		log.Printf("fallback: %s 回源失败: %v", code, err)
		return nil
	}

	if err := r.save(letters, num, code, entry); err != nil {
		log.Printf("fallback: %s 落库失败: %v", code, err)
	}
	if entry == nil {
		return nil
	}
	return entry
}

func (r *Resolver) load(letters string, num int64) (entry *Entry, fresh bool) {
	var (
		code, cid, pv string
		fetchedAt     int64
	)
	err := r.db.QueryRow(
		`SELECT code, cid, pv_url, fetched_at FROM fallback WHERE k_letters=?1 AND k_num=?2`,
		letters, num,
	).Scan(&code, &cid, &pv, &fetchedAt)
	if err != nil {
		return nil, false
	}

	// cid 为空 = 上次回源确认过「查不到」，只在一段时间内有效
	if cid == "" {
		if time.Since(time.Unix(fetchedAt, 0)) > time.Duration(r.cfg.MissTTLDays)*24*time.Hour {
			return nil, false
		}
		return nil, true
	}

	return &Entry{Code: code, CID: cid, PVURL: pv, FetchedAt: time.Unix(fetchedAt, 0)}, true
}

func (r *Resolver) save(letters string, num int64, code string, entry *Entry) error {
	cid, pv := "", ""
	if entry != nil {
		cid, pv = entry.CID, entry.PVURL
	}
	_, err := r.db.Exec(
		`INSERT INTO fallback (k_letters, k_num, code, cid, pv_url, fetched_at)
		 VALUES (?1, ?2, ?3, ?4, ?5, ?6)
		 ON CONFLICT(k_letters, k_num) DO UPDATE SET
		   code=excluded.code, cid=excluded.cid, pv_url=excluded.pv_url,
		   fetched_at=excluded.fetched_at`,
		letters, num, strings.ToUpper(code), cid, pv, time.Now().Unix(),
	)
	return err
}

// fetch 打一次 DMM 搜索页，解析出 cid。返回 (nil, nil) 表示「确实没有这部片」。
func (r *Resolver) fetch(code string) (*Entry, error) {
	r.fetchMu.Lock()
	defer r.fetchMu.Unlock()

	if r.cfg.MinInterval > 0 {
		if wait := r.cfg.MinInterval - time.Since(r.lastAt); wait > 0 {
			time.Sleep(wait)
		}
	}
	r.lastAt = time.Now()

	searchURL := r.cfg.SearchBase + "/search/=/searchstr=" + url.PathEscape(code) + "/"
	req, err := http.NewRequest(http.MethodGet, searchURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "+
		"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "ja-JP,ja;q=0.9,en;q=0.8")
	// 没有这个 cookie 会被 302 到年龄确认页，拿不到任何结果
	req.Header.Set("Cookie", "age_check_done=1")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("搜索页 HTTP %d", resp.StatusCode)
	}

	// 85KB 左右；给 2MB 上限，防止被畸形响应拖成整站读进内存
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}

	cid, pvURL := pickCID(string(body), code)
	if cid == "" {
		return nil, nil
	}
	return &Entry{Code: strings.ToUpper(code), CID: cid, PVURL: pvURL, FetchedAt: time.Now()}, nil
}

/* ---------- 解析 ---------- */

var (
	// 预览直链：https://cc3001.dmm.co.jp/pv/<62位token>/<文件名>.mp4
	// 文件名就是「cid + 清晰度」，例如 1fns00272sm.mp4 —— 这是拿完整 cid 的入口，
	// 搜索页里 `cid=1fns272` 那种是**去掉前导零**的短形态，不能直接拼路径。
	pvURLRe = regexp.MustCompile(`https://cc3001\.dmm\.co\.jp/pv/[A-Za-z0-9_-]+/[a-z0-9]+\.mp4`)
	// 兜底：没有 pv 直链时用 cid= 参数（短形态，需要补零）
	cidParamRe = regexp.MustCompile(`cid=([a-z0-9]+)`)
)

// 文件名尾部的清晰度标记。**从长到短**匹配，否则 "dmb" 会被 "mb" 之类切坏。
var qualitySuffixes = []string{
	"hhb4k", "dmb", "mhb", "hhb", "sm_w", "dm_w", "dmb_w", "mhb_w",
	"4k", "sm", "dm", "hd", "sd",
}

// pickCID 从搜索页 HTML 里找出与 code **精确匹配**的完整 cid。
//
// 搜索会返回一堆相似结果（同一部片的不同版本：通常版 / BR / TK），必须靠
// normalizeCode 归一化后逐一比对，不能取第一个。
func pickCID(html, code string) (cid, pvURL string) {
	wantLetters, wantNum, ok := normalizeCode(code)
	if !ok {
		return "", ""
	}

	matches := func(candidate string) bool {
		letters, num, ok := normalizeCode(candidate)
		return ok && letters == wantLetters && num == wantNum
	}

	// ① 优先用 /pv/ 直链：文件名里的 cid 是完整形态，可以直接拼镜像路径
	for _, raw := range pvURLRe.FindAllString(html, -1) {
		base := raw[strings.LastIndex(raw, "/")+1:]
		base = strings.TrimSuffix(base, ".mp4")
		if c := stripQuality(base); c != "" && matches(c) {
			return c, raw
		}
	}

	// ② 没有直链就用 cid= 参数。短形态补零到 5 位是 DMM 的常规约定，
	//    但个别厂牌不是，所以补出来的结果要由调用方用镜像 HEAD 再确认一次。
	for _, m := range cidParamRe.FindAllStringSubmatch(html, -1) {
		candidate := m[1]
		if matches(candidate) {
			return padCID(candidate), ""
		}
	}

	return "", ""
}

func stripQuality(base string) string {
	for _, q := range qualitySuffixes {
		if strings.HasSuffix(base, q) && len(base) > len(q) {
			return base[:len(base)-len(q)]
		}
	}
	return base
}

// padCID 把 `1fns272` 补成 `1fns00272`（数字段补到 5 位）。
// 已经够长或没有数字段就原样返回。
func padCID(cid string) string {
	i := len(cid)
	for i > 0 && cid[i-1] >= '0' && cid[i-1] <= '9' {
		i--
	}
	if i == len(cid) {
		return cid // 没有数字段
	}
	digits := cid[i:]
	for len(digits) < 5 {
		digits = "0" + digits
	}
	return cid[:i] + digits
}

// mirrorHLSPath 由 cid 推导镜像里的 HLS 清单路径。
//
// DMM 的 freepv 目录结构是 <首字符>/<前三位>/<完整cid>，HLS 用完整 cid 作目录名
// （mp4 那套用的是去零 stem，两者不同）。实测：
//
//	1fns00272 → /hlsvideo/freepv/1/1fn/1fns00272/playlist.m3u8
//	ssis00095 → /hlsvideo/freepv/s/ssi/ssis00095/playlist.m3u8
func mirrorHLSPath(cid string) string {
	cid = strings.ToLower(strings.TrimSpace(cid))
	if len(cid) < 3 {
		return ""
	}
	return "/hlsvideo/freepv/" + cid[:1] + "/" + cid[:3] + "/" + cid + "/playlist.m3u8"
}
