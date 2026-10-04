# DMM 预览视频解析与静态代理服务 —— 设计方案

> 项目：DmmRequest  
> 日期：2026-10-05（当日修订：接入 r18.dev 每日数据库 §0；**清晰度=URL 后缀，已实测验证** §0.5）  
> 前置：解析规则已从 `F:\codx\javdbweb` 提取（见 `DMM-预览视频解析规则.md`、`dmm-preview-reference.ts`）

---

## 0. 数据源重大更新：r18.dev 每日数据库（2026-10-05 实测）

用户可每日获取 r18.dev 数据库 dump。当前版本 **`r18dotdev_dump_2026-09-29.sql`**（1.38 GB，同名 `.gz` 272 MB）；
上一版样例在 `F:\codx\javsp-go\r18devdump\r18dotdev_dump_2026-05-26.sql`。

**两个关键结论：① 库里直接有 DMM 预览视频直链；② 清晰度只是 URL 后缀之差（已实测换档成功）。**

### 0.1 库内视频链接盘点（2026-09-29 版）

| 表 | url 非空 / 总行 | 内容 |
|---|---|---|
| `source_dmm_trailer` | **787,671** / 1,797,870 | `content_id + url`，`cc3001.dmm.co.jp/litevideo/freepv/…mp4` 直链 |
| `derived_video.sample_url` | 268,613（cc3001 直链） | 同为 freepv 直链；另有约 13.6 万条 `www.dmm.co.jp/litevideo/-/part/=/cid=…` 嵌入页（非直链，价值低） |

比 5 月版（773,515）净增约 1.4 万条，**每日同步即可覆盖新片**。
⚠️ `source_dmm_trailer` 是最有价值的一张表，但 `convert.py` 的表清单**漏了它**，导入时必须补上。

后缀（=清晰度）分布：`dmb_w` 225,259 / `mhb_w` 188,772 / `sm_s` 44,520 / `dmb_s` 40,643 /
`sm_w` 12,542 / `dm_*`+`mhb_s` ~3,500 / **`hhb_w` 1,029（库里的天花板）**。

### 0.2 freepv 直链形态

```
https://cc3001.dmm.co.jp/litevideo/freepv/{cid首字符}/{cid前3字符}/{目录名}/{目录名}_{清晰度}_{w|s}.mp4
例：…/freepv/s/ssn/ssni00036/ssni00036_dmb_w.mp4
```

**无签名、纯静态路径。** ⚠️ 目录名的数字段去零规则**不统一**（`ssni00036` 保留前导零，
`nash00605→nash605` 去零，`h_491fone00062→h_491fone062` 去两位零），
所以**目录部分必须照抄库里的 URL**，只有清晰度后缀可自由替换。

### 0.3 地域封锁（实测确认）

本机 Clash 出口在**非日本节点**时，`GET`/`HEAD` 一律 **403 CloudFront "Request blocked"**，
与 `Referer`、`Range`、请求方法**都无关**。换**日本东京出口**（`103.62.49.138` GSL Networks）后
同一链接 **200/206 正常**。→ 反代 + 日本代理是硬需求；同时说明本机（Clash 切日本）可直接做实测。

### 0.4 ⚠️ CloudFront 错误页：不存在 = 404 + 150 字节

不存在（或已下线）的档位返回 **`HTTP 404` + `Content-Length: 150`**，响应体是 openresty 的
`<title>404 Not Found</title>` HTML 页（`X-Cache: Error from cloudfront`，`X-Amz-Cf-Pop: NRT57`）。

两个必须注意的实现细节：

1. **不能用状态码单独判存在性，要看字节数**。真实视频是 MB 级，错误页固定 150 字节。
   判据统一为 **`bytes > 1024`**（`Range: bytes=0-1` 返回 206 亦可）。
