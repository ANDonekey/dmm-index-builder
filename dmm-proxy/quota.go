package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// errQuotaExceeded 用于在读取中途掐断上游连接。
var errQuotaExceeded = errors.New("proxy quota exceeded")

// Quota 统计「经代理回源」消耗的节点流量，并在超限时拒绝新的回源。
//
// 计量口径：所有配额与统计都按**计费后**的字节算（默认 ×0.1，即 0.1 倍节点），
// 这样你在订阅后台看到的扣量和服务里的数字是一致的。
// 只想按真实字节计时把 QUOTA_BILLING_FACTOR 设成 1。
type Quota struct {
	mu     sync.Mutex
	path   string
	factor float64

	dailyLimit   int64 // 计费口径，0 = 不限
	monthlyLimit int64

	day         string // 2006-01-02
	month       string // 2006-01
	dailyUsed   int64  // 计费口径
	monthlyUsed int64
	dailyRaw    int64 // 真实字节，仅用于展示
	monthlyRaw  int64

	dirty bool
}

type quotaFile struct {
	Day         string `json:"day"`
	Month       string `json:"month"`
	DailyUsed   int64  `json:"daily_used"`
	MonthlyUsed int64  `json:"monthly_used"`
	DailyRaw    int64  `json:"daily_raw"`
	MonthlyRaw  int64  `json:"monthly_raw"`
}

// QuotaSnap 是一次回源开始时的用量快照，用于下载结束后按真实字节回算。
type QuotaSnap struct {
	Day, Month           string
	Daily, Monthly       int64
	DailyRaw, MonthlyRaw int64
}

func NewQuota(cfg *Config) (*Quota, error) {
	q := &Quota{
		path:         filepath.Join(cfg.CacheDir, "quota.json"),
		factor:       cfg.QuotaFactor,
		dailyLimit:   cfg.QuotaDailyBytes,
		monthlyLimit: cfg.QuotaMonthlyBytes,
	}
	if q.factor <= 0 || q.factor > 1 {
		q.factor = 1
	}
	q.load()
	log.Printf("quota: 日 %s / 月 %s（计费口径，系数 %.2f）",
		humanBytes(q.dailyUsed, q.dailyLimit), humanBytes(q.monthlyUsed, q.monthlyLimit), q.factor)
	return q, nil
}

func (q *Quota) load() {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now()
	q.day, q.month = now.Format("2006-01-02"), now.Format("2006-01")

	b, err := os.ReadFile(q.path)
	if err != nil {
		return
	}
	var f quotaFile
	if err := json.Unmarshal(b, &f); err != nil {
		log.Printf("quota: 状态文件损坏，重新开始计数: %v", err)
		return
	}
	// 跨天只清日计数，跨月两个都清
	if f.Month == q.month {
		q.monthlyUsed, q.monthlyRaw = f.MonthlyUsed, f.MonthlyRaw
	}
	if f.Day == q.day {
		q.dailyUsed, q.dailyRaw = f.DailyUsed, f.DailyRaw
	}
}

// rolloverLocked 必须在持有锁时调用。
func (q *Quota) rolloverLocked(now time.Time) {
	day, month := now.Format("2006-01-02"), now.Format("2006-01")
	if day != q.day {
		q.day, q.dailyUsed, q.dailyRaw = day, 0, 0
		q.dirty = true
	}
	if month != q.month {
		q.month, q.monthlyUsed, q.monthlyRaw = month, 0, 0
		q.dirty = true
	}
}

// Bill 把真实字节换算成计费字节。
func (q *Quota) Bill(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return int64(float64(n)*q.factor + 0.5)
}

// Check 判断还能不能发起一次预计消耗 billed（计费口径）字节的回源。
// 返回 (是否允许, 超的是哪个额度)。
func (q *Quota) Check(billed int64) (bool, string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.rolloverLocked(time.Now())
	if q.dailyLimit > 0 && q.dailyUsed+billed > q.dailyLimit {
		return false, "daily"
	}
	if q.monthlyLimit > 0 && q.monthlyUsed+billed > q.monthlyLimit {
		return false, "monthly"
	}
	return true, ""
}

// Snap 记录当前用量；配合 Settle 在下载完成后按真实字节回算。
func (q *Quota) Snap() QuotaSnap {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.rolloverLocked(time.Now())
	return QuotaSnap{
		Day: q.day, Month: q.month,
		Daily: q.dailyUsed, Monthly: q.monthlyUsed,
		DailyRaw: q.dailyRaw, MonthlyRaw: q.monthlyRaw,
	}
}

