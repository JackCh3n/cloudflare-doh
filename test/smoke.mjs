/**
 * 零依赖冒烟测试
 *
 * 直接运行：node test/smoke.mjs
 * 覆盖：路径映射解析、转发目标拼接、请求头剥离、POST 请求体透传、
 *       最长前缀匹配、DOMAIN_MAPPINGS 覆盖、两个平台入口的基本可用性、
 *       DNS 报文解析（缓存键/TTL 递减/OPT 跳过）、进程内应答缓存、
 *       上游熔断与故障转移、健康检查与 metrics。
 * 说明：不发起真实外网请求，统一用注入的 fetchImpl 桩函数断言转发参数。
 */

import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import {
	proxyRequest,
	resolveTarget,
	getPathMappings,
	parseDohMessageKey,
	dnsMinTTL,
	decrementTTLs,
	rewriteID,
	createEdgeState,
	HOMEPAGE_HTML,
	DEFAULT_PATH_MAPPINGS,
} from '../src/core.js';
import cfWorker from '../_worker.js';
import { middleware as edgeMiddleware } from '../middleware.js';

let passed = 0;
const failures = [];

async function check(name, fn) {
	try {
		await fn();
		passed += 1;
		console.log(`  ✔ ${name}`);
	} catch (error) {
		failures.push({ name, error });
		console.error(`  ✘ ${name}\n    ${error.message}`);
	}
}

/** 构造一个记录调用参数的 fetch 桩 */
function createFetchStub(response = new Response('ok', { status: 200 })) {
	const calls = [];
	const stub = async (url, init) => {
		calls.push({ url, init });
		return response;
	};
	stub.calls = calls;
	return stub;
}

/** 按调用次数依次返回不同结果/抛错的 fetch 桩（测故障转移/熔断） */
function createSequenceFetch(steps) {
	const calls = [];
	const stub = async (url, init) => {
		calls.push({ url, init });
		const step = steps[calls.length - 1];
		if (!step) throw new Error('超出预期的上游调用次数');
		if (step.error) throw step.error;
		return step.response;
	};
	stub.calls = calls;
	return stub;
}

/** 每次调用用工厂新建 Response 的 fetch 桩（避免复用已读的 Response 体） */
function createFactoryFetch(makeResponse) {
	const calls = [];
	const stub = async (url, init) => {
		calls.push({ url, init });
		return makeResponse();
	};
	stub.calls = calls;
	return stub;
}

// ---------- DNS 报文构造器（手工字节，语义同 Go 版测试） ----------

function dnsQnameBytes(name) {
	const out = [];
	for (const label of name.split('.')) {
		out.push(label.length);
		for (const ch of label) out.push(ch.charCodeAt(0));
	}
	out.push(0);
	return out;
}

/** 构造 DNS 查询报文（RD=1，单 question） */
function makeDnsQuery(id, name = 'example.com', type = 1) {
	const qname = dnsQnameBytes(name);
	const b = new Uint8Array(12 + qname.length + 4);
	const dv = new DataView(b.buffer);
	dv.setUint16(0, id);
	dv.setUint16(2, 0x0100); // flags: RD
	dv.setUint16(4, 1); // QDCOUNT
	b.set(qname, 12);
	dv.setUint16(12 + qname.length, type);
	dv.setUint16(12 + qname.length + 2, 1); // QCLASS IN
	return b;
}

/**
 * 构造 DNS 应答报文：单条 A 记录（非压缩名字，TTL 可定位）+ 可选 OPT
 * example.com 的 QNAME = 13 字节，A 记录 TTL 字段偏移恒为 46
 */
const A_TTL_OFFSET = 12 + 13 + 4 + 13 + 4; // = 46
const OPT_TTL_OFFSET = 12 + 13 + 4 + (13 + 10 + 4) + 1 + 2 + 2; // = 61
function makeDnsResponse(id, ttl = 300, { rcode = 0, withOPT = false } = {}) {
	const qname = dnsQnameBytes('example.com');
	const answer = new Uint8Array(13 + 10 + 4);
	answer.set(dnsQnameBytes('example.com'), 0);
	const adv = new DataView(answer.buffer);
	adv.setUint16(13, 1); // TYPE A
	adv.setUint16(15, 1); // CLASS IN
	adv.setUint32(17, ttl);
	adv.setUint16(21, 4); // RDLENGTH
	answer.set([1, 2, 3, 4], 23);
	const opt = withOPT ? new Uint8Array(11) : null;
	if (opt) {
		const odv = new DataView(opt.buffer);
		odv.setUint16(1, 41); // TYPE OPT（name 为根标签 0）
		odv.setUint16(3, 1232); // CLASS = UDP 载荷大小
		// TTL 字段保持全 0（扩展 RCODE/标志位，不可被递减触碰）
	}
	const total = 12 + qname.length + 4 + answer.length + (opt ? opt.length : 0);
	const b = new Uint8Array(total);
	const dv = new DataView(b.buffer);
	dv.setUint16(0, id);
	dv.setUint16(2, 0x8180 | rcode); // QR|RD|RA + rcode
	dv.setUint16(4, 1); // QDCOUNT
	dv.setUint16(6, 1); // ANCOUNT
	dv.setUint16(10, withOPT ? 1 : 0); // ARCOUNT
	b.set(qname, 12);
	dv.setUint16(12 + qname.length, 1);
	dv.setUint16(12 + qname.length + 2, 1);
	b.set(answer, 12 + qname.length + 4);
	if (opt) b.set(opt, 12 + qname.length + 4 + answer.length);
	return b;
}

function dnsPostRequest(path, body) {
	return new Request(`https://x.dev${path}`, {
		method: 'POST',
		headers: { accept: 'application/dns-message', 'content-type': 'application/dns-message' },
		body,
	});
}

console.log('cloudflare-doh 冒烟测试');

await check('默认映射包含 6 家 DoH（含国内可达的 dnspod/alidns）', () => {
	assert.deepEqual(Object.keys(DEFAULT_PATH_MAPPINGS).sort(), [
		'/adguard',
		'/alidns',
		'/cloudflare',
		'/dnspod',
		'/google',
		'/quad9',
	]);
});

