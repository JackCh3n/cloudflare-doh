/**
 * 伪装 404 页截图像素验证（零依赖 PNG 解码 + 采样断言）
 *
 * 先用无头 Edge 生成四态截图，再本脚本校验颜色/布局特征：
 *   "C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe" --headless --disable-gpu \
 *     --window-size=1280,800 --screenshot=light.png http://127.0.0.1:18080/
 *   （深色态加 --force-dark-mode；移动端把窗口尺寸换成 --window-size=375,667）
 * 用法：node test/verify-camo-shots.mjs <light.png> <dark.png> <mobile-light.png> <mobile-dark.png>
 */
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import zlib from 'node:zlib';

/** 解码 8-bit PNG（支持彩色类型 2/6），返回 {width, height, get(x,y)->[r,g,b]} */
function decodePng(buf) {
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

/** 全图统计与指定色的匹配像素数及包围盒 */
function countColor(img, want, tol = 10) {
	let n = 0, minX = 1e9, minY = 1e9, maxX = -1, maxY = -1;
	for (let y = 0; y < img.height; y += 2) {
		for (let x = 0; x < img.width; x += 2) {
			if (near(img.get(x, y), want, tol)) {
				n++;
				if (x < minX) minX = x; if (x > maxX) maxX = x;
				if (y < minY) minY = y; if (y > maxY) maxY = y;
			}
		}
	}
	return { n, minX, minY, maxX, maxY };
}

const [lightP, darkP, mobLightP, mobDarkP] = process.argv.slice(2);
assert.equal(process.argv.length, 6, '需要 4 张截图参数');

// ---- 浅色桌面 ----
const light = decodePng(await readFile(lightP));
assert.equal(light.width, 1280);
assert.ok(near(light.get(5, 5), [255, 255, 255]), '浅色背景应为白色');
const btnLight = countColor(light, [26, 115, 232]);
assert.ok(btnLight.n >= 200, `浅色模式应出现按钮蓝 #1a73e8，实际 ${btnLight.n} 个采样点`);
assert.ok(btnLight.minY > 200 && btnLight.maxY < 800, `按钮应在首屏内（y∈[${btnLight.minY},${btnLight.maxY}]）`);
const grayText = countColor(light, [95, 99, 104]);
assert.ok(grayText.n >= 8, `错误码应使用灰色 #5f6368，实际 ${grayText.n} 个采样点`);
console.log(`✔ 浅色桌面 1280x${light.height}：白底、按钮蓝、错误码灰（按钮区 x∈[${btnLight.minX},${btnLight.maxX}]）`);

// ---- 深色桌面 ----
const dark = decodePng(await readFile(darkP));
assert.equal(dark.width, 1280);
assert.ok(near(dark.get(5, 5), [32, 33, 36]), '深色背景应为 #202124');
const btnDark = countColor(dark, [138, 180, 248]);
assert.ok(btnDark.n >= 200, `深色模式按钮应为 #8ab4f8，实际 ${btnDark.n} 个采样点`);
const lightText = countColor(dark, [232, 234, 237]);
assert.ok(lightText.n >= 50, `深色正文应为 #e8eaed，实际 ${lightText.n} 个采样点`);
console.log(`✔ 深色桌面 1280x${dark.height}：#202124 底、按钮 #8ab4f8、正文 #e8eaed`);

// ---- 移动端（375 宽）----
for (const [name, p, bg] of [['浅色', mobLightP, [255, 255, 255]], ['深色', mobDarkP, [32, 33, 36]]]) {
	const img = decodePng(await readFile(p));
	assert.equal(img.width, 375, `${name}移动端宽度应为 375`);
	assert.ok(near(img.get(5, 5), bg), `${name}移动端背景色应为 ${bg}`);
	// 顶部留白用 clamp：375x667 视口 14vh≈93px，内容从 ~93px 开始，须在首屏可见
	const btn = countColor(img, name === '浅色' ? [26, 115, 232] : [138, 180, 248]);
	assert.ok(btn.n >= 60, `${name}移动端应出现按钮，实际 ${btn.n} 个采样点`);
	assert.ok(btn.minY > 60 && btn.maxY < 667, `${name}移动端按钮须在首屏内（y∈[${btn.minY},${btn.maxY}]），不得被固定 125px 顶出视口`);
	assert.ok(btn.maxX <= 375, `${name}移动端按钮不得溢出视口`);
	console.log(`✔ ${name}移动端 375x${img.height}：内容与按钮均在首屏内（按钮 y∈[${btn.minY},${btn.maxY}]）`);
}

console.log('\n伪装 404 页四态截图全部验证通过');
