package main

import (
	"os"
	"testing"
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