2. **⚠️ 走 HTTP 代理时 curl 会在最前面插入隧道行 `HTTP/1.1 200 Connection Established`**，
   真正的上游状态码在**最后一个** `HTTP/` 行。解析响应头时若取第一行，会把所有 404
   误判成 200 + 150 字节 → 整套档位探测全部失效（这个坑本项目已踩过一次）。
   Go 里用 `net/http` 无此问题（自动处理 CONNECT），手写 curl/脚本解析时必须取最后一条状态行。

**个别片子会单独失效**：复核发现第一轮实测可用的 9 条里 8 条仍然有效（`Age` 头显示
已被 CDN 缓存，最长 `Age≈5,017,415` 秒 ≈ 58 天），仅 `jufe00225` 一条变为 404——
属于**单片下线**而非系统性失效。因此不能因为个别失败就判定"整套不可用"，
反之也不能假定永不变（见 §9 风险 2 的抽样探活）。

### 0.5 ✅ 核心结论：清晰度就是 URL 后缀，可直接换档

**实测证据**（日本出口）：

1. **同 cid 的两条库记录目录完全相同、只有文件名后缀不同**——"后缀即清晰度"的直接证据：
   ```
   trailer 表：…/j/jufe00225/jufe00225_mhb_w.mp4     (46,617,357 字节)
   sample  表：…/j/jufe00225/jufe00225_dmb_w.mp4     (23,660,624 字节)
   ```
   两表交集 267,643 个 cid 中 **73,602 个后缀不同**（几乎全是 `trailer=mhb_w` vs `sample=dmb_w`）
   → **trailer 表档位普遍高一档，优先信 `source_dmm_trailer`**。

2. **逐档探测同目录全部候选**（每片 20 个组合），真实存在的档位：

   | 片子 | 实际存在档位（字节数） |
   |---|---|
   | `jufe00225` | mhb 46.6MB / dmb 23.7MB / dm 15.9MB / sm 5.6MB |
   | `meyd00548` | mhb 35.8MB / dmb 18.2MB / dm 12.3MB / sm 4.3MB |
   | `hmn00283` | mhb 72.1MB / dmb 37.3MB / dm 25.8MB / sm 9.8MB |
   | `ssni00036` | dmb 17.1MB / dm 11.5MB / sm 4.0MB（**无 mhb**） |

   → 典型四档 `mhb > dmb > dm > sm`，**因片而异**，不是每片都有高档。

3. **向上试档有效但要验证**：`mhb → hhb` 对 4/4 片子是假 200（150 字节，即这些片子没 1080p）；
   而库里 `hhb_w` 那批（sone/juq 系列）**8/8 真实可用，最大 102,288,077 字节（≈1080p）**。
   → 做法：**从库里那档出发逐级向上试**，存在才入库。

4. **`4k` / `4ks` 在 freepv 体系不存在**：对 hhb 档片子探测 → 真 404。
   freepv 天花板就是 `hhb`。**4K 必须走 `/pv/` 签名链**（§3.1）。

5. **`_s` 与 `_w` 不能横向替换**：`sm_s` 与 `sm_w` 互斥（`_s` 4/4 可用，对应 `_w` 4/4 假 200）。
   推测是宽/竖屏或不同剪辑版本，**以库里那档为准**。

6. **协议要规范化**：库里少数条目是 `http://`（或 `cc3001.dmm.com`），
   实测 `http://` 下 curl **取不到状态行**，换 `https://` 立刻 200 → **入库统一改写 https**。

### 0.6 与 `/pv/` 签名链的关系（两套体系，别混）

| 维度 | freepv（库内，主路径） | `/pv/` 签名链（在线解析产出） |
|---|---|---|
| 天花板 | `hhb` ≈ 1080p（实测 102MB） | `4ks`/`4k` 4K 60fps |
| 签名 | 无 | 有（固定、永不过期） |
| 目录 | `/litevideo/freepv/{a}/{b}/{stem}/{stem}_{q}_{w\|s}.mp4` | `/pv/{签名}/{cid}{q}.mp4` |
| 获取 | **查库 + 换后缀**，零 API 零凭据 | affiliate API + html5_player + 日本出口 |

