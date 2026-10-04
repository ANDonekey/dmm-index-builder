# DMM 预览视频解析规则（备用）

> 提取自 `F:\codx\javdbweb`（Cloudflare Pages Functions 实现）
> 源文件：`functions/_utils/dmm.ts`、`functions/_utils/preview-common.ts`、`functions/api/preview/dmm.ts`、`functions/api/preview/dmm/video.ts`
> 提取日期：2026-10-05
> 用途：javdbweb 里这套是**死代码**（后端完整可用但前端零引用），此处留档以便复用。

---

## 一、链路总览

```
番号 code（如 SSIS-095）
  │
  ├─① codeToDmmCid(code)                → cid（同时用作缓存 key）
  │
  ├─② searchDmmContentItems(code)       → api.dmm.com/affiliate/v3/ItemList
  │     用 3 种关键词变体逐个搜 → 命中项给 service_code / floor_code / content_id
  │     最多保留 2 条候选
  │
  ├─③ extractDmmTrailerLinks(item)      → www.dmm.co.jp html5_player 页
  │     正则抠出 `const args = {...}` → args.bitrates[].src
  │     按清晰度后缀去重 → mp4 直链
  │
  └─④ 视频流代理                       → 带 referer/origin 转发 Range 请求
```

---

## 二、规则明细

### 1. 番号 → CID

```
cleaned = code.toLowerCase().trim().replace(/\s+/g, '')
match   = /^([a-z]+)-?(\d+)$/
cid     = `${字母}${数字.padStart(5, '0')}`
```

- `SSIS-095` → `ssis00095`
- `ABP888` → `abp00888`
- 不匹配正则 → 返回 `null`，错误码 `cannot_build_cid`
- **CID 同时是缓存 key**，所以 `ABP-888` 和 `ABP888` 命中同一条缓存

### 2. 搜索关键词的 3 种变体（按顺序试）

```
[0] code.replace('-', '00')   // SSIS-095  → SSIS00095   ← 只替换第一个连字符
[1] code                     // SSIS-095  → SSIS-095
[2] code.replace(/-/g, '')   // SSIS-095  → SSIS095
```
用 `Set` 去重、过滤空串。**顺序有意义**：先试 `00` 补零写法（DMM 侧的标准写法），命中即返回，不再往下试。

### 3. Affiliate API 请求

```
GET https://api.dmm.com/affiliate/v3/ItemList
  ?api_id=<DMM_API_ID>
  &affiliate_id=<DMM_AFFILIATE_ID>
  &output=json
  &site=FANZA
  &sort=match
  &keyword=<变体>
```

必需请求头：
| 头 | 值 |
|---|---|
| `accept` | `application/json` |
| `accept-language` | `ja-JP,ja;q=0.9` |
| `user-agent` | Chrome 124 桌面 UA |
| `redirect` | `follow` |

- 非 2xx → 直接试下一个关键词（`continue`）
- `result.result_count` 为 0 或 `items` 非数组 → 同样 `continue`

### 4. 候选筛选（命中判定）

对每个 item，缺 `content_id` / `service_code` / `floor_code` 任一即丢弃。满足以下**任一**条件即收入候选：

```
contentId.toLowerCase().includes(当前关键词去掉连字符)   // 主判据
|| maker_product.toLowerCase() === 番号.toLowerCase()
|| contentId.includes(番号去连字符)
```

**最多收 2 条**，收满即 `break`。有候选就返回，不再搜后续关键词。

### 5. 播放器页 URL 构造（核心）

```
https://www.dmm.co.jp/service/digitalapi/-/html5_player/=/cid=<contentId>/mtype=AhRVShI_/service=<serviceCode>/floor=<floorCode>/mode=/
```

- `mtype=AhRVShI_` 是固定常量
- 三个字段都要 `encodeURIComponent`
- `mode=` 留空

必需请求头（**缺一不可**）：

| 头 | 值 | 作用 |
|---|---|---|
| `cookie` | `age_check_done=1` | 绕过 R18 年龄确认 |
| `referer` | 搜索结果的 `URL` 字段，无则退回 `https://www.dmm.co.jp/` | 防盗链 |
| `accept` | `text/html,application/xhtml+xml` | |
| `accept-language` | `ja-JP,ja;q=0.9` | |
| `user-agent` | Chrome 124 桌面 UA | |

### 6. 从 HTML 抠流

```
正则：/const\s+args\s+=\s+({[\s\S]*?});/
```

