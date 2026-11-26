/**
 * 伪装 404 页 ↔ 本机 Chrome 真实网络错误页 对照检查（零依赖；需要 Node ≥ 22）
 *
 * 回答一个问题：伪装页现在看起来还像不像「当前装的 Chrome」的 DNS 错误页？
 * Chrome 每次改版，这个页面都得跟着改；这个脚本负责量出差在哪里。
 *
 * 做法：用 DevTools 协议把本机 Chrome / Edge 启动两次，访问同一个地址 http://doh-probe.invalid/
 * （.invalid 是 RFC 2606 保留的顶级域，永远不会解析出地址）：
 *   真实侧：原样访问 → Chrome 显示自己的 DNS 错误页
 *   伪装侧：加 --host-resolver-rules 把这个域名指到本地静态服务 → 拿到伪装页
 * 两侧的地址完全相同（主机名、端口都一样），所以可以逐项比较：
 *   1. 布局：60 种视口（桌面 / 平板 / 手机竖屏横屏 / 小窗口 / 各断点两侧）下各元素的坐标与计算样式
 *   2. 像素：浅色 / 深色 × 桌面 / 手机、125% 缩放、高对比度，逐像素比对
 *   3. 文案：各界面语言下的文字、文字方向、字体栈
 *
 * 用法：node test/compare-camo-chrome.mjs [--languages zh-CN,en-US,...|all|none]
 *   --languages 缺省抽 12 种代表语言；all 检查伪装页收录的全部语言；none 跳过文案比对
 *
 * 前提：所在网络要对 .invalid 域名返回「不存在」（离线也算，DNS 解析失败同样触发错误页）。
 * 如果运营商对不存在的域名做了解析劫持，或者浏览器走了代理，真实错误页就触发不了，脚本会说明原因后退出。
 * 有差异时把两侧截图写到系统临时目录下的 cloudflare-doh-camo-compare/ 供肉眼比对，退出码为 1。
 *
 * 两条走不通的捷径（别再试）：
 *   - chrome://net-error/-105 这类内置预览页在 Chrome 157 上已不可用，会回落到新标签页
 *   - `chrome --headless --screenshot=<文件> <地址>` 在页面加载失败时直接中止，根本拍不到错误页；
 *     而且它不能在两侧用同一个地址，只能走 DevTools 协议
 *
 * 注意：坐标与色值是否「正确」不在这里判断（那是 verify-camo-shots.mjs 的事），
 * 这里只回答「和真实 Chrome 一不一样」。
 */