await check('/dnspod 与 /alidns 映射到国内 DoH', () => {
	assert.equal(resolveTarget(new URL('https://x.dev/dnspod/query-dns?name=a.com')).targetUrl, 'https://doh.pub/dns-query?name=a.com');
	assert.equal(resolveTarget(new URL('https://x.dev/dnspod/dns-query?name=a.com')).targetUrl, 'https://doh.pub/dns-query?name=a.com');
	assert.equal(resolveTarget(new URL('https://x.dev/alidns/query-dns?name=a.com')).targetUrl, 'https://dns.alidns.com/dns-query?name=a.com');
});

await check('/google/query-dns 转发到 dns.google/dns-query 且保留查询串', () => {
	const target = resolveTarget(new URL('https://doh.example.com/google/query-dns?name=example.com&type=A'));
	assert.equal(target.targetUrl, 'https://dns.google/dns-query?name=example.com&type=A');
});

await check('/cloudflare/query-dns 转发到 one.one.one.one/dns-query', () => {
	const target = resolveTarget(new URL('https://doh.example.com/cloudflare/query-dns?name=a.com'));
	assert.equal(target.targetUrl, 'https://one.one.one.one/dns-query?name=a.com');
});

await check('/quad9 与 /adguard 映射正确', () => {
	assert.equal(
		resolveTarget(new URL('https://x.dev/quad9/query-dns?name=a.com')).targetUrl,
		'https://dns.quad9.net/dns-query?name=a.com'
	);
	assert.equal(
		resolveTarget(new URL('https://x.dev/adguard/query-dns?name=a.com')).targetUrl,
		'https://dns.adguard-dns.com/dns-query?name=a.com'
	);
});

await check('仅域名无剩余路径时补根路径', () => {
	const target = resolveTarget(new URL('https://x.dev/google?name=a.com'));
	assert.equal(target.targetUrl, 'https://dns.google/?name=a.com');
});

await check('根路径与未命中路径不转发', () => {
	assert.equal(resolveTarget(new URL('https://x.dev/')), null);
	assert.equal(resolveTarget(new URL('https://x.dev/index.html')), null);
	assert.equal(resolveTarget(new URL('https://x.dev/unknown/path')), null);
});

await check('裸 /dns-query 默认转发到 cloudflare（兼容标准 DoH 客户端写法）', () => {
	assert.equal(resolveTarget(new URL('https://x.dev/dns-query?dns=abc')).targetUrl, 'https://one.one.one.one/dns-query?dns=abc');
	assert.equal(resolveTarget(new URL('https://x.dev/query-dns')).targetUrl, 'https://one.one.one.one/dns-query');
	assert.equal(resolveTarget(new URL('https://x.dev/dns-query/')).targetUrl, 'https://one.one.one.one/dns-query');
});

await check('DEFAULT_UPSTREAM 可改写裸路径的上游', () => {
	assert.equal(
		resolveTarget(new URL('https://x.dev/dns-query?dns=abc'), { DEFAULT_UPSTREAM: 'google' }).targetUrl,
		'https://dns.google/dns-query?dns=abc'
	);
	assert.equal(
		resolveTarget(new URL('https://x.dev/dns-query'), { DEFAULT_UPSTREAM: '/adguard' }).targetUrl,
		'https://dns.adguard-dns.com/dns-query'
	);
	// 不存在的上游名回退为不转发
	assert.equal(resolveTarget(new URL('https://x.dev/dns-query'), { DEFAULT_UPSTREAM: 'nope' }), null);
});

await check('根路径只有 DoH 请求才转发（浏览器访问仍是伪装 404）', async () => {
	const stub = createFetchStub(new Response('bin', { status: 200 }));
	const dohPost = await proxyRequest(
		new Request('https://x.dev/', {
			method: 'POST',
			headers: { 'content-type': 'application/dns-message', accept: 'application/dns-message' },
			body: new Uint8Array([1, 2, 3]),
		}),
		{ fetchImpl: stub }
	);
	assert.equal(dohPost.status, 200);
	assert.equal(stub.calls[0].url, 'https://one.one.one.one/dns-query');

	const browser = await proxyRequest(new Request('https://x.dev/', { headers: { accept: 'text/html' } }), {
		fetchImpl: () => {
			throw new Error('不应发起请求');
		},
	});
	assert.equal(browser.status, 404);
});

await check('最长前缀优先匹配', () => {
	const env = {
		DOMAIN_MAPPINGS: {
			'/google': { targetDomain: 'dns.google', pathMapping: {} },
			'/google-v2': { targetDomain: 'v2.example.com', pathMapping: {} },
		},
	};
	assert.equal(resolveTarget(new URL('https://x.dev/google-v2/query'), env).targetDomain, 'v2.example.com');
	assert.equal(resolveTarget(new URL('https://x.dev/google/query'), env).targetDomain, 'dns.google');
});

await check('pathMapping 只做前缀重写且最长键优先（中间出现不误替换）', () => {
	const env = {
		DOMAIN_MAPPINGS: {
			'/demo': { targetDomain: 'd.example.com', pathMapping: { '/dns': '/x', '/dns-query': '/dns-query' } },
		},
	};
	// '/dns' 出现在中间不得被替换；rest 不以任何键开头时原样保留
	assert.equal(resolveTarget(new URL('https://x.dev/demo/v1/dns-query'), env).targetUrl, 'https://d.example.com/v1/dns-query');
	// 键长倒序：rest='/dns-query' 应命中更长的 '/dns-query' 而不是 '/dns'
	assert.equal(resolveTarget(new URL('https://x.dev/demo/dns-query'), env).targetUrl, 'https://d.example.com/dns-query');
});

await check('DOMAIN_MAPPINGS 字符串 JSON 生效并覆盖默认值', () => {
	const env = {
		DOMAIN_MAPPINGS: JSON.stringify({
			'/demo': { targetDomain: 'demo.doh.cn', pathMapping: { '/query-dns': '/dns-query' } },
		}),
	};
	assert.deepEqual(Object.keys(getPathMappings(env)), ['/demo']);
	assert.equal(resolveTarget(new URL('https://x.dev/demo/query-dns'), env).targetUrl, 'https://demo.doh.cn/dns-query');
	assert.equal(resolveTarget(new URL('https://x.dev/google/query-dns'), env), null);
});

