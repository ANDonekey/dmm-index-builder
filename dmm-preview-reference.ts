/**
 * DMM 预览视频解析 —— 去 Cloudflare 依赖的单文件参考实现
 *
 * 提取自 F:\codx\javdbweb\functions\_utils\{dmm,preview-common}.ts
 * 规则说明见同目录 `DMM-预览视频解析规则.md`
 *
 * 与原实现的差异（都是为了脱离 CF 环境）：
 *   - 去掉 caches.default 缓存，改用调用方传入的 Map（或不缓存）
 *   - 去掉 relayDmmRequestToRegion 的跨 Worker 中继
 *   - 凭据走参数/env，不硬编码
 *
 * 运行环境要求：fetch + AbortSignal.timeout（Node 18+ / Cloudflare Workers / Deno）
 */

const DMM_AFFILIATE_API = 'https://api.dmm.com/affiliate/v3/ItemList';
const UA =
  'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36';
const TIMEOUT_MS = 12_000;

/** 清晰度白名单。顺序即匹配优先级：4ks 必须排在 4k 前面，否则 4ks.mp4 会被误判为 4k */
export const DMM_QUALITIES = [
  { quality: '4ks', label: '4K 60fps' },
  { quality: '4k', label: '4K' },
  { quality: 'hhbs', label: 'fullhd 60fps' },
  { quality: 'hhb', label: 'fullhd' },
  { quality: 'hmb', label: 'hd' },
  { quality: 'mhb', label: 'high' },
  { quality: 'mmb', label: 'medium' },
  { quality: 'dm', label: 'standard' },
  { quality: 'sm', label: 'small' },
];

export interface DmmCredentials {
  apiId: string;
  affiliateId: string;
}

export interface DmmPreviewVideo {
  quality: string;
  label: string;
  url: string;
}

export interface DmmPreviewResult {
  code: string;
  cid: string | null;
  contentId: string | null;
  matchedUrl: string | null;
  previewUrl: string | null;
  videos: DmmPreviewVideo[];
  error?: string;
}

interface DmmContentItem {
  serviceCode: string;
  floorCode: string;
  contentId: string;
  pageUrl: string | null;
}

// ---------- 基础工具 ----------

async function fetchWithTimeout(url: string, init: RequestInit = {}, timeoutMs = TIMEOUT_MS) {
  return fetch(url, { ...init, signal: AbortSignal.timeout(timeoutMs) });
}

/**
 * 番号 → DMM CID。
 * SSIS-095 → ssis00095 ; ABP888 → abp00888
 * 格式不合法返回 null。
 */
export function codeToDmmCid(code: string) {
  const cleaned = code.toLowerCase().trim().replace(/\s+/g, '');
  const match = cleaned.match(/^([a-z]+)-?(\d+)$/);
  if (!match) return null;
  return `${match[1]}${match[2].padStart(5, '0')}`;
}

/** 三种搜索关键词变体，按优先级排列。顺序有意义：00 补零写法命中即返回 */
export function buildDmmSearchKeywords(code: string) {
  const trimmed = code.trim();
  const noHyphen = trimmed.replace(/-/g, '');
  return [...new Set([trimmed.replace('-', '00'), trimmed, noHyphen].filter(Boolean))];
}

