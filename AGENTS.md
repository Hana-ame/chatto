# Instructions for Agents

Read this file first. It contains repo-wide rules that should not be hidden in
path-specific guidance.

## Product Boundaries And Instruction Routing

This repository contains two independent products plus an incubating shared
framework boundary:

- **Chatto** is the chat server, bundled client, CLI, and existing public
  protocols. Unless a path is explicitly Authling-owned or shared, existing
  repository content belongs to Chatto.
- **Authling** is the independent identity-provider product under `authling/`.
  It is not a Chatto component, runtime unit, feature, or deployment mode.
- **Shared framework code** is application-neutral event-sourcing, embedded
  NATS, data-cryptography, and configuration-loading machinery intended for
  consumption by both products. The independently versioned but unstable
  modules live under `pkg/events/`, `pkg/natsruntime/`, `pkg/datacrypto/`, and
  `pkg/appconfig/`.

Authling's presence in this repository is explicitly temporary. It is being
incubated here only while Authling provides the concrete second application
needed to extract and harden the shared framework. Once that boundary is
stable, Authling is intended to move to its own repository. Do not describe
this repository as Authling's permanent home, and do not introduce coupling
that would make the eventual extraction harder.

Before changing files, classify the task as Chatto, Authling, shared-framework,
or repository-wide work. Follow these routing rules:

1. Any task that concerns Authling or changes anything under `authling/` must
   read [`authling/AGENTS.md`](authling/AGENTS.md) in full before acting. Do
   this explicitly; do not assume nested instructions or skills were discovered
   automatically.
2. Authling behavior, architecture, features, vocabulary, and runtime inventory
   belong under `authling/docs/`. Do not put them in Chatto's `docs/adr/`,
   `docs/fdr/`, `docs/architecture/`, or `docs/GLOSSARY.md`.
3. Repository-local skills must live in the repository-root `.agents/skills/`
   directory. Agentic tools do not discover project skills under
   `authling/.agents/`. Authling skills must live under
   `.agents/skills/authling-<name>/`, use an `authling-` name, and state their
   Authling scope explicitly. These files are repository-level agent
   infrastructure, not product release inputs; Release Please excludes
   `.agents/skills/` from Chatto's root component. Global, plugin, and other
   configured skills remain applicable when their trigger rules match.
4. Existing Chatto documentation and skills are Chatto-specific unless their
   text explicitly says they are repository-wide or Authling-specific. Do not
   apply a Chatto workflow to Authling merely because it has the generic name
   `adr`, `fdr`, or `glossary`.
5. A shared-framework change must read both `cli/AGENTS.md` and
   `authling/AGENTS.md`, the target module's `AGENTS.md`, ADR-057, and the
   module-specific ADR: ADR-056 for `pkg/events`, ADR-058 for
   `pkg/natsruntime`, ADR-060 for `pkg/datacrypto`, or ADR-061 for
   `pkg/appconfig`. Shared packages must not import either product's domain,
   configuration, protobuf envelopes, subjects, resource names, or lifecycle
   policy.
6. Cross-product decisions may be recorded in root ADRs. Product-specific
   decisions must stay with their product. ADR-057 is repository-wide because
   it defines the monorepo boundary; that does not make other Authling ADRs
   Chatto ADRs.
7. Chatto and Authling product code and documentation have independent
   versions, changelogs, release pull requests, tags, binaries, and release
   notes. Never include one product in the other's release artifacts or
   documentation by default. Future artifact types such as container images
   also remain product-owned when introduced. Root-level CI, workspace,
   release, and agent-discovery files are repository infrastructure rather than
   either product's release payload.
8. Keep Authling-owned implementation and documentation beneath `authling/`
   except for the minimum repository-wide workspace, CI, release, instruction,
   and shared-framework integration points. Optimize those exceptions for
   deletion or relocation when Authling leaves this repository.

If a task crosses these boundaries, keep the product impacts explicit in code,
tests, documentation, and the final report. Do not use a cross-product task as
permission to reorganize unrelated product code.

## Where Context Lives

