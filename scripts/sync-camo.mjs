/**
 * 伪装 404 页三处同步：以 index.html 为唯一来源，写回 src/core.js 与 server/main.go
 *
 * 页面同时内嵌在三个文件里（Worker/EdgeOne 核心、Go 服务、静态兜底页），
 * 手工同步容易漂移，改页面时只改 index.html，然后执行：
 *
 *   npm run camo:sync
 *
 * 约束（由嵌入方式决定，脚本会先校验再写入）：
 *   - 不能出现反引号：core.js 用模板字符串、main.go 用原生字符串字面量包裹
 *   - 不能出现 "${"：模板字符串会把它当成插值开头
 *   - 不能出现反斜杠：模板字符串会把反斜杠当转义序列处理（"\"" 求值后少一个字符，
 *     内嵌脚本直接变成语法错误），而 main.go 的原生字符串却原样保留——三处就悄悄分叉了
 */

import { readFile, writeFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const read = (file) => readFile(path.join(root, file), 'utf8');

const html = (await read('index.html')).trim();
if (!html.startsWith('<!DOCTYPE html>') || !html.endsWith('</html>')) {
	throw new Error('index.html 不是一份完整的 HTML 文档');
}
if (html.includes('`')) throw new Error('index.html 含有反引号，无法嵌入模板字符串 / Go 原生字符串');
if (html.includes('${')) throw new Error('index.html 含有 "${"，会被 core.js 的模板字符串当成插值');
if (html.includes('\\')) throw new Error('index.html 含有反斜杠：core.js 的模板字符串会把它当成转义序列吃掉（JS 字符串里改用单引号包裹）');

/** 把 content 里 source 与 target 之间的内嵌页面替换成 html，返回是否发生变化 */
function embed(content, pattern, render, file) {
	if (!pattern.test(content)) throw new Error(`${file} 里找不到内嵌页面的起止标记`);
	const next = content.replace(pattern, render);
	if (next === content) return { file, changed: false };
	return { file, changed: true, content: next };
}

const results = [
	embed(await read('src/core.js'), /export const HOMEPAGE_HTML = `[\s\S]*?`;/, () => `export const HOMEPAGE_HTML = \`${html}\`;`, 'src/core.js'),
	embed(await read('server/main.go'), /const homepageHTML = `[\s\S]*?`/, () => `const homepageHTML = \`${html}\``, 'server/main.go'),
];

for (const r of results) {
	if (!r.changed) {
		console.log(`  ${r.file.padEnd(16)} 已一致`);
		continue;
	}
	await writeFile(path.join(root, r.file), r.content);
	console.log(`  ${r.file.padEnd(16)} 已更新`);
}
console.log(`\n伪装 404 页 ${Buffer.byteLength(html, 'utf8')} 字节，三处副本已对齐 index.html`);