freepv 覆盖 77 万+ 部且免解析，作主路径；`/pv/` 保留为 **4K 升级路径**，仅在明确要 4K 时才消耗 API 配额。

### 0.7 修订后的架构：查库为主 + 档位探测

```
番号 → ① 查本地库（每日 dump 同步）
        ├─ 命中 → 档位探测（§0.7.1）→ freepv 直链 → 代理链接，零 API 调用
        └─ 未命中 或 要 4K → ② 在线解析器（§3.1，走日本代理）→ /pv/ 签名链
```

**档位探测算法**（一次入库、永久复用）：

```
输入: 库里那条 URL → 拆出 {dir, stem, quality, variant}
候选（从高到低，保持库里的 _w/_s 变体不变）:
   库里是 hhb → 就此一档（freepv 天花板），不必探测
   否则依次试: hhb → mhb → dmb → dm → sm
判定: Content-Length > 1024 或 Range 请求返回 206 才算存在；150 字节 = 错误页，丢弃
入库: 探测结果写回 SQLite（cid → 各档 URL + 字节数），之后不再探测
```

收益：稳态 API 调用 ≈ 0；一次探测永久复用（URL 无签名不会变）；用户拿到的是
**该片实际存在的最高档**而非库里碰巧存的那档；4K 走 `/pv/` 互补。

落地改动：
- 新增 **ingest 模块**：每日下载 dump → 解析 `source_dmm_trailer`（主）+ `derived_video.sample_url`（补充）
  → **URL 规范化**（http→https、主机统一 `cc3001.dmm.co.jp`）→ 合并去重写 SQLite；
- `video` 表增加 `bytes INTEGER`、`verified_at`，按 `(cid, quality)` 存多档；
- `/api/resolve` 返回**已验证档位列表**；`resolver` 策略 `off | fallback | on-demand`。

---

## 1. 背景与目标

从 DMM（FANZA）按番号解析官方预览视频（trailer）直链，并提供**长期稳定、可被 CDN 缓存**的访问入口。

三个硬约束：

| 约束                                                                                | 应对                                                                         |
| --------------------------------------------------------------------------------- | -------------------------------------------------------------------------- |
| DMM 全站（API + 播放器页 + 视频 CDN）按出口 IP 地域封锁，需要日本 IP                                    | 所有出站请求经 sing-box 代理（VPS + 日本节点订阅），代理地址可配置                                  |
| DMM 的 mp4 直链**签名固定、永不过期**（如 `https://cc3001.dmm.co.jp/pv/FzMg…/dazd00314hhb.mp4`） | 解析结果一次性入库、永久复用，不做定期刷新                                                      |
| 播放器页防盗链（校验 referer/origin），浏览器无法直接引用 DMM 链接                                       | 服务端反代：把 `cc3001.dmm.co.jp` 替换为 `cc3001.<主机域名>`，静态路径不变，前端可直接嵌 `<video src>` |

**目标产物形态**：给定番号 `DAZD-314`，返回：

```json
{
  "code": "DAZD-314",
  "cid": "dazd00314",
  "videos": [
    { "quality": "hhb", "label": "fullhd",
      "dmmUrl":    "https://cc3001.dmm.co.jp/pv/FzMg…/dazd00314hhb.mp4",
      "proxyUrl":  "https://cc3001.dmm.example.com/pv/FzMg…/dazd00314hhb.mp4",
      "localUrl":  "https://dmm.example.com/files/dazd00314_hhb.mp4",   // 可选，已下载副本
      "uploadUrl": null }                                                 // 可选，已上传副本
  ]
}
```

---

## 2. 总体架构

