/**
 * DMM 预览视频查询 Worker
 *
 * 职责边界（重要）：
 *   本 Worker 只做「查 D1 + 域名替换」——纯计算，不 fetch DMM。
 *   DMM 按 IP 地域封锁，而 Workers 出站 IP 无法稳定落在日本，
 *   所以视频流一律走 CF CDN → VPS 反代 → sing-box 日本出口 → DMM CDN。
 *   因此本 Worker 对地域零依赖。
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

function buildUrls(r: Row, host: string) {
  const path = `/litevideo/freepv/${r.dirpath}/${r.stem}_${r.quality}_${r.variant}.mp4`;
  return {
    quality: `${r.quality}_${r.variant}`,
    cdn: r.cdn,
    path,
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

/**
 * GET /api/resolve?code=ABP-888
 *
 * 返回替换域名后的 VPS 反代地址。链接永不变，可长期缓存。
 */
export async function onRequestGet(context: { request: Request; env: Env }): Promise<Response> {
  const { request, env } = context;
  const url = new URL(request.url);
  const code = (url.searchParams.get("code") || "").trim();

  if (!code) return json({ error: "missing code" }, 400);

  const key = normalizeCode(code);
  if (!key) return json({ error: `invalid code: ${code}` }, 400);

  const row = (await env.DB.prepare(
    `SELECT dirpath, stem, quality, variant, cdn
       FROM video
      WHERE k_letters = ?1 AND k_num = ?2
      LIMIT 1`
  )
    .bind(key.letters, key.num)
    .first()) as Row | null;

  if (!row) {
    // 未收录：可能是新片（r18.dev 的 freepv 数据滞后，实测 18 周零变化）。
    // 打响应头告知调用方，便于决定是否走 VPS 在线解析兜底。
    return json({ code, error: "not found" }, 404, { "x-dmm-miss": "1" });
  }

  return json({ code, ...buildUrls(row, url.host) }, 200, {
    "cache-control": "public, max-age=86400",
  });
}

/**
 * 批量：GET /api/resolve?code=A&code=B&code=C
 *
 * 按字母段分组，每组一条 IN 查询，避免 N 条 SQL。
 */
export async function onRequestGetMulti(context: { request: Request; env: Env }): Promise<Response> {
  const { request, env } = context;
  const url = new URL(request.url);
  const codes = url.searchParams.getAll("code").slice(0, 50);
  if (!codes.length) return json({ error: "missing code" }, 400);

  const parsed = codes.map((c) => ({ c, k: normalizeCode(c) }));

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

  // key 用 "letters|num" 便于回填
  const found = new Map<string, Row>();
  for (const [letters, nums] of byLetters) {
    if (!nums.size) continue;
    const placeholders = [...nums].map(() => "?").join(",");
    const stmt = env.DB.prepare(
      `SELECT dirpath, stem, quality, variant, cdn
         FROM video
        WHERE k_letters = ? AND k_num IN (${placeholders})`
    );
    const { results } = await stmt.bind(letters, ...nums).all();
    for (const r of results as Row[]) {
      const num = (r as unknown as { k_num: number }).k_num;
      const k = `${letters}|${num}`;
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
    out[c] = r ? buildUrls(r, url.host) : { error: "not found" };
  }

  return json(out, 200, { "cache-control": "public, max-age=3600" });
}
