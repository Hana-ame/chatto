# Main 与 Upstream 分歧概览

**生成时间**: 2026-01-XX  
**本地分支**: main (Hana-ame/chatto)  
**上游分支**: upstream/main (chattocorp/chatto)

## 提交差异统计

- **上游领先**: 2 个提交（最新: `eee27b7f6 fix(frontend): hide inactive preview scroll fades (#2261)`）
- **本地领先**: 85 个提交（从合并 ci/deploy 到 main 开始累积）
- **最后合并点**: `8875c9458 Merge remote-tracking branch 'upstream/main'`

## 改动文件统计

| 类别 | 文件数量 | 说明 |
|------|---------|------|
| Go 后端代码 | 28 | cli/internal/**, cli/cmd/** |
| 前端源码 (Svelte/TS) | 15 | apps/frontend/src/** |
| 前端测试 | 10 | *.spec.ts, *.test.ts |
| CI/CD 工作流 | 4 | .github/workflows/** |
| 文档 | 2 | docs/fdr/, realtime-v2-migration.md |
| 配置文件 | 1 | cli/chatto.toml |
| **总计** | **60** | 含本地改动标记的文件 |

## 三大核心行为分歧（长期有效）

这些是 git 无冲突但语义上故意不同的设计决策，每次合并必须重审：

### 1. 消息正文内联图片渲染

- **上游做法**: 禁用 `![alt](url)` 语法，不出 `<img>` 标签（安全默认）
- **Fork 做法**: 允许渲染，src 重写为 `https://proxy.moonchan.xyz/...` 代理取图，隐藏观看者 IP/Referer
- **关键文件**: 
  - `apps/frontend/src/lib/markdown.ts` (`proxyImageSource`)
  - `apps/frontend/src/lib/components/MessageContent.svelte.spec.ts`
- **安全风险**: 已评估接受 302 重定向透传风险（见 markdown.ts 注释），待 proxy 部署方处理
- **防护**: validateLink 拦截 javascript:/data:/file:，锁死 http(s)，加 loading=lazy + referrerpolicy=no-referrer

### 2. 浏览器侧附件 URL 访问控制

- **上游做法**: 携带 per-user 签名 ticket (`?access=`)，per-user、不可共享、不可 CDN 缓存；服务端校验签名用户仍是 room 成员，kick/leave 可撤销未来访问
- **Fork 做法**: 公开版 `/assets/files/{assetID}/{fn.ext}`，无 ticket、无成员校验，assetID 即凭证，`public, max-age=31536000, immutable`
- **关键文件**:
  - `cli/internal/core/attachments.go` (`GetPublicStable*`, `stableAttachmentPath`)
  - `cli/internal/http_server/assets.go` (`servePublicStableAttachment`)
  - `apps/frontend/e2e/authorized-asset-urls.test.ts`
  - `apps/frontend/e2e/messages.test.ts`
- **影响**: 涉及访问控制与撤销语义（kick/leave 不再撤销未来访问），比图片分歧更重

### 3. 消息正文 LaTeX 公式渲染

- **上游做法**: 禁用 `$...$` / `$$...$$` 语法，不出公式（聊天产品不支持数学排版）
- **Fork 做法**: 启用 KaTeX 渲染，`$...$` 行内 / `$$...$$` 独立行；懒加载 katex（JS + CSS），首屏 bundle 零开销；未启用 mhchem/html 插件，throwOnError=false 防崩溃；行内公式要求内容含字母或 LaTeX 运算符才触发，避免 `$10` 等金额误识别
- **关键文件**:
  - `apps/frontend/src/lib/markdown.ts` (`mathInline`, `replaceMathPlaceholders`)
  - `apps/frontend/src/lib/components/MessageContent.svelte.spec.ts`
- **引入时间**: 2026-09-01

## 其他主要本地改动分类

### AVIF 附件重编码
- **文件**: `cli/internal/assets/avif.go`, `cli/internal/assets/images.go`
- **目的**: 支持 AVIF 格式附件上传和转换
- **测试适配**: `cli/internal/assets/avif_test.go`, `cli/internal/core/media_model_test.go`

### 服务器绑定地址配置
- **文件**: `cli/chatto.toml`, `cli/internal/config/process.go`
- **改动**: 添加 `bind_address` 配置项，支持绑定到特定地址（如 127.0.0.1）
- **部署**: cloudcone 上使用 `bind_address = '127.0.0.1'`，通过 nginx 反向代理

### Fork 品牌与外部链接
- **文件**: 
  - `apps/frontend/src/routes/chat/modals/AboutChattoModal.svelte`
  - `apps/frontend/src/lib/ui/AppHeader.svelte`
  - `links.json`
- **改动**: 
  - 添加 GitHub icon 指向 Hana-ame/chatto 仓库
  - 添加 nyaa.moonchan.xyz 外部链接
  - 替换 placeholder 链接为真实的 EX-mirror 和 push-image 条目
  - 使用推图 PWA manifest icon

### UI 布局调整（Server Gutter）
- **文件**: 
  - `apps/frontend/src/lib/ServerGutter.svelte`
  - `apps/frontend/src/lib/components/MobileSidebarChrome.svelte`
  - `apps/frontend/src/lib/components/chat/Chrome.svelte`
- **改动历史**: 
  - 最初移除 server-gutter sidebar
  - 后改为隐藏而非删除
  - 最终恢复 hamburger 菜单，仅在非服务器页面隐藏 server-gutter
  - 修复移动端汉堡菜单在非服务器路由的行为

### 通知系统优化
- **文件**: 
  - `cli/internal/core/notification_occurrence_model.go`
  - `cli/internal/core/notification_decision_projection.go`
  - `cli/internal/core/notification_occurrence_model_test.go`
- **改动**: 
  - 通过 stream retention range 确认 signal 真正 absent 后再标记 cleaned
  - 停止重试已 absent signals 的物理删除
  - 适配上游 #2258 的通知删除确认逻辑

### CI/CD 工作流调整
- **文件**: `.github/workflows/ci.yml`, `.github/workflows/build-linux.yml`, `.github/workflows/release.yml`, `.github/workflows/build-docs-image.yml`
- **改动**:
  - build-release 触发分支从 ci/deploy 改为 main
  - 串行化 ci.yml 和 build-release（避免并发冲突）
  - 跳过 upstream-only jobs（docs image workflow 不推 chattocorp GHCR）
  - rolling release tag 固定为 `ci/dev`，每次构建删除重建
  - 部署流程改为在 cloudcone 上直接下载产物（避免本机代理和 scp 不稳定问题）

### API 类型生成适配
- **文件**: 
  - `packages/api-types/src/chatto/api/v1/member_directory_*.ts`
  - `packages/api-types/src/chatto/api/v1/user_service_pb.ts`
  - `proto/chatto/api/v1/member_directory.proto`
  - `proto/chatto/api/v1/user_service.proto`
- **原因**: 上游 #2259 重构 UserService protobuf source，生成本地 TypeScript API 类型需重新生成并适配

### 文档更新
- **文件**: 
  - `docs/fdr/FDR-008-file-attachments-and-video.md`
  - `apps/docs-website/src/content/docs/releases/0-5-0.mdx`
  - `apps/docs-website/src/content/docs/guides/integrations/api-compatibility.mdx`
  - `realtime-v2-migration.md`
- **内容**: 记录 fork 特有的附件 URL 语义、API 兼容性指南、realease notes

## 合并风险点

### 高风险区域（需逐文件审计）

1. **`cli/internal/core/attachments.go`** - 附件 URL 生成逻辑，上游可能收紧鉴权
2. **`apps/frontend/src/lib/markdown.ts`** - 图片和 LaTeX 渲染，上游可能升级为安全修复
3. **`cli/internal/http_server/assets.go`** - 静态资源服务，fork 的公开附件端点
4. **`apps/frontend/e2e/authorized-asset-urls.test.ts`** - E2E 测试断言 fork 的公开 URL 语义
5. **`apps/frontend/e2e/messages.test.ts`** - E2E 测试断言 fork 的图片代理语义

### 中等风险区域

- **通知系统** - 上游持续优化，fork 的 absent-check 逻辑需保持同步
- **UI 组件** - ServerGutter、MobileSidebarChrome、AppHeader 等布局组件，上游可能有新交互
- **API 类型** - 上游 protobuf 重构频繁，需重新生成并检查兼容性

### 低风险区域

- **CI/CD 工作流** - fork 独有，上游改动不影响
- **品牌与链接** - fork 独有，上游改动不影响
- **AVIF 支持** - fork 独有功能，除非上游也添加 AVIF

## 维护建议

1. **合并频率**: 当前落后上游仅 2 个提交，建议尽快合并以避免漂移
2. **语义审计**: 每次合并后必须执行 AGENTS.md 规定的语义冲突审计步骤
3. **注释完整性**: 确保所有本地改动都有 `【本地改动 <hash>】` 标记
4. **测试覆盖**: fork 的三个核心分歧必须有对应的 E2E 测试保护（当前已有）
5. **安全审查**: 图片代理 302 风险和附件公开 URL 的访问控制需定期重审

## 相关链接

- [AGENTS.md - 已知 fork/upstream 行为分歧](AGENTS.md#已知的-fork--upstream-行为分歧长期有效每次合并必须逐条重审)
- [GitHub Actions - build-release workflow](https://github.com/Hana-ame/chatto/actions/workflows/build-release.yml)
- [cloudcone 部署脚本](~/script/ssh/cloudcone.sh)
