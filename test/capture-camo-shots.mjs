/**
 * 伪装 404 页的浏览器工具（零依赖；需要 Node ≥ 22，用到内置 WebSocket）
 *
 * 本文件提供三样东西，test/verify-camo-shots.mjs 与 test/compare-camo-chrome.mjs 共用：
 *   - findBrowser()        查找本机 Chrome / Edge
 *   - startCamoServer()    内置静态服务：对任意路径返回伪装页（状态码 404，响应头与代理的伪装应答一致）
 *   - withBrowser()        用 DevTools 协议启动无头浏览器并建立一个标签页
 * 以及 captureCamoShots()：拍伪装页四态截图（桌面 1280x800@1x、手机 375x667@2x × 浅色 / 深色）。
 *
 * 为什么不用 `--window-size=375,667 --screenshot`：那是窗口外框尺寸。Windows 上窗口最小宽度
 * 约 500px，高度还要扣掉标题栏，拍到的既不是 375px 宽也不是手机布局（旧做法会让压扁的图标照样通过）。
 *
 * 用法：node test/capture-camo-shots.mjs <输出目录> [页面地址]
 *   页面地址缺省时由 startCamoServer() 提供页面，不依赖先编译 / 启动 Go 服务。缺省提供的页面取自
 *   src/core.js 里**求值后**的 HOMEPAGE_HTML——也就是 Worker / EdgeOne 实际发出去的那一份：它嵌在
 *   模板字符串里，源码逐字一致不代表求值结果一致（反斜杠会被当成转义序列吃掉），必须在真实浏览器里跑一遍。
 *   也可以指向运行中的 Go 代理或线上域名。
 *   浏览器自动查找 Chrome / Edge；找不到时用环境变量 CAMO_BROWSER 指定可执行文件。
 */

import { spawn } from 'node:child_process';
import { existsSync } from 'node:fs';
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { createServer } from 'node:http';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');

/** 四态：桌面 1x、手机 2x（触摸 + viewport meta 生效） */
export const SHOT_STATES = [
	{ name: 'light', scheme: 'light', width: 1280, height: 800, dpr: 1, mobile: false },
	{ name: 'dark', scheme: 'dark', width: 1280, height: 800, dpr: 1, mobile: false },
	{ name: 'phone-light', scheme: 'light', width: 375, height: 667, dpr: 2, mobile: true },
	{ name: 'phone-dark', scheme: 'dark', width: 375, height: 667, dpr: 2, mobile: true },
];

/** 查找本机可用的 Chromium 系浏览器 */
export function findBrowser() {
	if (process.env.CAMO_BROWSER) return process.env.CAMO_BROWSER;
	const pf = process.env.PROGRAMFILES;
	const pf86 = process.env['PROGRAMFILES(X86)'];
	const candidates = [
		pf && path.join(pf, 'Google', 'Chrome', 'Application', 'chrome.exe'),
		pf86 && path.join(pf86, 'Google', 'Chrome', 'Application', 'chrome.exe'),
		pf86 && path.join(pf86, 'Microsoft', 'Edge', 'Application', 'msedge.exe'),
		pf && path.join(pf, 'Microsoft', 'Edge', 'Application', 'msedge.exe'),
		'/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
		'/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge',
		'/usr/bin/google-chrome',
		'/usr/bin/google-chrome-stable',
		'/usr/bin/chromium',
		'/usr/bin/chromium-browser',
	];
	return candidates.find((p) => p && existsSync(p)) || '';
}

/**
 * 启动内置静态服务：任意路径都回伪装页，状态码与响应头对齐代理的 camo 应答
 * @param {string} [html] 要返回的页面，缺省为 src/core.js 里求值后的 HOMEPAGE_HTML
 * @returns {Promise<{port: number, html: string, close: () => void}>}
 */
