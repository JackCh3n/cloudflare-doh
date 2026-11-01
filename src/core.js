/**
 * 平台无关的核心逻辑：路径映射解析 + DoH 反向代理
 *                + 应答缓存（LRU） + 上游熔断/故障转移 + 健康检查/指标
 *
 * 该模块同时被 Cloudflare Worker（_worker.js）与
 * 腾讯 EdgeOne Pages 边缘函数（middleware.js）复用，
 * 因此只能使用 Web 标准 API（fetch / Request / Response / URL / Headers / Uint8Array），
 * 不得引入任何 Node.js 内置模块或第三方依赖。
 *
 * 缓存键/报文改写/熔断语义与 Go 版（server/main.go）逐条对齐：
 *   - 缓存键 = 上游名 + DNS 头 flags + question 段（QNAME/QTYPE/QCLASS），事务 ID 不参与
 *   - 命中时先按存活秒数递减记录 TTL（跳过 OPT/TKEY/TSIG），再改写应答 ID 为客户端 ID
 *   - 只缓存 rcode 0/3、最小记录 TTL>0（rcode=3 无 SOA 自然不缓存）的 200 应答
 *   - 熔断：连续失败 CB_THRESHOLD 次 OPEN，冷却 CB_COOLDOWN_SEC 后放行；
 *     全部 OPEN 时仍打首选上游兜底；4xx 不算失败，原样透传
 *
 * 平台差异（与 Go 版的已知不同，勿混淆）：
 *   - 缓存/熔断/计数状态都是 **isolate 级**（每个边缘节点、每次部署/闲置回收后重置），
 *     属 best-effort，不承诺 Go 进程级的稳定性
 *   - 无 MAX_INFLIGHT / MAX_RPS / MEM_LIMIT：边缘运行时的并发与内存由平台治理，
 *     用户态信号量在同一 isolate 内意义有限
 *   - 环境变量 CACHE_MAX_TTL / CACHE_ENTRIES / CB_THRESHOLD / CB_COOLDOWN_SEC /
 *     FAILOVER_CHAIN 在 isolate 首个请求时读取一次
 */

/** 默认路径映射（可按需增删） */
export const DEFAULT_PATH_MAPPINGS = {
	'/google': {
		targetDomain: 'dns.google',
		pathMapping: {
			'/query-dns': '/dns-query',
		},
	},
	'/cloudflare': {
		targetDomain: 'one.one.one.one',
		pathMapping: {
			'/query-dns': '/dns-query',
		},
	},
	'/quad9': {
		targetDomain: 'dns.quad9.net',
		pathMapping: {
			'/query-dns': '/dns-query',
		},
	},
	'/adguard': {
		targetDomain: 'dns.adguard-dns.com',
		pathMapping: {
			'/query-dns': '/dns-query',
		},
	},
	// 国内可达的 DoH（部署在国内边缘节点时优先使用）
	'/dnspod': {
		targetDomain: 'doh.pub',
		pathMapping: {
			'/query-dns': '/dns-query',
			'/dns-query': '/dns-query',
		},
	},
	'/alidns': {
		targetDomain: 'dns.alidns.com',
		pathMapping: {
			'/query-dns': '/dns-query',
		},
	},
	// 在此继续添加映射
};

/** 未命中映射时返回的伪装 404 页面（刻意伪装成浏览器断网页） */
export const HOMEPAGE_HTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
    <meta charset="UTF-8">
    <meta http-equiv="X-UA-Compatible" content="IE=edge">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>404</title>
    <style>
        .main{
            width: 600px;
            max-width: 92%;
            height:417px;
            margin: 0 auto;
            margin-top: 125px;
        }
        img{
            margin-bottom: 20px;
        }
        p{
            font-size: 14px;
            color: rgb(32, 33, 36);
        }
        span{
            color:rgb(26, 115, 232);
            cursor: pointer;
        }
        span:hover{
            border-bottom: 1px solid rgb(26, 115, 232);
        }
        button{
            float: right;
            margin-top: 60px;
            background-color: #1970e5;
            color: #fff;
            border-width: 0px;
            padding:10px;
            border-radius: 10%;
            cursor: pointer;
        }
    </style>
