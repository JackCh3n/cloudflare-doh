/**
 * 伪装 404 页截图验证（零依赖：采集 + PNG 解码 + 采样断言）
 *
 * 直接运行：node test/verify-camo-shots.mjs
 *   1. 用无头 Chrome/Edge 的设备模拟拍四态（桌面 1280x800@1x、手机 375x667@2x × 浅色/深色）。
 *      页面取 src/core.js 里求值后的 HOMEPAGE_HTML（Worker / EdgeOne 实际发出去的那一份），
 *      由内置静态服务提供（对任意路径返回伪装页，状态码 404，与代理的伪装应答一致）
 *   2. 解码截图，按坐标与色值断言
 *   验证运行中的 Go 代理或线上站点：node test/verify-camo-shots.mjs --url <页面地址>
 *   验证现成的四张截图：node test/verify-camo-shots.mjs <light> <dark> <phone-light> <phone-dark>
 *   （Node ≥ 22：采集用到内置 WebSocket）
 *
 * 这里只回答「伪装页的坐标与色值对不对」；要回答「和本机 Chrome 的真实错误页像不像」，
 * 用 test/compare-camo-chrome.mjs（npm run camo:chrome）。
 *
 * 断言依据是 Chrome 157（Windows）真实 DNS 错误页的实测结果：
 *   - 桌面 1280x800：内容列 600px 居中（左缘 x=340），图标盒 72x72 在 (340,112)，
 *     正文 y=278，链接 y=317，错误码 y=366，按钮 y=429~464 且与正文左对齐（不是浮动到右侧）
 *   - 手机 375x667：两侧留白 24px，按钮是固定在底部的通栏（x 24~351，y 593~645）
 *   - 浅色：底 #fff、正文 #5f6368、链接/按钮 #1a73e8、图标 #535353
 *   - 深色：底 #202124、正文/标题 #9aa0a6、链接/按钮 #8ab4f8、图标反转为 #acacac
 * 按钮的纵向位置受系统字体行高影响，只在 Windows 上是精确值，其它系统给了容差。
 */
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import zlib from 'node:zlib';
import { fileURLToPath } from 'node:url';
import { SHOT_STATES, captureCamoShots } from './capture-camo-shots.mjs';

/** 解码 8-bit PNG（支持彩色类型 2/6），返回 {width, height, get(x,y)->[r,g,b]} */
export function decodePng(buf) {
	assert.equal(buf.readUInt32BE(0), 0x89504e47, 'PNG 魔数');
	let pos = 8;
	let width = 0, height = 0, bitDepth = 0, colorType = 0;
	const idat = [];
	while (pos < buf.length) {
		const len = buf.readUInt32BE(pos);
		const type = buf.toString('ascii', pos + 4, pos + 8);
		const data = buf.subarray(pos + 8, pos + 8 + len);
		if (type === 'IHDR') {
			width = data.readUInt32BE(0);
			height = data.readUInt32BE(4);
			bitDepth = data[8];
			colorType = data[9];
		} else if (type === 'IDAT') {
			idat.push(data);
		} else if (type === 'IEND') break;
		pos += 12 + len;
	}
	assert.equal(bitDepth, 8, '仅支持 8-bit');
	assert([2, 6].includes(colorType), `仅支持彩色类型 2/6，实际 ${colorType}`);
	const channels = colorType === 6 ? 4 : 3;
	const raw = zlib.inflateSync(Buffer.concat(idat));
	const stride = width * channels;
	const out = Buffer.alloc(height * stride);
	for (let y = 0; y < height; y++) {
		const filter = raw[y * (stride + 1)];
		const row = raw.subarray(y * (stride + 1) + 1, (y + 1) * (stride + 1));
		const prev = y > 0 ? out.subarray((y - 1) * stride, y * stride) : null;
		const cur = out.subarray(y * stride, (y + 1) * stride);
		for (let x = 0; x < stride; x++) {
			const a = x >= channels ? cur[x - channels] : 0;
			const b = prev ? prev[x] : 0;
			const c = x >= channels && prev ? prev[x - channels] : 0;
			let v = row[x];
			if (filter === 1) v = (v + a) & 0xff;
			else if (filter === 2) v = (v + b) & 0xff;
			else if (filter === 3) v = (v + ((a + b) >> 1)) & 0xff;
			else if (filter === 4) {
				const p = a + b - c, pa = Math.abs(p - a), pb = Math.abs(p - b), pc = Math.abs(p - c);
				v = (v + (pa <= pb && pa <= pc ? a : pb <= pc ? b : c)) & 0xff;
			}
			cur[x] = v;
		}
	}
	return { width, height, get: (x, y) => [out[y * stride + x * channels], out[y * stride + x * channels + 1], out[y * stride + x * channels + 2]] };
}

