/**
 * DoH 代理自检脚本（真实二进制 DoH 查询：POST + application/dns-message）
 *
 * 用法示例：
 *   node scripts/check-doh.mjs --host dohcn.example.com --ip 203.0.113.20 --insecure
 *   node scripts/check-doh.mjs --host my-app.edgeone.dev --cookie "eo_token=xxx; eo_time=123"
 *   node scripts/check-doh.mjs --host dohcn.example.com --ip 203.0.113.20 --insecure --providers dnspod,alidns
 *
 * 参数：
 *   --host       必填，代理域名（同时作为 SNI 与 Host 头）
 *   --ip         可选，直连 IP（本地 DNS 解析不到该域名时用，例如证书未生效的自定义域名）
 *   --insecure   可选，跳过证书校验（证书未签发时用于排障）
 *   --cookie     可选，预览网关的 eo_token cookie（*.edgeone.cool / *.edgeone.dev 需要）
 *   --providers  可选，逗号分隔，默认 google,cloudflare,quad9,adguard,dnspod,alidns
 *   --domains    可选，逗号分隔，默认 qq.com,test2.example.com
 */

import https from 'node:https';

const ALL_PROVIDERS = ['google', 'cloudflare', 'quad9', 'adguard', 'dnspod', 'alidns'];

function parseArgs(argv) {
	const args = {};
	for (let i = 0; i < argv.length; i++) {
		const key = argv[i];
		if (!key.startsWith('--')) continue;
		const name = key.slice(2);
		if (name === 'insecure') {
			args.insecure = true;
		} else {
			args[name] = argv[++i];
		}
	}
	return args;
}

const args = parseArgs(process.argv.slice(2));
if (!args.host) {
	console.error('缺少 --host，例如：node scripts/check-doh.mjs --host doh.example.com');
	process.exit(2);
}

const providers = (args.providers || ALL_PROVIDERS.join(',')).split(',').filter(Boolean);
const domains = (args.domains || 'qq.com,test2.example.com').split(',').filter(Boolean);

function buildQuery(name, type = 1, id = 0x1a2b) {
	const labels = name.split('.');
	const size = 12 + labels.reduce((n, l) => n + 1 + Buffer.byteLength(l), 0) + 1 + 4;
	const buf = Buffer.alloc(size);
	buf.writeUInt16BE(id, 0);
	buf.writeUInt16BE(0x0100, 2); // RD=1
	buf.writeUInt16BE(1, 4); // QDCOUNT
	let o = 12;
	for (const l of labels) {
		buf[o++] = Buffer.byteLength(l);
		buf.write(l, o);
		o += Buffer.byteLength(l);
	}
	buf[o++] = 0;
	buf.writeUInt16BE(type, o);
	buf.writeUInt16BE(1, o + 2);
	return buf;
}

/** 读取域名（支持压缩指针），返回 [域名, 下一个字节偏移] */
function readName(buf, off) {
	const parts = [];
	let pos = off;
	let next = off;
	let jumped = false;
	for (let guard = 0; guard < 128; guard++) {
		const len = buf[pos];
		if (len === undefined) break;
		if (len === 0) {
			if (!jumped) next = pos + 1;
			break;
		}
		if ((len & 0xc0) === 0xc0) {
			const ptr = ((len & 0x3f) << 8) | buf[pos + 1];
			if (!jumped) {
				next = pos + 2;
				jumped = true;
			}
			pos = ptr;
			continue;
		}
		parts.push(buf.toString('ascii', pos + 1, pos + 1 + len));
		pos += 1 + len;
		if (!jumped) next = pos;
	}
	return [parts.join('.'), next];
}

function parseResponse(buf) {
	if (buf.length < 12) return { error: `响应过短（${buf.length}B）` };
	const flags = buf.readUInt16BE(2);
	const qd = buf.readUInt16BE(4);
	const an = buf.readUInt16BE(6);
	let o = 12;
	for (let i = 0; i < qd; i++) {
		const [, next] = readName(buf, o);
		o = next + 4;
	}
	const answers = [];
	for (let i = 0; i < an; i++) {
		const [name, next] = readName(buf, o);
		const type = buf.readUInt16BE(next);
		const ttl = buf.readUInt32BE(next + 4);
		const rdlen = buf.readUInt16BE(next + 8);
		const rd = next + 10;
		let data;
		if (type === 1) data = `${buf[rd]}.${buf[rd + 1]}.${buf[rd + 2]}.${buf[rd + 3]}`;
		else if (type === 5) data = readName(buf, rd)[0];
		else if (type === 28) {
			const seg = [];
			for (let k = 0; k < 8; k++) seg.push(buf.readUInt16BE(rd + k * 2).toString(16));
			data = seg.join(':');
		} else data = `<type ${type}>`;
		answers.push({ name, type, ttl, data });
		o = rd + rdlen;
	}
	return { rcode: flags & 0xf, answers };
}

function post(provider, body) {
	return new Promise((resolve) => {
		const req = https.request(
			{
				host: args.ip || args.host,
				servername: args.host,
				path: `/${provider}/query-dns`,
				method: 'POST',
				headers: {
					Host: args.host,
					'content-type': 'application/dns-message',
					accept: 'application/dns-message',
					'content-length': body.length,
					...(args.cookie ? { cookie: args.cookie } : {}),
				},
				agent: new https.Agent({ rejectUnauthorized: !args.insecure, keepAlive: false }),
				timeout: 12000,
			},
			(res) => {
				const chunks = [];
				res.on('data', (c) => chunks.push(c));
				res.on('end', () => resolve({ status: res.statusCode, body: Buffer.concat(chunks) }));
			}
		);
		req.on('timeout', () => req.destroy(new Error('客户端 12s 超时')));
		req.on('error', (e) => resolve({ status: 0, error: e.message }));
		req.end(body);
	});
}

console.log(`\n目标：${args.host}${args.ip ? ` (直连 ${args.ip})` : ''}${args.insecure ? ' [跳过证书校验]' : ''}\n`);

const rows = [];
for (const provider of providers) {
	for (const domain of domains) {
		const started = Date.now();
		const res = await post(provider, buildQuery(domain));
		const ms = Date.now() - started;
		let verdict = '❌';
		let detail = res.error || `HTTP ${res.status}`;
		if (res.status === 200) {
			const parsed = parseResponse(res.body);
			const a = (parsed.answers || []).filter((x) => x.type === 1);
			if (parsed.error) {
				detail = parsed.error;
			} else if (parsed.rcode === 0 && a.length > 0) {
				verdict = '✅';
				detail = a.map((x) => x.data).join(', ');
			} else if (parsed.rcode === 0) {
				verdict = '⚠️';
				detail = 'rcode=0 但无 A 记录';
			} else {
				verdict = '⚠️';
				detail = `rcode=${parsed.rcode}`;
			}
		}
		rows.push({ provider, domain, verdict });
		console.log(`${provider.padEnd(11)} ${domain.padEnd(11)} ${verdict} ${String(ms).padStart(5)}ms  ${detail.slice(0, 80)}`);
	}
}

console.log('\n=== 汇总');
for (const provider of providers) {
	const hit = rows.filter((r) => r.provider === provider && r.verdict === '✅').length;
	console.log(`${provider.padEnd(11)} ${hit}/${domains.length} 通过`);
}

if (rows.every((r) => r.verdict !== '✅')) process.exitCode = 1;
