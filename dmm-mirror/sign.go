// 预览链接签名。
//
// ⚠️ 当前生产**未启用**：`DMM_SIGN_SECRET` 留空，三个服务都不签不验。
//
// 原因是要把 CF 边缘缓存用到极致 —— 签名参数会让 URL 每天变一次（即便 CF 的
// /mirror/ 规则忽略 query，浏览器缓存和 /litevideo 的缓存键仍然会碎），
// 换来的是「缓存未命中那一小撮请求」的防护，不划算。
// 现在只保留**查询准入**（`DMM_QUERY_KEY`，见 main.go 的 handle）。
//
// 要重新启用：三个服务配上同一个 secret、两个反代开 DMM_SIGN_ENFORCE=1，
// 按 docs/dmm-preview-auth.md 的顺序灰度。下面这套实现是完整的、测过的。
//
// 为什么当初这么设计
// ----------
// 视频 URL 裸着的时候，任何人拿到就能无限拉流：`/mirror/` 吃 VPS 带宽，
// `/litevideo`、`/pv/` 吃日本出口的日/月配额。而 `<video src>` 根本没法带自定义头，
// hls.js 能带但会触发 CORS 预检 —— 媒体路径的 OPTIONS 回 405（实测），预检一挂
// 播放直接死。所以鉴权只能做在 URL 上，没有别的选择。
//
// 为什么 exp 按天对齐
// -----------------
// 签名参数会进 Cloudflare 的缓存键。如果每次请求现签一个新值，同一部片就会按
// 访客各存一份缓存 —— 而 mirror 的响应是 `max-age=31536000, immutable`，
// 边缘缓存命中正是这套架构的全部意义，碎掉就全完了。
//
// 所以 exp 取「按天对齐」：
//
//	exp = (now/86400 + TTL_DAYS) * 86400
//
// 同一天内所有人拿到的 URL 逐字节相同 → CF 照常 HIT；跨天换一次，TTL_DAYS 后失效。
// 代价是泄露的 URL 在 TTL 内仍然可用，而且 CF 已缓存的副本在签名过期后还能播
// （不回源就不校验）—— 这是刻意的取舍，不是漏洞。
//
// ⚠️ 本文件在 dmm-query / dmm-proxy / dmm-mirror 三个目录里各有一份，
// 内容必须逐字一致（三个独立 module，共享不了包）。改一处就要同步另外两处，
// 否则签发方与校验方对不上，表现为「所有预览 403」。
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	signVersion = "v1"
	// 参数名取单字母：URL 会进缓存键，越短越好，也少一个被误伤成业务参数的机会
	signParamExp = "e"
	signParamSig = "s"
	// 截断到 16 字节（22 个 base64url 字符）。HMAC-SHA256 全量是 32 字节，
	// 128 bit 对「猜签名」这个用途远远够用，URL 还短一半。
	signSigBytes = 16
	// 有效期（天）。按天对齐，所以实际可用时间是 TTL_DAYS ~ TTL_DAYS+1 天。
	signDefaultTTLDays = 7
)

// Signer 同时用于签发（dmm-query）和校验（两个反代）。
//
// secret 为空时：
//   - 签发方原样返回 URL（不加参数）；
//   - 校验方一律放行。
//
// 这就是灰度/回滚开关：先把三个服务的 secret 都配上再打开 enforce，
// 中途任何一步出问题，把 secret 清空就退回改动前的行为。
type Signer struct {
	secret  []byte
	ttlDays int64
	// enforce 只有校验方用：secret 配了但 enforce=false 时只记日志不拦，
	// 用来「先观察一天，确认没有误杀再真拦」。
	enforce bool
}

func NewSigner(secret string, ttlDays int, enforce bool) *Signer {
	if ttlDays <= 0 {
		ttlDays = signDefaultTTLDays
	}
	return &Signer{
		secret:  []byte(strings.TrimSpace(secret)),
		ttlDays: int64(ttlDays),
		enforce: enforce,
	}
}

// Enabled 表示 secret 已配置（签发方据此决定要不要加参数）。
func (s *Signer) Enabled() bool {
	return s != nil && len(s.secret) > 0
}

