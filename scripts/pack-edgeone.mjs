/**
 * 打包 EdgeOne Pages 手动上传目录
 *
 * 只保留运行时真正需要的三个文件：
 *   middleware.js  —— 平台入口（边缘中间件）
 *   src/core.js    —— 共享核心，构建时会被 esbuild 内联进 middleware
 *   index.html     —— 静态兜底页（内容与伪装 404 一致）
 *
 * 用法：npm run pack:edgeone
 * 产物：dist/edgeone/（已在 .gitignore 中忽略，控制台「直接上传」选这个文件夹）
 */

import { copyFile, mkdir, rm, stat } from 'node:fs/promises';
import path from 'node:path';

const root = process.cwd();
const outDir = path.join(root, 'dist', 'edgeone');
const FILES = ['middleware.js', 'index.html', path.join('src', 'core.js')];

await rm(outDir, { recursive: true, force: true });
await mkdir(path.join(outDir, 'src'), { recursive: true });

for (const file of FILES) {
	await copyFile(path.join(root, file), path.join(outDir, file));
}

console.log(`\n已生成手动上传目录：${outDir}\n`);
for (const file of FILES) {
	const { size } = await stat(path.join(outDir, file));
	console.log(`  ${file.padEnd(18)} ${String(size).padStart(6)} B`);
}

console.log('\n用法：EdgeOne 控制台 → Pages 项目 → 直接上传 → 选择该文件夹（或把该文件夹压成 zip 上传）');
console.log('如需改映射：改 dist/edgeone/src/core.js 的 DEFAULT_PATH_MAPPINGS，或重新执行本脚本。\n');