```
                        ┌────────────────────────────────────────────┐
                        │              Cloudflare CDN                │
                        │  *.dmm.example.com（泛解析，橙云代理）       │
                        │  mp4 边缘缓存（Cache Rule: Everything）     │
                        └───────────────┬────────────────────────────┘
                                        │ 回源（miss 时）
浏览器 / 播放器                          ▼
  │  https://cc3001.dmm.example.com/pv/…mp4    ┌─────────────────────────────┐
  │  https://dmm.example.com/api/resolve?code= │ │  主机（VPS 或本机+Tunnel）  │
  └──────────────────────────────────────────►│                             │
                                              │  ① 反代模块                 │
                                              │    cc3001.本域 → cc3001.dmm │──┐
                                              │ ② 解析模块（查库→miss→解析） │  │ 日本出口
                                              │ ③ 下载/上传 worker（可选）  │  ▼
                                              │ ④ SQLite（永久结果）       │ sing-box
                                              └─────────────────────────────┘ (SOCKS5/mixed)
                                                                              │
                                                                              ▼
                                                        api.dmm.com / www.dmm.co.jp
                                                        cc3001.dmm.co.jp（视频 CDN）
```

模块一览：

| 模块                | 职责                                                       | 出站是否走代理 |
| ----------------- | -------------------------------------------------------- | ------- |
| **resolver** 解析模块 | 番号 → Affiliate API 搜索 → html5_player 抠 bitrates → mp4 直链 | ✅       |
| **store** 存储模块    | SQLite 永久保存解析结果（成功与负结果都存）                                | —       |
| **proxy** 反代模块    | `*.<domain>` → `*.dmm.co.jp` 流式转发，透传 Range               | ✅       |
| **download**（可选）  | 拉取 mp4 存本地；或经 PicGo API / 自定义 HTTP 上传                    | ✅       |

---

## 3. 关键设计

### 3.1 解析层（复用已提取规则）

完全沿用 javdbweb 验证过的流程，规则细节见《DMM-预览视频解析规则》：

1. `code → cid`：`/^([a-z]+)-?(\d+)$/`，数字 `padStart(5,'0')`（`DAZD-314` → `dazd00314`）。CID 兼作主键。
2. 关键词三变体按序搜索 Affiliate API（`api.dmm.com/affiliate/v3/ItemList`，`site=FANZA`）：`code.replace('-','00')` → 原码 → 去连字符。
3. 命中项取 `service_code / floor_code / content_id / URL`，最多 2 条候选。
4. 拼播放器页：`https://www.dmm.co.jp/service/digitalapi/-/html5_player/=/cid=<contentId>/mtype=AhRVShI_/service=<svc>/floor=<floor>/mode=/`，必带 `cookie: age_check_done=1` + referer。
5. 正则 `/const\s+args\s+=\s+({[\s\S]*?});/` 抠 `args.bitrates[].src`，只收 `.mp4`，按清晰度后缀（`4ks/4k/hhbs/hhb/hmb/mhb/mmb/dm/sm`）去重。

**本项目的差异点**——出站 fetch 全部挂代理：

```
proxy_url: socks5://127.0.0.1:2080     # sing-box mixed 端口
```

- 解析模块对 `api.dmm.com` 与 `www.dmm.co.jp` 的请求统一经此代理发出；
- 代理不可达/握手失败时返回明确错误 `proxy_unreachable`，**不要**降级直连（直连必被地域封锁，只会白白超时）；
- 启动时做一次代理连通性探测（对 `https://www.dmm.co.jp/` 发 HEAD，预期能拿到响应），失败则在 `/api/health` 里报告。

### 3.2 存储层：结果永久有效

freepv 直链无签名、`/pv/` 直链签名固定，因此两者都**永久有效**：

- **成功结果永久保存**，永不自动重解析；只有手动 `POST /api/resolve?force=1` 才覆盖。
- **档位探测结果一并入库**（`bytes` + `verified_at`），同一片不重复探测。
- **负结果也保存**（`not_found` / `region_blocked` / `player_args_not_found`…），带 `negative_ttl`（建议 7 天）避免反复撞墙；过期后自动重试。
- 存储引擎：**SQLite**（单文件、零运维；量级完全够——77 万+ 直链、一个番号 1~4 条档位记录）。

数据模型（已按 §0.5 的多档位探测修订）：

