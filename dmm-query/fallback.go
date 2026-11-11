// 库未命中时的实时回退：番号 → DMM 搜索 → cid → 预览地址，结果落独立 SQLite 永久复用。
//
// 为什么存在
// ----------
// 主索引库（dmm-index.db）是从 r18.dev dump 抽出来的，天生滞后：新片、冷门片、
// 或者 dump 里没收录的，查库就是 404。以前这种情况前端只能显示「未收录」。
//
// 主路走 **DMM affiliate API**（`api.dmm.com/affiliate/v3/ItemList`）：
//
//	GET https://api.dmm.com/affiliate/v3/ItemList
//	      ?api_id=&affiliate_id=&site=FANZA&sort=match&keyword=FNS-272&output=json
//	  → result.items[].content_id = 1fns00272（数字层就是完整 cid）
//	  → 拼镜像 HLS：/mirror/hlsvideo/freepv/1/1fn/1fns00272/playlist.m3u8
//
// 为什么不再抓搜索页：那需要日本出口 + `age_check_done` cookie + 解析 85KB HTML，
// 单次 2.6~5.4s，还有 bot 风险。affiliate API 直连 0.6s、纯 JSON、不挑出口
// （实测美国 IP 可用，地域限制只在 www.dmm.co.jp 上）。搜索页只在 API 未配置
// 或硬失败（限流/凭据失效）时兜底 —— 它的 cid 要从 `/pv/` 文件名里抠，见 pickCID。
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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
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
	SearchBase  string // https://www.dmm.co.jp（仅搜索页兜底用）
	ProxyURL    string // socks5://127.0.0.1:1080（仅搜索页兜底用）
	Timeout     time.Duration
	MissTTLDays int
	MinInterval time.Duration // 两次回源之间的最小间隔，防止把源站打爆

	// affiliate API 凭据。两者都非空才启用 API 主路。
	APIID       string
	AffiliateID string
	APIBase     string // 默认 https://api.dmm.com/affiliate/v3/ItemList
	// MirrorBase 本地 dmm-mirror 的 /mirror 前缀，用来验证 cid 在镜像里到底
	// 有没有收录。空 = 不验证（退回「按 cid 直接返回」的旧行为）。
	MirrorBase string
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
	client *http.Client // 走日本出口，只给搜索页兜底用
	// affiliate API 不挑出口，直连更快，也不要让 socks 成为单点
	apiClient *http.Client

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
		cfg:    cfg,
		db:     db,
		client: &http.Client{Transport: transport, Timeout: cfg.Timeout},
		// API 直连：不复用上面那个挂了 socks 的 transport
		apiClient: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				TLSHandshakeTimeout:   8 * time.Second,
				ResponseHeaderTimeout: cfg.Timeout,
				MaxIdleConns:          4,
				IdleConnTimeout:       90 * time.Second,
			},
			Timeout: cfg.Timeout,
		},
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

// fetch 解析一个番号的 cid。返回 (nil, nil) 表示「确实没有这部片」。
//
// 顺序：affiliate API → 搜索页兜底。
// 只有**硬失败**（网络、限流、凭据失效）才降级到搜索页；API 明确回了 0 条结果
// 就是「没有」，不该再去打一次 85KB 的页面。
func (r *Resolver) fetch(code string) (*Entry, error) {
	// 回源串行化。api_id 的限流是「同 key 跨 IP 全局生效」的（实测 20 并发后
	// 整批 400 好几分钟），搜索页更怕并发 —— 所以两条路共用一把锁。
	r.fetchMu.Lock()
	defer r.fetchMu.Unlock()

	if r.cfg.APIID != "" && r.cfg.AffiliateID != "" {
		entry, err := r.fetchViaAPI(code)
		if err == nil {
			return entry, nil
		}
		if errors.Is(err, errMirrorMiss) {
			// API 认识这部片，但镜像没收录它的 HLS。搜索页给的是 DMM 的 /pv/
			// 直链，能覆盖镜像的缺口，所以这里**降级**而不是直接判 404。
			log.Printf("fallback: %s 镜像无此片，改走搜索页取 /pv/", code)
		} else {
			log.Printf("fallback: %s affiliate API 失败，降级搜索页: %v", code, err)
		}
	}
	return r.fetchViaPage(code)
}