await check('DOMAIN_MAPPINGS 为非法 JSON 时回退默认映射', () => {
	const env = { DOMAIN_MAPPINGS: '{invalid json' };
	assert.deepEqual(getPathMappings(env), DEFAULT_PATH_MAPPINGS);
});

await check('未命中映射返回伪装 404 页面', async () => {
	const response = await proxyRequest(new Request('https://x.dev/'), { fetchImpl: () => { throw new Error('不应发起请求'); } });
	assert.equal(response.status, 404);
	assert.match(response.headers.get('content-type'), /text\/html/);
	assert.match(await response.text(), /无法访问此网站/);
});

await check('GET 转发不携带请求体，且剥离 host 头', async () => {
	const stub = createFetchStub();
	await proxyRequest(
		new Request('https://x.dev/google/query-dns?name=example.com', {
			headers: { accept: 'application/dns-json', host: 'x.dev' },
		}),
		{ fetchImpl: stub }
	);
	assert.equal(stub.calls.length, 1);
	assert.equal(stub.calls[0].url, 'https://dns.google/dns-query?name=example.com');
	assert.equal(stub.calls[0].init.method, 'GET');
	assert.equal(stub.calls[0].init.body, undefined);
	assert.equal(stub.calls[0].init.headers.get('accept'), 'application/dns-json');
	assert.equal(stub.calls[0].init.headers.get('host'), null);
});

await check('POST 转发保留二进制请求体与 content-type', async () => {
	const stub = createFetchStub();
	const payload = new Uint8Array([0x00, 0x01, 0x02, 0xff]);
	await proxyRequest(
		new Request('https://x.dev/cloudflare/query-dns', {
			method: 'POST',
			headers: { 'content-type': 'application/dns-message' },
			body: payload,
		}),
		{ fetchImpl: stub }
	);
	const { init } = stub.calls[0];
	assert.equal(init.method, 'POST');
	assert.equal(init.headers.get('content-type'), 'application/dns-message');
	assert.deepEqual(Array.from(new Uint8Array(init.body)), [0x00, 0x01, 0x02, 0xff]);
});

await check('上游响应状态码、响应体、content-type 保留', async () => {
	const upstream = new Response('dns-binary', { status: 200, headers: { 'content-type': 'application/dns-message' } });
	const response = await proxyRequest(new Request('https://x.dev/google/query-dns?name=a.com'), { fetchImpl: createFetchStub(upstream) });
	assert.equal(response.status, 200);
	assert.equal(response.headers.get('content-type'), 'application/dns-message');
	assert.equal(await response.text(), 'dns-binary');
});

await check('默认给上游响应加 no-store（避免 Workers Cache 启发式缓存 2 小时）', async () => {
	const upstream = new Response('{}', { status: 200, headers: { 'content-type': 'application/dns-json' } });
	const response = await proxyRequest(new Request('https://x.dev/google/query-dns?name=a.com'), { fetchImpl: createFetchStub(upstream) });
	assert.equal(response.headers.get('cache-control'), 'no-store');
});

await check('伪装 404 带 no-store 与 nosniff', async () => {
	const response = await proxyRequest(new Request('https://x.dev/unknown'), { fetchImpl: () => { throw new Error('不应发起请求'); } });
	assert.equal(response.status, 404);
	assert.equal(response.headers.get('cache-control'), 'no-store');
	assert.equal(response.headers.get('x-content-type-options'), 'nosniff');
});

await check('上游多条 set-cookie 透传时不被覆盖', async () => {
	const upstream = new Response('x', { status: 200, headers: { 'content-type': 'application/dns-json' } });
	upstream.headers.append('set-cookie', 'a=1');
	upstream.headers.append('set-cookie', 'b=2');
	const response = await proxyRequest(new Request('https://x.dev/google/query-dns?name=a.com', { headers: { accept: 'application/dns-json' } }), {
		fetchImpl: createFetchStub(upstream),
	});
	assert.match(response.headers.get('set-cookie') || '', /a=1,\s*b=2/);
});

await check('CACHE_TTL>0 时仅缓存 GET + dns-message，并带 Vary: Accept', async () => {
	const stub = createFetchStub(new Response('bin', { status: 200, headers: { 'content-type': 'application/dns-message' } }));
	const env = { CACHE_TTL: '30' };
	const cached = await proxyRequest(
		new Request('https://x.dev/google/query-dns?dns=abc', { headers: { accept: 'application/dns-message' } }),
		{ env, fetchImpl: stub }
	);
	assert.equal(cached.headers.get('cache-control'), 'public, max-age=30');
	assert.match(cached.headers.get('vary') || '', /Accept/i);

	const notCached = await proxyRequest(new Request('https://x.dev/google/query-dns?name=a.com', { headers: { accept: 'application/dns-json' } }), {
		env,
		fetchImpl: stub,
	});
	assert.equal(notCached.headers.get('cache-control'), 'no-store');
});

await check('POST 永不缓存（即使 CACHE_TTL>0）', async () => {
	const stub = createFetchStub(new Response('bin', { status: 200 }));
	const response = await proxyRequest(
		new Request('https://x.dev/google/query-dns', {
			method: 'POST',
			headers: { accept: 'application/dns-message', 'content-type': 'application/dns-message' },
			body: new Uint8Array([1, 2, 3]),
		}),
		{ env: { CACHE_TTL: '30' }, fetchImpl: stub }
	);
	assert.equal(response.headers.get('cache-control'), 'no-store');
});

await check('ALLOWED_HOSTS 命中时正常转发', async () => {
	const stub = createFetchStub();
	await proxyRequest(new Request('https://doh.example.com/google/query-dns?name=a.com'), {
		env: { ALLOWED_HOSTS: 'doh.example.com, FuckDNS.Nobige.cn ' },
		fetchImpl: stub,
	});
	assert.equal(stub.calls.length, 1);
});

