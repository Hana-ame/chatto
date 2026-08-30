import type { Page } from '@playwright/test';
import { test, expect } from './setup';
import { createAndLoginTestUser } from './fixtures/testUser';
import {
  connectRemoteInstance,
  createUserOnRemote,
  getRoomOnRemote,
  postMessageAttachmentOnRemote,
  startSecondServer,
  stopSecondServer
} from './fixtures/multiServer';
import type { ServerInfo } from './fixtures/server';
import { waitForRoomReady } from './fixtures/realtimeSync';
import * as routes from './routes';

function remoteBaseURL(server: ServerInfo): string {
  return server.baseURL.replace('localhost', '127.0.0.1');
}

async function ensureServiceWorkerControlsPage(page: Page): Promise<void> {
  await page.evaluate(async () => {
    if (!('serviceWorker' in navigator)) {
      throw new Error('Service workers are not available in this browser');
    }
    await navigator.serviceWorker.ready;
  });

  await page.reload({ waitUntil: 'domcontentloaded' });
  await expect
    .poll(() => page.evaluate(() => Boolean(navigator.serviceWorker.controller)))
    .toBe(true);
}

test.describe('authorized remote asset URLs', () => {
  let remoteServer: ServerInfo | undefined;

  test.afterEach(async ({}, testInfo) => {
    if (remoteServer) {
      await stopSecondServer(remoteServer, testInfo);
      remoteServer = undefined;
    }
  });

  // 【本地改动 2026-08-30】上游原测试名与断言是「through signed asset URLs」+
  // expect(...).toContain('access='):上游契约要求浏览器侧附件 URL 携带 per-user 签名
  // ticket(cli/AGENTS.md「The ticket is the browser capability」),服务端校验签名用户对
  // asset 所在 room 仍是成员,kick/leave 因此能撤销未来访问,URL 也因此 per-user、不可
  // 共享、不可 CDN 缓存。
  // 【目的】本 fork 故意反转:connectapi 这层改调 Public* 版本,URL 形如
  // /assets/files/{assetID}/{fn.ext},assetID 本身就是凭证——无 ticket、无成员校验,
  // 响应 public, max-age=31536000, immutable,为 CF/CDN 长期缓存。
  // 【踩坑】2026-08-30 把 build-release 触发分支从 ci/deploy 改到 main 后,ci.yml 第一次
  // 在本仓库 main 上跑完整 e2e 矩阵,本用例立刻红:Expected substring "access="。fork 的
  // Public* URL 构造器此前只在 fork 自己的 Go 单元测试里被断言(media_model_test.go 的
  // TestMediaModelPublicStableAttachmentURLShapes),从未被 ci.yml 的 e2e 覆盖。
  // 【边界】两套实现并存:ticket 版保留给旧的无文件名 URL(/assets/files/{id})继续有效,
  // 浏览器侧现在拿到的是公开版。fork 明确的知情取舍:assetID 泄露即可读取,已缓存内容
  // 在成员移除后仍有效。完整取舍见 cli/internal/core/attachments.go 的【本地改动
  // 2026-08-18】节,以及 AGENTS.md「已知的 fork / upstream 行为分歧」。
  // 【合并提醒】合回 upstream 时:恢复下面两处 toContain('access=')、删除 toMatch 形状
  // 断言与两条 not.toContain('access='),并把测试名改回 signed asset URLs。
  test('renders remote server attachments through the fork public asset URLs', async ({
    page,
    chatPage
  }) => {
    await createAndLoginTestUser(page);
    await chatPage.goto();
    await ensureServiceWorkerControlsPage(page);

    remoteServer = await startSecondServer(test.info());
    const baseURL = remoteBaseURL(remoteServer);
    const remoteUser = await createUserOnRemote(baseURL, 'remoteassetuser', 'password123');
    const roomId = await getRoomOnRemote(baseURL, remoteUser.token, 'general');
    const body = `Remote attachment ${Date.now()}`;

    const remotePost = await postMessageAttachmentOnRemote(
      baseURL,
      remoteUser.token,
      roomId,
      body,
      'e2e/fixtures/brighton.jpg',
      'brighton.jpg',
      'image/jpeg'
    );

    expect(remotePost.attachmentUrl).toContain('/assets/files/');
    // fork 形态:/assets/files/{assetID}/{尾段}.<ext> —— 无 query 串,与上游 ticket 版相反。
    expect(remotePost.attachmentUrl).not.toContain('access=');
    expect(remotePost.attachmentUrl).toMatch(
      /\/assets\/files\/[^/?#]+\/.+\.[a-z0-9]+/i
    );

    await connectRemoteInstance(page, { ...remoteServer, baseURL }, remoteUser.userId);
    await page.goto(routes.remote.room('127.0.0.1', roomId));
    await waitForRoomReady(page, 'general');
    await expect(page.getByText(body)).toBeVisible();

    const attachmentImage = page
      .locator(`[data-event-id="${remotePost.eventId}"] button[aria-label^="View"] img`)
      .first();
    await expect(attachmentImage).toBeVisible();
    await expect
      .poll(() =>
        attachmentImage.evaluate((element) => (element as HTMLImageElement).naturalWidth)
      )
      .toBeGreaterThan(0);

    const src = await attachmentImage.getAttribute('src');
    expect(src).toBeTruthy();
    // 同本 test.describe 顶部的【本地改动】:上游此处断言 toContain('access='),fork 是公开 URL。
    expect(src).not.toContain('access=');

    const srcUrl = new URL(src!, page.url());
    const expectedRemoteUrl = new URL(baseURL);
    expect(srcUrl.protocol).toBe(expectedRemoteUrl.protocol);
    expect(srcUrl.port).toBe(expectedRemoteUrl.port);
    expect(srcUrl.pathname).toContain('/assets/files/');
    expect(srcUrl.pathname).not.toContain('/__chatto/');
  });
});