// errMirrorMiss 表示「API 查到了 cid，但镜像没有对应的 HLS」。
// 它和「查无此片」必须分开：前者还能靠搜索页的 /pv/ 直链救回来。
var errMirrorMiss = errors.New("镜像未收录该 cid")

// mirrorHas 用本地 dmm-mirror 探一下 cid 的 HLS 清单在不在。
//
// 为什么要探：镜像的上游（javtrailers）覆盖率不是 100%，实测 MXGS-1456 及之后
// 整批没有，而 1455 及之前都有。cid 是从 API 推出来的，光看 API 判断不了镜像
// 有没有 —— 直接回一条 404 的地址给前端，用户看到的是「有源但播不了」。
//
// 探的是 127.0.0.1 上的 mirror（不经 CF），所以**不会**把 404 写进 CF 的边缘
// 缓存（那条 cache rule 会把 404 也缓存一年）。
func (r *Resolver) mirrorHas(cid string) (string, bool, error) {
	if r.cfg.MirrorBase == "" {
		return "", false, nil
	}
	base := strings.TrimRight(r.cfg.MirrorBase, "/")

	var lastErr error
	for _, c := range cidVariants(cid) {
		p := mirrorHLSPath(c)
		if p == "" {
			continue
		}
		req, err := http.NewRequest(http.MethodGet, base+p, nil)
		if err != nil {
			return "", false, err
		}
		resp, err := r.apiClient.Do(req)
		if err != nil {
			// 探不动 ≠ 没有。交给调用方决定（当前策略：当作有，保持旧行为）
			lastErr = err
			continue
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return c, true, nil
		}
		if resp.StatusCode != http.StatusNotFound {
			lastErr = fmt.Errorf("mirror HTTP %d", resp.StatusCode)
		}
	}
	return "", false, lastErr
}

// cidVariants 给出同一个作品可能用的几种 cid 写法。
//
// `h_068mxgs1457`（API 给的 DVD 形态，数字不补零）和 `h_068mxgs01457`
// （索引库里存的形态，补到 5 位）在镜像里只会有其中一个，所以要都试。
// 顺序很重要：原样优先 —— `118abp888` 这种补零反而会变成不存在的路径。
func cidVariants(cid string) []string {
	out := []string{cid}
	if padded := padCID(cid); padded != cid {
		out = append(out, padded)
	}
	return out
}

/* ---------- affiliate API 主路 ---------- */

const defaultAffiliateAPI = "https://api.dmm.com/affiliate/v3/ItemList"

type affiliateItem struct {
	ContentID    string `json:"content_id"`
	FloorCode    string `json:"floor_code"`
	ServiceCode  string `json:"service_code"`
	MakerProduct string `json:"maker_product"`
	URL          string `json:"URL"`
}

type affiliateResponse struct {
	Result struct {
		Status      int               `json:"status"`
		ResultCount int               `json:"result_count"`
		Items       []affiliateItem   `json:"items"`
		Errors      map[string]string `json:"errors"`
	} `json:"result"`
}

// keywordVariants 给出 DMM 搜索的关键词变体。
//
// 必须先试补零形态：`FNS-272` 的数字层 cid 是 `1fns00272`，直接搜原码更容易
// 命中 DVD（mono）那条 `1fns272`。补零后 videoa 的条目排得更前。
func keywordVariants(code string) []string {
	c := strings.TrimSpace(code)
	if c == "" {
		return nil
	}
	seen := make(map[string]bool, 3)
	out := make([]string, 0, 3)
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	if i := strings.Index(c, "-"); i > 0 {
		add(c[:i] + "00" + c[i+1:])
	}
	add(c)
	add(strings.ReplaceAll(c, "-", ""))
	return out
}