await check('ALLOWED_HOSTS 未命中的域名（如 pages.dev）返回伪装 404 且不转发', async () => {
	const response = await proxyRequest(new Request('https://cloudflare-doh-5s9.pages.dev/google/query-dns?name=a.com'), {
		env: { ALLOWED_HOSTS: 'doh.example.com' },
		fetchImpl: () => {
			throw new Error('不应发起请求');
		},
	});
	assert.equal(response.status, 404);
	assert.match(await response.text(), /无法访问此网站/);
});

await check('未配置 ALLOWED_HOSTS 时不限制域名', async () => {
	const stub = createFetchStub();
	await proxyRequest(new Request('https://whatever.workers.dev/google/query-dns?name=a.com'), { env: {}, fetchImpl: stub });
	assert.equal(stub.calls.length, 1);
});

await check('Cloudflare 入口可调用且透传 env', async () => {
	const stub = createFetchStub();
	const original = globalThis.fetch;
	globalThis.fetch = stub;
	try {
		const response = await cfWorker.fetch(
			new Request('https://x.dev/google/query-dns?name=a.com'),
			{ DOMAIN_MAPPINGS: { '/google': { targetDomain: 'cf.doh.cn', pathMapping: {} } } },
			{}
		);
		assert.equal(response.status, 200);
		assert.equal(stub.calls[0].url, 'https://cf.doh.cn/query-dns?name=a.com');
	} finally {
		globalThis.fetch = original;
	}
});

await check('EdgeOne 中间件入口可从 context 读取 request/env', async () => {
	const stub = createFetchStub();
	const original = globalThis.fetch;
	globalThis.fetch = stub;
	try {
		const response = await edgeMiddleware({
			request: new Request('https://x.dev/demo/query-dns?name=a.com'),
			env: { DOMAIN_MAPPINGS: { '/demo': { targetDomain: 'eo.doh.cn', pathMapping: { '/query-dns': '/dns-query' } } } },
		});
		assert.equal(response.status, 200);
		assert.equal(stub.calls[0].url, 'https://eo.doh.cn/dns-query?name=a.com');
	} finally {
		globalThis.fetch = original;
	}
});

// ==================== DNS 报文解析（对齐 Go 版单测语义） ====================

await check('parseDohMessageKey：键不含事务 ID，不同 ID 同键', () => {
	const a = parseDohMessageKey(makeDnsQuery(0x1111));
	const b = parseDohMessageKey(makeDnsQuery(0x2222));
	assert.equal(a.id, 0x1111);
	assert.equal(Buffer.from(a.keyBytes).equals(Buffer.from(b.keyBytes)), true);
	assert.equal(a.keyBytes.length, 2 + 17); // flags 2B + question 17B(qname13+qtype2+qclass2)
});

await check('parseDohMessageKey：短报文 / QDCOUNT≠1 / question 带指针 均返回 null', () => {
	assert.equal(parseDohMessageKey(new Uint8Array([1, 2, 3])), null);
	const twoQ = makeDnsQuery(1);
	twoQ[5] = 2; // QDCOUNT=2
	assert.equal(parseDohMessageKey(twoQ), null);
	const ptr = makeDnsQuery(1);
	ptr[12] = 0xc0; // QNAME 首字节是指针
	assert.equal(parseDohMessageKey(ptr), null);
});

await check('dnsMinTTL：A 记录 300；带 OPT 时忽略 OPT 的 0 TTL；畸形报文 -1', () => {
	assert.equal(dnsMinTTL(makeDnsResponse(1, 300)), 300);
	assert.equal(dnsMinTTL(makeDnsResponse(1, 300, { withOPT: true })), 300);
	assert.equal(dnsMinTTL(new Uint8Array(5)), -1);
	// 名字区出现 0x40/0x80（非法标签长度，非 0xC0 指针）→ 解析失败，对齐 Go
	const evil = makeDnsResponse(1, 300);
	evil[12 + 17] = 0x40; // 应答区名字首字节改成非法长度
	assert.equal(dnsMinTTL(evil), -1);
	const evil2 = makeDnsResponse(1, 300);
	evil2[12 + 17] = 0x80;
	assert.equal(dnsMinTTL(evil2), -1);
});

await check('decrementTTLs：A 记录递减、OPT 的 TTL 字段不动、原报文不被修改、超额钳 0', () => {
	const resp = makeDnsResponse(1, 300, { withOPT: true });
	const snapshot = Buffer.from(resp);
	const out = decrementTTLs(resp, 100);
	const dv = new DataView(out.buffer);
	assert.equal(dv.getUint32(A_TTL_OFFSET), 200);
	assert.equal(dv.getUint32(OPT_TTL_OFFSET), 0); // OPT TTL 恒 0
	assert.equal(Buffer.from(resp).equals(snapshot), true); // 原报文不被就地修改
	const out2 = decrementTTLs(resp, 999);
	assert.equal(new DataView(out2.buffer).getUint32(A_TTL_OFFSET), 0);
});

await check('rewriteID：改写头两字节为客户端 ID', () => {
	const out = rewriteID(makeDnsResponse(0xabcd, 300), 0x2222);
	assert.equal((out[0] << 8) | out[1], 0x2222);
});

// ==================== 进程内应答缓存 ====================

await check('POST 查询 MISS→HIT：换事务 ID 也能命中，且只打一次上游', async () => {
	const state = createEdgeState({});
	const stub = createFetchStub(
		new Response(makeDnsResponse(0xabcd, 300), { status: 200, headers: { 'content-type': 'application/dns-message' } })
	);
	const r1 = await proxyRequest(dnsPostRequest('/cloudflare/query-dns', makeDnsQuery(0x1111)), { env: {}, fetchImpl: stub, state });
	assert.equal(r1.status, 200);
	assert.equal(r1.headers.get('x-cache'), 'MISS');

	const r2 = await proxyRequest(dnsPostRequest('/cloudflare/query-dns', makeDnsQuery(0x2222)), { env: {}, fetchImpl: stub, state });
	assert.equal(stub.calls.length, 1, '命中缓存不应再打上游');
	assert.equal(r2.headers.get('x-cache'), 'HIT');
	assert.equal(r2.headers.get('content-type'), 'application/dns-message');
	const body = new Uint8Array(await r2.arrayBuffer());
	assert.equal((body[0] << 8) | body[1], 0x2222, '命中应答 ID 已改写为客户端 ID');
	assert.equal(r2.headers.get('cache-control'), 'no-store', 'POST 命中也保持 no-store 响应头');
});

