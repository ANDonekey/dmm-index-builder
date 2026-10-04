# DMM 预览视频索引构建

从 [r18.dev](https://r18.dev/dumps) 的数据库 dump 中提取 **番号 → DMM 预览视频直链**，
生成 50 MB 量级的 SQLite 索引库，供小内存 VPS 直接查询与反向代理。

数据源 r18.dev 每周二更新，许可 **CC0**。本项目只做数据抽取与索引，不存储、不分发影片内容。

## 效果

| 指标 | 值 |
|---|---|
| 输入 | `r18dotdev_dump_<date>.sql.gz`（约 260 MiB，展开 1.38 GB） |
| 输出 | `dmm-index.db`（约 **53 MB**） |
| 记录数 | **620,025** 部影片 |
| 构建耗时 | **14~30 秒** |
| 提速 | 字节偏移定位目标表块，8 分钟 → 30 秒 |

档位分布：

| 档位 | 数量 | 说明 |
|---|---|---|
| dmb | 328,627 | standard |
| mhb | 216,465 | high |
| sm | 70,109 | small |
| dm | 3,125 | low |
| hhb | 1,699 | full HD（freepv 体系的天花板） |

## 用法

### 构建

```bash
curl -fL -C - -o dump.sql.gz https://r18.dev/dumps/latest
zstd -dc dump.sql.gz > dump.sql
python build-index.py -i dump.sql -o dmm-index.db --vacuum
python verify-index.py dump.sql dmm-index.db    # 质量闸门，exit code 非 0 即失败
```

### 查询

⚠️ **不能直接 `WHERE code = 'ABP-888'`** —— 索引库里 `code` 只有 18% 非空，
因为 DMM 的 content_id 与番号不是一一对应（厂牌带数字前缀、形态不规则）。
用 `dmm_index.py` 归一化匹配：

```python
from dmm_index import Index

idx = Index("dmm-index.db")
idx.lookup("ABP-888")
# {'cid': '118abp00888', 'quality': 'mhb_w', 'source': 'trailer',
#  'url': 'https://cc3001.dmm.co.jp/litevideo/freepv/1/118/118abp888/118abp888_mhb_w.mp4'}

idx.lookup_many(["SSIS-095", "MIDE-800", "IPX-777"])   # 批量，1ms 级
```

匹配规则：剥离连字符/空格 → 拆出「字母段 + 数字段（去前导零）」→ 匹配。
因此 `ABP-888` / `abp888` / `abp 888` / `118abp-888` 等价，
字母段互为前缀时互通（`STAR-359` ↔ `STARS`）。

性能：倒排索引构建约 3 s（一次性），之后单次查询 O(1)，1000 次重复查询 2.5 ms。

### 探测清晰度

```bash
python probe-quality.py "https://cc3001.dmm.co.jp/litevideo/freepv/s/ssi/ssis00095/ssis00095_mhb_w.mp4"
```

逐级向上试档（`hhb → mhb → dmb → dm → sm`），输出该片实际存在的档位与字节数。

## 文件

| 文件 | 用途 |
|---|---|
| `build-index.py` | dump → 索引库。流式处理，峰值内存约 300 MB |
| `verify-index.py` | 质量闸门：620,025 条逐条与 dump 比对 |
| `dmm_index.py` | 运行时查询模块（番号 → 直链） |
| `probe-quality.py` | 探测某片实际有哪些清晰度档位 |

## 索引库结构

URL **不落库**，由 `Index.build_url()` 运行时拼装 —— 固定前缀 43 字符 × 62 万条 ≈ 26 MB
纯冗余，且 stem 在 URL 路径里出现两次。分列存储后从 82.4 MB 降到 52.9 MB。

```sql
CREATE TABLE video (
  cid     TEXT PRIMARY KEY,  -- DMM content_id，如 118abp00888
  code    TEXT,              -- 番号（仅 18% 非空，不可靠，查询请用 dmm_index.py）
  cdn     TEXT NOT NULL,     -- cc3001 | pv3001 | …
  dirpath TEXT NOT NULL,     -- freepv 之后的目录路径，如 1/118/118abp888
  stem    TEXT NOT NULL,     -- 文件名主体（⚠️ 与 cid 可能不同）
  quality TEXT NOT NULL,     -- hhb/mhb/dmb/dm/sm
  variant TEXT NOT NULL,     -- w | s
  size_hint      INTEGER,
  source         TEXT,       -- trailer（高）| sample（低）
  verified       INTEGER,    -- 档位探测标记
  verified_bytes INTEGER,
  updated_at     TEXT
) WITHOUT ROWID;
```

`meta` 表另存 dump 文件名 / sha256 / 构建时间 / 记录总数。

## GitHub Actions

每周二自动构建（官方 03:00-04:00 UTC 上传 dump，触发设在 04:10 UTC）。
`dump_date` 留空 = 跟随 latest；填日期 = 回溯指定版本（**官方只保留 90 天**）。
CI 里 `verify-index.py` 作为质量闸门，不一致直接 fail。

## 实现要点

| 坑 | 处理 |
|---|---|
| dump 是 **CRLF 行尾** | `rstrip("\r\n")`，否则 URL 全匹配失败、出库 0 条 |
| **`stem ≠ cid` 占 57.6%** | 目录名去零规则不一致，`stem` 单独存列 |
| **两表破平只换 quality** | 会拼出混搭的不存在 URL，必须整条替换 |
| **CDN 不止 `cc3001`** | 还有 `pv3001`（多 mhb 高档），按行存 `cdn` 字段 |
| 逐行扫全文件 | 按字节偏移定位目标表块 |
| 定期全量重写整个字典 | O(n²/batch)，改增量只写 dirty 集合 |

## 注意

⚠️ **CDN 需日本出口**：DMM 按 IP 做地域封锁，服务端请求必须经代理。
非日本 IP 会收到 CloudFront `403 Request blocked`。