import { mkdir, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { findBrowser, startCamoServer, withBrowser, waitFor } from './capture-camo-shots.mjs';
import { decodePng } from './verify-camo-shots.mjs';

/** .invalid 是 RFC 2606 保留的顶级域，永远不会解析出地址 */
const PROBE_HOST = 'doh-probe.invalid';
const PROBE_URL = `http://${PROBE_HOST}/`;

/** 缺省抽样的界面语言：简繁中文、英语两种、日韩泰、德法、俄语、阿拉伯语（从右到左）、葡萄牙语 */
const SAMPLE_LANGUAGES = ['zh-CN', 'zh-TW', 'en-US', 'en-GB', 'ja', 'ko', 'de', 'fr', 'ru', 'ar', 'pt-PT', 'th'];

// [宽, 高, 是否移动端]：覆盖桌面 / 平板 / 手机竖屏 / 手机横屏 / 小窗口 / 各断点两侧
const VIEWPORTS = [
	[2560, 1440], [1920, 1080], [1280, 800], [1024, 768], [800, 600], [701, 900], [700, 900],
	[641, 960], [640, 960], [600, 800], [500, 561], [500, 560], [500, 500], [499, 550], [421, 900], [421, 300],
	[420, 900], [420, 300], [414, 896], [390, 844], [375, 900], [375, 667], [375, 650], [375, 649], [375, 600],
	[375, 500], [375, 401], [375, 400], [375, 299], [360, 640], [320, 568], [320, 480], [240, 320], [239, 320], [200, 150],
	[900, 500], [844, 390], [812, 375], [737, 420], [736, 420], [667, 375], [568, 320], [421, 239], [421, 240],
	[1280, 641], [1280, 640], [1280, 561], [1280, 560], [1280, 351], [1280, 350], [1280, 300], [1280, 240], [1280, 239], [100, 100],
	[375, 667, 1], [414, 896, 1], [360, 640, 1], [320, 568, 1], [844, 390, 1], [375, 400, 1],
];

/** 逐像素比对的七种状态 */
const SHOTS = [
	{ name: '浅色桌面', width: 1280, height: 800, dpr: 1, scheme: 'light' },
	{ name: '深色桌面', width: 1280, height: 800, dpr: 1, scheme: 'dark' },
	{ name: '浅色手机', width: 375, height: 667, dpr: 2, mobile: true, scheme: 'light' },
	{ name: '深色手机', width: 375, height: 667, dpr: 2, mobile: true, scheme: 'dark' },
	{ name: '125% 缩放', width: 1280, height: 800, dpr: 1.25, scheme: 'light' },
	{ name: '高对比度浅色', width: 1280, height: 800, dpr: 1, scheme: 'light', forcedColors: 'active' },
	{ name: '高对比度深色手机', width: 375, height: 667, dpr: 2, mobile: true, scheme: 'dark', forcedColors: 'active' },
];

/** 两边对应的元素：真实页选择器 → 伪装页选择器 */
const PAIRS = {
	内容列: ['#main-frame-error', '.interstitial'],
	图标: ['.icon', '.icon'],
	标题: ['#main-message h1', '.message h1'],
	首段: ['#main-message > p', '.message > p'],
	建议句: ['.single-suggestion', '.suggestions ul'],
	链接: ['#diagnose-link', '#diagnose-link'],
	错误码: ['.error-code', '.error-code'],
	按钮栏: ['#buttons', '.nav'],
	按钮: ['#reload-button', '#reload-button'],
};

/** 每个元素要比较的计算样式（挑会影响观感的） */
const STYLE_KEYS = {
	内容列: ['margin', 'padding', 'fontSize', 'lineHeight'],
	图标: ['margin'],
	标题: ['fontSize', 'fontWeight', 'lineHeight', 'color', 'margin'],
	首段: ['fontSize', 'lineHeight', 'color'],
	建议句: ['marginBottom'], // margin-top 两侧实现方式不同（真实页多一个空 <p>）但位置等价，不比
	链接: ['color', 'fontSize'],
	错误码: ['fontSize', 'color'],
	按钮栏: ['position', 'margin', 'padding'],
	按钮: ['fontSize', 'fontWeight', 'fontFamily', 'color', 'bg', 'padding', 'radius'],
};

const LAYOUT_SCRIPT = `(() => {
	const sel = ${JSON.stringify(PAIRS)};
	const keys = ${JSON.stringify(STYLE_KEYS)};
	const out = {};
	for (const [name, [real, camo]] of Object.entries(sel)) {
		const el = document.querySelector(real) || document.querySelector(camo);
		if (!el) { out[name] = null; continue; }
		const r = el.getBoundingClientRect(), cs = getComputedStyle(el);
		const style = { margin: cs.margin, padding: cs.padding, fontSize: cs.fontSize, fontWeight: cs.fontWeight,
			lineHeight: cs.lineHeight, color: cs.color, bg: cs.backgroundColor, position: cs.position,
			radius: cs.borderRadius, fontFamily: cs.fontFamily, marginBottom: cs.marginBottom };
		const picked = {};
		for (const k of (keys[name] || [])) picked[k] = style[k];
		out[name] = { x: r.x, y: r.y, w: r.width, h: r.height,
			text: (el.textContent || '').replace(/\\s+/g, ' ').trim(), ...picked };
	}
	out.body = { font: getComputedStyle(document.body).fontFamily, fontSize: getComputedStyle(document.body).fontSize,
		margin: getComputedStyle(document.body).margin, color: getComputedStyle(document.body).color, bg: getComputedStyle(document.body).backgroundColor };
	out.html = { lang: document.documentElement.lang, dir: document.documentElement.dir, fontSize: getComputedStyle(document.documentElement).fontSize };
	out.title = document.title;
	return out;
})()`;

/** 某个界面语言下页面上所有可见文字 */
const TEXT_SCRIPT = `(() => {
	const q = (s) => document.querySelector(s);
	const norm = (s) => (s || '').replace(/\\s+/g, ' ').trim();
	return {
		title: document.title,
		lang: document.documentElement.lang,
		dir: document.documentElement.dir,
		h1: norm((q('h1') || {}).textContent),
		p: norm((q('#main-message > p, #typo') || {}).textContent),
		s: norm((q('li') || {}).textContent),
		code: norm((q('.error-code') || {}).textContent),
		button: norm((q('#reload-button') || {}).textContent),
		link: norm((q('#diagnose-link') || {}).textContent),
		font: getComputedStyle(document.body).fontFamily,
		linkColor: getComputedStyle(q('#diagnose-link')).color,
	};
})()`;

/** 就绪条件（语言无关）：加载完成、有「重新加载」按钮、首段出现主机名、错误码不再是「正在诊断」 */
const READY = `(() => {
	const code = (document.querySelector('.error-code') || {}).textContent || '';
	const p = document.querySelector('#main-message > p, #typo');
	return document.readyState === 'complete' && !!document.querySelector('#reload-button') &&
		/doh-probe/.test((p || {}).textContent || '') && !/STARTED/.test(code);
})()`;

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const rectTolerance = 0.6;

/** 打开一侧并量取数据 */
async function measure({ side, lang, html, full }) {
	let server = null;
	const args = [];
	if (side === 'camo') {
		server = await startCamoServer(html);
		// 把探测域名指到本地静态服务：与真实侧的地址一模一样，只是「解析」结果不同
		args.push(`--host-resolver-rules=MAP ${PROBE_HOST} 127.0.0.1:${server.port}`);
	}
	try {
		return await withBrowser({ lang, args }, async (tab) => {
			await tab.emulate({ width: 1280, height: 800, dpr: 1 });
			await tab.cmd('Page.navigate', { url: PROBE_URL });
			try {
				await waitFor(tab.evaluate, READY, { label: `${side === 'real' ? '真实错误页' : '伪装页'}` });
			} catch (error) {
				if (side === 'real') {
					throw new Error(
						`无法触发 Chrome 的真实 DNS 错误页（${error.message}）。\n` +
							'  常见原因：所在网络对不存在的域名做了解析劫持，或浏览器配置了代理；\n' +
							'  真实页触发不了时无从对照，请换一个网络环境再试。'
					);
				}
				throw error;
			}
			const code = await tab.evaluate("(document.querySelector('.error-code') || {}).textContent || ''");
			if (side === 'real' && !/FINISHED_NXDOMAIN/.test(code)) {
				throw new Error(`Chrome 显示的不是「域名不存在」错误页，而是 ${code || '未知页面'}；无从对照`);
			}
			await sleep(150);

			const result = { product: tab.product, text: await tab.evaluate(TEXT_SCRIPT) };
			if (!full) return result;

			result.layout = [];
			for (const [width, height, mobile] of VIEWPORTS) {
				await tab.emulate({ width, height, mobile: !!mobile, dpr: mobile ? 2 : 1 });
				await sleep(120);
				result.layout.push({ viewport: `${width}x${height}${mobile ? 'm' : ''}`, ...(await tab.evaluate(LAYOUT_SCRIPT)) });
			}
			result.shots = {};
			for (const s of SHOTS) {
				await tab.emulate(s);
				await sleep(250);
				result.shots[s.name] = await tab.screenshot();
			}
			if (tab.pageErrors.length) throw new Error(`${side} 侧页面脚本有未捕获的异常：\n  ${tab.pageErrors.join('\n  ')}`);
			return result;
		});
	} finally {
		if (server) server.close();
	}
}

/** 逐像素比较两张尺寸相同的截图 */
function diffPixels(real, camo) {
	let diff = 0, maxDelta = 0;
	const bbox = { minX: Infinity, minY: Infinity, maxX: -1, maxY: -1 };
	for (let y = 0; y < real.height; y++) {
		for (let x = 0; x < real.width; x++) {
			const a = real.get(x, y), b = camo.get(x, y);
			const delta = Math.max(Math.abs(a[0] - b[0]), Math.abs(a[1] - b[1]), Math.abs(a[2] - b[2]));
			if (delta > 0) {
				diff++;
				if (delta > maxDelta) maxDelta = delta;
				if (x < bbox.minX) bbox.minX = x;
				if (x > bbox.maxX) bbox.maxX = x;
				if (y < bbox.minY) bbox.minY = y;
				if (y > bbox.maxY) bbox.maxY = y;
			}
		}
	}
	return { diff, total: real.width * real.height, maxDelta, bbox };
}

function compareLayout(realLayout, camoLayout) {
	const diffs = [];
	realLayout.forEach((real, i) => {
		const camo = camoLayout[i];
		const where = real.viewport;
		if (!camo || camo.viewport !== where) {
			diffs.push({ where, what: '视口列表不一致' });
			return;
		}
		const compare = (name, a, b, keys) => {
			if (!a || !b) {
				diffs.push({ where, what: `${name}：${a ? '伪装页缺少' : '真实页没有'}这个元素` });
				return;
			}
			for (const k of ['x', 'y', 'w', 'h']) {
				if (Math.abs(a[k] - b[k]) > rectTolerance) diffs.push({ where, what: `${name}.${k}  ${round(a[k])} → ${round(b[k])}` });
			}
			for (const k of ['text', ...keys]) {
				if (JSON.stringify(a[k]) !== JSON.stringify(b[k])) diffs.push({ where, what: `${name}.${k}  ${a[k]} → ${b[k]}` });
			}
		};
		for (const [name, keys] of Object.entries(STYLE_KEYS)) compare(name, real[name], camo[name], keys);
		compare('body', real.body, camo.body, ['font', 'fontSize', 'margin', 'color', 'bg']);
		compare('html', real.html, camo.html, ['lang', 'dir', 'fontSize']);
		if (real.title !== camo.title) diffs.push({ where, what: `title  ${real.title} → ${camo.title}` });
	});
	return diffs;
}

function compareText(real, camo) {
	const diffs = [];
	for (const k of Object.keys(real)) {
		if (JSON.stringify(real[k]) !== JSON.stringify(camo[k])) diffs.push(`${k}：「${real[k]}」→「${camo[k]}」`);
	}
	return diffs;
}

const round = (v) => (typeof v === 'number' ? v.toFixed(2) : v);

/** 从伪装页的多语言表里读出它收录的全部界面语言 */
export function languagesOf(html) {
	return [...html.matchAll(/^\t"([a-zA-Z-]+)":\[/gm)].map((m) => m[1]);
}

/**
 * 执行对照
 * @param {{html?: string, languages?: string[], browser?: string}} [opts]
 * @returns {Promise<{product: string, layout: {total: number, diffs: object[]}, pixels: {name: string, diff: number, total: number, bbox: object, sizeMismatch: boolean}[], texts: {lang: string, diffs: string[]}[], shotDir: string}>}
 */
export async function compareCamoWithChrome({ html, languages = SAMPLE_LANGUAGES, browser = findBrowser() } = {}) {
	const opts = { browser };
	const real = await measure({ side: 'real', html, full: true, ...opts });
	const camo = await measure({ side: 'camo', html, full: true, ...opts });

	const layoutDiffs = compareLayout(real.layout, camo.layout);
	const pixels = [];
	const shotDir = path.join(tmpdir(), 'cloudflare-doh-camo-compare');
	for (const s of SHOTS) {
		const a = decodePng(real.shots[s.name]), b = decodePng(camo.shots[s.name]);
		if (a.width !== b.width || a.height !== b.height) {
			pixels.push({ name: s.name, sizeMismatch: true, realSize: `${a.width}x${a.height}`, camoSize: `${b.width}x${b.height}` });
			continue;
		}
		const d = diffPixels(a, b);
		pixels.push({ name: s.name, ...d });
		if (d.diff > 0) await saveShot(shotDir, s.name, real.shots[s.name], camo.shots[s.name]);
	}

	const texts = [];
	for (const lang of languages) {
		const realText = await measure({ side: 'real', lang, ...opts });
		const camoText = await measure({ side: 'camo', lang, html, ...opts });
		const diffs = compareText(realText.text, camoText.text);
		if (diffs.length > 0) texts.push({ lang, diffs });
	}
	return { product: real.product, layout: { total: VIEWPORTS.length, diffs: layoutDiffs }, pixels, texts, textsTotal: languages.length, shotDir };
}

async function saveShot(dir, name, realPng, camoPng) {
	await mkdir(dir, { recursive: true });
	await writeFile(path.join(dir, `${name}-real.png`), realPng);
	await writeFile(path.join(dir, `${name}-camo.png`), camoPng);
}

/** 打印报告；返回是否存在差异 */
export function printReport(result) {
	const { product, layout, pixels, texts } = result;
	const badShots = pixels.filter((p) => p.sizeMismatch || p.diff > 0);
	console.log(`${product} ↔ 伪装页\n`);
	console.log(`布局  ${layout.total - layout.diffs.length} / ${layout.total} 种视口一致`);
	for (const d of layout.diffs.slice(0, 40)) console.log(`  ✘ ${d.where}  ${d.what}`);
	if (layout.diffs.length > 40) console.log(`  ……另有 ${layout.diffs.length - 40} 处`);

	console.log(`像素  ${pixels.length - badShots.length} / ${pixels.length} 张逐像素一致`);
	for (const p of badShots) {
		if (p.sizeMismatch) console.log(`  ✘ ${p.name}：截图尺寸不同 ${p.realSize} → ${p.camoSize}`);
		else {
			const pct = ((p.diff / p.total) * 100).toFixed(3);
			console.log(`  ✘ ${p.name}：差异像素 ${p.diff}（${p}%），范围 x[${p.bbox.minX},${p.bbox.maxX}] y[${p.bbox.minY},${p.bbox.maxY}]`);
		}
	}

	if (texts.length === 0 && result.textsTotal > 0) console.log(`文案  ${result.textsTotal} / ${result.textsTotal} 种语言一致`);
	else if (result.textsTotal > 0) {
		console.log(`文案  ${result.textsTotal - texts.length} / ${result.textsTotal} 种语言一致`);
		for (const t of texts) for (const d of t.diffs) console.log(`  ✘ ${t.lang}  ${d}`);
	}

	const clean = layout.diffs.length === 0 && badShots.length === 0 && texts.length === 0;
	if (clean) {
		console.log('\n伪装页与本机 Chrome 的 DNS 错误页没有可测出的差异');
	} else {
		console.log(`\n有差异：Chrome 可能改版了，或者伪装页被改坏了。有差异的截图已存到\n  ${result.shotDir}\n  （*-real.png 是 Chrome 的真实页面，*-camo.png 是伪装页）`);
	}
	return !clean;
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
	const argv = process.argv.slice(2);
	const arg = (name) => (argv.includes(name) ? argv[argv.indexOf(name) + 1] : undefined);
	if (argv.includes('--help') || argv.includes('-h')) {
		console.log('用法：node test/compare-camo-chrome.mjs [--languages zh-CN,en-US,...|all|none]');
		process.exit(0);
	}
	let html;
	const langArg = arg('--languages');
	let languages = SAMPLE_LANGUAGES;
	if (langArg === 'none') {
		languages = [];
	} else if (langArg === 'all') {
		// 检查伪装页自己收录的全部语言：先把页面取出来读它的多语言表
		const probe = await startCamoServer();
		html = probe.html;
		probe.close();
		languages = languagesOf(html);
	} else if (langArg) {
		languages = langArg.split(',').map((s) => s.trim()).filter(Boolean);
	}
	try {
		const result = await compareCamoWithChrome({ languages, html });
		process.exitCode = printReport(result) ? 1 : 0;
	} catch (error) {
		console.error(`✘ ${error.message}`);
		process.exitCode = 2;
	}
}