- [README.md](README.md) — general project overview.
- [authling/AGENTS.md](authling/AGENTS.md) — mandatory Authling product,
  architecture, documentation, security, and testing rules.
- [authling/docs/README.md](authling/docs/README.md) — Authling-owned ADR, FDR,
  architecture, and glossary entry points.
- [pkg/events/AGENTS.md](pkg/events/AGENTS.md) — shared event-framework module
  boundary, compatibility, and verification rules.
- [pkg/natsruntime/AGENTS.md](pkg/natsruntime/AGENTS.md) — shared embedded-NATS
  lifecycle module boundary and verification rules.
- [pkg/datacrypto/AGENTS.md](pkg/datacrypto/AGENTS.md) — shared authenticated
  encryption and key-wrapping boundary and verification rules.
- [pkg/appconfig/AGENTS.md](pkg/appconfig/AGENTS.md) — shared TOML and
  environment configuration-loading boundary and verification rules.
- [cli/AGENTS.md](cli/AGENTS.md) — Go backend, ConnectRPC, NATS/JetStream, authz, live events, backup/restore, and backend tests.
- [apps/frontend/AGENTS.md](apps/frontend/AGENTS.md) — SvelteKit frontend, Tailwind, i18n, browser verification, frontend tests, e2e, and Storybook.
- [proto/AGENTS.md](proto/AGENTS.md) — protobuf and generated public API reference guidance.
- [proto/chatto/api/v1/AGENTS.md](proto/chatto/api/v1/AGENTS.md) — public ConnectRPC API consistency rules for `chatto.api.v1`.
- [proto/chatto/admin/v1/AGENTS.md](proto/chatto/admin/v1/AGENTS.md) — administrative ConnectRPC API consistency rules for `chatto.admin.v1`.
- [proto/chatto/auth/v1/AGENTS.md](proto/chatto/auth/v1/AGENTS.md) — public authentication and capability-token API consistency rules.
- [proto/chatto/discovery/v1/AGENTS.md](proto/chatto/discovery/v1/AGENTS.md) — unauthenticated discovery and bootstrap API consistency rules.
- [proto/chatto/realtime/v1/AGENTS.md](proto/chatto/realtime/v1/AGENTS.md) — realtime WebSocket protobuf protocol rules for `chatto.realtime.v1`.
- [apps/desktop/AGENTS.md](apps/desktop/AGENTS.md) — desktop integration and native-helper testing guidance.
- [apps/docs-website/AGENTS.md](apps/docs-website/AGENTS.md) — public docs website guidance.
- `.agents/skills/**` — discoverable workflow skills. Skills prefixed
  `authling-` are Authling-specific; existing generic and `chatto-` skills are
  Chatto-specific unless their text explicitly says otherwise.
- `docs/fdr/INDEX.md` — Chatto feature behavior and rationale.
- `docs/adr/INDEX.md` — Chatto and explicitly repository-wide architecture
  decisions.
- `docs/architecture/INDEX.md` — current Chatto runtime inventory, split by
  components, projections, NATS resources, subjects, runtime state, effects,
  interfaces, and realtime delivery.
- `docs/GLOSSARY.md` — canonical Chatto terminology.

## Instruction Strength

- **Must** and **never** mark requirements, safety boundaries, or invariants.
- **Prefer** marks the default; depart from it only with a concrete reason.
- **Consider** marks a review prompt rather than a required action.
- The nearest applicable `AGENTS.md` owns path-specific guidance. Root rules
  still apply when nested guidance is more specific.

## Prime Directives

- Prefer simple, clear changes over clever abstractions.
- Add concise code documentation for public APIs and for otherwise important
  fields, functions, types, invariants, and lifecycle behavior that future
  maintainers should not have to infer from call sites.
- Keep tests and documentation up to date when changing behavior.
- Run verification that would actually catch regressions in the area touched.
- Never claim full verification when only a partial signal was run.
- Never silence lint, type, vet, or Svelte warnings as a routine fix. Fix the
  cause; discuss rare scoped exceptions before adding them.