/** 从 mp4 文件名末尾反查清晰度 */
export function getDmmQualityFromUrl(url: string) {
  const match = url.match(/\/([^/?#]+)\.mp4(?:[?#].*)?$/);
  const filename = match?.[1] || '';
  return DMM_QUALITIES.find(({ quality }) => filename.endsWith(quality)) || null;
}

/** 协议相对 URL 补全 */
export function makeAbsoluteDmmUrl(url: string) {
  return url.startsWith('//') ? `https:${url}` : url;
}

// ---------- 第一步：搜内容 ----------

export async function searchDmmContentItems(
  code: string,
  creds: DmmCredentials,
): Promise<DmmContentItem[]> {
  const carNumLower = code.toLowerCase();
  const carNumNoHyphen = code.replace(/-/g, '').toLowerCase();

  for (const keyword of buildDmmSearchKeywords(code)) {
    const apiUrl = `${DMM_AFFILIATE_API}?${new URLSearchParams({
      api_id: creds.apiId,
      affiliate_id: creds.affiliateId,
      output: 'json',
      site: 'FANZA',
      sort: 'match',
      keyword,
    }).toString()}`;

    const response = await fetchWithTimeout(apiUrl, {
      headers: {
        accept: 'application/json',
        'accept-language': 'ja-JP,ja;q=0.9',
        'user-agent': UA,
      },
      redirect: 'follow',
    });

    if (!response.ok) continue;

    const data = (await response.json()) as {
      result?: {
        result_count?: number;
        items?: Array<{
          content_id?: string;
          maker_product?: string;
          service_code?: string;
          floor_code?: string;
          URL?: string;
        }>;
      };
    };

    if (!data.result?.result_count || !Array.isArray(data.result.items)) continue;

    const currentKeyword = keyword.toLowerCase();
    const currentKeywordNoHyphen = currentKeyword.replace(/-/g, '');
    const items: DmmContentItem[] = [];

    for (const item of data.result.items) {
      if (items.length >= 2) break;

      const contentId = (item.content_id || '').toLowerCase();
      const makerProduct = (item.maker_product || '').toLowerCase();
      const serviceCode = item.service_code || '';
      const floorCode = item.floor_code || '';

      if (!contentId || !serviceCode || !floorCode) continue;

      // maker_product 用精确相等，避免误配同名不同片
      if (
        contentId.includes(currentKeywordNoHyphen) ||
        makerProduct === carNumLower ||
        contentId.includes(carNumNoHyphen)
      ) {
        items.push({
          serviceCode,
          floorCode,
          contentId: item.content_id || contentId,
          pageUrl: item.URL || null,
        });
      }
    }

    if (items.length) return items;
  }

  return [];
}

// ---------- 第二步：播放器页抠流 ----------

export async function extractDmmTrailerLinks(item: DmmContentItem): Promise<DmmPreviewVideo[]> {
  const trailerPageUrl =
    `https://www.dmm.co.jp/service/digitalapi/-/html5_player/=/cid=${encodeURIComponent(item.contentId)}` +
    `/mtype=AhRVShI_/service=${encodeURIComponent(item.serviceCode)}/floor=${encodeURIComponent(item.floorCode)}/mode=/`;

  const response = await fetchWithTimeout(trailerPageUrl, {
    headers: {
      accept: 'text/html,application/xhtml+xml',
      'accept-language': 'ja-JP,ja;q=0.9',
      // 绕过 R18 年龄确认
      cookie: 'age_check_done=1',
      // 防盗链，缺了会被拒
      referer: item.pageUrl || 'https://www.dmm.co.jp/',
      'user-agent': UA,
    },
    redirect: 'follow',
  });

  if (!response.ok) throw new Error(`DMM player page returned ${response.status}`);

  const html = await response.text();

  // 地域限制：出口 IP 不在日本会被拦
  if (html.includes('このサービスはお住まいの地域からは')) throw new Error('region_blocked');

  const match = html.match(/const\s+args\s+=\s+({[\s\S]*?});/);
  if (!match) throw new Error('player_args_not_found');

  const args = JSON.parse(match[1]) as { bitrates?: Array<{ src?: string }> };
  if (!Array.isArray(args.bitrates)) throw new Error('bitrates_not_found');

  const videos: DmmPreviewVideo[] = [];
  const seen = new Set<string>();

  for (const bitrate of args.bitrates) {
    const src = bitrate?.src;
    if (!src || typeof src !== 'string' || !src.endsWith('.mp4')) continue;

    const absoluteSrc = makeAbsoluteDmmUrl(src);
    const qualityMeta = getDmmQualityFromUrl(absoluteSrc);
    if (!qualityMeta || seen.has(qualityMeta.quality)) continue;

    seen.add(qualityMeta.quality);
    videos.push({
      quality: qualityMeta.quality,
      label: qualityMeta.label,
      url: absoluteSrc,
    });
  }

  if (!videos.length) throw new Error('no_matching_bitrates');
  return videos;
}

// ---------- 编排 ----------

function emptyResult(code: string, error?: string): DmmPreviewResult {
  return {
    code,
    cid: null,
    contentId: null,
    matchedUrl: null,
    previewUrl: null,
    videos: [],
    ...(error ? { error } : {}),
  };
}

/**
 * 完整解析：番号 → 多清晰度 mp4 直链列表。
 * 两个候选依次尝试，全部失败时返回**最后一条**错误码。
 */
export async function resolveDmmPreview(
  code: string,
  creds: DmmCredentials,
): Promise<DmmPreviewResult> {
  const cid = codeToDmmCid(code);
  if (!cid) return emptyResult(code, 'cannot_build_cid');

  const contentItems = await searchDmmContentItems(code, creds);
  if (!contentItems.length) return { ...emptyResult(code), cid, error: 'content_not_found' };

  let lastError = 'trailer_not_found';
  for (const item of contentItems) {
    try {
      const videos = await extractDmmTrailerLinks(item);
      return {
        code,
        cid,
        contentId: item.contentId,
        matchedUrl: item.pageUrl,
        previewUrl: videos[0]?.url || null,
        videos,
      };
    } catch (error) {
      lastError = error instanceof Error ? error.message : 'trailer_not_found';
    }
  }

  return { ...emptyResult(code), cid, error: lastError };
}

/**
 * 下载预览视频流。必须自己带 referer/origin，否则源站拒绝。
 * 透传 Range，支持断点与拖动进度。
 */
export async function fetchDmmVideoStream(
  url: string,
  init: RequestInit = {},
): Promise<Response> {
  return fetchWithTimeout(url, {
    ...init,
    headers: {
      ...(init.headers as Record<string, string> | undefined),
      referer: 'https://www.dmm.co.jp/',
      origin: 'https://www.dmm.co.jp',
      'user-agent': UA,
    },
    redirect: 'follow',
  });
}

// ---------- 命令行自测 ----------
//
// npx tsx dmm-preview-reference.ts SSIS-095
// 或设置环境变量后运行：
//   DMM_API_ID=xxx DMM_AFFILIATE_ID=yyy npx tsx dmm-preview-reference.ts SSIS-095

if (typeof process !== 'undefined' && process.argv?.[1]?.endsWith('dmm-preview-reference.ts')) {
  const code = process.argv[2];
  const apiId = process.env.DMM_API_ID || '';
  const affiliateId = process.env.DMM_AFFILIATE_ID || '';

  if (!code) {
    console.error('用法: tsx dmm-preview-reference.ts <番号>');
    process.exit(1);
  }
  if (!apiId || !affiliateId) {
    console.error('缺少 DMM_API_ID / DMM_AFFILIATE_ID 环境变量');
    process.exit(1);
  }

  resolveDmmPreview(code, { apiId, affiliateId })
    .then((result) => console.log(JSON.stringify(result, null, 2)))
    .catch((error) => {
      console.error(error);
      process.exit(1);
    });
}
