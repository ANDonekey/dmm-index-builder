# DMM 预览视频索引构建

从 [r18.dev](https://r18.dev/dumps) 的每日/每周数据库 dump 中，提取 **番号 → DMM 预览视频直链**，
生成一份 50 MB 量级的 SQLite 索引库，供小内存 VPS 直接查询与反向代理。

## 这是什么

r18.dev 每周二发布一次数据库 dump（1.38 GB gzipped SQL，含演员/封面/评论/评分等大量无关数据）。
本项目只需要其中一个很小的子集：番号与 DMM 官方预览视频（trailer）直链的对应关系。

抽取结果：

| 指标 | 值 |
|---|---|
| 输入 | `r18dotdev_dump_<date>.sql.gz`（约 260 MiB，展开 1.38 GB） |
| 输出 | `dmm-index.db`（约 **53 MB**） |
| 记录数 | **620,025** 部影片 |
| 构建耗时 | **29~33 秒** |
| 提速技巧 | 字节偏移定位目标表块（而非逐行扫描），8 分钟 → 29 秒 |

档位分布：

| 档位 | 数量 | 说明 |
|---|---|---|
| dmb | 328,627 | standard |
| mhb | 216,465 | high |
| sm | 70,109 | small |
| dm | 3,125 | low |
| hhb | 1,699 | full HD（freepv 天花板） |

## 快速开始

```bash
# 拉取最新 dump（约 260 MiB）
curl -fL -C - -o dump.sql.gz https://r18.dev/dumps/latest

# 解压（zstd 比 gzip 快 3~4 倍）
zstd -dc dump.sql.gz > dump.sql

# 构建索引库（约 30 秒）
python build-index.py -i dump.sql -o dmm-index.db --vacuum

# 质量校验：逐条比对产物与 dump 原始 URL，exit code 非 0 即失败
python verify-index.py
```

`build-index.py` 也支持 `.sql.gz`、`.sql.zst` 直接输入。

## 运行时使用

```python
import sqlite3

def build_url(cdn, dirpath, stem, quality, variant):
    return f"https://{cdn}.dmm.co.jp/litevideo/freepv/{dirpath}/{stem}_{quality}_{variant}.mp4"

def lookup(db_path: str, code: str):
    """按番号取预览视频直链。code 形如 'ABP-888'（大小写不敏感）。"""
    c = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    row = c.execute(
        "SELECT cdn, dirpath, stem, quality, variant, source FROM video "
        "WHERE code = ? COLLATE NOCASE",
        (code.upper(),),
    ).fetchone()
    if not row:
        return None
    cdn, dirpath, stem, quality, variant, source = row
    return {
        "quality": f"{quality}_{variant}",
        "source": source,
        "url": build_url(cdn, dirpath, stem, quality, variant),
    }
```

⚠️ **CDN 需日本出口**：DMM 按 IP 做地域封锁，服务器侧请求必须经代理。

## 工具

| 文件 | 用途 |
|---|---|
| `build-index.py` | dump → 索引库。流式处理，峰值内存约 300 MB |
| `verify-index.py` | 质量闸门：620,025 条逐条与 dump 比对 |
| `probe-quality.py` | 探测某部影片实际存在哪些清晰度档位 |

### 索引库结构

URL **不落库**——由 `build_url()` 运行时拼装（固定前缀 43 字符 × 62 万条 ≈ 26 MB 纯冗余，
且 stem 在 URL 里重复两次）。分列存储后从 82.4 MB 降到 52.9 MB。

```sql
CREATE TABLE video (
  cid     TEXT PRIMARY KEY,  -- DMM content_id，如 104fsmd00029
  code    TEXT,              -- 规范化番号，如 abp-888
  cdn     TEXT NOT NULL,     -- cc3001 | pv3001 | …
  dirpath TEXT NOT NULL,     -- freepv 之后的目录路径，如 1/104/104fsmd29
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

## GitHub Actions

`.github/workflows/build-dmm-index.yml` 每周二自动构建（r18.dev 官方更新后 10 分钟触发），
产物同时发到 artifact 和 rolling release。

- `dump_date` 留空 = 跟随 latest；填日期 = 回溯指定版本（注意官方只保留 90 天）
- CI 里 `verify-index.py` 作为质量闸门，不一致直接 fail

## 踩过的坑（都有实测依据）

| 坑 | 修法 |
|---|---|
| **dump 是 CRLF 行尾** | `rstrip("\r\n")`，否则 URL 全匹配失败、出库 0 条 |
| **`stem ≠ cid` 占 57.6%** | 目录名数字段去零规则不一致，`stem` 必须单独存列 |
| **两表破平只换 quality** | 会拼出混搭的不存在 URL（742 条），必须整条替换 |
| **CDN 不止 `cc3001`** | 还有 `pv3001`（多 mhb 高档），只认一个会误降级 28 条 |
| 逐行扫全文件 | 按字节偏移定位目标表块 |
| 定期全量重写整个字典 | O(n²/batch)，改增量只写 dirty 集合 |
| 走代理时 `curl -sI` 首行是隧道行 | 取**最后**一个 `HTTP/` 行，否则 404 全被误判成 200 |

## 数据来源与许可

数据来自 [r18.dev](https://r18.dev/dumps)，**许可为 CC0**（公有领域贡献）。
本仓库仅做数据抽取与索引，不存储、也不分发任何影片内容。
