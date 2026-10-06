/**
 * DMM 预览视频查询 Worker
 *
 * 职责边界（重要）：
 *   本 Worker 只做「查 D1 + 域名替换」——纯计算，不 fetch DMM。
 *   DMM 按 IP 地域封锁，而 Workers 出站 IP 无法稳定落在日本，
 *   所以视频流一律走 CF CDN → VPS 反代 → sing-box 日本出口 → DMM CDN。
 *   因此本 Worker 对地域零依赖。
 *
 * 同时导出两种入口，Pages 与 Workers 都能用：
 *   - default.fetch  → wrangler deploy（Workers）
 *   - onRequestGet   → Pages Functions
 */

interface Env {
  DB: D1Database;
}

interface Row {
  dirpath: string;
  stem: string;
  quality: string;
  variant: string;
  cdn: string;
}

/** 番号归一化：剥离连字符/空格，拆出字母段与数字段（去前导零） */
function normalizeCode(code: string): { letters: string; num: number } | null {
  const s = (code || "").trim().toLowerCase().replace(/[-_\s]+/g, "");
  const m = /^(\d*?)([a-z]+?)(\d+)$/.exec(s);
  if (!m) return null;
  return { letters: m[2], num: Number(m[3]) };
}

function buildResult(r: Row, host: string, code: string) {
  const path = `/litevideo/freepv/${r.dirpath}/${r.stem}_${r.quality}_${r.variant}.mp4`;
  return {
    code,
    quality: `${r.quality}_${r.variant}`,
    cdn: r.cdn,
    dmmUrl: `https://${r.cdn}.dmm.co.jp${path}`,
    proxyUrl: `https://${r.cdn}.${host}${path}`,
  };
}

function json(body: unknown, status = 200, extra: Record<string, string> = {}) {
  return new Response(JSON.stringify(body, null, 2), {
    status,
    headers: {
      "content-type": "application/json; charset=utf-8",
      "access-control-allow-origin": "*",
      ...extra,
    },
  });
}

async function lookupOne(env: Env, code: string, host: string) {
  const key = normalizeCode(code);
  if (!key) return { status: 400, body: { code, error: `invalid code: ${code}` } };

  const row = (await env.DB.prepare(
    `SELECT dirpath, stem, quality, variant, cdn
       FROM video
      WHERE k_letters = ?1 AND k_num = ?2
      LIMIT 1`
  )
    .bind(key.letters, key.num)
    .first()) as Row | null;

  if (!row) return { status: 404, body: { code, error: "not found" } };
  return { status: 200, body: buildResult(row, host, code) };
}

async function lookupMany(env: Env, codes: string[], host: string) {
  const parsed = codes.map((c) => ({ c, k: normalizeCode(c) }));

  // 按字母段分组，每组一条 IN 查询，避免 N 条 SQL
  const byLetters = new Map<string, Set<number>>();
  for (const { k } of parsed) {
    if (!k) continue;
    let s = byLetters.get(k.letters);
    if (!s) {
      s = new Set();
      byLetters.set(k.letters, s);
    }
    s.add(k.num);
  }

  const found = new Map<string, Row & { k_num: number }>();
  for (const [letters, nums] of byLetters) {
    if (!nums.size) continue;
    const placeholders = [...nums].map(() => "?").join(",");
    const { results } = await env.DB.prepare(
      `SELECT k_num, dirpath, stem, quality, variant, cdn
         FROM video
        WHERE k_letters = ? AND k_num IN (${placeholders})`
    )
      .bind(letters, ...nums)
      .all();
    for (const r of results as (Row & { k_num: number })[]) {
      const k = `${letters}|${r.k_num}`;
      if (!found.has(k)) found.set(k, r);
    }
  }

  const out: Record<string, unknown> = {};
  for (const { c, k } of parsed) {
    if (!k) {
      out[c] = { error: "invalid" };
      continue;
    }
    const r = found.get(`${k.letters}|${k.num}`);
    out[c] = r ? buildResult(r, host, c) : { error: "not found" };
  }
  return { status: 200, body: out };
}

async function handle(request: Request, env: Env): Promise<Response> {
  const url = new URL(request.url);
  const host = request.headers.get("host") || url.host;

  if (request.method === "OPTIONS") {
    return new Response(null, {
      status: 204,
      headers: {
        "access-control-allow-origin": "*",
        "access-control-allow-methods": "GET, OPTIONS",
      },
    });
  }

  if (url.pathname === "/health") {
    // 绝不能用 SELECT COUNT(*) FROM video：那是 29 万行的全表扫描，
    // 免费版一天才 500 万行读取额度，几次健康检查就烧掉一大截。
    // 改成读只有 1 行的 d1_sync_state（1 row read）。
    const row = (await env.DB
      .prepare("SELECT index_id, rows, updated_at FROM d1_sync_state WHERE id = 1")
      .first()) as { index_id: string; rows: number; updated_at: string } | null;
    return json({
      ok: true,
      index_id: row?.index_id ?? null,
      rows: row?.rows ?? 0,
      updated_at: row?.updated_at ?? null,
    });
  }

  const codes = url.searchParams.getAll("code");
  if (codes.length === 0) return json({ error: "missing code" }, 400);

  // 批量：?code=A&code=B&code=C
  if (codes.length > 1) {
    const { status, body } = await lookupMany(env, codes.slice(0, 50), host);
    return json(body, status, { "cache-control": "public, max-age=3600" });
  }

  const code = codes[0].trim();
  if (!code) return json({ error: "missing code" }, 400);

  const { status, body } = await lookupOne(env, code, host);
  if (status === 404) {
    // 未收录：可能是新片（r18.dev 的 freepv 数据滞后，实测 18 周零变化）。
    // 响应头告知调用方，便于决定是否走 VPS 在线解析兜底。
    return json(body, 404, { "x-dmm-miss": "1" });
  }
  // 链接永不变，可长期缓存
  return json(body, status, { "cache-control": "public, max-age=86400" });
}

/* ---------- Workers 入口（wrangler deploy） ---------- */
export default {
  fetch: (request: Request, env: Env) => handle(request, env),
};

/* ---------- Pages Functions 入口 ---------- */
export const onRequestGet = ({ request, env }: { request: Request; env: Env }) =>
  handle(request, env);