- Never log PII: no raw login names, display names, email addresses, submitted
  auth identifiers, OAuth/OIDC provider subjects, tokens, passwords, auth codes,
  reset links, raw IPs, or full query strings.
- Never expose NATS or JetStream storage coordinates through normal client or
  integration APIs. Public cursors and tokens must not reveal stream names or
  incarnations, subjects, sequence numbers, revisions, consumer positions, or
  equivalent internal facts, including through reversible encodings such as
  base64. Opaque coordinates must be integrity-protected and confidential;
  bind them to their viewer/resource scope where applicable, and reject or
  safely reset when validation fails. Explicit owner-only broker diagnostics
  and event-log inspection APIs are the sole exception: their operational
  purpose and fields must clearly identify the NATS/JetStream details exposed.
- Treat optional operational telemetry as best-effort: its failure must not make
  broader diagnostics unavailable. Preserve an explicit unavailable state across
  API and UI boundaries instead of replacing unknown values with healthy-looking
  zeroes, empty strings, or timestamps.
- Chatto is public, self-hosted, pre-1.0 software with real user data and mixed
  versions in use. Follow ADR-045 and `proto/AGENTS.md` for public and persisted
  protocol compatibility. A breaking experimental API change requires explicit
  user approval and a compatibility plan; a release milestone does not waive
  that requirement.

## Tooling

Tools are managed by `mise`; prefer tasks when available.

```sh
mise test
mise test-cli
mise test-events
mise test-natsruntime
mise test-datacrypto
mise test-appconfig
mise test-frontend
mise test-e2e
mise codegen
mise codegen-proto
(cd authling && mise test)
(cd authling && mise test-e2e)
(cd authling && mise build)
```

Run Authling's unprefixed tasks from `authling/`; its nested `mise.toml` owns
the Authling toolchain and workflow.

For ad-hoc tool invocations, use `mise x -- ...` rather than assuming `go`,
`pnpm`, `node`, or related binaries are on `PATH`.

When an agent needs the long-running development stack, launch `mise dev`; the
task runs the child processes through `tools/dev-supervisor.sh` so lifecycle
signals reach them directly. Stop it before handing control back to the user.
Never leave a dev stack running in a detached or yielded terminal session.

## Chatto Documentation Updates

- Use FDRs for feature behavior/rationale and ADRs for cross-cutting decisions.
- Update the relevant file in `docs/architecture/` when changing runtime
  components, projections, EVT events or subjects, NATS resources, runtime
  state, durable effects, realtime delivery, or mounted ConnectRPC services.
- Update `docs/GLOSSARY.md` when introducing, renaming, or clarifying canonical
  vocabulary.
- Update the docs website when changing user-facing features, config,
  deployment behavior, or public APIs.
- Keep `NOTICE` current when adding, removing, or materially changing bundled
  dependencies or shipped assets.

## License Metadata

- Chatto uses REUSE/SPDX license metadata. Keep `mise license-check` passing
  when adding files or changing license boundaries.
- Files are AGPL-3.0-or-later by default unless `REUSE.toml`, an SPDX header,
  or an adjacent `.license` file says otherwise.
- Apache-2.0 applies to the independently versioned shared framework modules
  under `pkg/events/`, `pkg/natsruntime/`, `pkg/datacrypto/`, and
  `pkg/appconfig/`, the framework-neutral `packages/lingua` runtime, plus
  explicit integration and documentation surfaces such as the standalone
  frontend source and image, public protocol/API definitions, generated
  TypeScript API clients, documentation, and examples.
- The Chatto server, CLI, and bundled server release artifacts should stay
  AGPL-3.0-or-later unless the license boundary is deliberately changed.

## Issues, Commits, And PRs

- Use or update GitHub issues only when the user asks for issue or roadmap
  management, or when an explicitly invoked workflow requires it.
- Use Conventional Commit format for commits and PR titles, for example
  `fix(api): ...` or `feat(frontend)!: ...`. Only mark breaking changes when
  they really are breaking.