const near = (c, want, tol = 6) => c.every((v, i) => Math.abs(v - want[i]) <= tol);

/**
 * 在 region=[x0,y0,x1,y1]（设备像素，含头不含尾）内每隔 2px 采样，
 * 统计与指定色匹配的采样点及包围盒
 */
function countColor(img, want, region, tol = 10) {
	const [x0, y0, x1, y1] = region;
	let n = 0, minX = Infinity, minY = Infinity, maxX = -1, maxY = -1;
	for (let y = y0; y < y1; y += 2) {
		for (let x = x0; x < x1; x += 2) {
			if (near(img.get(x, y), want, tol)) {
				n++;
				if (x < minX) minX = x;
				if (x > maxX) maxX = x;
				if (y < minY) minY = y;
				if (y > maxY) maxY = y;
			}
		}
	}
	return { n, minX, minY, maxX, maxY };
}

const box = (c) => `x∈[${c.minX},${c.maxX}] y∈[${c.minY},${c.maxY}]`;

const INK = [83, 83, 83]; // #535353 图标墨色
const INK_DARK = [172, 172, 172]; // #acacac，深色下图标整体反转
const TEXT = [95, 99, 104]; // #5f6368
const TEXT_DARK = [154, 160, 166]; // #9aa0a6
const BLUE = [26, 115, 232]; // #1a73e8
const BLUE_DARK = [138, 180, 248]; // #8ab4f8

// 按钮纵向位置随系统字体行高变化：Windows 上是精确值，其它系统放宽
const exactMetrics = process.platform === 'win32';
const buttonY = (top, bottom) => (exactMetrics ? [top - 2, bottom + 2] : [top - 16, bottom + 16]);

