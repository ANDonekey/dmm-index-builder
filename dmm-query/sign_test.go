package main

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// 固定一个时刻，让 exp 可预测：(1782000000/86400+7)*86400
var testNow = time.Unix(1782000000, 0)

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", raw, err)
	}
	return u
}

func TestSignRoundTrip(t *testing.T) {
	s := NewSigner("s3cret", 7, true)
	raw := "https://cc3001.fastcdn.dpdns.org/litevideo/freepv/1/118/118abp888/118abp888_mhb_w.mp4"

	signed := s.SignURL(raw, testNow)
	if signed == raw {
		t.Fatal("secret 已配置，URL 应该被签名")
	}
	if !strings.Contains(signed, "?e=") || !strings.Contains(signed, "&s=") {
		t.Fatalf("签名参数没拼上: %s", signed)
	}

	req := httptest.NewRequest("GET", signed, nil)
	if got := s.Verify(req, testNow); got != signOK {
		t.Fatalf("Verify = %s，期望 ok（URL: %s）", got, signed)
	}
}

// 已有的 ?cdn= 不能被拼坏 —— 这是 net/url 相对手拼字符串的唯一理由
func TestSignPreservesExistingQuery(t *testing.T) {
	s := NewSigner("s3cret", 7, true)
	signed := s.SignURL("https://cc3001.fastcdn.dpdns.org/litevideo/freepv/a/b.mp4?cdn=pv3001", testNow)

	u := mustParse(t, signed)
	if u.Query().Get("cdn") != "pv3001" {
		t.Fatalf("cdn 参数丢了: %s", signed)
	}
	if got := s.Verify(httptest.NewRequest("GET", signed, nil), testNow); got != signOK {
		t.Fatalf("Verify = %s，期望 ok", got)
	}
}

// 同一天内两次签名必须逐字节相同 —— 否则 CF 缓存键天天变，缓存直接碎
func TestSignIsStableWithinDay(t *testing.T) {
	s := NewSigner("s3cret", 7, true)
	raw := "https://cc3001.fastcdn.dpdns.org/mirror/litevideo/freepv/a/b.mp4"

	a := s.SignURL(raw, time.Unix(1782000000, 0))
	b := s.SignURL(raw, time.Unix(1782000000+3600, 0))
	if a != b {
		t.Fatalf("同一 UTC 日内签名应一致:\n a=%s\n b=%s", a, b)
	}

	c := s.SignURL(raw, time.Unix(1782000000+86400, 0))
	if a == c {
		t.Fatal("跨天后 exp 应该变化")
	}
}

func TestVerifyRejectsTampering(t *testing.T) {
	s := NewSigner("s3cret", 7, true)
	signed := s.SignURL("https://cc3001.fastcdn.dpdns.org/mirror/litevideo/freepv/a/b.mp4", testNow)
	u := mustParse(t, signed)

	cases := []struct {
		name string
		path string
		url  string
		want string
	}{
		{"换目录", "/mirror/litevideo/freepv/other/b.mp4", "", signInvalid},
		{"改签名", "", u.Query().Get("s") + "x", signInvalid},
		{"缺签名", "", "", signMissing},
	}

	for _, c := range cases {
		target := signed
		if c.path != "" {
			cu := *u
			cu.Path = c.path
			target = cu.String()
		}
		if c.name == "改签名" {
			q := u.Query()
			q.Set("s", c.url)
			cu := *u
			cu.RawQuery = q.Encode()
			target = cu.String()
		}
		if c.name == "缺签名" {
			q := u.Query()
			q.Del("s")
			cu := *u
			cu.RawQuery = q.Encode()
			target = cu.String()
		}

		if got := s.Verify(httptest.NewRequest("GET", target, nil), testNow); got != c.want {
			t.Fatalf("%s: Verify = %s，期望 %s（%s）", c.name, got, c.want, target)
		}
	}
}