</head>
<body>
    <div class="main">
        <img src="data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAEgAAABIAQMAAABvIyEEAAAABlBMVEUAAABTU1OoaSf/AAAAAXRSTlMAQObYZgAAAENJREFUeF7tzbEJACEQRNGBLeAasBCza2lLEGx0CxFGG9hBMDDxRy/72O9FMnIFapGylsu1fgoBdkXfUHLrQgdfrlJN1BdYBjQQm3UAAAAASUVORK5CYII=" alt="">
        <p style="font-size:24px;">无法访问此网站</p>
        <p>检查是否有拼写错误。</p>
        <p>如果拼写无误，请<span>尝试运行 Windows 网络诊断</span>。</p>
        <p style="font-size:12px;">DNS_PROBE_FINISHED_NXDOMAIN</p>
        <button>重新加载</button>
    </div>

</body>
</html>`;

/** 需要剥离的请求头（由运行时或目标域名自行决定） */
const DROPPED_REQUEST_HEADERS = ['host', 'content-length'];

/** hop-by-hop 头：逐跳语义，不应透传（对齐 Go 版 hopHeaders） */
const HOP_BY_HOP_HEADERS = [
	'connection',
	'keep-alive',
	'proxy-authenticate',
	'proxy-authorization',
	'te',
	'trailer',
	'transfer-encoding',
	'upgrade',
];

/** 请求体/缓存应答的最大字节数（对齐 Go 版 maxBodyBytes） */
const MAX_BODY_BYTES = 64 * 1024;

/** 单次上游请求超时（对齐 Go 版 client 15s） */
const UPSTREAM_TIMEOUT_MS = 15000;

/**
 * 读取主机白名单
 * 配置 ALLOWED_HOSTS（逗号分隔）后，只允许这些 Host 访问，
 * 其余（例如 *.workers.dev、*.pages.dev、预览域名）一律返回伪装 404。
 * @param {Object} [env]
 * @returns {string[]|null} null 表示不限制
 */
export function getAllowedHosts(env) {
	const raw = env && env.ALLOWED_HOSTS;
	if (!raw) return null;
	const list = Array.isArray(raw) ? raw : String(raw).split(',');
	const hosts = list.map((h) => String(h).trim().toLowerCase()).filter(Boolean);
	return hosts.length > 0 ? hosts : null;
}

/**
 * 判断本次请求的 Host 是否在白名单内
 * @param {string} hostname
 * @param {Object} [env]
 * @returns {boolean}
 */
export function isHostAllowed(hostname, env) {
	const hosts = getAllowedHosts(env);
	if (!hosts) return true;
	return hosts.includes(String(hostname).toLowerCase());
}

/**
 * 计算响应的缓存策略
 *
 * Workers Cache 开启后，响应若不带 Cache-Control 会走 RFC 9111 启发式缓存
 * （200 默认缓存 2 小时），对 DoH 是灾难，因此这里对每条响应都显式声明：
 *   - 仅 GET + Accept: application/dns-message + CACHE_TTL>0 才允许短缓存
 *   - 其余（含所有 POST、伪装 404）一律 no-store
 *
 * 注意：这只控制响应头（给浏览器/CDN 用的提示）；进程内 LRU 应答缓存由
 * CACHE_MAX_TTL 独立控制（POST 也会有进程内缓存，与 Go 版一致）。
 * @param {Request} request
 * @param {Object} [env]
 * @returns {{value: string, vary: boolean}}
 */
export function cachePolicy(request, env) {
	const ttl = Number((env && env.CACHE_TTL) || 0);
	const method = (request.method || 'GET').toUpperCase();
	const accept = request.headers.get('accept') || '';
	if (method === 'GET' && Number.isFinite(ttl) && ttl > 0 && accept.includes('application/dns-message')) {
		return { value: `public, max-age=${Math.floor(ttl)}`, vary: true };
	}
	return { value: 'no-store', vary: false };
}

/**
 * 读取路径映射配置
 * 优先使用环境变量 DOMAIN_MAPPINGS（JSON 字符串或对象），否则回退默认值
 * @param {Object} [env] 平台环境变量对象（Cloudflare env / EdgeOne context.env）
 * @returns {Object} 路径映射配置
 */
export function getPathMappings(env) {
	try {
		if (env && env.DOMAIN_MAPPINGS) {
			if (typeof env.DOMAIN_MAPPINGS === 'string') {
				return JSON.parse(env.DOMAIN_MAPPINGS);
			}
			return env.DOMAIN_MAPPINGS;
		}
	} catch (error) {
		console.error('Error accessing DOMAIN_MAPPINGS variable:', error);
	}

	return DEFAULT_PATH_MAPPINGS;
}

/** 根路径直连 DoH 的别名（多数 DoH 客户端默认就用 /dns-query） */
const BARE_DOH_PATHS = ['/dns-query', '/query-dns'];

/**
 * 构造「无路径前缀」的默认 DoH 目标
 * @param {URL} url
 * @param {Object} [env]
 * @returns {{prefix: string, targetDomain: string, targetUrl: string}|null}
 */
export function bareDohTarget(url, env) {
	const name = String((env && env.DEFAULT_UPSTREAM) || 'cloudflare').replace(/^\/+/, '');
	const mapping = getPathMappings(env)[`/${name}`];
	if (!mapping || !mapping.targetDomain) return null;
	return {
		prefix: '/',
		targetDomain: mapping.targetDomain,
		targetUrl: `https://${mapping.targetDomain}/dns-query${url.search}`,
	};
}