- HTML 含 `このサービスはお住まいの地域からは` → 抛 `region_blocked`（**地域限制**）
- 抠不到 → `player_args_not_found`
- `JSON.parse` 后 `args.bitrates` 必须是数组，否则 `bitrates_not_found`
- 遍历 `bitrates[]`：
  - `src` 必须是字符串且 **以 `.mp4` 结尾**（其他格式一律丢）
  - `//` 开头的补 `https:`（协议相对 URL）
  - 从文件名末尾反查清晰度后缀，识别不出的丢弃
  - 同清晰度只保留第一条
- 结果为空 → `no_matching_bitrates`

### 7. 清晰度映射（按文件名后缀，长后缀优先）

| quality | label | 说明 |
|---|---|---|
| `4ks` | 4K 60fps | |
| `4k` | 4K | |
| `hhbs` | fullhd 60fps | |
| `hhb` | fullhd | |
| `hmb` | hd | |
| `mhb` | high | |
| `mmb` | medium | |
| `dm` | standard | |
| `sm` | small | |

匹配方式：取 `.mp4` 前的最后一段文件名，`DMM_QUALITIES.find(q => filename.endsWith(q.quality))`。
数组顺序即优先级顺序，所以 `4ks` 必须排在 `4k` 前面，否则 `4ks.mp4` 会被误判成 `4k`。

### 8. 视频流转发

```
GET /api/preview/dmm/video?code=<code>&quality=<quality>
```
- `quality` 不在白名单 → 400
- 缓存里没视频 → 现查一次并回填
- 指定 quality 不存在 → 404

转发到源站时**强制覆盖**请求头：
```
referer: https://www.dmm.co.jp/
origin:   https://www.dmm.co.jp
user-agent: Chrome 124 UA
redirect: follow
```
Range 相关头透传（`range` / `if-range`），源站返回 206 视为成功。

响应回传：透传 `accept-ranges` / `cache-control` / `content-length` / `content-range` / `content-type` / `date` / `etag` / `expires` / `last-modified` / `vary`，
再加 `access-control-allow-origin: *`、`cross-origin-resource-policy: cross-origin`。HEAD 请求只回状态码不回 body。

### 9. 缓存与错误码

| 项 | 值 |
|---|---|
| 缓存 key | `/cache/dmm/<cid>` |
| 正缓存 TTL | 86400s（24h） |
| 负缓存 TTL | 1800s（30min） |
| 判定 | `previewUrl` 非空即算成功 |

错误码（`error` 字段）：`cannot_build_cid`、`content_not_found`、`region_blocked`、`player_args_not_found`、`bitrates_not_found`、`no_matching_bitrates`、`trailer_not_found`。
所有异常在 `lookupDmmPreview` 里被捕获，取**最后一条**错误码返回——即两个候选都失败时，报的是第二条的失败原因。

### 10. 区域中继（Cloudflare 专用）

```
DMM_REGION_WORKER_ORIGIN 存在 且 请求头 x-dmm-region-relay != 1
  → 转发到 {origin}{pathname}{search}，并带上
      x-dmm-region-relay: 1
      x-public-origin: <当前请求 origin>
```
用途：把请求转到非日本的出口 IP 绕地域限制。
`x-public-origin` 是为了让被中继的 Worker 生成**客户端可访问**的视频代理 URL（否则会指向那个中间 Worker 域名）。

### 11. 凭据

`DMM_API_ID` / `DMM_AFFILIATE_ID` 从环境变量读，**缺失直接抛错**，不设回退默认值。
（历史版本曾硬编码在源码里，已迁走——重新实现时不要把它写回代码。）

---

## 三、已知坑（复现时注意）

1. **地域限制是硬门槛**。`www.dmm.co.jp` 会按出口 IP 拦截，HTML 里直接给日文提示。
   本地直连大概率失败，需要海外/日本出口，或走上面的区域中继。
2. **关键词变体顺序不能改**。`00` 补零先试能省掉两次无谓请求。
3. **候选只留 2 条**，且遍历时 `items.length >= 2` 就 break——DMM 搜索结果里常有同名不同片，
   留太多容易选错片。
4. **`4ks` / `4k` 这类嵌套后缀**，顺序错了清晰度会全部塌成低一档。
5. **`maker_product === 番号` 是精确相等**，不做包含匹配，避免误配。
6. 全流程走 `fetchWithTimeout(url, init, 12000)`，没有它源站挂起会拖到平台执行上限。

---

## 四、参考实现

同目录 `dmm-preview-reference.ts`：去凭据、去 Cloudflare 依赖的单文件实现，
只依赖标准 `fetch` / `AbortSignal.timeout`，Node 18+ / Workers / Deno 均可直接跑。