- Always create pull requests as full, ready-for-review PRs. Create a draft PR
  only when the user explicitly asks for a draft.
- PR bodies should summarize changes and link relevant FDRs, ADRs, glossary
  terms, and issues.
- If a PR closes an issue, include a GitHub closing keyword such as
  `Closes #123.` in the body.
- When using `gh` for multiline PR/issue bodies, write Markdown to a file/stdin
  and use `--body-file`; do not pass escaped `\n` to `--body`.
- Do not rename the current branch unless explicitly asked.

## Chatto 部署 (cloudcone) — 绝对禁止本地编译

- **绝对禁止在本地编译 chatto 二进制再上传**。本地 `go build` 不带前端
  (`.client` 为空，无 `200.html`)，部署后 SPA 全部 500 ("Failed to load app")。
  2026-08-08 曾因此事故。
- **永远不要用本地 `go build`/`go vet`/`mise test-cli` 等来验证编译或跑测试**：
  本地构建产物不内嵌 bundled client (`clients/` 未编译进二进制)，编译通过 ≠
  可用产物；本地测试也不能代表 CI 环境。验证直接靠 GitHub Actions：
  改动完成后 push 到 `ci/deploy`，看 `build-release` workflow 结果。
- 部署 chatto 的唯一正确来源：**GitHub Actions `build-release` workflow
  (push 到 `ci/deploy` 分支触发) 的 rolling release**，产物内嵌前端。
- 部署流程：
  1. `git push origin ci/deploy`（可含提交改动）触发 `build-release`。
  2. `gh run watch <run-id> -R Hana-ame/chatto --exit-status` 等构建成功。
  3. **用 `curl -L` 下载 release 资产**（rolling release tag 固定为 `ci/dev`，
     每次构建删除重建，永远指向最新）：
     ```sh
     curl -fL -o /tmp/chatto_Linux_x86_64.tar.gz https://github.com/Hana-ame/chatto/releases/download/ci/dev/chatto_Linux_x86_64.tar.gz
     tar -xzf /tmp/chatto_Linux_x86_64.tar.gz -C /tmp/opencode chatto
     ```
  4. 经 `~/script/ssh/cloudcone.sh` 上传：先写 `/opt/chatto/chatto.new`，
     再 `systemctl stop chatto && mv .new chatto && systemctl start chatto`。
  5. 验证：`curl 127.0.0.1:4000` 与 `curl -k https://chatto.moonchan.xyz/`
     均应 200，`journalctl -u chatto | grep -c 200.html` 为 0。
- cloudcone 上 chatto：`/opt/chatto/chatto`，配置 `/opt/chatto/chatto.toml`
  (`bind_address = '127.0.0.1'`)。入口：DNS `chatto.moonchan.xyz` → Cloudflare
  → cloudcone nginx:443 (`/etc/nginx/sites-available/chatto.conf`) →
  `proxy_pass http://127.0.0.1:4000`。**与 zen 8443 无关**。改配置先备份到
  `chatto.toml.bak.*`。

## 本地改动中文注释（硬性要求）

本仓库是 `Hana-ame/chatto` fork，在 upstream（`chattocorp/chatto`）之上有
一批本地独有改动（AVIF 附件重编码、bind_address、fork 链接、build-linux
workflow 等）。本地改动的去向是 `ci/deploy` 分支，会持续与 upstream `main`
合并。**所有本地改动必须就地加中文注释**，写明当时的思路、目的、踩坑。
合并时注释会被保留；没有注释的本地代码一旦在冲突中被上游版本覆盖，
改动就无声无息地丢了。

### 如何读注释

- 代码里出现 `【本地改动 <短hash>】`（Go 注释、YAML/TOML `#`、Svelte
  `<!-- -->` 均可）＝该处是本 fork 独有，merge 上游时必须保留并重新审视。
- 没有该标记的代码默认是上游代码，不要误以为它是本地功能。
- 引用 hash 是引入该改动的本地 commit 短 hash（如 `32e1f566`、
  `218426d6`、`92d33bff`），可用 `git show <hash>` 回溯完整思路。
