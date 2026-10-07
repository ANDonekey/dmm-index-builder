package main

import (
	"io"
	"net/http"
	"testing"
)

func rebuildQuota(t *testing.T, s *Server) {
	t.Helper()
	q, err := NewQuota(s.cfg)
	if err != nil {
		t.Fatalf("NewQuota: %v", err)
	}
	s.quota = q
}

// ── 回源流量被正确计入（factor=1，按真实字节）──
func TestQuota_CountedAfterFetch(t *testing.T) {
	up := newFakeUpstream(t, 1<<20)
	s, srv, _ := newTestServer(t, up, 8<<20)

	r := do(t, srv, http.MethodGet, "cc3001.example.com", testPath, nil)
	readAll(t, r)
	if r.StatusCode != 200 {
		t.Fatalf("状态码 %d", r.StatusCode)
	}
	du, _, mu, _ := s.quota.Usage()
	if du < 1<<20 || du > (1<<20)+8192 {
		t.Errorf("日用量 %d，应在 1MB 附近", du)
	}
	if mu < 1<<20 {
		t.Errorf("月用量 %d 未累计", mu)
	}
}

// ── 计费系数：0.1 倍节点只计 1/10 ──
func TestQuota_BillingFactor(t *testing.T) {
	up := newFakeUpstream(t, 1<<20)
	s, srv, _ := newTestServer(t, up, 8<<20)
	s.cfg.QuotaFactor = 0.1
	rebuildQuota(t, s)

	r := do(t, srv, http.MethodGet, "cc3001.example.com", testPath, nil)
	readAll(t, r)
	if r.StatusCode != 200 {
		t.Fatalf("状态码 %d", r.StatusCode)
	}
	du, _, _, _ := s.quota.Usage()
	want := int64(1<<20) / 10
	if du < want*9/10 || du > want*11/10 {
		t.Errorf("0.1 倍计费后日用量 %d，应在 %d 附近", du, want)
	}
}

// ── 日配额用尽：新片回源被拒，且不落盘 ──
func TestQuota_ExceededBlocksFetch(t *testing.T) {
	up := newFakeUpstream(t, 1<<20)
	s, srv, dir := newTestServer(t, up, 8<<20)
	s.cfg.QuotaDailyBytes = 4096
	rebuildQuota(t, s)

	r := do(t, srv, http.MethodGet, "cc3001.example.com", testPath, nil)
	readAll(t, r)
	if r.StatusCode != http.StatusTooManyRequests {
		t.Errorf("状态码 %d want 429", r.StatusCode)
	}
	if got := r.Header.Get("X-Cache"); got != "quota-exceeded:daily" {
		t.Errorf("X-Cache=%q want quota-exceeded:daily", got)
	}
	if got := r.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("429 必须 no-store，实际 %q", got)
	}
	if files := mp4Files(t, dir); len(files) != 0 {
		t.Errorf("超限时不应落盘，实际 %d 个文件", len(files))
	}
	if du, _, _, _ := s.quota.Usage(); du != 0 {
		t.Errorf("被拒的请求不应计费，实际 %d", du)
	}
}

// ── 核心：配额用尽后，已经缓存的内容照常能看 ──
func TestQuota_CachedContentStillServedWhenExhausted(t *testing.T) {
	up := newFakeUpstream(t, 1<<20)
	s, srv, dir := newTestServer(t, up, 8<<20)
	s.cfg.QuotaDailyBytes = 1 << 20 // 刚好够下这一部
	rebuildQuota(t, s)

	r1 := do(t, srv, http.MethodGet, "cc3001.example.com", testPath, nil)
	readAll(t, r1)
	if r1.StatusCode != 200 {
		t.Fatalf("首次状态码 %d", r1.StatusCode)
	}
	if files := waitFiles(t, dir, 1); len(files) != 1 {
		t.Fatalf("首次未落盘（%d 个）", len(files))
	}

	// 配额已经用光了
	r2 := do(t, srv, http.MethodGet, "cc3001.example.com", testPath, nil)
	readAll(t, r2)
	if r2.StatusCode != 200 {
		t.Errorf("配额用尽后已缓存内容应仍能播放，实际 %d", r2.StatusCode)
	}
	if got := r2.Header.Get("X-Cache"); got != "disk" {
		t.Errorf("X-Cache=%q want disk", got)
	}

	// 换一部没缓存的，就该被拒
	r3 := do(t, srv, http.MethodGet, "cc3001.example.com",
		"/litevideo/freepv/9/999/999abc999/999abc999_mhb_w.mp4", nil)
	readAll(t, r3)
	if r3.StatusCode != http.StatusTooManyRequests {
		t.Errorf("新片应被拒，实际 %d", r3.StatusCode)
	}
}