func TestVerifyExpired(t *testing.T) {
	s := NewSigner("s3cret", 7, true)
	signed := s.SignURL("https://cc3001.fastcdn.dpdns.org/mirror/litevideo/freepv/a/b.mp4", testNow)

	// 把时钟拨到有效期之后
	later := testNow.Add(8 * 24 * time.Hour)
	if got := s.Verify(httptest.NewRequest("GET", signed, nil), later); got != signExpired {
		t.Fatalf("Verify = %s，期望 expired", got)
	}
}

// 清单改写用的一行一签：同一路径多次调用结果必须一致（CF 缓存键靠它稳定），
// 不同路径必须不同（否则一条签名能通行整部片）。
func TestSignPathPerLine(t *testing.T) {
	s := NewSigner("s3cret", 7, true)

	a := s.SignPath("/mirror/hlsvideo/freepv/s/ssi/ssis00095/ssis00095_sm_w.m3u8", testNow)
	b := s.SignPath("/mirror/hlsvideo/freepv/s/ssi/ssis00095/ssis00095_sm_w.m3u8", testNow)
	c := s.SignPath("/mirror/hlsvideo/freepv/s/ssi/ssis00095/ssis00095_sm_w0.ts", testNow)

	if a != b {
		t.Fatalf("同一路径两次签名不一致: %s vs %s", a, b)
	}
	if a == c {
		t.Fatal("不同路径的签名不该相同")
	}

	// 拿 SignPath 的结果拼回 URL，Verify 必须认
	req := httptest.NewRequest("GET", "https://cc3001.fastcdn.dpdns.org/mirror/hlsvideo/freepv/s/ssi/ssis00095/ssis00095_sm_w0.ts?"+c, nil)
	if got := s.Verify(req, testNow); got != signOK {
		t.Fatalf("SignPath 的结果应该能通过 Verify，得到 %s", got)
	}
}

// 没配 secret：签发方不加参数、校验方一律放行 —— 灰度与回滚都靠这个
func TestDisabledSigner(t *testing.T) {
	s := NewSigner("", 7, true)
	raw := "https://cc3001.fastcdn.dpdns.org/litevideo/freepv/a/b.mp4"

	if got := s.SignURL(raw, testNow); got != raw {
		t.Fatalf("secret 为空时不应改动 URL，得到 %s", got)
	}
	if got := s.Verify(httptest.NewRequest("GET", raw, nil), testNow); got != signDisabled {
		t.Fatalf("Verify = %s，期望 disabled", got)
	}
	if s.Enforcing() {
		t.Fatal("secret 为空时不该处于 enforce 状态")
	}
}

// secret 配了但 enforce 关：验签结论照常给出，只是不拦 —— 调用方据此记日志
func TestEnforceOffStillReportsVerdict(t *testing.T) {
	s := NewSigner("s3cret", 7, false)
	raw := "https://cc3001.fastcdn.dpdns.org/litevideo/freepv/a/b.mp4"

	if s.Enforcing() {
		t.Fatal("enforce=false 时 Enforcing() 应为 false")
	}
	if got := s.Verify(httptest.NewRequest("GET", raw, nil), testNow); got != signMissing {
		t.Fatalf("Verify = %s，期望 missing（enforce 关闭只影响拦不拦，不影响结论）", got)
	}
}

// 畸形 exp 不能被 ParseInt 吞掉后当成合法
func TestVerifyRejectsBadExp(t *testing.T) {
	s := NewSigner("s3cret", 7, true)
	for _, bad := range []string{"abc", "-1", "1e9", "999999999999999"} {
		u := "https://cc3001.fastcdn.dpdns.org/litevideo/freepv/a/b.mp4?e=" + url.QueryEscape(bad) + "&s=xxxxxxxxxxxxxxxxxxxxxx"
		if got := s.Verify(httptest.NewRequest("GET", u, nil), testNow); got != signInvalid {
			t.Fatalf("exp=%q 应判 invalid，得到 %s", bad, got)
		}
	}
}