/**
 * 判断请求是否真的是 DoH 请求（用于根路径兜底，避免把浏览器访问也放行）
 * @param {Request} request
 * @returns {boolean}
 */
export function isDohRequest(request) {
	const accept = (request.headers.get('accept') || '').toLowerCase();
	const contentType = (request.headers.get('content-type') || '').toLowerCase();
	return accept.includes('application/dns-message') || contentType.includes('application/dns-message');
}

/**
 * 根据请求 URL 解析转发目标
 * @param {URL} url 原始请求 URL
 * @param {Object} [env] 平台环境变量对象
 * @returns {{prefix: string, targetDomain: string, targetUrl: string}|null} 未命中返回 null
 */
export function resolveTarget(url, env) {
	const path = url.pathname;
	const pathMappings = getPathMappings(env);

	// 最长前缀优先，避免 '/google' 抢先匹配 '/google-v2'
	const pathPrefix = Object.keys(pathMappings)
		.filter((prefix) => path.startsWith(prefix))
		.sort((a, b) => b.length - a.length)[0];

	if (!pathPrefix) {
		// 兼容 https://域名/dns-query 这种「无前缀」的标准 DoH 写法
		const bare = path.replace(/\/+$/, '');
		return BARE_DOH_PATHS.includes(bare) ? bareDohTarget(url, env) : null;
	}

	const mapping = pathMappings[pathPrefix] || {};
	if (!mapping.targetDomain) {
		return null;
	}

	const remainingPath = path.substring(pathPrefix.length);
	const pathMapping = mapping.pathMapping || {};

	// 按键长度倒序匹配，保证最长匹配（对齐 Go 版的 keys 排序）；
	// 只做「前缀」重写——目标串出现在中间时不得误替换（对齐 Go 版 HasPrefix + 拼接）
	let targetPath = remainingPath;
	const rewriteRules = Object.entries(pathMapping).sort((a, b) => b[0].length - a[0].length);
	for (const [sourcePath, destPath] of rewriteRules) {
		if (remainingPath.startsWith(sourcePath)) {
			targetPath = destPath + remainingPath.slice(sourcePath.length);
			break;
		}
	}

	// 前缀后无剩余路径时补根路径，避免拼出 https://dns.google?name=...
	if (!targetPath) {
		targetPath = '/';
	}

	return {
		prefix: pathPrefix,
		targetDomain: mapping.targetDomain,
		targetUrl: `https://${mapping.targetDomain}${targetPath}${url.search}`,
	};
}

/**
 * 构造伪装 404 响应
 * @returns {Response}
 */
export function camoResponse() {
	return new Response(HOMEPAGE_HTML, {
		status: 404,
		headers: {
			'Content-Type': 'text/html; charset=utf-8',
			// 显式禁止缓存：否则 Workers Cache 会按启发式给 404 缓存 3 分钟
			'Cache-Control': 'no-store',
		},
	});
}

/**
 * 复制请求头，剥离由运行时接管、会引起歧义或 hop-by-hop 的头部
 * @param {Headers} headers
 * @returns {Headers}
 */
function forwardableHeaders(headers) {
	const result = new Headers();
	headers.forEach((value, key) => {
		const lower = key.toLowerCase();
		if (!DROPPED_REQUEST_HEADERS.includes(lower) && !HOP_BY_HOP_HEADERS.includes(lower)) {
			result.set(key, value);
		}
	});
	return result;
}