/** 校验四张截图，files 顺序与 SHOT_STATES 一致 */
export async function verifyCamoShots(files) {
	const [light, dark, phoneLight, phoneDark] = await Promise.all(files.map((f) => readFile(f).then(decodePng)));

	// ---- 浅色桌面 1280x800 ----
	assert.equal(light.width, 1280);
	assert.ok(near(light.get(5, 5), [255, 255, 255]), '浅色背景应为白色');
	const iconLight = countColor(light, INK, [320, 100, 440, 200]);
	assert.ok(iconLight.n >= 80, `浅色图标应完整显示（72x72 盒内的难过文档），实际 ${iconLight.n} 个采样点`);
	assert.ok(iconLight.minX >= 339 && iconLight.maxX <= 374, `浅色图标水平位置异常（应在 x 340~373）：${box(iconLight)}`);
	assert.ok(iconLight.minY >= 141 && iconLight.maxY <= 184, `浅色图标垂直位置异常（应在 y 142~184，不得被压扁）：${box(iconLight)}`);
	const grayText = countColor(light, TEXT, [0, 0, light.width, light.height]);
	assert.ok(grayText.n >= 8, `正文/错误码应使用 #5f6368，实际 ${grayText.n} 个采样点`);
	const linkLight = countColor(light, BLUE, [300, 305, 1000, 350]);
	assert.ok(linkLight.n >= 8, `正文区的「Windows 网络诊断」链接应为 #1a73e8，实际 ${linkLight.n} 个采样点`);
	const [btnTop, btnBottom] = buttonY(429, 464);
	const btnLight = countColor(light, BLUE, [0, btnTop - 4, light.width, light.height]);
	assert.ok(btnLight.n >= 200, `浅色按钮应出现（#1a73e8），实际 ${btnLight.n} 个采样点`);
	assert.ok(btnLight.minX >= 338 && btnLight.minX <= 342, `桌面端按钮应与正文左对齐（左缘 x≈340），实际 ${box(btnLight)}`);
	assert.ok(btnLight.maxX < 440, `桌面端按钮不应是通栏（宽约 84px），实际 ${box(btnLight)}`);
	assert.ok(btnLight.minY >= btnTop && btnLight.maxY <= btnBottom, `桌面端按钮应紧跟在正文下方（y ${btnTop}~${btnBottom}），实际 ${box(btnLight)}`);
	console.log(`✔ 浅色桌面 1280x${light.height}：白底、图标 ${box(iconLight)}、按钮左对齐 ${box(btnLight)}`);

	// ---- 深色桌面 1280x800 ----
	assert.equal(dark.width, 1280);
	assert.ok(near(dark.get(5, 5), [32, 33, 36]), '深色背景应为 #202124');
	const iconDark = countColor(dark, INK_DARK, [320, 100, 440, 200]);
	assert.ok(iconDark.n >= 80, `深色图标应反转为 #acacac（否则在深底上看不见），实际 ${iconDark.n} 个采样点`);
	const textDark = countColor(dark, TEXT_DARK, [0, 0, dark.width, dark.height]);
	assert.ok(textDark.n >= 50, `深色正文/标题应为 #9aa0a6，实际 ${textDark.n} 个采样点`);
	const linkDark = countColor(dark, BLUE_DARK, [300, 305, 1000, 350]);
	assert.ok(linkDark.n >= 8, `深色链接应为 #8ab4f8，实际 ${linkDark.n} 个采样点`);
	const btnDark = countColor(dark, BLUE_DARK, [0, btnTop - 4, dark.width, dark.height]);
	assert.ok(btnDark.n >= 200, `深色按钮应为 #8ab4f8，实际 ${btnDark.n} 个采样点`);
	assert.ok(btnDark.minX >= 338 && btnDark.minX <= 342, `深色桌面端按钮应左对齐（x≈340），实际 ${box(btnDark)}`);
	console.log(`✔ 深色桌面 1280x${dark.height}：#202124 底、图标反转、正文 #9aa0a6、链接/按钮 #8ab4f8`);

	// ---- 手机 375x667@2x（截图 750x1334，换算成 CSS 像素后断言）----
	for (const [name, img, bg, ink, blue, textColor] of [
		['浅色', phoneLight, [255, 255, 255], INK, BLUE, TEXT],
		['深色', phoneDark, [32, 33, 36], INK_DARK, BLUE_DARK, TEXT_DARK],
	]) {
		const k = img.width / 375;
		assert.equal(img.width, 750, `${name}手机截图宽度应为 750（375 CSS 像素 @2x）`);
		assert.equal(img.height, 1334, `${name}手机截图高度应为 1334（667 CSS 像素 @2x）`);
		assert.ok(near(img.get(10, 10), bg), `${name}手机背景色应为 ${bg}`);
		// 图标在左上：距左缘 24px（两侧留白生效），盒顶在 10vh 处、图形本身在盒内 y 29~72
		const icon = countColor(img, ink, [0, 0, 120 * k, 150 * k]);
		assert.ok(icon.n >= 400, `${name}手机图标应清晰完整（2x 位图），实际 ${icon.n} 个采样点`);
		assert.ok(icon.minX >= 24 * k - 2 && icon.minX <= 24 * k + 2, `${name}手机图标应距左缘 24px，实际 ${box(icon)}`);
		assert.ok(icon.maxX <= 61 * k && icon.minY >= 94 * k && icon.maxY <= 140 * k, `${name}手机图标应在 CSS (24,95) 起的 36x44 内，实际 ${box(icon)}`);
		// 正文在图标下方、按钮上方
		const text = countColor(img, textColor, [0, 150 * k, img.width, 560 * k]);
		assert.ok(text.n >= 8, `${name}手机正文应可见，实际 ${text.n} 个采样点`);
		// 按钮：固定在底部的通栏（x 24~351，y 593~645），完整落在视口内
		const btn = countColor(img, blue, [0, 560 * k, img.width, img.height]);
		assert.ok(btn.n >= 10000, `${name}手机底部应为通栏按钮，实际 ${btn.n} 个采样点`);
		assert.ok(btn.minX >= 24 * k - 2 && btn.minX <= 24 * k + 2 && btn.maxX <= 352 * k, `${name}手机按钮应左右各留 24px，实际 ${box(btn)}`);
		const [phoneTop, phoneBottom] = buttonY(593 * k, 646 * k);
		assert.ok(btn.minY >= phoneTop && btn.maxY <= phoneBottom, `${name}手机按钮应固定在底部（y 593~645），实际 ${box(btn)}`);
		console.log(`✔ ${name}手机 375x667@2x：图标 ${box(icon)}、正文可见、底部通栏按钮 ${box(btn)}`);
	}

	console.log('\n伪装 404 页四态截图全部验证通过');
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
	const given = process.argv.slice(2);
	if (given[0] === '--url') {
		// 验证运行中的 Go 代理或线上站点：node test/verify-camo-shots.mjs --url https://doh.example.com/
		if (!given[1]) {
			console.error('用法：node test/verify-camo-shots.mjs --url <页面地址>');
			process.exit(2);
		}
		const outDir = path.join(tmpdir(), 'cloudflare-doh-camo-shots');
		const { browser } = await captureCamoShots(outDir, given[1]);
		console.log(`浏览器：${browser}\n页面：${given[1]}\n截图：${outDir}\n`);
		await verifyCamoShots(SHOT_STATES.map((s) => path.join(outDir, `${s.name}.png`)));
	} else if (given.length === 0) {
		const outDir = path.join(tmpdir(), 'cloudflare-doh-camo-shots');
		const { browser, url } = await captureCamoShots(outDir);
		console.log(`浏览器：${browser}\n页面：${url}\n截图：${outDir}\n`);
		await verifyCamoShots(SHOT_STATES.map((s) => path.join(outDir, `${s.name}.png`)));
	} else {
		await verifyCamoShots(given);
	}
}
