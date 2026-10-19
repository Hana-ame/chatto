<!--
@component

Single audited sink for HTML produced by the full and restricted renderers in
`$lib/markdown`.

The markdown renderer disables source HTML and owns every tag/attribute it emits.
Keep all rendered markdown HTML flowing through this component so raw HTML usage
does not spread into feature components. Callers may pass only markdown renderer
output, plus the mention and edited-marker post-processing in the message path.
This component also owns the copy action for rendered code blocks and the
attachment-style lightbox viewer for rendered inline Markdown images.
-->
<script lang="ts">
  import { m } from '$lib/i18n/messages';
  import { trustedMarkdownHtml } from '$lib/security/trustedHtml';
  import { toast } from '$lib/ui/toast';
  import AttachmentModal from './AttachmentModal.svelte';
  import ZoomableImage from './ZoomableImage.svelte';
  import { trackScrollEdges } from './scrollEdges';

  let {
    html
  }: {
    html: string;
  } = $props();

  type MarkdownLightboxItem = {
    src: string;
    originalUrl: string;
    alt: string;
    filename: string;
    contentType: string;
  };

  const trusted = $derived(trustedMarkdownHtml(html));
  // 【本地改动 2026-10-19】对齐附件图片「点击大图」交互：记录当前打开的 Markdown 内联图集
  // 与选中索引，复用 AttachmentModal (imageViewer) + ZoomableImage 渲染可缩放/平移/翻页的大图灯箱。
  let lightbox = $state<{ items: MarkdownLightboxItem[]; index: number } | null>(null);

  function extractImageFilename(url: string, alt: string, fallbackIndex: number): string {
    try {
      const parsed = new URL(url);
      const lastSegment = parsed.pathname.split('/').filter(Boolean).pop();
      if (lastSegment) {
        const decoded = decodeURIComponent(lastSegment).trim();
        if (decoded) return decoded;
      }
    } catch {
      /* ignore malformed URLs */
    }
    return alt || `image-${fallbackIndex}.png`;
  }

  function inferImageContentType(filename: string): string {
    const ext = filename.split('.').pop()?.toLowerCase();
    switch (ext) {
      case 'jpg':
      case 'jpeg':
        return 'image/jpeg';
      case 'gif':
        return 'image/gif';
      case 'webp':
        return 'image/webp';
      case 'avif':
        return 'image/avif';
      case 'svg':
        return 'image/svg+xml';
      default:
        return 'image/png';
    }
  }

  async function handleClick(event: MouseEvent) {
    const target = event.target;
    if (!(target instanceof Element)) return;
    const button = target.closest<HTMLButtonElement>('button[data-markdown-copy]');
    if (button) {
      event.preventDefault();
      event.stopPropagation();
      const code = button.closest('pre')?.getAttribute('data-copy-source');
      if (code === null || code === undefined) return;

      try {
        await navigator.clipboard.writeText(code);
        toast.success(m('common.copied_to_clipboard'));
      } catch {
        toast.error(m('room.message.actions.copy_text_failed'));
      }
      return;
    }

    // 【本地改动 2026-10-19】拦截对内联图片（a.markdown-image-link）的普通左键点击，
    // 阻止 MessageContent 跳新标签页，改为弹出与附件一致的灯箱大图查看器（支持缩放与多图翻页）。
    const imageLink = target.closest<HTMLAnchorElement>('a.markdown-image-link');
    if (
      imageLink &&
      event.button === 0 &&
      !event.metaKey &&
      !event.ctrlKey &&
      !event.shiftKey &&
      !event.altKey
    ) {
      const clickedImg = imageLink.querySelector<HTMLImageElement>('img');
      const clickedSrc = clickedImg?.getAttribute('src');
      if (clickedImg && clickedSrc && clickedSrc !== '#') {
        event.preventDefault();
        event.stopPropagation();
        const root = imageLink.closest('.markdown-html') ?? imageLink.parentElement;
        const allLinks = Array.from(
          root?.querySelectorAll<HTMLAnchorElement>('a.markdown-image-link') ?? [imageLink]
        );
        const items: MarkdownLightboxItem[] = [];
        let selectedIndex = 0;
        for (const link of allLinks) {
          const img = link.querySelector<HTMLImageElement>('img');
          const src = img?.getAttribute('src');
          if (!img || !src || src === '#') continue;
          if (link === imageLink) selectedIndex = items.length;
          const originalUrl = link.getAttribute('href') || src;
          const alt = img.getAttribute('alt')?.trim() || '';
          const filename = extractImageFilename(originalUrl, alt, items.length + 1);
          items.push({
            src,
            originalUrl,
            alt,
            filename,
            contentType: inferImageContentType(filename)
          });
        }
        if (items.length > 0) {
          lightbox = { items, index: selectedIndex };
        }
      }
    }
  }

  // Markdown buttons and inline image galleries come from the audited HTML renderer,
  // so they use delegated listeners and scroll-edge observers on this component's DOM root.
  function attachCopyHandler(element: HTMLDivElement) {
    // Reference trusted so scroll-edge observers re-bind when rendered HTML updates.
    void trusted;
    element.addEventListener('click', handleClick);
    const cleanups: (() => void)[] = [];
    const tracks = element.querySelectorAll<HTMLElement>('.markdown-image-gallery-track');
    for (const track of tracks) {
      const container = track.closest('.markdown-image-gallery-container');
      const startFade = container?.querySelector<HTMLElement>(
        '[data-testid="message-image-gallery-start-fade"]'
      );
      const endFade = container?.querySelector<HTMLElement>(
        '[data-testid="message-image-gallery-end-fade"]'
      );
      const cleanup = trackScrollEdges('x', (edges) => {
        startFade?.classList.toggle('opacity-0', !edges.start);
        endFade?.classList.toggle('opacity-0', !edges.end);
      })(track);
      if (typeof cleanup === 'function') cleanups.push(cleanup);
    }
    return () => {
      element.removeEventListener('click', handleClick);
      for (const cleanup of cleanups) cleanup();
    };
  }
</script>

<div class="markdown-html contents" {@attach attachCopyHandler}>
  <!-- eslint-disable-next-line svelte/no-at-html-tags -- rendered markdown from `$lib/markdown`; see component docs above -->
  {@html trusted}
</div>

{#if lightbox}
  {@const activeItem = lightbox.items[lightbox.index]}
  {#if activeItem}
    <AttachmentModal
      filename={activeItem.filename}
      contentType={activeItem.contentType}
      description={activeItem.alt || undefined}
      size={null}
      sizeLoading={false}
      index={lightbox.index}
      count={lightbox.items.length}
      onnavigate={(direction) => {
        if (!lightbox) return;
        lightbox.index =
          (lightbox.index + direction + lightbox.items.length) % lightbox.items.length;
      }}
      downloadUrl={activeItem.originalUrl}
      imageViewer
      ondownload={() => {}}
      onclose={() => (lightbox = null)}
    >
      {#key lightbox.index}
        <ZoomableImage
          src={activeItem.src}
          alt={activeItem.alt || activeItem.filename}
          onerror={() => {}}
        />
      {/key}
    </AttachmentModal>
  {/if}
{/if}