// ============================================================
// DNS wire format（Uint8Array，语义对齐 Go 版 parseDNSQuery/dnsMinTTL/...）
// ============================================================

/** 大端读 u32 */
function readBE32(b, off) {
	return (((b[off] << 24) | (b[off + 1] << 16) | (b[off + 2] << 8) | b[off + 3]) >>> 0);
}

/** 大端写 u32 */
function writeBE32(b, off, v) {
	b[off] = (v >>> 24) & 0xff;
	b[off + 1] = (v >>> 16) & 0xff;
	b[off + 2] = (v >>> 8) & 0xff;
	b[off + 3] = v & 0xff;
}

function bytesToHex(b) {
	let s = '';
	for (let i = 0; i < b.length; i++) s += b[i].toString(16).padStart(2, '0');
	return s;
}

/**
 * 解析 DoH 查询报文，提取缓存键（对齐 Go parseDNSQuery）
 * 键 = DNS 头 flags(2B) + question 段（QNAME+QTYPE+QCLASS）；
 * 事务 ID 只返回不参与键（不同客户端 ID 不同但应答相同）。
 * @param {Uint8Array} body
 * @returns {{id: number, keyBytes: Uint8Array}|null} 解析失败返回 null
 */
export function parseDohMessageKey(body) {
	if (!body || body.length < 17) return null; // 12B 头 + 最短 question(根标签1B + QTYPE/QCLASS 4B)
	const qdcount = (body[4] << 8) | body[5];
	if (qdcount !== 1) return null; // 必须恰好 1 个 question
	let i = 12;
	while (i < body.length) {
		const l = body[i];
		if ((l & 0xc0) !== 0) return null; // question 不允许压缩指针
		i += 1;
		if (l === 0) break;
		i += l;
	}
	if (i + 4 > body.length) return null; // QTYPE+QCLASS 不完整
	const id = (body[0] << 8) | body[1];
	const keyBytes = new Uint8Array(2 + (i + 4 - 12));
	keyBytes[0] = body[2];
	keyBytes[1] = body[3];
	keyBytes.set(body.subarray(12, i + 4), 2);
	return { id, keyBytes };
}

/**
 * 跳过 QDCOUNT 个 question，返回应答区起点与 AN+NS+AR 总条数（对齐 Go dnsSectionStart）
 * @returns {{offset: number, count: number}|null}
 */
function dnsSectionStart(body) {
	if (body.length < 12) return null;
	const qd = (body[4] << 8) | body[5];
	const an = (body[6] << 8) | body[7];
	const ns = (body[8] << 8) | body[9];
	const ar = (body[10] << 8) | body[11];
	let i = 12;
	for (let c = 0; c < qd; c++) {
		while (i < body.length) {
			const l = body[i];
			if ((l & 0xc0) !== 0) {
				i += 2; // 指针占 2 字节并终结名字
				break;
			}
			i += 1;
			if (l === 0) break;
			i += l;
		}
		i += 4; // QTYPE + QCLASS
	}
	if (i > body.length) return null;
	return { offset: i, count: an + ns + ar };
}

/** 跳过记录名（支持压缩指针），返回名字结束偏移；失败 -1 */
function skipName(body, i) {
	while (i < body.length) {
		const l = body[i];
		if ((l & 0xc0) !== 0) return i + 2;
		i += 1;
		if (l === 0) return i;
		i += l;
	}
	return -1;
}

/** 伪记录：OPT(41) 的 TTL 字段是扩展 RCODE/标志位，TKEY(249)/TSIG(250) 同理不可动 */
function isPseudoRR(type) {
	return type === 41 || type === 249 || type === 250;
}

/**
 * 读取偏移 i 处的一条资源记录（对齐 Go rrNext）
 * @returns {{type: number, ttlOff: number, next: number}|null}
 */
function rrNext(body, i) {
	const nameEnd = skipName(body, i);
	if (nameEnd < 0 || nameEnd + 10 > body.length) return null; // 固定头 10B：type2+class2+ttl4+rdlen2
	const type = (body[nameEnd] << 8) | body[nameEnd + 1];
	const ttlOff = nameEnd + 4;
	const rdlen = (body[nameEnd + 8] << 8) | body[nameEnd + 9];
	const next = nameEnd + 10 + rdlen;
	if (next > body.length) return null;
	return { type, ttlOff, next };
}

/**
 * 应答报文的最小记录 TTL（跳过伪记录）；解析失败返回 -1（对齐 Go dnsMinTTL）
 * @param {Uint8Array} body
 * @returns {number}
 */
