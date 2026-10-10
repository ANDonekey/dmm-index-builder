package main

import (
	"net/http"
	"net/url"
	"testing"
)

func testConfig() *Config {
	return &Config{
		HostSuffix:     "example.com",
		UpstreamSuffix: ".dmm.co.jp",
		UpstreamScheme: "https",
		PathPrefixes:   []string{"/litevideo/freepv/", "/pv/"},
		AllowedCDN:     map[string]bool{"cc3001": true, "pv3001": true},
		CacheQualityAllow: map[string]bool{
			"sm": true, "dm": true, "dmb": true,
		},
	}
}

func TestResolve(t *testing.T) {
	s := &Server{cfg: testConfig()}
	cases := []struct {
		name, host, path string
		want             string
		ok               bool
	}{
		{"正常", "cc3001.example.com", "/litevideo/freepv/1/118/118abp888/118abp888_mhb_w.mp4",
			"https://cc3001.dmm.co.jp/litevideo/freepv/1/118/118abp888/118abp888_mhb_w.mp4", true},
		{"第二个 cdn", "pv3001.example.com", "/litevideo/freepv/a/b_c_sm_w.mp4",
			"https://pv3001.dmm.co.jp/litevideo/freepv/a/b_c_sm_w.mp4", true},
		{"带端口", "cc3001.example.com:443", "/litevideo/freepv/a/b_c_sm_w.mp4",
			"https://cc3001.dmm.co.jp/litevideo/freepv/a/b_c_sm_w.mp4", true},
		{"未授权 cdn", "cc9999.example.com", "/litevideo/freepv/a/b_c_sm_w.mp4", "", false},
		{"非本域", "cc3001.evil.com", "/litevideo/freepv/a/b_c_sm_w.mp4", "", false},
		{"路径前缀错", "cc3001.example.com", "/other/a/b_c_sm_w.mp4", "", false},
		{"非 mp4", "cc3001.example.com", "/litevideo/freepv/a/b_c_sm_w.m3u8", "", false},
		{"目录穿越", "cc3001.example.com", "/litevideo/freepv/../../etc/passwd", "", false},
		{"编码穿越", "cc3001.example.com", "/litevideo/freepv/%2e%2e/%2e%2e/etc/passwd.mp4", "", false},
		{"绝对路径", "cc3001.example.com", "/litevideo/freepv//etc/passwd.mp4", "", false},
		// 新体系 /pv/<token>/<file>.mp4：token 是 62 位随机串，每作品一个
		{"pv 新体系", "cc3001.example.com", "/pv/PTd7ARkWDp2tJrweQGyEiWfriWAl6-N2L3RQojqzKexVXZ4o7vqOmP2mkgbgcR_Z/1sdjs00383mhb.mp4",
			"https://cc3001.dmm.co.jp/pv/PTd7ARkWDp2tJrweQGyEiWfriWAl6-N2L3RQojqzKexVXZ4o7vqOmP2mkgbgcR_Z/1sdjs00383mhb.mp4", true},
		{"pv token 含加号", "cc3001.example.com", "/pv/ab+cD_1-2/a_b_mhb_w.mp4",
			"https://cc3001.dmm.co.jp/pv/ab+cD_1-2/a_b_mhb_w.mp4", true},
		{"pv 空 token", "cc3001.example.com", "/pv//a_mhb_w.mp4", "", false},
		{"pv 目录穿越", "cc3001.example.com", "/pv/../../etc/passwd.mp4", "", false},
	}
	for _, c := range cases {
		u, _ := url.Parse(c.path)
		r := &http.Request{Host: c.host, URL: u, Method: http.MethodGet}
		got, ok := s.resolve(r)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("%s: got (%q,%v) want (%q,%v)", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestParseRange(t *testing.T) {
	const size = 41316658
	cases := []struct {
		in            string
		start, length int64
		ok            bool
	}{
		{"bytes=0-1023", 0, 1024, true},
		{"bytes=0-", 0, size, true},
		{"bytes=100-200", 100, 101, true},
		{"bytes=-500", size - 500, 500, true},
		{"bytes=99999999-", 0, 0, false}, // 起点越界
		{"bytes=0-0,100-200", 0, 0, false},
		{"items=0-5", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		s, l, ok := parseRange(c.in, size)
		if s != c.start || l != c.length || ok != c.ok {
			t.Errorf("parseRange(%q): got (%d,%d,%v) want (%d,%d,%v)", c.in, s, l, ok, c.start, c.length, c.ok)
		}
	}
}

func TestFileQuality(t *testing.T) {
	cases := map[string]string{
		// A 形态：{stem}_{quality}_{variant}.mp4
		"https://cc3001.dmm.co.jp/litevideo/freepv/1/118/118abp888/118abp888_mhb_w.mp4": "mhb",
		"https://cc3001.dmm.co.jp/litevideo/freepv/a/b_sm_w.mp4":                        "sm",
		// B 形态：{stem}{quality}{variant}.mp4（没有下划线 —— 旧实现在这里返回 ""）
		"https://cc3001.dmm.co.jp/pv/PTd7ARkWDp2t/1sdjs00383mhb.mp4":   "mhb",
		"https://cc3001.dmm.co.jp/litevideo/freepv/a/b/abcdmb.mp4":     "dmb",
		"https://cc3001.dmm.co.jp/pv/tok/1sdjs003834k.mp4":             "4k",
		// 认不出来的
		"https://cc3001.dmm.co.jp/litevideo/freepv/a/b.mp4": "",
	}
	for in, want := range cases {
		if got := fileQuality(in); got != want {
			t.Errorf("fileQuality(%q)=%q want %q", in, got, want)
		}
	}
}

func TestAllowCache(t *testing.T) {
	s := &Server{cfg: testConfig()}
	if !s.allowCache("https://x/litevideo/freepv/a/b_sm_w.mp4") {
		t.Error("sm 应允许缓存")
	}
	if s.allowCache("https://x/litevideo/freepv/a/b_mhb_w.mp4") {
		t.Error("mhb 不应缓存（白名单只有 sm/dm/dmb）")
	}
	open := &Server{cfg: testConfig()}
	open.cfg.CacheQualityAllow = nil
	if !open.allowCache("https://x/litevideo/freepv/a/b_mhb_w.mp4") {
		t.Error("空白名单应缓存全部档位")
	}
}

func TestWindowWriter(t *testing.T) {
	var got []byte
	w := &windowWriter{dst: writerFunc(func(p []byte) (int, error) {
		got = append(got, p...)
		return len(p), nil
	}), skip: 3, remain: 4}
	if _, err := w.Write([]byte("0123456789")); err != nil {
		t.Fatal(err)
	}
	if string(got) != "3456" {
		t.Errorf("windowWriter got %q want %q", got, "3456")
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