await check('GET ?dns= 查询同样可命中缓存', async () => {
	const state = createEdgeState({});
	const param = Buffer.from(makeDnsQuery(7)).toString('base64url');
	const stub = createFetchStub(new Response(makeDnsResponse(7, 300), { status: 200 }));
	const url = `https://x.dev/google/query-dns?dns=${param}`;
	const r1 = await proxyRequest(new Request(url, { headers: { accept: 'application/dns-message' } }), { env: {}, fetchImpl: stub, state });
	assert.equal(r1.headers.get('x-cache'), 'MISS');
	const r2 = await proxyRequest(new Request(url, { headers: { accept: 'application/dns-message' } }), { env: {}, fetchImpl: stub, state });
	assert.equal(stub.calls.length, 1);
	assert.equal(r2.headers.get('x-cache'), 'HIT');
});

await check('JSON DoH 无 wire 报文：不缓存、无 X-Cache 头', async () => {
	const state = createEdgeState({});
	const stub = createFetchStub(new Response('{}', { status: 200, headers: { 'content-type': 'application/dns-json' } }));
	const r = await proxyRequest(new Request('https://x.dev/google/query-dns?name=a.com', { headers: { accept: 'application/dns-json' } }), {
		env: {},
		fetchImpl: stub,
		state,
	});
	assert.equal(r.headers.get('x-cache'), null);
	const r2 = await proxyRequest(new Request('https://x.dev/google/query-dns?name=a.com', { headers: { accept: 'application/dns-json' } }), {
		env: {},
		fetchImpl: stub,
		state,
	});
	assert.equal(stub.calls.length, 2);
	assert.equal(r2.headers.get('x-cache'), null);
});

await check('rcode=2 与 TTL=0 的应答不缓存', async () => {
	const stateErr = createEdgeState({});
	const stubErr = createFactoryFetch(() => new Response(makeDnsResponse(1, 300, { rcode: 2 }), { status: 200 }));
	await proxyRequest(dnsPostRequest('/cloudflare/query-dns', makeDnsQuery(1)), { env: {}, fetchImpl: stubErr, state: stateErr });
	await proxyRequest(dnsPostRequest('/cloudflare/query-dns', makeDnsQuery(1)), { env: {}, fetchImpl: stubErr, state: stateErr });
	assert.equal(stubErr.calls.length, 2);

	const stateZero = createEdgeState({});
	const stubZero = createFactoryFetch(() => new Response(makeDnsResponse(1, 0), { status: 200 }));
	await proxyRequest(dnsPostRequest('/cloudflare/query-dns', makeDnsQuery(1)), { env: {}, fetchImpl: stubZero, state: stateZero });
	await proxyRequest(dnsPostRequest('/cloudflare/query-dns', makeDnsQuery(1)), { env: {}, fetchImpl: stubZero, state: stateZero });
	assert.equal(stubZero.calls.length, 2);
});

await check('CACHE_MAX_TTL=0 关闭进程内缓存', async () => {
	const state = createEdgeState({ CACHE_MAX_TTL: '0' });
	assert.equal(state.cache, null);
	const stub = createFetchStub(new Response(makeDnsResponse(1, 300), { status: 200 }));
	await proxyRequest(dnsPostRequest('/cloudflare/query-dns', makeDnsQuery(1)), { env: {}, fetchImpl: stub, state });
	await proxyRequest(dnsPostRequest('/cloudflare/query-dns', makeDnsQuery(1)), { env: {}, fetchImpl: stub, state });
	assert.equal(stub.calls.length, 2);
});

await check('请求体超过 64KB 返回 413 且不打上游', async () => {
	const state = createEdgeState({});
	const stub = createFetchStub();
	const r = await proxyRequest(dnsPostRequest('/cloudflare/query-dns', new Uint8Array(64 * 1024 + 1)), { env: {}, fetchImpl: stub, state });
	assert.equal(r.status, 413);
	assert.equal(stub.calls.length, 0);
});

await check('hop-by-hop 请求头不透传给上游，accept-encoding 由运行时自管', async () => {
	const stub = createFetchStub();
	await proxyRequest(
		new Request('https://x.dev/google/query-dns?name=a.com', {
			headers: { accept: 'application/dns-json', connection: 'keep-alive', te: 'trailers', host: 'x.dev', 'accept-encoding': 'gzip, br' },
		}),
		{ env: {}, fetchImpl: stub }
	);
	assert.equal(stub.calls[0].init.headers.get('connection'), null);
	assert.equal(stub.calls[0].init.headers.get('te'), null);
	assert.equal(stub.calls[0].init.headers.get('host'), null);
	// 透传 accept-encoding 会让压缩应答进缓存，未带该头的客户端命中后拿到压缩体（对齐 Go 版）
	assert.equal(stub.calls[0].init.headers.get('accept-encoding'), null);
});

// ==================== 上游熔断与故障转移 ====================

await check('首选上游网络错误 → 按 FAILOVER_CHAIN 转移到下一家', async () => {
	const state = createEdgeState({ FAILOVER_CHAIN: 'google' });
	const stub = createSequenceFetch([{ error: new Error('connection refused') }, { response: new Response(makeDnsResponse(1, 300), { status: 200 }) }]);
	const r = await proxyRequest(dnsPostRequest('/cloudflare/query-dns', makeDnsQuery(1)), { env: {}, fetchImpl: stub, state });
	assert.equal(r.status, 200);
	assert.equal(stub.calls[0].url.startsWith('https://one.one.one.one/'), true);
	assert.equal(stub.calls[1].url.startsWith('https://dns.google/dns-query'), true);
	assert.equal(r.headers.get('x-cache'), 'MISS');
});

