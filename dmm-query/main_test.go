package main

import (
	"strings"
	"testing"
)

// 归一化必须与 cidkey.py::code_key / worker/index.ts::normalizeCode 完全一致，
// 否则库里的键对不上，查询静默 404。这批用例就是从审查报告里挑出来的坑。
func TestNormalizeCode(t *testing.T) {
	cases := []struct {
		in      string
		letters string
		num     int64
		ok      bool
	}{
		{"ABP-888", "abp", 888, true},
		{"abp888", "abp", 888, true},
		{"ABP 888", "abp", 888, true},
		{"118abp-888", "abp", 888, true},
		{"SSIS-095", "ssis", 95, true},
		{"STAR-359", "star", 359, true},
		{"1STAR-359", "star", 359, true},
		{"118tre00024", "tre", 24, true}, // 旧非贪婪正则的经典漏收案例
		{"ABC-123", "abc", 123, true},
		{"", "", 0, false},
		{"ABC", "", 0, false}, // 没有数字段
		{"123", "", 0, false}, // 没有字母段
		{"ABC-", "", 0, false},
	}
	for _, c := range cases {
		l, n, ok := normalizeCode(c.in)
		if ok != c.ok || (ok && (l != c.letters || n != c.num)) {
			t.Errorf("normalizeCode(%q) = (%q,%d,%v)，期望 (%q,%d,%v)",
				c.in, l, n, ok, c.letters, c.num, c.ok)
		}
	}
}

// fmt 决定路径前缀与下划线形态，四种组合都要覆盖 ——
// 历史上漏掉 fmt 导致 pv 新体系 9 万条「能搜到但播不了」。
func TestBuildPaths(t *testing.T) {
	s := &Server{cfg: Config{ProxyHost: "fastcdn.dpdns.org"}}
	cases := []struct {
		fmt      int
		wantPath string
	}{
		{0, "/litevideo/freepv/1/118/118abp888/118abp888_mhb_w.mp4"},
		{1, "/litevideo/freepv/1/118/118abp888/118abp888mhbw.mp4"},
		{2, "/pv/AbCdEf/118abp888_mhb_w.mp4"},
		{3, "/pv/AbCdEf/118abp888mhbw.mp4"},
	}
	for _, c := range cases {
		r := Row{cid: "118abp00888", cdn: "cc3001", dirpath: "1/118/118abp888",
			stem: "118abp888", quality: "mhb", variant: "w", fmt: c.fmt}
		if c.fmt >= 2 {
			r.dirpath = "AbCdEf"
		}
		got := s.build(r, "ABP-888")["dmmUrl"].(string)
		if !strings.HasSuffix(got, c.wantPath) {
			t.Errorf("fmt=%d dmmUrl=%q，期望以 %q 结尾", c.fmt, got, c.wantPath)
		}
	}
}

// HLS 的目录用完整 cid（小写），不是 mp4 那个去零 stem。
func TestHlsUrlUsesFullCid(t *testing.T) {
	s := &Server{cfg: Config{ProxyHost: "x", MirrorProxy: true, MirrorFirst: true}}
	r := Row{cid: "1STARS00359", cdn: "cc3001", dirpath: "1/1st/1stars359",
		stem: "1stars359", quality: "mhb", variant: "w"}
	var hls string
	for _, u := range s.build(r, "STAR-359")["urls"].([]map[string]string) {
		if u["type"] == "hls" {
			hls = u["url"]
		}
	}
	// URL 必须是**纯路径**：CF 对 /mirror/ 的缓存规则忽略 query，但浏览器缓存
	// 不是 —— 多带任何参数都会让老访客的缓存失效，命中率白掉。
	want := "https://cc3001.x/mirror/hlsvideo/freepv/1/1st/1stars00359/playlist.m3u8"
	if hls != want {
		t.Errorf("hls 候选 = %q，期望 %q", hls, want)
	}
}

// 关掉镜像反代时，不应下发任何镜像候选 —— 客户端只能走 proxy / direct
func TestNoMirrorProxyOmitsMirror(t *testing.T) {
	s := &Server{cfg: Config{ProxyHost: "x"}} // MirrorProxy 零值 false
	r := Row{cid: "ssis00095", cdn: "cc3001", dirpath: "s/ssi/ssis00095",
		stem: "ssis00095", quality: "mhb", variant: "w"}
	out := s.build(r, "SSIS-095")
	if _, ok := out["mirrorProxyUrl"]; ok {
		t.Error("MIRROR_PROXY 关闭时不应有 mirrorProxyUrl")
	}
	for _, u := range out["urls"].([]map[string]string) {
		if u["via"] == "mirror_proxy" {
			t.Error("MIRROR_PROXY 关闭时 urls 里不应有镜像候选")
		}
	}
}

// 候选顺序决定客户端先打谁：镜像优先 = 省反代配额，反代优先 = 图稳定。
func TestURLPriority(t *testing.T) {
	row := Row{cid: "118abp00888", cdn: "cc3001", dirpath: "1/118/118abp888",
		stem: "118abp888", quality: "mhb", variant: "w"}

	mirrorFirst := &Server{cfg: Config{ProxyHost: "p", MirrorProxy: true, MirrorFirst: true}}
	urls := mirrorFirst.build(row, "ABP-888")["urls"].([]map[string]string)
	want := []string{"mirror_proxy", "mirror_proxy", "proxy", "direct"}
	if len(urls) != len(want) {
		t.Fatalf("urls 数量 %d，期望 %d", len(urls), len(want))
	}
	for i, w := range want {
		if urls[i]["via"] != w {
			t.Errorf("第 %d 个候选 via=%q，期望 %q", i, urls[i]["via"], w)
		}
	}
	if urls[0]["type"] != "mp4" || urls[1]["type"] != "hls" {
		t.Error("镜像候选应先 mp4 后 hls")
	}

	proxyFirst := &Server{cfg: Config{ProxyHost: "p", MirrorProxy: true, MirrorFirst: false}}
	urls2 := proxyFirst.build(row, "ABP-888")["urls"].([]map[string]string)
	if urls2[0]["via"] != "proxy" {
		t.Errorf("反代优先时首个候选应为 proxy，实际 %q", urls2[0]["via"])
	}
	if urls2[len(urls2)-1]["via"] != "mirror_proxy" {
		t.Errorf("反代优先时镜像应排最后，实际 %q", urls2[len(urls2)-1]["via"])
	}
}