export function dnsMinTTL(body) {
	const sec = dnsSectionStart(body);
	if (!sec) return -1;
	let min = -1;
	let i = sec.offset;
	for (let c = 0; c < sec.count; c++) {
		const rr = rrNext(body, i);
		if (!rr) return -1;
		if (!isPseudoRR(rr.type)) {
			const ttl = readBE32(body, rr.ttlOff);
			if (min < 0 || ttl < min) min = ttl;
		}
		i = rr.next;
	}
	return min;
}

/**
 * 按已过秒数递减应答区所有记录的 TTL（拷贝后修改，跳过伪记录，钳 0 下限）
 * 解析失败时原样返回入参报文——宁可 TTL 偏大也不破坏应答（对齐 Go decrementTTLs）
 * @param {Uint8Array} body
 * @param {number} sec 已过秒数
 * @returns {Uint8Array}
 */
export function decrementTTLs(body, sec) {
	const out = body.slice();
	const start = dnsSectionStart(out);
	if (!start) return body;
	let i = start.offset;
	for (let c = 0; c < start.count; c++) {
		const rr = rrNext(out, i);
		if (!rr) return body;
		if (!isPseudoRR(rr.type)) {
			const ttl = readBE32(out, rr.ttlOff);
			writeBE32(out, rr.ttlOff, ttl > sec ? ttl - sec : 0);
		}
		i = rr.next;
	}
	return out;
}

/** 把应答报文的事务 ID 改写为客户端 ID（对齐 Go rewriteID） */
export function rewriteID(body, id) {
	const out = body.slice();
	out[0] = (id >> 8) & 0xff;
	out[1] = id & 0xff;
	return out;
}

/** base64url / base64 解码 ?dns= 参数（无填充优先，带填充兜底） */
function decodeDnsParam(raw) {
	if (!raw) return null;
	const normalized = String(raw).replace(/-/g, '+').replace(/_/g, '/');
	const attempts = [normalized, normalized + '='.repeat((4 - (normalized.length % 4)) % 4)];
	for (const s of attempts) {
		try {
			const bin = atob(s);
			const out = new Uint8Array(bin.length);
			for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
			return out;
		} catch {
			// 继续尝试下一种填充
		}
	}
	return null;
}

/**
 * 从请求中提取 DNS 查询（对齐 Go dohQueryOf）
 * 非GET/HEAD：解析请求体；GET/HEAD：解析 ?dns= 参数。
 * 解析失败返回 null（不缓存、不报错），JSON DoH 拿不到 wire 报文同样返回 null。
 * @param {Request} request
 * @param {URL} url
 * @param {Uint8Array|null} bodyBuf 已读取的请求体（GET/HEAD 传 null）
 * @returns {{id: number, keyBytes: Uint8Array}|null}
 */
function parseDohQuery(request, url, bodyBuf) {
	const method = (request.method || 'GET').toUpperCase();
	if (method !== 'GET' && method !== 'HEAD') {
		return bodyBuf && bodyBuf.byteLength > 0 ? parseDohMessageKey(bodyBuf) : null;
	}
	return parseDohMessageKey(decodeDnsParam(url.searchParams.get('dns')));
}

// ============================================================
// isolate 级运行状态：应答缓存（LRU）+ 上游熔断 + 计数器
// ============================================================

/**
 * 进程内 LRU 应答缓存（对齐 Go dnsCache：惰性过期 + 按条数淘汰）
 * @param {number} maxEntries 条目上限，<=0 返回 null（关闭）
 * @param {number} maxTtl 单条最长保留秒数，<=0 返回 null（关闭）
 */
function createDnsCache(maxEntries, maxTtl) {
	if (!maxEntries || maxEntries <= 0 || !maxTtl || maxTtl <= 0) return null;
	const map = new Map(); // key -> {body, contentType, fetchedAt, ttl}
	let hits = 0;
	let misses = 0;
	return {
		get(key) {
			const e = map.get(key);
			if (!e) {
				misses += 1;
				return null;
			}
			const elapsedSec = Math.floor((Date.now() - e.fetchedAt) / 1000);
			if (elapsedSec >= e.ttl) {
				map.delete(key);
				misses += 1;
				return null;
			}
			map.delete(key);
			map.set(key, e); // LRU 触碰
			hits += 1;
			return e;
		},
		put(key, body, contentType, ttl) {
			map.delete(key);
			map.set(key, { body, contentType: contentType || '', fetchedAt: Date.now(), ttl });
			while (map.size > maxEntries) {
				map.delete(map.keys().next().value);
			}
		},
		get hits() {
			return hits;
		},
		get misses() {
			return misses;
		},
		get size() {
			return map.size;
		},
	};
}