```sql
CREATE TABLE resolve (
  cid          TEXT PRIMARY KEY,      -- dazd00314
  code         TEXT NOT NULL,         -- 规范化番号 dazd-314
  content_id   TEXT,
  service_code TEXT,
  floor_code   TEXT,
  matched_url  TEXT,                  -- DMM 商品页
  status       TEXT NOT NULL,         -- success | not_found | error
  error        TEXT,
  source       TEXT,                  -- db（dump 导入）| resolver（在线解析）
  discovered_at TEXT NOT NULL,
  updated_at    TEXT NOT NULL
);

CREATE TABLE video (
  cid      TEXT NOT NULL REFERENCES resolve(cid),
  quality  TEXT NOT NULL,             -- hhb/mhb/dmb/dm/sm（freepv）；4ks/4k/hhb…（/pv/）
  variant  TEXT,                      -- w | s（库里那档的变体，不可横向替换，见 §0.5-5）
  dmm_url  TEXT NOT NULL,             -- DMM 直链（无签名/固定签名，永久有效）
  proxy_url TEXT NOT NULL,            -- 域名替换后的静态链接
  bytes    INTEGER,                   -- Content-Length；<=150 视为不存在（§0.4）
  verified_at TEXT,                   -- 档位探测时间，NULL=未探测
  -- 可选下载/上传副本
  local_path  TEXT, local_size INTEGER, sha256 TEXT,
  upload_provider TEXT, upload_url TEXT,
  PRIMARY KEY (cid, quality)
);
CREATE INDEX idx_video_cid ON video(cid);
```

**导入注意**：`source_dmm_trailer` 与 `derived_video.sample_url` 合并时按 `(cid, quality)`
取**档位更高**者（trailer 表通常高一档），并做 URL 规范化（`http`→`https`、
主机统一 `cc3001.dmm.co.jp`、`cc3001.dmm.com`→`cc3001.dmm.co.jp`）。

### 3.3 静态代理层：泛域名替换（本方案核心）

**规则极简：入站 host 去掉自有域名后缀，剩下的拼回 `.dmm.co.jp`。**

```
入站: https://cc3001.dmm.example.com/pv/FzMg…/dazd00314hhb.mp4
出站: https://cc3001.dmm.co.jp/pv/FzMg…/dazd00314hhb.mp4
```

实现要点：

1. **域名规划**：用专属泛子域 `*.dmm.example.com`（不要泛解析主域 `*.example.com`，会劫走主站所有子域）。DNS 一条 `*` A 记录指向服务，CF 橙云开启。
2. **入站校验**：host 必须匹配 `^[a-z0-9-]+\.dmm\.example\.com$`，提取 `<sub>` 后**只能**拼 `https://<sub>.dmm.co.jp`——sub 不参与任何其他拼接，天然无 SSRF。
3. **请求改写**（转发到 DMM 时强制覆盖）：
   ```
   Host:      <sub>.dmm.co.jp
   Referer:   https://www.dmm.co.jp/
   Origin:    https://www.dmm.co.jp
   User-Agent: Chrome UA（或透传客户端 UA）
   ```
   透传：`Range` / `If-Range`（拖动进度条必需）。
4. **响应改写**（给 CDN 和浏览器的）：
   ```
   Cache-Control: public, max-age=31536000, immutable   ← 路径签名固定，可永久缓存
   Access-Control-Allow-Origin: *
   Cross-Origin-Resource-Policy: cross-origin
   Accept-Ranges / Content-Range / Content-Length / Content-Type 透传
   ```
5. **流式转发**：`io.Copy`（Go）/ `pipe`（Node），绝不整段缓冲，单文件几十~几百 MB。
6. **HEAD** 支持（CDN 预热/探活用）。
7. **出站走日本代理**：视频 CDN 同样有地域校验风险，统一经 sing-box 出口。
8. 只允许 `GET`/`HEAD`，其余 405。

URL 替换函数（解析成功后生成 proxy_url 时调用）：