await check('上游 5xx 记熔断失败：单上游时返回 503 + Retry-After', async () => {
	const state = createEdgeState({ FAILOVER_CHAIN: '', CB_THRESHOLD: '1' });
	const stub = createFetchStub(new Response('boom', { status: 500 }));
	const r = await proxyRequest(dnsPostRequest('/cloudflare/query-dns', makeDnsQuery(1)), { env: {}, fetchImpl: stub, state });
	assert.equal(r.status, 503);
	assert.equal(r.headers.get('retry-after'), '2');
	assert.deepEqual(state.health.snapshot().cloudflare, { healthy: false, consecutive_failures: 1 });
});

await check('全部上游网络错误 → 502 + Retry-After；全部 5xx → 503', async () => {
	const state502 = createEdgeState({ FAILOVER_CHAIN: 'google' });
	const stub502 = createSequenceFetch([{ error: new Error('e1') }, { error: new Error('e2') }]);
	const r502 = await proxyRequest(dnsPostRequest('/cloudflare/query-dns', makeDnsQuery(1)), { env: {}, fetchImpl: stub502, state: state502 });
	assert.equal(r502.status, 502);
	assert.equal(r502.headers.get('retry-after'), '2');

	const state503 = createEdgeState({ FAILOVER_CHAIN: 'google' });
	const stub503 = createSequenceFetch([{ response: new Response('x', { status: 500 }) }, { response: new Response('x', { status: 502 }) }]);
	const r503 = await proxyRequest(dnsPostRequest('/cloudflare/query-dns', makeDnsQuery(1)), { env: {}, fetchImpl: stub503, state: state503 });
	assert.equal(r503.status, 503);
});

await check('熔断达阈值后跳过故障上游；全 OPEN 时仍打首选兜底', async () => {
	const state = createEdgeState({ CB_THRESHOLD: '2', FAILOVER_CHAIN: '' });
	const stub = createSequenceFetch([
		{ response: new Response('x', { status: 500 }) }, // r1: 失败1
		{ response: new Response('x', { status: 500 }) }, // r2: 失败2 → OPEN
		{ response: new Response(makeDnsResponse(1, 300), { status: 200 }) }, // r3: 冷却未到但全 OPEN → 兜底仍打首选
	]);
	const r1 = await proxyRequest(dnsPostRequest('/cloudflare/query-dns', makeDnsQuery(1)), { env: {}, fetchImpl: stub, state });
	const r2 = await proxyRequest(dnsPostRequest('/cloudflare/query-dns', makeDnsQuery(1)), { env: {}, fetchImpl: stub, state });
	assert.equal(r1.status, 503);
	assert.equal(r2.status, 503);
	const r3 = await proxyRequest(dnsPostRequest('/cloudflare/query-dns', makeDnsQuery(1)), { env: {}, fetchImpl: stub, state });
	assert.equal(r3.status, 200);
	assert.equal(stub.calls.length, 3);
});

await check('4xx 不算熔断失败：原样透传且不触发转移', async () => {
	const state = createEdgeState({ FAILOVER_CHAIN: 'google' });
	const stub = createFetchStub(new Response('bad request', { status: 400 }));
	const r = await proxyRequest(dnsPostRequest('/cloudflare/query-dns', makeDnsQuery(1)), { env: {}, fetchImpl: stub, state });
	assert.equal(r.status, 400);
	assert.equal(await r.text(), 'bad request');
	assert.equal(stub.calls.length, 1, '4xx 不转移');
	assert.deepEqual(state.health.snapshot().cloudflare, { healthy: true, consecutive_failures: 0 });
});

// ==================== 健康检查 / metrics ====================

await check('HEALTH_PATH 配置后返回 200 ok；未配置走伪装 404', async () => {
	const state = createEdgeState({ HEALTH_PATH: '/healthz' });
	const ok = await proxyRequest(new Request('https://x.dev/healthz'), { env: { HEALTH_PATH: '/healthz' }, fetchImpl: () => { throw new Error('不应发起请求'); }, state });
	assert.equal(ok.status, 200);
	assert.equal(await ok.text(), 'ok');
	assert.equal(ok.headers.get('cache-control'), 'no-store');

	const off = await proxyRequest(new Request('https://x.dev/healthz'), { env: {}, fetchImpl: () => { throw new Error('不应发起请求'); } });
	assert.equal(off.status, 404);
});

await check('METRICS_PATH 输出 JSON：providers / cache 计数 / 累计指标', async () => {
	const state = createEdgeState({ METRICS_PATH: '/metrics' });
	const stub = createFetchStub(new Response(makeDnsResponse(1, 300), { status: 200 }));
	await proxyRequest(dnsPostRequest('/cloudflare/query-dns', makeDnsQuery(1)), { env: {}, fetchImpl: stub, state });
	const r = await proxyRequest(new Request('https://x.dev/metrics'), { env: { METRICS_PATH: '/metrics' }, fetchImpl: () => { throw new Error('不应发起请求'); }, state });
	assert.equal(r.status, 200);
	assert.equal(r.headers.get('cache-control'), 'no-store');
	const m = await r.json();
	assert.equal(m.total_requests, 2);
	assert.equal(m.upstream_errors, 0);
	assert.equal(typeof m.uptime_seconds, 'number');
	assert.deepEqual(m.providers.cloudflare, { healthy: true, consecutive_failures: 0 });
	// providers 预登记全部映射上游（对齐 Go），而不是只有访问过的
	assert.deepEqual(Object.keys(m.providers).sort(), ['adguard', 'alidns', 'cloudflare', 'dnspod', 'google', 'quad9']);
	assert.equal(m.cache_hits, 0);
	assert.equal(m.cache_misses, 1);
	assert.equal(m.cache_entries, 1);
	assert.equal(m.cache_max_ttl, 300);
});

await check('createEdgeState 默认值：缓存开启、链默认三家', () => {
	const s = createEdgeState({});
	assert.notEqual(s.cache, null);
	assert.deepEqual(s.failoverChain, ['cloudflare', 'google', 'adguard']);
	assert.equal(s.cacheMaxTtl, 300);
});

