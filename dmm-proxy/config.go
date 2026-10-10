package main

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config 全部由环境变量驱动，默认值与《VPS-反代服务实施计划.md》§五一致。
type Config struct {
	// —— 监听与白名单 ——
	Listen         string          // 监听地址，默认 127.0.0.1:8080（nginx 后面）
	AllowedCDN     map[string]bool // Host 首段白名单：cc3001 / pv3001
	HostSuffix     string          // 对外域名后缀，用于校验 Host
	UpstreamSuffix string          // 上游固定后缀：.dmm.co.jp
	// 路径白名单。原来是单个 /litevideo/freepv/，但 DMM 后来换了新体系：
	// /pv/<62位随机token>/<文件名>.mp4 —— 那 9 万条预览走旧前缀会被整类 403。
	// 两个前缀都必须放行，且上游路径是原样拼接的，所以前缀是什么就回源什么。
	PathPrefixes []string
	UpstreamScheme string          // https

	// —— 上游代理 ——
	ProxyURL *url.URL // socks5://127.0.0.1:1080（sing-box，dmm.co.jp 已分流到 jp-group）

	// —— 出站头 ——
	UA             string
	Referer        string
	Origin         string
	AcceptLanguage string

	// —— 缓存 ——
	CacheDir          string
	CacheMaxBytes     int64           // 硬上限，默认 6 GB
	CacheMinFreeBytes int64           // 分区剩余低于此值不再落盘，默认 2 GB
	CacheMaxDownloads int             // 并发回源数，默认 2
	CacheQualityAllow map[string]bool // 只缓存这些档位（空 = 全部）；档位取文件名末段前的字段
	MinBodyBytes      int64           // 小于此值视为“假 200”，不落盘，默认 1024

	// —— 流量限制（防止节点流量超支）——
	// 配额单位一律是「计费后」的字节（真实字节 × QuotaFactor），
	// 这样和订阅后台看到的扣量一致。想按真实字节计量就把系数设成 1。
	QuotaDailyBytes    int64   // 每日回源上限；0 = 不限
	QuotaMonthlyBytes  int64   // 每月回源上限；0 = 不限
	QuotaFactor        float64 // 计费系数：0.1 = 0.1 倍节点
	QuotaMaxPerRequest int64   // 单次回源真实字节硬上限；0 = 不限
	QuotaExceededCode  int     // 超限时的状态码，默认 429
	RateLimitPerMin    int     // 每分钟回源次数上限；0 = 不限

	// —— 其它 ——
	CacheControl    string        // 覆盖源站 no-store/no-cache
	UpstreamTimeout time.Duration // 单次回源总超时（含下载完整个文件）
	FlightWait      time.Duration // follower 等待 leader 下载完成的最长时间
}

func envStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt64(k string, def int64) int64 {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	return n
}

func envInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envDur(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

func envFloat(k string, def float64) float64 {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f <= 0 || f > 1 {
		return def
	}
	return f
}

func envSet(k string) map[string]bool {
	m := map[string]bool{}
	for _, s := range strings.Split(os.Getenv(k), ",") {
		s = strings.TrimSpace(strings.ToLower(s))
		if s != "" {
			m[s] = true
		}
	}
	return m
}

// envPathPrefixes 解析路径白名单：PATH_PREFIXES（逗号分隔）优先，
// 退回旧的 PATH_PREFIX（单值），都没配就用两个官方前缀。
func envPathPrefixes() []string {
	raw := strings.TrimSpace(os.Getenv("PATH_PREFIXES"))
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("PATH_PREFIX"))
	}
	if raw == "" {
		raw = "/litevideo/freepv/,/pv/"
	}
	out := []string{}
	for _, s := range strings.Split(raw, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func LoadConfig() (*Config, error) {
	c := &Config{
		Listen:         envStr("LISTEN", "127.0.0.1:8080"),
		HostSuffix:     envStr("HOST_SUFFIX", ""),
		UpstreamSuffix: envStr("UPSTREAM_SUFFIX", ".dmm.co.jp"),
		// 兼容旧的 PATH_PREFIX（单值）。新部署请用 PATH_PREFIXES（逗号分隔）。
		PathPrefixes: envPathPrefixes(),
		UpstreamScheme: envStr("UPSTREAM_SCHEME", "https"),

		UA:             envStr("UA", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"),
		Referer:        envStr("REFERER", "https://www.dmm.co.jp/"),
		Origin:         envStr("ORIGIN", "https://www.dmm.co.jp"),
		AcceptLanguage: envStr("ACCEPT_LANGUAGE", "ja-JP,ja;q=0.9,en;q=0.8"),

		CacheDir:          envStr("CACHE_DIR", "/var/cache/dmmpv"),
		CacheMaxBytes:     envInt64("CACHE_MAX_BYTES", 6*1024*1024*1024),
		CacheMinFreeBytes: envInt64("CACHE_MIN_FREE_BYTES", 2*1024*1024*1024),
		CacheMaxDownloads: envInt("CACHE_MAX_DOWNLOADS", 2),
		CacheQualityAllow: envSet("CACHE_QUALITY_ALLOW"),
		MinBodyBytes:      envInt64("MIN_BODY_BYTES", 1024),

		CacheControl:    envStr("CACHE_CONTROL", "public, max-age=31536000, s-maxage=31536000, immutable"),
		UpstreamTimeout: envDur("UPSTREAM_TIMEOUT", 10*time.Minute),
		FlightWait:      envDur("FLIGHT_WAIT", 3*time.Minute),
	}

	// 流量限制：默认按 0.1 倍节点计费口径，日 2GB / 月 30GB
	c.QuotaDailyBytes = envInt64("QUOTA_DAILY_BYTES", 2*1024*1024*1024)
	c.QuotaMonthlyBytes = envInt64("QUOTA_MONTHLY_BYTES", 30*1024*1024*1024)
	c.QuotaFactor = envFloat("QUOTA_BILLING_FACTOR", 0.1)
	c.QuotaMaxPerRequest = envInt64("QUOTA_MAX_PER_REQUEST", 64*1024*1024)
	c.QuotaExceededCode = envInt("QUOTA_EXCEEDED_CODE", http.StatusTooManyRequests)
	c.RateLimitPerMin = envInt("RATE_LIMIT_PER_MIN", 60)

	c.AllowedCDN = envSet("ALLOWED_CDN")
	if len(c.AllowedCDN) == 0 {
		// ⚠️ 必须与 build-index.py 的 CDN_HOSTS 保持一致：那边认得、这边不放行，
		//    结果就是索引库里有一批行、反代对它们一律 403（审查报告 P1-6）。
		//    改一边就要改另一边，两边都写了这条注释互相指向。
		c.AllowedCDN = map[string]bool{"cc3001": true, "pv3001": true, "cc3002": true, "cc3003": true}
	}

	raw := envStr("UPSTREAM_PROXY", "socks5://127.0.0.1:1080")
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("UPSTREAM_PROXY 解析失败: %w", err)
	}
	if u.Scheme != "socks5" && u.Scheme != "socks5h" && u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("UPSTREAM_PROXY scheme 不支持: %s", u.Scheme)
	}
	c.ProxyURL = u

	// 启动校验：这些错在部署时就应该炸掉，而不是等到线上返回 403
	if c.HostSuffix == "" {
		return nil, fmt.Errorf("HOST_SUFFIX 必须设置（例如 example.com）")
	}
	c.HostSuffix = strings.TrimPrefix(c.HostSuffix, ".")

	if err := os.MkdirAll(c.CacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("CACHE_DIR 不可写: %w", err)
	}
	if abs, err := filepath.Abs(c.CacheDir); err == nil {
		c.CacheDir = abs
	}
	return c, nil
}