// Enforcing 表示校验方真的会拦（secret 配了 且 enforce 打开）。
func (s *Signer) Enforcing() bool {
	return s.Enabled() && s.enforce
}

func (s *Signer) expFor(now time.Time) int64 {
	return (now.Unix()/86400 + s.ttlDays) * 86400
}

// mac 只覆盖 path 与 exp。
//
// 不覆盖 query 里的 `cdn`：那个参数只有 dmm-proxy 认，且取值被白名单卡死
// （改动它只会让上游换一个 <cdn>.dmm.co.jp 去取同一个路径，结果必然是 404）。
// 把 cdn 也签进去会让签发方和校验方的归一化规则多一处可能分叉的地方，
// 换不来实际防护。
func (s *Signer) mac(path string, exp int64) string {
	h := hmac.New(sha256.New, s.secret)
	h.Write([]byte(signVersion))
	h.Write([]byte{'|'})
	h.Write([]byte(path))
	h.Write([]byte{'|'})
	h.Write([]byte(strconv.FormatInt(exp, 10)))
	sum := h.Sum(nil)
	return base64.RawURLEncoding.EncodeToString(sum[:signSigBytes])
}

// SignURL 给绝对 URL 追加 ?e=&s=。
//
// 用 net/url 拼参数而不是字符串拼接：现有 URL 可能已经带了 `?cdn=`，
// 手拼很容易拼出 `?cdn=x?e=y` 这种。Encode() 会把参数按名字排序，
// 结果确定，缓存键不会因为顺序抖动。
func (s *Signer) SignURL(raw string, now time.Time) string {
	if !s.Enabled() || raw == "" {
		return raw
	}

	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}

	exp := s.expFor(now)
	q := u.Query()
	q.Set(signParamExp, strconv.FormatInt(exp, 10))
	q.Set(signParamSig, s.mac(u.Path, exp))
	u.RawQuery = q.Encode()

	return u.String()
}

// SignPath 返回「给这条路径用的签名查询串」（不含前导 `?`）。
//
// 给 dmm-mirror 改写 m3u8 用：清单里的变体清单和 .ts 分片都是相对路径，
// 浏览器解析相对 URI 会把 query 丢掉，所以镜像必须在返回清单前，把每一条
// 相对 URI 换成带签名的地址。路径不同签名就不同，所以是一行一签。
func (s *Signer) SignPath(path string, now time.Time) string {
	if !s.Enabled() || path == "" {
		return ""
	}

	exp := s.expFor(now)
	q := url.Values{}
	q.Set(signParamExp, strconv.FormatInt(exp, 10))
	q.Set(signParamSig, s.mac(path, exp))

	return q.Encode()
}

// 校验结论。ok / disabled 之外的都算失败，由调用方决定拦还是只记日志。
const (
	signOK       = "ok"
	signDisabled = "disabled"
	signMissing  = "missing"
	signInvalid  = "invalid"
	signExpired  = "expired"
)

// Verify 校验一个请求的签名。
//
// ⚠️ 先验签、后判过期：反过来的话，任何人随便编一个 exp 就能从「expired」这个
// 返回值里确认签名是对的，等于送了一个免费的探测接口。这和 /api/v/resolve
// 票据那边的口径一致。
func (s *Signer) Verify(r *http.Request, now time.Time) string {
	if !s.Enabled() {
		return signDisabled
	}

	q := r.URL.Query()
	expRaw := q.Get(signParamExp)
	sig := q.Get(signParamSig)
	if expRaw == "" || sig == "" {
		return signMissing
	}

	// 只认纯数字：Expires 之类畸形值不该进 ParseInt
	if len(expRaw) > 12 || strings.Trim(expRaw, "0123456789") != "" {
		return signInvalid
	}
	exp, err := strconv.ParseInt(expRaw, 10, 64)
	if err != nil {
		return signInvalid
	}

	// hmac.Equal 是定长比较，别用 == 比字符串
	if !hmac.Equal([]byte(s.mac(r.URL.Path, exp)), []byte(sig)) {
		return signInvalid
	}

	if exp < now.Unix() {
		return signExpired
	}

	return signOK
}