/**
 * 上游熔断状态（对齐 Go providerHealth：无显式 half-open，冷却到期即放行）
 * @param {number} threshold 连续失败阈值
 * @param {number} cooldownMs 冷却毫秒
 */
function createProviderHealth(threshold, cooldownMs) {
	const map = new Map(); // name -> {fails, until}
	const get = (name) => {
		let s = map.get(name);
		if (!s) {
			s = { fails: 0, until: 0 };
			map.set(name, s);
		}
		return s;
	};
	return {
		allow(name) {
			if (threshold <= 0) return true;
			const s = get(name);
			if (s.fails < threshold) return true;
			return Date.now() > s.until; // 冷却到期 → 放行探测
		},
		record(name, ok) {
			const s = get(name);
			if (ok) {
				s.fails = 0;
				return;
			}
			s.fails += 1;
			if (threshold > 0 && s.fails >= threshold) {
				s.until = Date.now() + cooldownMs;
			}
		},
		snapshot() {
			const out = {};
			for (const [name, s] of map) {
				out[name] = { healthy: s.fails < threshold, consecutive_failures: s.fails };
			}
			return out;
		},
	};
}

/**
 * 创建 isolate 级运行状态（每个 isolate 首个请求时创建一次；测试可按需新建）
 * 环境变量读取一次并固化：CACHE_MAX_TTL / CACHE_ENTRIES / CB_THRESHOLD /
 * CB_COOLDOWN_SEC / FAILOVER_CHAIN
 * @param {Object} [env]
 */
export function createEdgeState(env = {}) {
	const num = (v, d) => {
		const n = Number(v);
		return Number.isFinite(n) && n >= 0 ? n : d;
	};
	const cacheMaxTtl = num(env.CACHE_MAX_TTL, 300);
	const cacheEntries = num(env.CACHE_ENTRIES, 10000);
	const cbThreshold = num(env.CB_THRESHOLD, 5);
	const cbCooldownSec = num(env.CB_COOLDOWN_SEC, 30);
	// 空字符串 = 显式禁用故障转移（与 Go 版 strings.Split 语义一致），仅未设置时取默认
	const chainRaw = env.FAILOVER_CHAIN === undefined || env.FAILOVER_CHAIN === null ? 'cloudflare,google,adguard' : String(env.FAILOVER_CHAIN);
	const failoverChain = chainRaw
		.split(',')
		.map((s) => s.trim().toLowerCase().replace(/^\/+/, ''))
		.filter(Boolean);
	return {
		cache: createDnsCache(cacheMaxTtl > 0 ? cacheEntries : 0, cacheMaxTtl),
		cacheMaxTtl,
		health: createProviderHealth(cbThreshold, cbCooldownSec * 1000),
		failoverChain,
		startedAt: Date.now(),
		totalRequests: 0,
		upstreamErrors: 0,
	};
}

/** isolate 级共享状态（懒创建；注入 options.state 可覆盖，测试用） */
let sharedState = null;
function stateFor(env) {
	if (!sharedState) sharedState = createEdgeState(env);
	return sharedState;
}

/** 上游名 → 转移目标 URL（恒为 /dns-query + 原查询串，对齐 Go providerTarget） */
function failoverTarget(name, url, env) {
	const mapping = getPathMappings(env)[`/${name}`];
	if (!mapping || !mapping.targetDomain) return null;
	return `https://${mapping.targetDomain}/dns-query${url.search}`;
}

/**
 * 计算本次请求的上游候选链（对齐 Go candidates）：
 * [primary(若 allow)] + FAILOVER_CHAIN 中健康且存在的其它上游；全 OPEN 时仍打 primary
 */
function failoverCandidates(primary, state, env) {
	const mappings = getPathMappings(env);
	const out = [];
	if (state.health.allow(primary)) out.push(primary);
	for (const name of state.failoverChain) {
		if (name === primary || out.includes(name)) continue;
		const mapping = mappings[`/${name}`];
		if (!mapping || !mapping.targetDomain) continue; // 不在映射表里的名字忽略
		if (state.health.allow(name)) out.push(name);
	}
	if (out.length === 0) out.push(primary);
	return out;
}