func (r *Resolver) fetchViaAPI(code string) (*Entry, error) {
	base := r.cfg.APIBase
	if base == "" {
		base = defaultAffiliateAPI
	}

	for _, kw := range keywordVariants(code) {
		items, err := r.callItemList(base, kw)
		if err != nil {
			return nil, err
		}
		cid := pickAffiliateCID(items, code)
		if cid == "" {
			continue
		}

		// 拿到 cid 先问镜像：没收录就返回 errMirrorMiss，让调用方去搜索页拿 /pv/。
		// 探不动（网络错）时不做判断，按 cid 原样返回 —— 不能因为探针故障就
		// 把本来能播的片判死。
		chosen, ok, probeErr := r.mirrorHas(cid)
		if probeErr != nil {
			log.Printf("fallback: %s 镜像探测失败（按有源处理）: %v", code, probeErr)
			return &Entry{Code: strings.ToUpper(code), CID: cid, FetchedAt: time.Now()}, nil
		}
		if r.cfg.MirrorBase != "" && !ok {
			return nil, errMirrorMiss
		}
		if ok {
			cid = chosen
		}
		return &Entry{Code: strings.ToUpper(code), CID: cid, FetchedAt: time.Now()}, nil
	}
	// 所有关键词变体都 0 命中 = 这部片 DMM 上确实没有
	return nil, nil
}
func (r *Resolver) callItemList(base, keyword string) ([]affiliateItem, error) {
	q := url.Values{}
	q.Set("api_id", r.cfg.APIID)
	q.Set("affiliate_id", r.cfg.AffiliateID)
	q.Set("output", "json")
	q.Set("site", "FANZA")
	q.Set("sort", "match")
	q.Set("hits", "20")
	q.Set("keyword", keyword)

	req, err := http.NewRequest(http.MethodGet, base+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "ja-JP,ja;q=0.9")

	resp, err := r.apiClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 400 一般是 api_id 限流（同 key 并发过高会整批 400 好几分钟）或凭据失效。
	// 这两种都要当**硬失败**降级，不能当「没有这部片」。
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("affiliate API HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var payload affiliateResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("affiliate API 响应不是 JSON: %w", err)
	}
	if payload.Result.Status != 200 {
		return nil, fmt.Errorf("affiliate API status=%d %v", payload.Result.Status, payload.Result.Errors)
	}
	return payload.Result.Items, nil
}

// pickAffiliateCID 从候选条目里挑出与番号精确匹配的那条，返回完整 cid。
//
// 匹配靠 normalizeCode（`1fns00272` / `1fns272` / `FNS-272` 归一后同键），
// 所以 BR/TK 这类带尾缀字母的版本会被自动排除（正则要求以数字结尾）。
//
// 选优规则（2026-11-12 用 8 个番号实测出来的，别凭直觉改）：
//
//	floor_code   content_id     镜像里真正用的 cid
//	videoa       1fns00272      1fns00272   ← 数字层，已经是完整形态
//	standard     ssis00095      ssis00095
//	premium      dass00426      dass00426
//	dvd          118abp888      118abp888   ← **不能补零**，补成 118abp00888 就 404
//	dvd          1fns272        1fns00272   ← 只有这条才需要补零
//
// 所以：优先非 dvd（数字层给的就是规范 cid）；只剩 dvd 时才判断要不要补零，
// 判据是「前导数字段只有 1 位、尾部数字不足 5 位」——`1fns272` 符合，
// `118abp888` / `7ipx176` 这类带多位前缀的不符合。
func pickAffiliateCID(items []affiliateItem, code string) string {
	wantLetters, wantNum, ok := normalizeCode(code)
	if !ok {
		return ""
	}

	best, bestScore := "", -1
	for _, it := range items {
		cid := strings.ToLower(strings.TrimSpace(it.ContentID))
		if cid == "" {
			continue
		}
		letters, num, ok := normalizeCID(cid)
		if !ok || letters != wantLetters || num != wantNum {
			continue
		}
		score := 0
		if it.FloorCode != "dvd" {
			score += 4 // 数字层：content_id 直接可用
		}
		if it.FloorCode == "videoa" {
			score += 2 // 同属数字层时优先它，和镜像命名最一致
		}
		if score > bestScore {
			best, bestScore = cid, score
		}
	}
	if best == "" {
		return ""
	}
	if bestScore < 4 {
		return padShortCID(best)
	}
	return best
}

