//go:build ignore

// 诊断：uTLS + CF 的 403 到底卡在哪一层。
// 跑法：go run probe.go
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

func newClient(host string, profile utls.ClientHelloID) *http.Client {
	tr := &http2.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
			raw, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			u := utls.UClient(raw, &utls.Config{
				ServerName: host,
				NextProtos: []string{"h2", "http/1.1"},
			}, profile)
			if err := u.HandshakeContext(ctx); err != nil {
				raw.Close()
				return nil, err
			}
			return u, nil
		},
	}
	return &http.Client{Transport: tr, Timeout: 25 * time.Second}
}

func try(label string, c *http.Client, u string, rng string) {
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	t0 := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		fmt.Printf("%-26s ERR %v\n", label, err)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
	resp.Body.Close()
	el := time.Since(t0).Round(time.Millisecond)
	if resp.StatusCode == 200 || resp.StatusCode == 206 {
		fmt.Printf("%-26s %d %s %v\n", label, resp.StatusCode, resp.Header.Get("Content-Range"), el)
		return
	}
	fmt.Printf("%-26s %d %v  cf-mitigated=%q ct=%q\n", label, resp.StatusCode, el,
		resp.Header.Get("Cf-Mitigated"), resp.Header.Get("Content-Type"))
	fmt.Printf("   body=%q\n", string(body[:min(160, len(body))]))
}

func main() {
	host := "media.javtrailers.com"
	mp4 := "https://" + host + "/litevideo/freepv/p/pwi/pwife555/pwife555_dmb_w.mp4"
	profiles := []struct {
		name string
		id   utls.ClientHelloID
	}{
		{"Chrome_Auto", utls.HelloChrome_Auto},
		{"Chrome_120", utls.HelloChrome_120},
		{"Firefox_Auto", utls.HelloFirefox_Auto},
		{"Firefox_120", utls.HelloFirefox_120},
		{"Safari_Auto", utls.HelloSafari_Auto},
		{"Edge_Auto", utls.HelloEdge_Auto},
	}
	for _, p := range profiles {
		c := newClient(host, p.id)
		try(p.name, c, mp4, "bytes=0-255")
	}

	fmt.Println("--- Firefox 稳定性（连续 6 次）---")
	ff := newClient(host, utls.HelloFirefox_Auto)
	for i := 0; i < 6; i++ {
		try(fmt.Sprintf("firefox #%d", i+1), ff, mp4, "bytes=0-255")
	}
}
