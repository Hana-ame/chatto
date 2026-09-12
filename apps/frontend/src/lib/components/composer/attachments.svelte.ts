import { toast } from '$lib/ui/toast';
import { resolveFileMimeType } from '$lib/attachments/mimeTypes';
import { prepareFiles } from '$lib/attachments/prepareFiles';

export type FileWithUrl = { file: File; url: string };

export type AttachmentLimits = {
  videoProcessingEnabled: boolean;
  maxUploadSize: number;
  maxVideoUploadSize: number;
};

export function formatFileSize(bytes: number): string {
  if (bytes >= 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(0)} MB`;
  if (bytes >= 1024) return `${(bytes / 1024).toFixed(0)} KB`;
  return `${bytes} bytes`;
}

export class AttachmentsState {
  filesWithUrls = $state<FileWithUrl[]>([]);
  pendingCount = $state(0);

  constructor(private readonly getLimits: () => AttachmentLimits) {}

  get selectedFiles(): File[] {
    return this.filesWithUrls.map((f) => f.file);
  }

  restore(files: FileWithUrl[]): void {
    this.filesWithUrls = files;
  }

  validateFiles(files: File[]): File[] {
    const limits = this.getLimits();
    const accepted: File[] = [];
    for (const file of files) {
      // 【本地改动 2026-09-12】这里必须用 resolveFileMimeType 而不是
      // file.type:上传方声明的 MIME 不可信(来自操作系统扩展名注册表),
      // 直接按声明判视频会在 prepareFiles 纠正类型之前就把图片以
      // "视频已禁用"拒掉,或让它占用视频体积上限。
      // 顺序上 validateFiles 必须先于 prepareFiles——现有测试断言被拒文件
      // 不触发 prepareFilesMock。
      const isVideo = resolveFileMimeType(file).startsWith('video/');
      if (isVideo && !limits.videoProcessingEnabled) {
        toast.error('Video uploads are disabled on this server.');
        continue;
      }

      const limit = isVideo ? limits.maxVideoUploadSize : limits.maxUploadSize;
      if (file.size > limit) {
        toast.error(
          `${file.name} is too large (${formatFileSize(file.size)}). Maximum is ${formatFileSize(limit)}.`
        );
      } else {
        accepted.push(file);
      }
    }
    return accepted;
  }

  filesToPreviewItems(files: File[]): FileWithUrl[] {
    return files.map((file) => ({
      file,
      url: URL.createObjectURL(file)
    }));
  }

  async stageFiles(files: File[]): Promise<void> {
    const validFiles = this.validateFiles(files);
    if (validFiles.length === 0) return;

    this.pendingCount += validFiles.length;
    try {
      const prepared = await prepareFiles(validFiles);
      if (prepared.length > 0) {
        this.filesWithUrls = [...this.filesWithUrls, ...this.filesToPreviewItems(prepared)];
      }
    } catch (err) {
      console.error('Error preparing attachment files:', err);
      toast.error('Failed to prepare attachment');
    } finally {
      this.pendingCount -= validFiles.length;
    }
  }

  removeFile(index: number): void {
    const removed = this.filesWithUrls[index];
    if (removed) URL.revokeObjectURL(removed.url);
    this.filesWithUrls = this.filesWithUrls.filter((_, i) => i !== index);
  }

  clear(): void {
    for (const { url } of this.filesWithUrls) {
      URL.revokeObjectURL(url);
    }
    this.filesWithUrls = [];
  }
}