await check('DEFAULT_UPSTREAM 大小写不敏感（对齐 Go 的 ToLower）', () => {
	assert.equal(
		resolveTarget(new URL('https://x.dev/dns-query?dns=abc'), { DEFAULT_UPSTREAM: 'GOOGLE' }).targetUrl,
		'https://dns.google/dns-query?dns=abc'
	);
	assert.equal(
		resolveTarget(new URL('https://x.dev/dns-query?dns=abc'), { DEFAULT_UPSTREAM: '/AdGuard' }).targetUrl,
		'https://dns.adguard-dns.com/dns-query?dns=abc'
	);
});

await check('压缩指针名字的应答可正常计算/递减 TTL（对齐 Go cache_test 的 0xC00C 场景）', () => {
	// header(12) + question qq.com(8B qname + 4) + RR(name=0xC00C 指向 question, A, ttl=300, rdlen=4)
	const qname = [2, 0x71, 0x71, 3, 0x63, 0x6f, 0x6d, 0]; // qq.com
	const rr = [0xc0, 0x0c, 0, 1, 0, 1, 0, 0, 1, 0x2c, 0, 4, 1, 2, 3, 4]; // ttl=300
	const b = new Uint8Array(12 + qname.length + 4 + rr.length);
	const dv = new DataView(b.buffer);
	dv.setUint16(2, 0x8180);
	dv.setUint16(4, 1); // QDCOUNT
	dv.setUint16(6, 1); // ANCOUNT
	b.set(qname, 12);
	dv.setUint16(12 + qname.length, 1); // QTYPE A
	dv.setUint16(12 + qname.length + 2, 1); // QCLASS IN
	b.set(rr, 12 + qname.length + 4);
	assert.equal(dnsMinTTL(b), 300);
	const out = decrementTTLs(b, 100);
	assert.equal(dnsMinTTL(out), 200);
	// 指针字节本身不得被改动
	assert.deepEqual(Array.from(out.slice(12 + qname.length + 4, 12 + qname.length + 6)), [0xc0, 0x0c]);
});

await check('请求体读取失败返回 400（对齐 Go，而不是未处理拒绝）', async () => {
	const state = createEdgeState({});
	const req = new Request('https://x.dev/cloudflare/query-dns', {
		method: 'POST',
		headers: { 'content-type': 'application/dns-message' },
		body: makeDnsQuery(1),
	});
	await req.body.cancel(); // 模拟客户端中断，使 arrayBuffer() reject
	const r = await proxyRequest(req, { env: {}, fetchImpl: () => { throw new Error('不应发起请求'); }, state });
	assert.equal(r.status, 400);
});

// 伪装页源文件（index.html 是唯一来源，core.js / main.go 里的副本由 npm run camo:sync 写入）。
// 换行统一成 LF 再比较：Windows 上 autocrlf 检出是 CRLF，而模板字符串求值与 Go 原生字符串都会丢弃 CR
const camoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const readText = async (file) => (await readFile(path.join(camoRoot, file), 'utf8')).replace(/\r\n/g, '\n');
const camoHTML = (await readText('index.html')).trim();