```ts
function toProxyUrl(dmmUrl: string, domain: string) {
  return dmmUrl.replace(/^https?:\/\/([a-z0-9-]+)\.dmm\.co\.jp/i, `https://$1.${domain}`);
  // cc3001.dmm.co.jp/... → cc3001.dmm.example.com/...
}
```

### 3.4 CDN 缓存策略（Cloudflare）

| 项       | 配置                                                                                                           |
| ------- | ------------------------------------------------------------------------------------------------------------ |
| DNS     | `*` → 服务 IP，Proxied（泛域名一条记录）                                                                                 |
| 证书      | CF Universal SSL 免费覆盖一级泛域名 `*.dmm.example.com` ✅                                                             |
| 缓存规则    | Cache Rule：`hostname ends with dmm.example.com` → **Cache Everything** + Edge TTL 1 年（源站已回 `immutable`，规则兜底） |
| mp4 扩展名 | 本就在 CF 默认缓存列表，但显式规则保证 206/无扩展场景也命中                                                                           |
| 回源      | miss 时 CF → 主机服务 → sing-box → DMM；命中后完全不回源                                                                   |

效果：同一文件全局只需回源一次，**日本代理带宽压力 = 独立文件数 × 文件大小**，与播放次数无关。

⚠️ 两个上限要写进运维注意：

- CF Free/Plan 单缓存对象 **512MB**——`hhb` 及以下档位（<300MB）无忧，`4k/4ks` 少数超限文件会直接不缓存（回源直传，仍可播放，只是不走缓存）。
- CF Free 对单 zone 泛域名代理无额外限制，但 `*.dmm.example.com` 与主域 `example.com` 同 zone，注意不要与已有子域冲突（`dmm` 前缀专属，天然隔离）。

### 3.5 下载与上传（可选模块，默认关闭）

解析成功后，异步把视频拉回并可选转存：

```
resolve success → 入队 → worker 逐个:
  ① GET dmm_url（经代理，Range 分块下载，落盘 videos/<cid>_<quality>.mp4）
  ② 校验 sha256、记录 local_path/local_size
  ③ 上传（若配置了 provider）→ 记录 upload_url
```

**存储/上传 provider 抽象**（可扩展）：

| provider | 说明                                                              | 适用                                   |
| -------- | --------------------------------------------------------------- | ------------------------------------ |
| `local`  | 只落本地盘，服务直接对外提供 `localUrl`                                       | 默认                                   |
| `picgo`  | `POST http://<picgo>:36677/upload`（multipart），解析返回 JSON 的 `url` | ⚠️ 图床普遍限大小/拒 mp4，只适合 `sm/dm` 小档位，需实测 |
| `http`   | 自定义 endpoint（S3 预签名 URL / R2 / Alist `PUT /api/fs/put` 均可套用）    | 大文件推荐                                |
| `none`   | 不下载                                                             |                                      |

> PicGo 的定位是图床客户端，对几十 MB 的 mp4 兼容性参差（Lankong/SM.MS 拒收，S3 类图床看配置）。方案把它做成"能用但需自测"的一档，大文件场景给 `http` 自定义端点兜底（R2/Alist 是现成的好选择）。

**URL 优先级**（API 返回顺序即降级顺序）：

```
upload_url（已转存，最稳） > local_url（本地副本） > proxy_url（CF 缓存 + 反代） > dmm_url（仅记录，不给前端）
```

---

## 4. API 设计

统一前缀 `/api`，JSON 返回，可选 `Authorization: Bearer <token>`（配置了才启用）。

| 方法       | 路径                                             | 说明                                |
| -------- | ---------------------------------------------- | --------------------------------- |
| GET      | `/api/resolve?code=DAZD-314`                   | 查库 → miss 则解析（经代理）→ 入库 → 返回多清晰度列表 |
| POST     | `/api/resolve` body `{codes:[…], force?:bool}` | 批量 / 强制重解析                        |
| GET      | `/api/status?code=`                            | 含下载/上传进度                          |
| POST     | `/api/download` body `{code, quality?}`        | 手动触发下载/上传任务                       |
| GET      | `/api/health`                                  | 代理连通性、库大小、worker 队列深度             |
| GET/HEAD | `<sub>.dmm.example.com/*`                      | 泛域名反代（3.3）                        |
| GET      | `dmm.example.com/files/*`                      | （可选）本地副本直读                        |

