/**
 * Normalizes each upload so the upload request carries the type the bytes
 * really are.
 *
 * 【本地改动 2026-09-12】客户端不再做任何图片编码:直接透传源文件。
 *
 * 目的:fork 的存储策略是「上传时由服务端统一重编码为原尺寸 AVIF(静态与
 * 动画都转),存一份、发一份,不再生成衍生图」。既然服务端负责重编码,客户端
 * 预先编码只有坏处:白白多一份 CPU/内存和一整套 wasm 依赖(heic2any),还会
 * 覆盖掉「原始字节」这个唯一的信源——服务端拿到的就不再是用户上传的东西。
 * 因此本函数只做一件事:纠正浏览器/操作系统声明的 MIME(扩展名为准)。它不
 * 是编码,不改一个字节。
 *
 * 保留 prepareFiles 这个异步入口的原因:调用点(composer 附件流程)已经在等
 * 一个 Promise,把它同步化会牵连一串测试与类型;透传本身是即时的。
 *
 * ⚠️ 未决风险(必须有人知道,不能默默吞掉):服务端 ffmpeg 没有 heif 解码器,
 * HEIC/HEIF 上传后**无法**在服务端转成 AVIF,只能原样存储,而多数浏览器
 * (Safari 除外)显示不了 HEIC。此前这里的 heic2any 转 JPEG 就是这个坑的
 * 兜底,现在兜底没了。出路二选一:① 部署方换带 libheif 的 ffmpeg;② 恢复
 * 仅针对 HEIC 的客户端转码。本次按「终止所有编码、直接 bypass 源文件」的
 * 明确指令实现,等于把选择权留给 ①;选 ② 时把 convertHeicToJpeg 加回来即可。
 *
 * 历史:upstream 版本在此处用 heic2any 把 HEIC/HEIF 转 JPEG 后再上传
 * (2026-09-12 之前 fork 也继承了这个行为)。
 */

import { withResolvedMimeType } from './mimeTypes';

async function prepareOne(file: File): Promise<File> {
	return withResolvedMimeType(file);
}

export async function prepareFiles(files: File[]): Promise<File[]> {
	return Promise.all(files.map(prepareOne));
}
