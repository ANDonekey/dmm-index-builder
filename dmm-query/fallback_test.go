package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"
)

// 真实搜索页样本（2026-11-12 抓的 FNS-272 结果页，85KB）
func loadFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("读 fixture 失败: %v", err)
	}
	return string(b)
}

// 这是整个回退链路的地基：搜索页里 cid= 是**去零短形态**（1fns272），
// 而镜像路径要的是完整形态（1fns00272）。完整形态只能从 /pv/ 直链的文件名里取。
func TestPickCIDFromPVURL(t *testing.T) {
	html := loadFixture(t, "search-fns272.html")

	cid, pv := pickCID(html, "FNS-272")
	if cid != "1fns00272" {
		t.Fatalf("cid = %q，期望 1fns00272", cid)
	}
	if pv == "" || pv[len(pv)-4:] != ".mp4" {
		t.Fatalf("pv 直链异常: %q", pv)
	}
}

// 各种写法都要能命中同一条
func TestPickCIDNormalizesCode(t *testing.T) {
	html := loadFixture(t, "search-fns272.html")
	for _, code := range []string{"FNS-272", "fns272", "FNS 272", "fns-0272"} {
		if cid, _ := pickCID(html, code); cid != "1fns00272" {
			t.Fatalf("%q 应该命中 1fns00272，得到 %q", code, cid)
		}
	}
}

// 不存在的番号不能误命中：搜索页里有一堆相似结果（1fns272br / 1fns272tk）
func TestPickCIDRejectsNearMiss(t *testing.T) {
	html := loadFixture(t, "search-fns272.html")
	if cid, _ := pickCID(html, "FNS-273"); cid != "" {
		t.Fatalf("FNS-273 不该命中任何结果，得到 %q", cid)
	}
	if cid, _ := pickCID(html, "ABP-888"); cid != "" {
		t.Fatalf("ABP-888 不该命中，得到 %q", cid)
	}
}