`/api/resolve` 幂等且快路径纯读库，可直接给前端列表页批量调（每番号一次）。

---

## 5. 配置文件

```yaml
# config.yaml
listen: ":8080"                      # 服务监听（CF 回源 / Tunnel 指向这里）
domain: "dmm.example.com"            # 泛域名基址 → *.dmm.example.com
public_origin: "https://dmm.example.com"

proxy_url: "socks5://127.0.0.1:2080" # sing-box mixed 端口（日本出口）
proxy_probe: "https://www.dmm.co.jp/" # 启动连通性探测目标

dmm:
  api_id: "${DMM_API_ID}"            # 环境变量注入，不落盘
  affiliate_id: "${DMM_AFFILIATE_ID}"
  timeout_ms: 12000

store:
  path: "./data/dmm.db"

negative_ttl_days: 7                 # not_found/error 结果的重试间隔

download:
  enabled: false
  dir: "./videos"
  max_quality: "hhb"                 # 高于此档不自动下（防 4K 大文件）
  concurrency: 1

upload:
  provider: "none"                   # none | local | picgo | http
  picgo:
    endpoint: "http://127.0.0.1:36677/upload"
  http:
    method: "PUT"
    url_template: "https://<r2-bucket>/{cid}_{quality}.mp4"
    headers: { "Authorization": "Bearer ${R2_TOKEN}" }
```

---

## 6. 部署形态

### 形态 A：服务跑在 VPS 上（推荐，链路最短）

```
DNS *.dmm.example.com → CF → VPS:8080
VPS:  dmmresolver（单二进制） + sing-box（127.0.0.1:2080，日本节点）
```

- 出站代理走 localhost，零额外延迟；
- 缺点：视频副本也落在 VPS 盘上（"保存本地"若指用户本机盘则不合适）。

### 形态 B：服务跑在用户本机 + Cloudflare Tunnel（贴合"保存本地"需求）

```
DNS *.dmm.example.com → CF（Proxied）
CF Tunnel（本机 cloudflared 运行，ingress: *.dmm.example.com → localhost:8080）
本机: dmmresolver + （本机 sing-box 客户端 或 VPS sing-box 的公网端口）
```

- 本机无公网 IP 也能接 CF CDN，`cloudflared` 的 ingress 原生支持泛域名；
- 出站代理指向 VPS 上 sing-box 的公网监听（务必加用户名密码：`socks5://user:pass@vps:2080`），或本机自跑 sing-box 挂同一订阅；
- 下载副本天然落用户本地盘 ✅；本机离线时代理链接 502（CF 保留缓存命中部分仍可播）。

两种形态代码完全一致，只差 `listen`/Tunnel/代理地址配置。

---

## 7. 技术选型

**推荐 Go**（单二进制、跨平台、依赖少）：

| 需求               | Go 方案                                                    |
| ---------------- | -------------------------------------------------------- |
| SOCKS5/HTTP 代理出站 | `golang.org/x/net/proxy`（SOCKS5）+ 自定义 `http.Transport`   |
| 泛域名流式反代          | `httputil.ReverseProxy` + `FlushInterval: -1`（立即下发，支持拖动） |
| SQLite           | `modernc.org/sqlite`（纯 Go，免 CGO，Windows 友好）              |
| 配置               | `gopkg.in/yaml.v3` + 环境变量展开                              |


备选 Node/TS（复用已提取的 `dmm-preview-reference.ts` 思路）：`undici` + `socks-proxy-agent` + `better-sqlite3`。若后续想并入 javdbweb 的 TS 生态再选它。

---

## 8. 实施计划