// padShortCID 只对「1 位前导数字 + 不足 5 位尾数字」的短形态补零。
//
// 这是 DVD(mono) 层的命名：`1fns272` → `1fns00272`。但同一个层也有
// `118abp888` 这种完整形态，补零会得到不存在的 `118abp00888`，
// 所以不能无脑调 padCID。
func padShortCID(cid string) string {
	lead := 0
	for lead < len(cid) && cid[lead] >= '0' && cid[lead] <= '9' {
		lead++
	}
	trail := len(cid)
	for trail > 0 && cid[trail-1] >= '0' && cid[trail-1] <= '9' {
		trail--
	}
	if lead != 1 || trail >= len(cid) || len(cid)-trail >= 5 {
		return cid
	}
	return padCID(cid)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

/* ---------- 搜索页兜底 ---------- */

// fetchViaPage 打一次 DMM 搜索页，解析出 cid。调用方已持 fetchMu。
func (r *Resolver) fetchViaPage(code string) (*Entry, error) {
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
	pvURLRe = regexp.MustCompile(`https://cc3001\.dmm\.co\.jp/pv/[A-Za-z0-9_-]+/[a-z0-9_]+\.mp4`)
	// 兜底：没有 pv 直链时用 cid= 参数（短形态，需要补零）
	cidParamRe = regexp.MustCompile(`cid=([a-z0-9_]+)`)
)

// cidSuffixRe 只认「尾部」的字母段 + 数字段，**不锚定开头**。
//
// 为什么不能像 normalizeCode 那样整串匹配：DMM 的 content_id 有带前缀的形态，
// 例如 `h_068mxgs1457`（厂商前缀 `h_068`）。整串正则会直接判失败，于是
// 「API 明明返回了这部片，我们却当没查到」—— MXGS-1457 就是这么丢的。
// 后缀匹配对 `1fns00272` / `118abp888` / `h_068mxgs1457` 都能取到 mxgs:1457 这类键。
var cidSuffixRe = regexp.MustCompile(`([a-z]+)(\d+)$`)

// normalizeCID 把 content_id / pv 文件名归一成 (字母段, 数字值)，用于判等。
// 与 normalizeCode 的区别只在「允许前缀」，输入校验那条路不要改。
func normalizeCID(raw string) (letters string, num int64, ok bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.NewReplacer("-", "", "_", "", " ", "", "\t", "").Replace(s)
	m := cidSuffixRe.FindStringSubmatch(s)
	if m == nil {
		return "", 0, false
	}
	n, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return m[1], n, true
}

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
		letters, num, ok := normalizeCID(candidate)
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

// mirrorHLSPath 由 cid 推导镜像里的 HLS 清单路径。//
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

/* ---------- 预览清晰度 ---------- */

// pvQualityLadder 是候选清晰度，**从高到低**。
//
// 为什么要有它：搜索页内嵌的 /pv/ 直链只有 `sm`（small，最低档），而同一个
// token 目录下通常还有 hhb/hmb/mhb/mmb/dm 各档 —— 实测 MXGS-1457 的
// `.../h_068mxgs1457sm.mp4` 换成 hhb/hmb/mhb/mmb/dm 全部 206，只有 4k 是 404。
// 只取页面里那一条，用户看到的就是最低分辨率。
//
// 为什么不在服务端逐档探：dmm-proxy 没有磁盘缓存，`Range: bytes=0-0` 也会
// 整文件回源 —— 实测探一次 hmb 就吃掉 2.2MB 配额。所以这里只生成候选列表，
// 交给前端串行探测（第一个能播就停），只有真正播的那一档会下载。
//
// 不含 4k：预览片一般几十秒，4k 动辄上百 MB，默认给 fullhd 更合适。
var pvQualityLadder = []string{"hhb", "hmb", "mhb", "mmb", "sm"}

// pvQualityVariants 把 `/pv/<token>/<cid>sm.mp4` 展开成高→低的一串地址。
// 拿不到 token 目录或文件名异常时返回 nil（调用方退回单条原地址）。
func pvQualityVariants(pvPath string) []string {
	if !strings.HasSuffix(pvPath, ".mp4") {
		return nil
	}
	i := strings.LastIndex(pvPath, "/")
	if i <= 0 {
		return nil
	}
	dir := pvPath[:i+1]
	stem := stripQuality(strings.TrimSuffix(pvPath[i+1:], ".mp4"))
	if stem == "" {
		return nil
	}
	out := make([]string, 0, len(pvQualityLadder))
	for _, q := range pvQualityLadder {
		out = append(out, dir+stem+q+".mp4")
	}
	return out
}