- merge 冲突时：本地标记的 hunk 冲突＝上游改了同一处，按语义决定保留
  本地行为还是跟上游，并在结果注释里写明取舍（如
  "merge upstream 时保留本地字段，同时跟随上游删除 …"）。

### 如何加注释

- 就地加在改动处附近（字段/函数/调用点旁），不另开文档。
- 格式跟随所在语言：Go 用 `//`，YAML/TOML 用 `#`，Svelte 用 `<!-- -->`。
- 以 `【本地改动 <短hash>】` 开头（hash 可后补，先写思路），
  后面接思路/目的/踩坑正文。
- 保持格式合法：Go 注释不要破坏 gofmt 缩进（struct 字段间的注释要
  tab 对齐），YAML 缩进不要破坏语法。
- 引用 commit hash 时用「发现/引入于 <日期>」辅助定位（如
  "2026-08-14 在合并 upstream main 后跑测试时发现"）。

### 写什么注释

注释必须包含（按重要性排序）：

1. **目的**：这段代码解决什么问题、在什么场景下触发。
2. **思路**：为什么这么设计（选这个方案而非别的）。
3. **踩坑**：踩过的坑、怎么发现的（测试/E2E/线上）、怎么修的、
   复现方式。这是最有价值的部分。
4. **边界**：影响范围（如"只影响 room 附件，头像/链接预览始终 WebP"）。
5. 测试函数必须标注「发现背景」：这个测试保护什么 bug、bug 怎么被发现、
   修复方式（对回归/防御性测试是硬性要求）。

### 必须加注释的场景

- 任何本地新增/修改的代码（Go、Svelte、YAML、TOML、测试）。
- 解决 merge 冲突后，给被保留的本地代码补注释说明取舍。
- 修改上游代码时：保留上游原有注释，把本地改动作为新增注释叠加，
  不要删上游注释（上游合并回来时会再次冲突）。
- 新增配置项、工作流、部署步骤时同样要写，不限于 Go 代码。

## 合并 upstream 后的语义冲突审计（硬性要求）

每次从 upstream `main` 合并进 `ci/deploy` 后、push/部署之前，必须做一次
**语义冲突审计**——git 报告零冲突 ≠ 安全：上游经常重命名符号、重构结构体、
改同文件的相邻区域，文本上不冲突但语义上可能已经打架。

### 审计步骤

1. **找交集文件**：upstream 本次动过的文件 ∩ 含 `【本地改动】` 标记的文件：

   ```sh
   git diff --name-only MERGE_SHA^1 MERGE_SHA > /tmp/upstream_files.txt
   grep -rl '本地改动' cli internal apps --include='*.go' -r | sort
   # 手工或脚本取两列表交集
   ```

2. **逐个核验交集文件的语义兼容性**，重点回答：
   - 上游是否重命名/删除了本地代码引用的符号？
     （例：2026-08-23 合并 #2087 把 `cookieCredentialFromSession`
     改名为 `cookieCredentialIDFromSession`，本地的 `/assets/*` 跳过
     Set-Cookie 逻辑依赖它的行为语义）
   - 上游的行为变更是否让本地改动失去意义或产生反效果？
     （例：上游删掉某条路径上的 cookie 下发，本地的对应 skip 是否还需要）
   - 上游重构的字段/函数与本地新增字段是否仍在同一处被初始化/消费？

3. **最小改动审计**：`git diff origin/main..HEAD --name-only` 应当只包含
   有明确用途的 fork 差异；出现来历不明的杂散文件要在报告里点名，
   由人决定去留（不要擅自删）。

4. 审计结果写进最终报告（表格：文件 × 上游改动 × 本地改动 × 判定）；
   发现有实际语义冲突的，修复时按「如何加注释」规则补记取舍。

## Testing Judgment

- Pick the lowest test layer that exercises the change, but do not stop below
  the layer where the bug could occur.
- When testing an early rejection, use input that would fail a later check. The
  test should still return the early error.
- Choose additional integration or end-to-end coverage when the regression can
  occur only across component or process boundaries.