export async function startCamoServer(html) {
	if (html === undefined) {
		({ HOMEPAGE_HTML: html } = await import(pathToFileURL(path.join(root, 'src', 'core.js')).href));
	}
	const server = createServer((_req, res) => {
		res.writeHead(404, { 'Content-Type': 'text/html; charset=utf-8', 'Cache-Control': 'no-store', 'X-Content-Type-Options': 'nosniff' });
		res.end(html);
	});
	await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
	return {
		port: server.address().port,
		html,
		// keep-alive 连接不先断掉的话 close() 会一直等，进程不退出
		close: () => {
			server.closeAllConnections();
			server.close();
		},
	};
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/**
 * 用 DevTools 协议启动无头浏览器，建立一个标签页并交给 fn 使用；fn 返回后（或抛错后）收拾干净
 * @param {{lang?: string, args?: string[], browser?: string}} [opts]
 * @param {(tab: object) => Promise<T>} fn
 * @returns {Promise<T>}
 */
export async function withBrowser({ lang = 'zh-CN', args = [], browser = findBrowser() } = {}, fn) {
	if (!browser) throw new Error('未找到 Chrome / Edge，请用环境变量 CAMO_BROWSER 指定浏览器可执行文件');

	const profile = await mkdtemp(path.join(tmpdir(), 'doh-camo-browser-'));
	const extra = [...args];
	if (process.getuid?.() === 0) extra.push('--no-sandbox'); // Linux 上以 root 运行（容器 / CI）时必需
	const child = spawn(
		browser,
		[
			'--headless=new',
			'--disable-gpu',
			'--no-first-run',
			'--no-default-browser-check',
			'--disable-extensions',
			'--remote-debugging-port=0',
			`--user-data-dir=${profile}`,
			`--lang=${lang}`,
			...extra,
			'about:blank',
		],
		{ stdio: ['ignore', 'ignore', 'pipe'] },
	);

	const wsUrl = await new Promise((resolve, reject) => {
		let buf = '';
		const timer = setTimeout(() => reject(new Error('等待浏览器调试端口超时')), 30000);
		child.stderr.on('data', (chunk) => {
			buf += chunk.toString();
			const m = buf.match(/DevTools listening on (ws:\/\/\S+)/);
			if (m) {
				clearTimeout(timer);
				resolve(m[1]);
			}
		});
		child.on('exit', (code) => {
			clearTimeout(timer);
			reject(new Error(`浏览器提前退出（code=${code}）`));
		});
	});

	const ws = new WebSocket(wsUrl);
	const pending = new Map();
	const pageErrors = []; // Runtime.exceptionThrown：页面脚本未捕获的异常
	let seq = 0;
	ws.addEventListener('message', (event) => {
		const msg = JSON.parse(event.data);
		if (msg.id && pending.has(msg.id)) {
			const { resolve, reject } = pending.get(msg.id);
			pending.delete(msg.id);
			if (msg.error) reject(new Error(`${msg.error.message}（浏览器不支持该 DevTools 指令）`));
			else resolve(msg.result);
		} else if (msg.method === 'Runtime.exceptionThrown') {
			const d = msg.params.exceptionDetails;
			pageErrors.push(`${d.text} ${d.exception?.description || ''}（行 ${d.lineNumber + 1}）`);
		}
	});
	const send = (method, params = {}, sessionId) =>
		new Promise((resolve, reject) => {
			const id = ++seq;
			const timer = setTimeout(() => {
				pending.delete(id);
				reject(new Error(`DevTools 指令 ${method} 超时`));
			}, 30000);
			pending.set(id, {
				resolve: (v) => {
					clearTimeout(timer);
					resolve(v);
				},
				reject: (e) => {
					clearTimeout(timer);
					reject(e);
				},
			});
			ws.send(JSON.stringify({ id, method, params, ...(sessionId ? { sessionId } : {}) }));
		});
	await new Promise((resolve, reject) => {
		ws.addEventListener('open', resolve, { once: true });
		ws.addEventListener('error', () => reject(new Error('连接浏览器调试端口失败')), { once: true });
	});

	try {
		const { targetId } = await send('Target.createTarget', { url: 'about:blank' });
		const { sessionId } = await send('Target.attachToTarget', { targetId, flatten: true });
		const cmd = (method, params) => send(method, params, sessionId);
		await cmd('Page.enable');
		await cmd('Runtime.enable');
		const evaluate = async (expression) => {
			const r = await cmd('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true });
			if (r.exceptionDetails) throw new Error(r.exceptionDetails.text + ' ' + (r.exceptionDetails.exception?.description || ''));
			return r.result.value;
		};
		/**
		 * 模拟视口 / 像素密度 / 触摸 / 深浅色 / 强制色。
		 * 给了视口就一并把深浅色与强制色复位成缺省值，避免沿用上一次模拟留下的状态
		 * （本机系统主题若是深色，不复位的话「浅色」截图会拍到深色页面）
		 */
		const emulate = async ({ width, height, dpr = 1, mobile = false, scheme = 'light', forcedColors = 'none' }) => {
			if (width) {
				await cmd('Emulation.setDeviceMetricsOverride', { width, height, deviceScaleFactor: dpr, mobile, screenWidth: width, screenHeight: height });
				await cmd('Emulation.setTouchEmulationEnabled', { enabled: mobile });
				await cmd('Emulation.setEmulatedMedia', {
					media: 'screen',
					features: [
						{ name: 'prefers-color-scheme', value: scheme },
						{ name: 'forced-colors', value: forcedColors },
					],
				});
			}
		};
		const tab = {
			browser,
			sessionId,
			product: (await send('Browser.getVersion')).product, // 形如 Chrome/157.0.8103.79
			pageErrors,
			cmd,
			evaluate,
			emulate,
			screenshot: async () => Buffer.from((await cmd('Page.captureScreenshot', { format: 'png' })).data, 'base64'),
		};
		return await fn(tab);
	} finally {
		ws.close();
		child.kill();
		await sleep(300);
		await rm(profile, { recursive: true, force: true });
	}
}

/**
 * 轮询到 expression 求值为 true 为止
 * @param {(expression: string) => Promise<unknown>} evaluate
 * @param {string} expression
 * @param {{timeout?: number, label?: string}} [opts]
 */
export async function waitFor(evaluate, expression, { timeout = 15000, label = '页面' } = {}) {
	const deadline = Date.now() + timeout;
	while (Date.now() < deadline) {
		if ((await evaluate(expression).catch(() => false)) === true) return;
		await sleep(100);
	}
	throw new Error(`${label}未在 ${timeout / 1000} 秒内就绪`);
}

/** 伪装页的就绪条件：内嵌脚本已执行（首段文字里出现主机名）且页面加载完成 */
export const CAMO_READY = "(() => { const p = document.getElementById('typo'); return document.readyState === 'complete' && !!p && p.textContent.indexOf(location.hostname || '404') >= 0; })()";

/**
 * 采集四态截图
 * @param {string} outDir 输出目录
 * @param {string} [pageUrl] 页面地址；缺省时用内置静态服务
 * @returns {Promise<{browser: string, url: string, files: string[]}>}
 */
export async function captureCamoShots(outDir, pageUrl) {
	let server = null;
	let url = pageUrl;
	if (!url) {
		server = await startCamoServer();
		url = `http://127.0.0.1:${server.port}/`;
	}
	await mkdir(outDir, { recursive: true });

	try {
		return await withBrowser({ args: ['--window-size=1280,800'] }, async (tab) => {
			const { emulate, cmd, evaluate, screenshot } = tab;
			const files = [];
			for (const s of SHOT_STATES) {
				await emulate(s);
				await cmd('Page.navigate', { url });
				await waitFor(evaluate, CAMO_READY, { label: `${s.name} 态的伪装页` });
				await sleep(300); // 留一点时间让布局与绘制稳定
				const file = path.join(outDir, `${s.name}.png`);
				await writeFile(file, await screenshot());
				files.push(file);
			}
			// 伪装页不该有任何脚本报错：有异常就说明页面本身有问题
			if (tab.pageErrors.length) throw new Error(`页面脚本有未捕获的异常：\n  ${tab.pageErrors.join('\n  ')}`);
			return { browser: tab.browser, url, files };
		});
	} finally {
		if (server) server.close();
	}
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
	const [outDir, pageUrl] = process.argv.slice(2);
	if (!outDir) {
		console.error('用法：node test/capture-camo-shots.mjs <输出目录> [页面地址]');
		process.exit(2);
	}
	const { browser, url, files } = await captureCamoShots(outDir, pageUrl);
	console.log(`浏览器：${browser}\n页面：${url}`);
	for (const f of files) console.log(`  ${f}`);
}