// ── 单次回源硬上限：超大文件被掐断，不落盘、不超额计费 ──
func TestQuota_MaxPerRequest(t *testing.T) {
	up := newFakeUpstream(t, 1<<20)
	s, srv, dir := newTestServer(t, up, 8<<20)
	s.cfg.QuotaMaxPerRequest = 64 << 10

	r := do(t, srv, http.MethodGet, "cc3001.example.com", testPath, nil)
	_, _ = io.Copy(io.Discard, r.Body) // 响应被截断，读会报错，忽略
	r.Body.Close()

	du, _, _, _ := s.quota.Usage()
	if du > 200<<10 {
		t.Errorf("单次上限 64KB 未生效，已计费 %d", du)
	}
	if files := mp4Files(t, dir); len(files) != 0 {
		t.Errorf("被掐断的下载不应落盘，实际 %d 个", len(files))
	}
}

// ── 回源频次限流 ──
func TestQuota_RateLimit(t *testing.T) {
	up := newFakeUpstream(t, 1<<20)
	s, srv, _ := newTestServer(t, up, 8<<20)
	s.cfg.RateLimitPerMin = 2
	s.limiter = newRateLimiter(2)
	s.cfg.QuotaDailyBytes = 0 // 别让流量配额先触发
	rebuildQuota(t, s)

	var last int
	for i := 0; i < 4; i++ {
		r := do(t, srv, http.MethodGet, "cc3001.example.com",
			"/litevideo/freepv/v/v"+string(rune('0'+i))+"/v_sm_w.mp4", nil)
		readAll(t, r)
		last = r.StatusCode
		if i < 2 && r.StatusCode != 200 {
			t.Errorf("第 %d 次（限额内）状态码 %d", i+1, r.StatusCode)
		}
		if i >= 2 && r.StatusCode != http.StatusTooManyRequests {
			t.Errorf("第 %d 次（超频次）状态码 %d want 429", i+1, r.StatusCode)
		}
	}
	_ = last
}

// ── 月度配额独立生效 ──
func TestQuota_MonthlyLimit(t *testing.T) {
	up := newFakeUpstream(t, 1<<20)
	s, srv, _ := newTestServer(t, up, 8<<20)
	s.cfg.QuotaMonthlyBytes = 4096
	s.cfg.QuotaDailyBytes = 0
	rebuildQuota(t, s)

	r := do(t, srv, http.MethodGet, "cc3001.example.com", testPath, nil)
	readAll(t, r)
	if r.StatusCode != http.StatusTooManyRequests {
		t.Errorf("状态码 %d want 429", r.StatusCode)
	}
	if got := r.Header.Get("X-Cache"); got != "quota-exceeded:monthly" {
		t.Errorf("X-Cache=%q want quota-exceeded:monthly", got)
	}
}

// ── 配额状态能落盘并在重启后保留 ──
func TestQuota_PersistsAcrossRestart(t *testing.T) {
	up := newFakeUpstream(t, 1<<20)
	s, srv, _ := newTestServer(t, up, 8<<20)

	r := do(t, srv, http.MethodGet, "cc3001.example.com", testPath, nil)
	readAll(t, r)
	before, _, _, _ := s.quota.Usage()
	s.quota.Flush()

	q2, err := NewQuota(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	after, _, _, _ := q2.Usage()
	if after != before {
		t.Errorf("重启后用量 %d ≠ 重启前 %d", after, before)
	}
}
