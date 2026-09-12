import { describe, it, expect } from 'vitest';
import { prepareFiles } from './prepareFiles';

// 【本地改动 2026-09-12】这套用例取代原来的 HEIC→JPEG 转码用例。
//
// 发现背景:线上 .avif 附件被认成 video/mp4(2026-09-12),排查后确认
// "上传方声明的 Content-Type 不可信"是根因,于是加了 MIME 纠正。随后的
// 策略调整是「终止客户端所有编码、直接透传源文件」,服务端负责统一重编码
// 成原尺寸 AVIF。本文件保护两件事:① 声明类型必须被纠正;② 字节必须一个
// 不改地被上传(尤其 HEIC 不再被偷偷换成 JPEG)。

function makeFile(name: string, type: string, size = 100): File {
	return new File([new Uint8Array(size)], name, { type });
}

async function bytesEqual(a: Blob, b: Blob): Promise<boolean> {
	const [x, y] = await Promise.all([a.arrayBuffer(), b.arrayBuffer()]);
	if (x.byteLength !== y.byteLength) return false;
	const va = new Uint8Array(x);
	const vb = new Uint8Array(y);
	for (let i = 0; i < va.length; i++) {
		if (va[i] !== vb[i]) return false;
	}
	return true;
}

describe('prepareFiles', () => {
	it('passes every file through unchanged', async () => {
		const jpeg = makeFile('photo.jpg', 'image/jpeg');
		const png = makeFile('screenshot.png', 'image/png');
		const gif = makeFile('anim.gif', 'image/gif');
		const mp4 = makeFile('clip.mp4', 'video/mp4');

		const result = await prepareFiles([jpeg, png, gif, mp4]);

		expect(result).toHaveLength(4);
		expect(result[0]).toBe(jpeg);
		expect(result[1]).toBe(png);
		expect(result[2]).toBe(gif);
		expect(result[3]).toBe(mp4);
	});

	it('corrects a misdeclared AVIF image back to image/avif', async () => {
		const avif = makeFile('photo.avif', 'video/mp4');

		const result = await prepareFiles([avif]);

		expect(result).toHaveLength(1);
		expect(result[0].type).toBe('image/avif');
		expect(result[0].name).toBe('photo.avif');
		expect(await bytesEqual(result[0], avif)).toBe(true);
	});

	it('fills an empty declared type from the extension', async () => {
		const heic = makeFile('photo.HEIC', '');

		const result = await prepareFiles([heic]);

		expect(result[0].type).toBe('image/heic');
	});

	it('fills an octet-stream declared type from the extension', async () => {
		const webp = makeFile('photo.webp', 'application/octet-stream');

		const result = await prepareFiles([webp]);

		expect(result[0].type).toBe('image/webp');
	});

	// 【本地改动 2026-09-12】关键回归点:HEIC 不再被转成 JPEG。服务端 ffmpeg
	// 没有 heif 解码器,存下来只能原样,所以前端必须把原字节交出去。
	it('uploads HEIC bytes as-is instead of transcoding them', async () => {
		const heic = makeFile('photo.heic', 'image/heic');

		const result = await prepareFiles([heic]);

		expect(result).toHaveLength(1);
		expect(result[0].type).toBe('image/heic');
		expect(result[0].name).toBe('photo.heic');
		expect(await bytesEqual(result[0], heic)).toBe(true);
	});

	it('handles mixed images and video in one batch', async () => {
		const jpeg = makeFile('normal.jpg', 'image/jpeg');
		const heic = makeFile('iphone.heic', 'video/mp4');
		const mp4 = makeFile('clip.mp4', 'video/mp4');

		const result = await prepareFiles([jpeg, heic, mp4]);

		expect(result).toHaveLength(3);
		expect(result[0]).toBe(jpeg);
		expect(result[1].type).toBe('image/heic');
		expect(result[1].name).toBe('iphone.heic');
		expect(result[2]).toBe(mp4);
	});
});