await check('伪装 404 页三处副本逐字一致（core.js / index.html / main.go）', async () => {
	const coreSrc = await readText('src/core.js');
	const goSrc = await readText('server/main.go');
	const jsHtml = coreSrc.match(/HOMEPAGE_HTML = `([\s\S]*?)`;/)[1].trim();
	const goHtml = goSrc.match(/homepageHTML = `([\s\S]*?)`/)[1].trim();
	assert.equal(jsHtml, camoHTML, 'core.js 与 index.html 不一致（改了页面请执行 npm run camo:sync）');
	assert.equal(goHtml, camoHTML, 'main.go 与 index.html 不一致（改了页面请执行 npm run camo:sync）');
	// 嵌入约束：core.js 用模板字符串、main.go 用原生字符串字面量包裹，出现以下字符会破坏嵌入
	assert.doesNotMatch(camoHTML, /`/, '页面不能含反引号');
	assert.doesNotMatch(camoHTML, /\$\{/, '页面不能含 "${"');
	// 模板字符串会把反斜杠当转义序列吃掉，main.go 却原样保留：源码逐字一致但求值结果悄悄分叉
	assert.doesNotMatch(camoHTML, /\\/, '页面不能含反斜杠（JS 字符串里改用单引号包裹）');
});

// 版式与交互守卫，全部以 Chrome 157（Windows）真实 DNS 错误页的实测结果为准
await check('伪装 404 页版式对齐 Chrome 实测（图标 72x72 + 1x/2x、主机名、按钮位置、手机底部通栏）', () => {
	// 标题与首行带主机名：Chrome 实测不带端口，所以用 hostname 而不是 host
	assert.match(camoHTML, /document\.title = location\.hostname/);
	assert.match(camoHTML, /location\.hostname\s*\|\|\s*'404'/);
	// 图标原本按 68x18 摆放，把 72x72 的位图压成一条；Chrome 是 72x72 盒子 + image-set 按像素密度选 1x/2x
	assert.match(camoHTML, /\.icon\{[^}]*?width:72px;\s*height:72px;/);
	assert.match(camoHTML, /background-image:image-set\(url\("[^"]+"\) 1x,url\("[^"]+"\) 2x\);/);
	// 桌面端按钮与正文左对齐（容器左浮动），窄屏时固定到底部
	assert.match(camoHTML, /\.controls\{\s*float:left;/);
	assert.match(camoHTML, /body \.nav\{\s*position:fixed;[^}]*?bottom:0;/);
	// 深浅色自适应
	assert.match(camoHTML, /prefers-color-scheme:dark/);
	assert.match(camoHTML, /<meta name="color-scheme" content="light dark">/);
	// 高对比度（强制色）模式会丢弃背景图，图标必须豁免，否则页面上少一个元素
	assert.match(camoHTML, /@media \(forced-colors:active\)\{\s*\.icon\{\s*forced-color-adjust:none;/);
	assert.doesNotMatch(camoHTML, /Windos |FINSHED/);
});

await check('伪装 404 页交互：重新加载可用，诊断链接是可聚焦的 <a> 且不改动地址', () => {
	assert.match(camoHTML, /onclick=function\(\)\{location\.reload\(\)\}/);
	assert.match(camoHTML, /<a href="javascript:void\(0\)" id="diagnose-link">/);
	// 不能再用样式冒充链接：鼠标点不动、键盘 Tab 也到不了
	assert.doesNotMatch(camoHTML, /<span>尝试运行/);
	assert.doesNotMatch(camoHTML, /href="#"/, '"#" 会在点击后把 # 追加到地址栏');
});

await check('伪装 404 页多语言表：Chrome 55 种界面语言，静态兜底文案与简中条目一致', () => {
	// 多语言表是 JS 字面量（含双引号的条目用单引号包裹），按 JS 求值而不是 JSON.parse
	const table = camoHTML.match(/var S=\{([\s\S]*?)\n\};/);
	assert.ok(table, '找不到多语言表 var S={...}');
	const S = new Function(`return {${table[1]}}`)();
	assert.equal(Object.keys(S).length, 55);
	for (const [tag, parts] of Object.entries(S)) {
		assert.equal(parts.length, 8, `${tag} 字段数`);
		assert.ok(parts.slice(0, 7).every((s) => typeof s === 'string'), `${tag} 前 7 项应为字符串`);
		assert.equal(typeof parts[7], 'number', `${tag} 第 8 项应为字体栈编号`);
		assert.ok(parts[7] >= 0 && parts[7] < 8, `${tag} 字体栈编号越界`);
	}
	// 文案取自 outerHTML，实体必须还原成字符：否则用 textContent 写入时会原样显示成 "&nbsp;"
	assert.doesNotMatch(camoHTML, /&nbsp;/);
	assert.match(camoHTML, /var R=\{"ar":1,"fa":1,"he":1,"ur":1\};/);
	const F = new Function(`return [${camoHTML.match(/var F=\[([\s\S]*?)\];/)[1]}]`)();
	assert.equal(F.length, 8);
	// 字体栈用 | 分隔，运行时拼成 CSS：带空格的名字加双引号
	assert.deepEqual(F.map((f) => f.split('|').map((n) => (n.includes(' ') ? `"${n}"` : n)).join(', ')), [
		'"Segoe UI", Tahoma, sans-serif',
		'"Segoe UI", Arial, "Microsoft Yahei", sans-serif',
		'"Segoe UI", Arial, "Microsoft Jhenghei", sans-serif',
		'"Segoe UI", Arial, Meiryo, sans-serif',
		'"Segoe UI", Arial, "Malgun Gothic", Gulim, sans-serif',
		'Tahoma, sans-serif',
		'Vrinda',
		'"Segoe UI", Arial, AnjaliOldLipi, Rachana, Kartika',
	]);
	// 静态标记里的简中文案（无脚本时的兜底）必须与表里的 zh-CN 条目相同
	const [heading, p1, , s1, link, s2, button] = S['zh-CN'];
	assert.match(camoHTML, new RegExp(`<h1><span id="heading">${heading}</span></h1>`));
	assert.match(camoHTML, new RegExp(`<p id="typo">${p1}<span id="host"></span>`));
	assert.match(camoHTML, new RegExp(`<span id="s1">${s1}</span><a href="javascript:void\\(0\\)" id="diagnose-link">${link}</a><span id="s2">${s2}</span>`));
	assert.match(camoHTML, new RegExp(`<button id="reload-button">${button}</button>`));
});

await check('伪装 404 页语言选择：精确匹配 → 主语言回退（zh/pt/en）→ 英文', () => {
	// pick 依赖页面里的小写键索引 L：连同 L 一起求值
	const i18n = camoHTML.match(/var S=\{[\s\S]*?\n\};/)[0];
	const index = camoHTML.match(/var L=\{\};\nfor\(var k in S\)L\[k\.toLowerCase\(\)\]=k;/)[0];
	const pickSrc = camoHTML.match(/var pick=(function\(t\)\{[\s\S]*?\n\});/)[1];
	const pick = new Function(`${i18n}\n${index}\nreturn ${pickSrc}`)();
	// 页面先把 navigator.language 转成小写再调用 pick；表里的键是 en-GB / pt-PT 这类规范写法，
	// 必须大小写不敏感地命中，否则英式英语、葡萄牙 Portuguese 会落到别的条目
	assert.equal(pick('zh-cn'), 'zh-CN');
	assert.equal(pick('zh'), 'zh-CN');
	assert.equal(pick('zh-hans-sg'), 'zh-CN');
	assert.equal(pick('zh-tw'), 'zh-TW');
	assert.equal(pick('zh-hk'), 'zh-TW');
	assert.equal(pick('zh-hant'), 'zh-TW');
	assert.equal(pick('pt'), 'pt-BR');
	assert.equal(pick('pt-br'), 'pt-BR');
	assert.equal(pick('pt-pt'), 'pt-PT');
	assert.equal(pick('pt-ao'), 'pt-BR');
	assert.equal(pick('en'), 'en-US');
	assert.equal(pick('en-us'), 'en-US');
	assert.equal(pick('en-gb'), 'en-GB');
	assert.equal(pick('en-au'), 'en-US');
	assert.equal(pick('es'), 'es');
	assert.equal(pick('es-419'), 'es-419');
	assert.equal(pick('sv'), 'sv');
	assert.equal(pick('xx-yy'), 'en-US');
	assert.equal(pick('si-lk'), 'en-US'); // 非 Chrome 界面语言
});

await check('HOMEPAGE_HTML 导出与 index.html 一致（入口直用场景）', () => {
	assert.equal(HOMEPAGE_HTML.trim(), camoHTML);
	assert.match(HOMEPAGE_HTML, /无法访问此网站/);
});

console.log(`\n通过 ${passed} 项，失败 ${failures.length} 项`);
if (failures.length > 0) {
	process.exitCode = 1;
}
