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
  import LoadRetry from './LoadRetry.svelte';
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
    <!-- 【本地改动 2026-10-20】灯箱组件必须懒加载。
         原因：MarkdownHtml 在 login/overview 等路由的首包图里可达（经 ServerGutter 的 MOTD
         单行预览链上来），AttachmentModal + ZoomableImage 若静态 import 会把整个大图查看器
         拉进首包，顶穿 `check-bundle-size` 预算。patch 0005 提交后 CI 实测
         login 338.4/337.0 KiB、overview 381.8/379.0 KiB 双 FAIL，`pnpm run build` 失败，
         连带 desktop 三个平台与全部 test-e2e 一起红。
         修法：沿用仓库既有 `{#await import()}` 模式（参见 MessageActionOverlays.svelte），
         分支渲染时才发 chunk 请求，因此只有首次点击图片才会加载它。
         验证（本地 pnpm run build，原始预算未改动）：login 335.2/337.0 PASS、
         overview 378.6/379.0 PASS、room 561.9/570.0 PASS、csp PASS —— 净回收约 3.2 KiB。
         踩坑：`{:catch}` 必须用 Svelte 5 的裸标识符 `{:catch error}`，不能写 `{:catch (error)}`；
         `{@render}` 只接受调用表达式，所以是 `{@render lightboxLoadError()}`。 -->
    {#await import('./AttachmentModal.svelte')}
      <!-- 待加载期不渲染任何 UI：模态框尚未弹出，缩略图仍在原位，无需占位。 -->
      <!-- 注意：pending 分支不能放 LoadRetry，否则「正在加载」和「加载失败」无法区分，
           组件测试里也会误判成网络错误。 -->
    {:then { default: AttachmentModal }}
      {#await import('./ZoomableImage.svelte') then { default: ZoomableImage }}
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
      {:catch}
        {@render lightboxLoadError()}
      {/await}
    {:catch}
      {@render lightboxLoadError()}
    {/await}
  {/if}
{/if}

{#snippet lightboxLoadError()}
  <!-- Retry 只是丢掉当前 lightbox 状态；块被销毁后下一次点击会重新发起 chunk 请求。 -->
  <LoadRetry onretry={() => (lightbox = null)} />
{/snippet}