/** 纯文本应答（错误/健康检查共用） */
function plainResponse(status, text, extraHeaders = {}) {
	return new Response(text, {
		status,
		headers: { 'Content-Type': 'text/plain; charset=utf-8', 'Cache-Control': 'no-store', ...extraHeaders },
	});
}

/** /metrics JSON（对齐 Go writeMetrics；省略进程级独有字段） */
function metricsResponse(state) {
	const payload = {
		total_requests: state.totalRequests,
		upstream_errors: state.upstreamErrors,
		uptime_seconds: Math.floor((Date.now() - state.startedAt) / 1000),
		providers: state.health.snapshot(),
	};
	if (state.cache) {
		payload.cache_hits = state.cache.hits;
		payload.cache_misses = state.cache.misses;
		payload.cache_entries = state.cache.size;
		payload.cache_max_ttl = state.cacheMaxTtl;
	} else {
		payload.cache_entries = 0;
	}
	return new Response(JSON.stringify(payload), {
		status: 200,
		headers: { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store' },
	});
}

/**
 * 核心代理：命中映射则转发到目标 DoH 服务，否则返回伪装 404。
 * 处理顺序对齐 Go 版 ServeHTTP：白名单 → healthz/metrics → 目标解析 →
 * 读请求体 → 缓存命中即返回 → 上游候选链（熔断+故障转移）→ 存缓存 → 响应。
 * @param {Request} request 原始请求
 * @param {Object} [options]
 * @param {Object} [options.env] 平台环境变量对象
 * @param {Function} [options.fetchImpl] 自定义 fetch 实现（便于测试）
 * @param {Object} [options.state] 运行状态（默认 isolate 级共享；测试注入全新状态）
 * @returns {Promise<Response>}
 */
export async function proxyRequest(request, options = {}) {
	const { env, fetchImpl = fetch } = options;
	const state = options.state || stateFor(env);
	state.totalRequests += 1;

	const url = new URL(request.url);

	// 主机白名单：未配置则放行；配置后只有绑定域名可用，
	// workers.dev / pages.dev / 预览域名都会被这条挡住
	if (!isHostAllowed(url.hostname, env)) {
		return camoResponse();
	}

	// 健康检查 / 指标端点（精确路径匹配，未配置时走下方伪装 404）
	const healthPath = env && env.HEALTH_PATH ? String(env.HEALTH_PATH) : '';
	if (healthPath && url.pathname === healthPath) {
		return plainResponse(200, 'ok');
	}
	const metricsPath = env && env.METRICS_PATH ? String(env.METRICS_PATH) : '';
	if (metricsPath && url.pathname === metricsPath) {
		return metricsResponse(state);
	}

	let target = resolveTarget(url, env);

	// 根路径兜底：只有「看起来确实是 DoH 请求」才转发，
	// 浏览器直接访问 https://域名/ 仍返回伪装 404
	if (!target && (url.pathname === '/' || url.pathname === '') && isDohRequest(request)) {
		target = bareDohTarget(url, env);
	}

	if (!target) {
		return camoResponse();
	}

	const method = (request.method || 'GET').toUpperCase();

	// 读取请求体（GET/HEAD 无体；上限对齐 Go maxBodyBytes）
	let bodyBuf = null;
	if (method !== 'GET' && method !== 'HEAD') {
		bodyBuf = new Uint8Array(await request.arrayBuffer());
		if (bodyBuf.byteLength > MAX_BODY_BYTES) {
			return plainResponse(413, 'request body too large');
		}
	}

	// 缓存键：provider 名 + DNS 报文键；JSON DoH / 解析失败 → 无键（不缓存、无 X-Cache）
	const provider = target.prefix === '/' ? String((env && env.DEFAULT_UPSTREAM) || 'cloudflare').replace(/^\/+/, '') : target.prefix.slice(1);
	const parsed = parseDohQuery(request, url, bodyBuf);
	const cacheKey = parsed && state.cache ? `${provider}|${bytesToHex(parsed.keyBytes)}` : null;

	// 缓存命中：直接本地返回（对齐 Go：熔断不影响命中路径）
	if (cacheKey && state.cache) {
		const entry = state.cache.get(cacheKey);
		if (entry) {
			const elapsedSec = Math.floor((Date.now() - entry.fetchedAt) / 1000);
			let out = decrementTTLs(entry.body, elapsedSec);
			out = rewriteID(out, parsed.id);
			const policy = cachePolicy(request, env);
			const headers = new Headers();
			headers.set('Content-Type', entry.contentType || 'application/dns-message');
			headers.set('Cache-Control', policy.value);
			if (policy.vary) headers.set('Vary', 'Accept');
			headers.set('X-Cache', 'HIT');
			return new Response(out, { status: 200, headers });
		}
	}

	// 上游候选链：失败（网络错误/超时/5xx）逐个转移；4xx 算成功原样透传
	const candidates = failoverCandidates(provider, state, env);
	let upstream = null; // 成功的上游响应
	let bufferedBody = null; // 已整读的响应体（入缓存时使用）
	let lastUpstreamStatus = 0; // 最后一次上游 5xx 状态码

	for (const cand of candidates) {
		const attemptUrl = cand === provider ? target.targetUrl : failoverTarget(cand, url, env);
		if (!attemptUrl) continue;
		const init = {
			method,
			headers: forwardableHeaders(request.headers),
			redirect: 'follow',
		};
		if (bodyBuf && bodyBuf.byteLength > 0) {
			init.body = bodyBuf;
		}
		if (typeof AbortSignal === 'function' && typeof AbortSignal.timeout === 'function') {
			init.signal = AbortSignal.timeout(UPSTREAM_TIMEOUT_MS);
		}

		let resp = null;
		try {
			resp = await fetchImpl(attemptUrl, init);
		} catch (error) {
			state.health.record(cand, false);
			continue;
		}
		if (resp.status >= 500) {
			state.health.record(cand, false);
			lastUpstreamStatus = resp.status;
			try {
				await resp.arrayBuffer(); // 消费掉以便连接复用
			} catch {
				// 忽略读体失败
			}
			continue;
		}

		state.health.record(cand, true);

		// 200 + 有缓存键 → 整读响应体并尝试入缓存（对齐 Go storeCache 条件）；
		// 读体失败（罕见：上游中途断流/连接中断）→ 记该上游失败，继续候选链
		if (cacheKey && resp.status === 200) {
			try {
				bufferedBody = new Uint8Array(await resp.arrayBuffer());
				if (bufferedBody.byteLength <= MAX_BODY_BYTES) {
					const rcode = bufferedBody[3] & 0x0f;
					if (rcode === 0 || rcode === 3) {
						const minTtl = dnsMinTTL(bufferedBody);
						if (minTtl > 0) {
							state.cache.put(cacheKey, bufferedBody, resp.headers.get('content-type') || '', Math.min(minTtl, state.cacheMaxTtl));
						}
					}
				}
			} catch (error) {
				state.health.record(cand, false);
				resp = null;
			}
		}
		if (!resp) continue;
		upstream = resp;
		break;
	}

	// 全部候选失败：最后一次是 5xx → 503，网络错误 → 502（对齐 Go）
	if (!upstream) {
		state.upstreamErrors += 1;
		return plainResponse(lastUpstreamStatus ? 503 : 502, 'upstream error\n', { 'Retry-After': '2' });
	}

	// 响应头：复制上游 → 剥 hop-by-hop → 覆盖缓存策略 → X-Cache
	const policy = cachePolicy(request, env);
	const headers = new Headers();
	upstream.headers.forEach((value, key) => {
		if (!HOP_BY_HOP_HEADERS.includes(key.toLowerCase())) {
			headers.set(key, value);
		}
	});
	headers.set('Cache-Control', policy.value);
	if (policy.vary) {
		const vary = headers.get('Vary');
		headers.set('Vary', vary ? `${vary}, Accept` : 'Accept');
	}
	if (cacheKey) {
		headers.set('X-Cache', 'MISS');
	}

	return new Response(bufferedBody || upstream.body, {
		status: upstream.status,
		statusText: upstream.statusText,
		headers,
	});
}

export default {
	resolveTarget,
	bareDohTarget,
	isDohRequest,
	getPathMappings,
	getAllowedHosts,
	isHostAllowed,
	cachePolicy,
	proxyRequest,
	camoResponse,
	parseDohMessageKey,
	dnsMinTTL,
	decrementTTLs,
	rewriteID,
	createEdgeState,
	HOMEPAGE_HTML,
	DEFAULT_PATH_MAPPINGS,
};