// 清晰度后缀要从长到短匹配，否则 dmb 会被 dm 切坏
func TestStripQuality(t *testing.T) {
	cases := map[string]string{
		"1fns00272sm":    "1fns00272",
		"1fns00272dmb":   "1fns00272",
		"1fns00272mhb_w": "1fns00272",
		"1fns00272hhb4k": "1fns00272",
		"1fns002724k":    "1fns00272",
		"1fns272brsm":    "1fns272br",
	}
	for in, want := range cases {
		if got := stripQuality(in); got != want {
			t.Errorf("stripQuality(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestPadCID(t *testing.T) {
	cases := map[string]string{
		"1fns272":   "1fns00272",
		"1fns00272": "1fns00272",
		"ssis95":    "ssis00095",
		"abc":       "abc",
		"1fns272br": "1fns272br", // 没有数字段，原样返回
	}
	for in, want := range cases {
		if got := padCID(in); got != want {
			t.Errorf("padCID(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// 镜像路径是 cid 的纯函数，这条规则错了整条链路就全 404
func TestMirrorHLSPath(t *testing.T) {
	cases := map[string]string{
		"1fns00272":   "/hlsvideo/freepv/1/1fn/1fns00272/playlist.m3u8",
		"ssis00095":   "/hlsvideo/freepv/s/ssi/ssis00095/playlist.m3u8",
		"1STARS00359": "/hlsvideo/freepv/1/1st/1stars00359/playlist.m3u8",
		"ab":          "",
	}
	for cid, want := range cases {
		if got := mirrorHLSPath(cid); got != want {
			t.Errorf("mirrorHLSPath(%q) = %q，期望 %q", cid, got, want)
		}
	}
}

func TestPVPath(t *testing.T) {
	in := "https://cc3001.dmm.co.jp/pv/Ki0iIho1SjkWIQRZ/1fns00272sm.mp4"
	want := "/pv/Ki0iIho1SjkWIQRZ/1fns00272sm.mp4"
	if got := pvPath(in); got != want {
		t.Fatalf("pvPath = %q，期望 %q", got, want)
	}
	if got := pvPath("https://example.com/x.mp4"); got != "" {
		t.Fatalf("非 /pv/ 地址应返回空，得到 %q", got)
	}
}

/* ---------- affiliate API 主路 ---------- */

// 真实响应结构（DMM ItemList v3）。同一部片通常同时有数字层(videoa)和 DVD(mono)
// 两条：数字层 content_id 是 `1fns00272`，DVD 层是去零的 `1fns272`。
const affiliateFixture = `{
  "request": {"parameters": {"keyword": "FNS-272"}},
  "result": {
    "status": 200,
    "result_count": 3,
    "items": [
      {"service_code":"mono","floor_code":"dvd","content_id":"1fns272",
       "maker_product":"FNS-272","URL":"https://www.dmm.co.jp/mono/dvd/-/detail/=/cid=1fns272/"},
      {"service_code":"digital","floor_code":"videoa","content_id":"1fns00272",
       "maker_product":"FNS-272","URL":"https://www.dmm.co.jp/digital/videoa/-/detail/=/cid=1fns00272/"},
      {"service_code":"digital","floor_code":"videoa","content_id":"1fns00272br",
       "maker_product":"FNS-272","URL":"https://www.dmm.co.jp/digital/videoa/-/detail/=/cid=1fns00272br/"}
    ]
  }
}`

func parseAffiliate(t *testing.T, raw string) []affiliateItem {
	t.Helper()
	var payload affiliateResponse
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("解析 fixture 失败: %v", err)
	}
	if payload.Result.Status != 200 {
		t.Fatalf("fixture status = %d", payload.Result.Status)
	}
	return payload.Result.Items
}

// 同键多条时优先数字层：videoa 的 content_id 就是镜像路径要的完整形态。
// 样本是 2026-11-12 抓的真实响应（keyword=FNS00272）。
func TestPickAffiliateCIDPrefersVideoa(t *testing.T) {
	items := parseAffiliate(t, loadFixture(t, "affiliate-fns272.json"))
	if got := pickAffiliateCID(items, "FNS-272"); got != "1fns00272" {
		t.Fatalf("cid = %q，期望 1fns00272", got)
	}
}

// 只有 DVD 条目时，`1fns272` 这种短形态要补零成完整 cid
func TestPickAffiliateCIDPadsMonoOnly(t *testing.T) {
	items := parseAffiliate(t, `{"result":{"status":200,"result_count":1,"items":[
	  {"service_code":"mono","floor_code":"dvd","content_id":"1fns272","maker_product":"FNS-272"}]}}`)
	if got := pickAffiliateCID(items, "FNS-272"); got != "1fns00272" {
		t.Fatalf("cid = %q，期望 1fns00272", got)
	}
}

// ⚠️ 但 DVD 层不全是短形态：ABP-888 只有 `118abp888`，补零成 `118abp00888`
// 就是一条不存在的路径。这个用例存在就是为了防止有人把补零改回无条件的。
func TestPickAffiliateCIDDoesNotPadMultiDigitPrefix(t *testing.T) {
	items := parseAffiliate(t, `{"result":{"status":200,"result_count":2,"items":[
	  {"service_code":"mono","floor_code":"dvd","content_id":"118abp888","maker_product":"ABP-888"},
	  {"service_code":"mono","floor_code":"dvd","content_id":"118abp888dod","maker_product":"ABP-888"}]}}`)
	if got := pickAffiliateCID(items, "ABP-888"); got != "118abp888" {
		t.Fatalf("cid = %q，期望 118abp888（不能被补零成 118abp00888）", got)
	}
}

// 数字层（standard / premium）同样直接可用
func TestPickAffiliateCIDAcceptsDigitalFloors(t *testing.T) {
	items := parseAffiliate(t, `{"result":{"status":200,"result_count":2,"items":[
	  {"service_code":"digital","floor_code":"standard","content_id":"ssis00095","maker_product":"SSIS-095"},
	  {"service_code":"mono","floor_code":"dvd","content_id":"ssis095","maker_product":"SSIS-095"}]}}`)
	if got := pickAffiliateCID(items, "SSIS-095"); got != "ssis00095" {
		t.Fatalf("cid = %q，期望 ssis00095", got)
	}
}

// 相似番号不能被当成命中（FNS-273 的结果不能给 FNS-272）
func TestPickAffiliateCIDRejectsNearMiss(t *testing.T) {
	items := parseAffiliate(t, `{"result":{"status":200,"result_count":1,"items":[
	  {"service_code":"digital","floor_code":"videoa","content_id":"1fns00273","maker_product":"FNS-273"}]}}`)
	if got := pickAffiliateCID(items, "FNS-272"); got != "" {
		t.Fatalf("不应命中，得到 %q", got)
	}
}

// 补零关键词要排在最前：原码更容易先命中 DVD 那条
func TestKeywordVariants(t *testing.T) {
	got := keywordVariants("FNS-272")
	want := []string{"FNS00272", "FNS-272", "FNS272"}
	if len(got) != len(want) {
		t.Fatalf("变体数量 %d，期望 %d（%v）", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("变体[%d] = %q，期望 %q（全量 %v）", i, got[i], want[i], got)
		}
	}
}

// 端到端：假 DMM 服务 + 真实 HTTP 客户端
func TestFetchViaAPIIntegration(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, affiliateFixture)
	}))
	defer srv.Close()

	r := &Resolver{
		cfg:       FallbackConfig{APIID: "id", AffiliateID: "aff", APIBase: srv.URL, Timeout: 5 * time.Second},
		apiClient: &http.Client{Timeout: 5 * time.Second},
	}

	entry, err := r.fetchViaAPI("FNS-272")
	if err != nil {
		t.Fatalf("fetchViaAPI 失败: %v", err)
	}
	if entry == nil || entry.CID != "1fns00272" {
		t.Fatalf("cid 异常: %+v", entry)
	}
	if gotQuery.Get("api_id") != "id" || gotQuery.Get("affiliate_id") != "aff" {
		t.Fatalf("凭据没带上: %v", gotQuery)
	}
	if gotQuery.Get("site") != "FANZA" || gotQuery.Get("sort") != "match" {
		t.Fatalf("参数异常: %v", gotQuery)
	}
}

// 400（api_id 限流/失效）必须是**硬失败**，由调用方降级到搜索页；
// 不能当成「这部片不存在」写进负结果。
func TestFetchViaAPIRateLimitedIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"result":{"status":400,"errors":{"api_id":"Invalid Request Error"}}}`)
	}))
	defer srv.Close()

	r := &Resolver{
		cfg:       FallbackConfig{APIID: "id", AffiliateID: "aff", APIBase: srv.URL, Timeout: 5 * time.Second},
		apiClient: &http.Client{Timeout: 5 * time.Second},
	}
	if _, err := r.fetchViaAPI("FNS-272"); err == nil {
		t.Fatal("限流应返回错误，实际返回 nil")
	}
}

// 0 命中 = 确实没有，返回 (nil, nil)，不写负结果也不需要降级
func TestFetchViaAPINoResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"result":{"status":200,"result_count":0,"items":[]}}`)
	}))
	defer srv.Close()

	r := &Resolver{
		cfg:       FallbackConfig{APIID: "id", AffiliateID: "aff", APIBase: srv.URL, Timeout: 5 * time.Second},
		apiClient: &http.Client{Timeout: 5 * time.Second},
	}
	entry, err := r.fetchViaAPI("QQQ-77777")
	if err != nil || entry != nil {
		t.Fatalf("期望 (nil,nil)，得到 (%+v, %v)", entry, err)
	}
}