// Reserve 先把预计消耗预扣上，防止并发回源绕过限额。
func (q *Quota) Reserve(billed, raw int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.rolloverLocked(time.Now())
	q.dailyUsed += billed
	q.monthlyUsed += billed
	q.dailyRaw += raw
	q.monthlyRaw += raw
	q.dirty = true
}

// Settle 用真实字节覆盖预扣值（下载可能提前中断）。
func (q *Quota) Settle(s QuotaSnap, actual int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.rolloverLocked(time.Now())
	billed := q.Bill(actual)
	if q.day == s.Day {
		q.dailyUsed = s.Daily + billed
		q.dailyRaw = s.DailyRaw + actual
	}
	if q.month == s.Month {
		q.monthlyUsed = s.Monthly + billed
		q.monthlyRaw = s.MonthlyRaw + actual
	}
	q.dirty = true
}

// Add 直接累加（用于未知长度的直通响应）。
func (q *Quota) Add(raw int64) {
	q.Settle(q.Snap(), raw)
}

// HasRoom 供流式读取时做中途熔断。
func (q *Quota) HasRoom() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.rolloverLocked(time.Now())
	if q.dailyLimit > 0 && q.dailyUsed >= q.dailyLimit {
		return false
	}
	if q.monthlyLimit > 0 && q.monthlyUsed >= q.monthlyLimit {
		return false
	}
	return true
}

// Usage 返回 (日已用, 日上限, 月已用, 月上限)，均为计费口径。
func (q *Quota) Usage() (int64, int64, int64, int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.rolloverLocked(time.Now())
	return q.dailyUsed, q.dailyLimit, q.monthlyUsed, q.monthlyLimit
}

// Flush 落盘。
func (q *Quota) Flush() {
	q.mu.Lock()
	f := quotaFile{
		Day: q.day, Month: q.month,
		DailyUsed: q.dailyUsed, MonthlyUsed: q.monthlyUsed,
		DailyRaw: q.dailyRaw, MonthlyRaw: q.monthlyRaw,
	}
	q.dirty = false
	q.mu.Unlock()

	b, err := json.Marshal(f)
	if err != nil {
		return
	}
	tmp := q.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Printf("quota: 写状态失败: %v", err)
		return
	}
	_ = os.Rename(tmp, q.path)
}

// Run 每 30 秒落盘一次，直到 stop 关闭。
func (q *Quota) Run(stop <-chan struct{}) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			q.mu.Lock()
			dirty := q.dirty
			q.mu.Unlock()
			if dirty {
				q.Flush()
			}
		case <-stop:
			q.Flush()
			return
		}
	}
}

// ─────────────────────────── 流式计数 ───────────────────────────

// quotaReader 边读边计数；超限或超过单次上限时直接中断上游读取。
type quotaReader struct {
	q        *Quota
	r        io.Reader
	n        int64
	maxBytes int64 // 单次请求真实字节硬上限，0 = 不限
	hard     bool  // true = 总额度用完就中断（用于未知长度的直通）
}

func (qr *quotaReader) Read(p []byte) (int, error) {
	n, err := qr.r.Read(p)
	qr.n += int64(n)
	if qr.maxBytes > 0 && qr.n > qr.maxBytes {
		return n, errQuotaExceeded
	}
	if qr.hard && !qr.q.HasRoom() {
		return n, errQuotaExceeded
	}
	return n, err
}

// ─────────────────────────── 回源频次限流 ───────────────────────────

// rateLimiter 固定窗口计数，限制单位时间内的回源次数。
// 防的是「刷不同 URL」——每部片都只回源一次，缓存层挡不住这种打法。
type rateLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	count   int
	resetAt time.Time
}

func newRateLimiter(perMin int) *rateLimiter {
	return &rateLimiter{limit: perMin, window: time.Minute, resetAt: time.Now().Add(time.Minute)}
}

func (rl *rateLimiter) Allow() bool {
	if rl.limit <= 0 {
		return true
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	if now.After(rl.resetAt) {
		rl.count, rl.resetAt = 0, now.Add(rl.window)
	}
	if rl.count >= rl.limit {
		return false
	}
	rl.count++
	return true
}

func humanBytes(used, limit int64) string {
	const gb = 1 << 30
	const mb = 1 << 20
	f := func(n int64) string {
		switch {
		case n >= gb:
			return fmt.Sprintf("%.2fGB", float64(n)/gb)
		case n >= mb:
			return fmt.Sprintf("%.1fMB", float64(n)/mb)
		default:
			return fmt.Sprintf("%.0fKB", float64(n)/1024)
		}
	}
	if limit <= 0 {
		return f(used) + " (不限)"
	}
	return fmt.Sprintf("%s / %s", f(used), f(limit))
}
