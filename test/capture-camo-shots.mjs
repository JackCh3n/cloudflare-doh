/**
 * 伪装 404 页截图采集（零依赖；需要 Node ≥ 22，用到内置 WebSocket）
 *
 * 用无头 Chrome/Edge + DevTools 协议的「设备模拟」拍四态截图：
 *   桌面 1280x800@1x、手机 375x667@2x（触摸、viewport meta 生效）× 浅色 / 深色
 *
 * 为什么不用 `--window-size=375,667 --screenshot`：那是窗口外框尺寸。Windows 上窗口最小宽度
 * 约 500px，高度还要扣掉标题栏，拍到的既不是 375px 宽也不是手机布局（旧做法会让压扁的图标照样通过）。
 *
 * 用法：node test/capture-camo-shots.mjs <输出目录> [页面地址]
 *   页面地址缺省时，脚本自己起一个本地静态服务：对任意路径返回伪装页（状态码 404，响应头与代理的
 *   伪装应答一致），因此不依赖先编译 / 启动 Go 服务。缺省提供的页面取自 src/core.js 里**求值后**的
 *   HOMEPAGE_HTML——也就是 Worker / EdgeOne 实际发出去的那一份：它嵌在模板字符串里，
 *   源码逐字一致不代表求值结果一致（反斜杠会被当成转义序列吃掉），必须在真实浏览器里跑一遍。
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
	const candidates = [
		process.env.PROGRAMFILES && path.join(process.env.PROGRAMFILES, 'Google', 'Chrome', 'Application', 'chrome.exe'),
		process.env['PROGRAMFILES(X86)'] && path.join(process.env['PROGRAMFILES(X86)'], 'Google', 'Chrome', 'Application', 'chrome.exe'),
		process.env['PROGRAMFILES(X86)'] && path.join(process.env['PROGRAMFILES(X86)'], 'Microsoft', 'Edge', 'Application', 'msedge.exe'),
		process.env.PROGRAMFILES && path.join(process.env.PROGRAMFILES, 'Microsoft', 'Edge', 'Application', 'msedge.exe'),
		'/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
		'/usr/bin/google-chrome',
		'/usr/bin/google-chrome-stable',
		'/usr/bin/chromium',
		'/usr/bin/chromium-browser',
	];
	return candidates.find((p) => p && existsSync(p)) || '';
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/**
 * 采集四态截图
 * @param {string} outDir 输出目录
 * @param {string} [pageUrl] 页面地址；缺省时用内置静态服务提供 index.html
 * @returns {Promise<{browser: string, url: string, files: string[]}>}
 */
export async function captureCamoShots(outDir, pageUrl) {
	const browser = findBrowser();
	if (!browser) throw new Error('未找到 Chrome / Edge，请用环境变量 CAMO_BROWSER 指定浏览器可执行文件');

	// 内置静态服务：任意路径都回伪装页，状态码与响应头对齐代理的 camo 应答
	let server = null;
	let url = pageUrl;
	if (!url) {
		const { HOMEPAGE_HTML } = await import(pathToFileURL(path.join(root, 'src', 'core.js')).href);
		const html = HOMEPAGE_HTML;
		server = createServer((_req, res) => {
			res.writeHead(404, { 'Content-Type': 'text/html; charset=utf-8', 'Cache-Control': 'no-store', 'X-Content-Type-Options': 'nosniff' });
			res.end(html);
		});
		await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
		url = `http://127.0.0.1:${server.address().port}/`;
	}
	await mkdir(outDir, { recursive: true });

	const profile = await mkdtemp(path.join(tmpdir(), 'doh-camo-shots-'));
	const args = [
		'--headless=new',
		'--disable-gpu',
		'--no-first-run',
		'--no-default-browser-check',
		'--disable-extensions',
		'--remote-debugging-port=0',
		`--user-data-dir=${profile}`,
		'--lang=zh-CN',
	];
	if (process.getuid?.() === 0) args.push('--no-sandbox'); // Linux 上以 root 运行（容器 / CI）时必需
	const child = spawn(browser, [...args, 'about:blank'], { stdio: ['ignore', 'ignore', 'pipe'] });

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
			if (msg.error) reject(new Error(`${msg.error.message}（${browser} 不支持该 DevTools 指令）`));
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

	const files = [];
	try {
		for (const s of SHOT_STATES) {
			const { targetId } = await send('Target.createTarget', { url: 'about:blank' });
			const { sessionId } = await send('Target.attachToTarget', { targetId, flatten: true });
			const cmd = (method, params) => send(method, params, sessionId);
			await cmd('Page.enable');
			await cmd('Runtime.enable');
			await cmd('Emulation.setDeviceMetricsOverride', { width: s.width, height: s.height, deviceScaleFactor: s.dpr, mobile: s.mobile, screenWidth: s.width, screenHeight: s.height });
			await cmd('Emulation.setTouchEmulationEnabled', { enabled: s.mobile });
			await cmd('Emulation.setEmulatedMedia', { media: 'screen', features: [{ name: 'prefers-color-scheme', value: s.scheme }] });
			await cmd('Page.navigate', { url });
			// 轮询到 load 完成，再留一点时间让布局与绘制稳定。
			// 不能用 requestAnimationFrame：无头模式下非活动标签页不会触发，会一直等下去。
			let ready = false;
			for (let i = 0; i < 100 && !ready; i++) {
				await sleep(100);
				const r = await cmd('Runtime.evaluate', { expression: "document.readyState === 'complete'", returnByValue: true });
				ready = r.result.value === true;
			}
			if (!ready) throw new Error(`页面 10 秒内没有加载完成：${url}`);
			await sleep(300);
			const shot = await cmd('Page.captureScreenshot', { format: 'png' });
			const file = path.join(outDir, `${s.name}.png`);
			await writeFile(file, Buffer.from(shot.data, 'base64'));
			files.push(file);
			await send('Target.closeTarget', { targetId });
		}
		// 伪装页不该有任何脚本报错：有异常就说明页面本身有问题
		if (pageErrors.length) throw new Error(`页面脚本有未捕获的异常：\n  ${pageErrors.join('\n  ')}`);
	} finally {
		ws.close();
		child.kill();
		if (server) {
			server.closeAllConnections(); // 否则 keep-alive 连接会让 close() 一直等，进程不退出
			server.close();
		}
		await sleep(300);
		await rm(profile, { recursive: true, force: true });
	}
	return { browser, url, files };
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