| 阶段            | 内容                                     | 验收                                                                    |
| ------------- | -------------------------------------- | --------------------------------------------------------------------- |
| **P1 导入+存储**  | ingest 模块（dump → SQLite，含 URL 规范化）+ `/api/resolve`（只查库） | 2026-09-29 dump 导入后 78 万+ 直链可查；抽 20 个番号命中率与库一致；二次调用 0 网络请求      |
| **P2 反代+CDN** | 泛域名 ReverseProxy + DNS/CF 规则           | `cc3001.dmm.example.com/…mp4` 浏览器可播、可拖动；CF 命中后 `cf-cache-status: HIT` |
| **P3 档位探测**  | 逐级向上探测（hhb→mhb→dmb→dm→sm）+ 假 200 过滤 | 抽 20 部片子，返回档位列表与 §0.5 实测一致（mhb/dmb/dm/sm 四档），无 150 字节假条目     |
| **P4 在线解析（4K）** | resolver（挂代理）接入 fallback       | miss 番号与 4K 档可解析入库；抽 10 个番号验证                              |
| P5 下载/上传     | worker 队列 + local/picgo/http provider  | `sm` 档下载落盘 + PicGo 上传实测通过                                             |
| P6 加固         | token 鉴权、限速、日志、`/api/health` 面板        | —                                                                     |

P1、P2 完成即可对外提供静态链接（全程不出站）；P3 决定用户拿到哪一档；P4 补 4K。

---

## 9. 风险与对策

| # | 风险                         | 对策                                                                        |
| - | -------------------------- | ------------------------------------------------------------------------- |
| 1 | DMM 改版（播放器页结构 / args 格式变化） | **freepv 主路径不受影响**（直链来自 dump，不经解析）；在线解析失败只影响 4K 升级；错误码保留便于定位；`force=1` 重解析 |
| 2 | 直链"永久有效"被打破（极端情况）          | 库里存 `discovered_at`；`/api/health` 加抽样校验（每日抽 10 条带 `bytes>1024` 判定探活），连续失效告警 |
| 3 | 日本节点失效 / sing-box 重启       | 启动探测 + `/api/health` 报告；错误码 `proxy_unreachable` 与业务错误区分；**反代回源失败要能明确报 502 而不是挂住** |
| 4 | **档位探测把假 200 当成真档位**        | 硬性判据 `bytes > 1024`（§0.4）；探测结果入库，后续不重复探测                    |
| 5 | CF Free 512MB 缓存上限         | freepv 天花板 `hhb` 实测最大约 102MB，**基本不会超限**；`/pv/` 的 4K 大文件走回源直传或上传副本兜底 |
| 6 | 泛域名被滥用刷流量（open proxy 担忧）   | 反代目标锁死为 `<sub>.dmm.co.jp`，无任意 URL 注入面；CF WAF 限路径前缀 `/pv/` `/litevideo/`（可选） |
| 7 | Affiliate API 配额           | 仅 4K 升级路径消耗；dump 覆盖 77 万+ 部使稳态调用 ≈ 0；批量接口限速 2 QPS      |
| 8 | dump 断更 / 格式变更         | ingest 保留原始表清单与版本戳；解析失败告警而非静默跳过；保留上一版库可用          |
| 9 | PicGo 拒收 mp4               | provider 抽象已隔离，`http`（R2/Alist）兜底                                         |

---

## 10. 目录结构（Go 实现）

```
dmmrequest/
├── cmd/server/main.go
├── internal/
│   ├── config/      # config.yaml 加载 + env 展开
│   ├── resolver/    # DMM 解析（affiliate 搜索 + html5_player 抠流），走代理
│   ├── proxystore/  # 泛域名反代 *.dmm.<domain> → *.dmm.co.jp
│   ├── store/       # SQLite（resolve / video 两表）
│   ├── download/    # 下载 worker + 上传 provider（local/picgo/http）
│   └── httpapi/     # /api/* 路由
├── config.yaml
└── data/            # dmm.db + videos/
```
