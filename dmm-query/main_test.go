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
		{"ABC", "", 0, false},   // 没有数字段
		{"123", "", 0, false},   // 没有字母段
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
	s := &Server{cfg: Config{ProxyHost: "fastcdn.dpdns.org", MirrorHost: "media.javatrailers.com"}}
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
	s := &Server{cfg: Config{ProxyHost: "x", MirrorHost: "media.javatrailers.com"}}
	r := Row{cid: "1STARS00359", cdn: "cc3001", dirpath: "1/1st/1stars359",
		stem: "1stars359", quality: "mhb", variant: "w"}
	hls := s.build(r, "STAR-359")["hlsUrl"].(string)
	want := "https://media.javatrailers.com/hlsvideo/freepv/1/1st/1stars00359/playlist.m3u8"
	if hls != want {
		t.Errorf("hlsUrl = %q，期望 %q", hls, want)
	}
}

// 没配镜像时不应下发试探型字段，避免客户端去打一个不存在的域名
func TestNoMirrorOmitsFallback(t *testing.T) {
	s := &Server{cfg: Config{ProxyHost: "x"}}
	r := Row{cid: "ssis00095", cdn: "cc3001", dirpath: "s/ssi/ssis00095",
		stem: "ssis00095", quality: "mhb", variant: "w"}
	out := s.build(r, "SSIS-095")
	if _, ok := out["fallbackUrl"]; ok {
		t.Error("MIRROR_HOST 为空时不应有 fallbackUrl")
	}
	if _, ok := out["hlsUrl"]; ok {
		t.Error("MIRROR_HOST 为空时不应有 hlsUrl")
	}
}
