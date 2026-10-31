/**
 * 零依赖冒烟测试
 *
 * 直接运行：node test/smoke.mjs
 * 覆盖：路径映射解析、转发目标拼接、请求头剥离、POST 请求体透传、
 *       最长前缀匹配、DOMAIN_MAPPINGS 覆盖、两个平台入口的基本可用性。
 * 说明：不发起真实外网请求，统一用注入的 fetchImpl 桩函数断言转发参数。
 */

import assert from 'node:assert/strict';
import { proxyRequest, resolveTarget, getPathMappings, DEFAULT_PATH_MAPPINGS } from '../src/core.js';
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

await check('伪装 404 带 no-store', async () => {
	const response = await proxyRequest(new Request('https://x.dev/unknown'), { fetchImpl: () => { throw new Error('不应发起请求'); } });
	assert.equal(response.status, 404);
	assert.equal(response.headers.get('cache-control'), 'no-store');
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

console.log(`\n通过 ${passed} 项，失败 ${failures.length} 项`);
if (failures.length > 0) {
	process.exitCode = 1;
}
