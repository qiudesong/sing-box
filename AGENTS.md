# 二开仓库维护约定

本仓库是 `SagerNet/sing-box` 的二开仓库。上游地址：
https://github.com/SagerNet/sing-box 。

## 分支

| 分支 | 初始来源 | 维护方式 |
| --- | --- | --- |
| `main` | 上游 `stable` | 默认分支，保存二开代码、维护文档和本仓库的 CI/发布配置 |
| `stable` | 上游 `stable` | 纯镜像，每小时强制同步；不得提交二开代码 |
| `testing` | 上游 `testing` | 纯镜像，每小时强制同步；不得提交二开代码 |
| `feature/*`、`fix/*` | `main` | 完成开发和验证后，通过 PR 合并回 `main` |

- 上游 remote 名称为 `upstream`，本仓库 remote 名称为 `origin`。
- `stable/testing` 会被覆盖，不能作为二开功能的长期存放位置。
- `main` 不自动合并上游，也不做强制重置。通过 `stable -> main` 的更新 PR 接入上游稳定版，解决冲突后验证。
- 更新长期分支时保留共同历史，使用 merge；不要 rebase 或强推已共享、已发布的 `main` 历史。
- 如需上游 testing 的个别修复，经过评估后 cherry-pick；不要直接把整个 testing 合并到稳定二开分支。
- 上游变更与本仓库 CI 冲突时，保留本仓库的分支、tag、镜像地址和发布策略。

## 上游同步

`.github/workflows/sync-upstream.yml` 位于默认分支 `main`，每小时第 17 分钟执行，也支持手动执行。

- 将本仓库 `stable/testing` 分别强制同步到上游同名分支，包括上游回退、改写历史的情况。
- 原样导入上游 tags；已有 tag 指向不一致时失败，不覆盖、不删除已有 tag。
- 同步任务不得创建、覆盖或删除 `*-custom` tag，不创建上游版本的 GitHub Release。
- 默认使用 `GITHUB_TOKEN`。如同步 workflow 文件遇到权限拒绝，配置仅授权本仓库、具有 Contents 和 Workflows 写权限的 `UPSTREAM_SYNC_TOKEN` Secret。
- 分支保护和 rulesets 必须允许同步身份更新、强推镜像分支；`main` 应禁止强推，并要求 PR/CI。
- GitHub 定时任务可能延迟；公开仓库长期无活动时可能自动停用，需在 Actions 页面检查并重新启用。
- 上游的 stale issues 定时任务仅允许在上游仓库运行，避免默认分支切换后自动关闭本 fork 的 issue/PR。

## Tag 和版本

- 上游 tags（如 `v1.14.4`）保留原名和原始指向，只能指向上游提交。
- 所有二开发行 tag 必须以 `v` 开头、以 `-custom` 结尾，例如：
  - `v1.14.4-custom`：基于上游 1.14.4 的首次二开发行。
  - `v1.14.4-2-custom`：同一上游基础上的第二次发行；后续递增数字。
  - `v1.15.0-alpha.13-custom`：基于上游预发布版的二开发行。
- 发布 tag 必须指向已经进入 `origin/main` 的确定提交；已发布的 tag 永远不能移动、覆盖或复用。
- 日常构建使用 `1.14.4-dev.N.gCOMMIT-custom` 这类版本，只保留 Actions Artifacts，不推送日常构建 tag。
- `-custom` 是二开标识。SemVer 工具会把它视为预发布字段，不能用该后缀自动判断 GitHub Release 是否稳定。
- `.github/custom_version.sh` 是本仓库构建版本的来源。不要用上游的 nightly 版本推导逻辑替换它，否则可能丢失 custom 后缀或 revision。
- APK 内部版本通过 `.github/apk_version.sh` 转换成支持的 `_pN` 等后缀；附件名及二进制版本保留完整 custom 标识。

## 构建与发布

1. 完成 `main` 的功能及上游更新 PR，确认 CI 通过。
2. 拉取最新 main，在确定的提交上创建 annotated tag，例如 `v1.14.4-custom`，只推送该 tag。
3. `Build` workflow 响应 main 推送和 `v*-custom` tag 推送：main 生成日常构建产物，tag 构建完成后创建 Draft Release。
4. 检查 Draft Release 的附件、版本和变更说明，写明上游基础 tag/SHA、二开改动和已知问题，再发布。
5. 基于 alpha/beta/rc 的 Release 标记为 Pre-release；验证通过的稳定二开版可手动设为 Latest。
6. 发布后有修复时创建新的 revision tag，不替换已发布 Release 的构建附件。

手动运行 `Build` 时，version 填已经存在的 custom tag（可带 v），默认选择 Binary；所有构建 job 必须使用该 tag 解析出的同一个提交，而不是运行时 main 的最新提交。

- 核心二进制和 DEB/RPM/APK 等包默认无需上游签名密钥。包名使用 `sing-box-custom`，二进制命令仍为 `sing-box`。
- Android、桌面和 Apple 客户端保留手动构建入口，使用前需配置自己的签名证书、应用标识、平台凭据和相应 runner；自动核心构建不触发这些发布操作。
- Docker workflow 只发布验证过的 custom tag，目标为本仓库的 `ghcr.io/<owner>/<repo>`，不会发布到 SagerNet 的镜像仓库。
- Docker 稳定渠道为 `latest-custom`，预发布渠道为 `latest-custom-beta`，同时保留完整版本镜像 tag。
- `Build Linux Packages` 为手动补充包构建入口，附件上传到对应的本仓库 Draft Release，不上传到上游 Fury；发布后禁止替换附件。
- 本地 `make release`（兼容入口 `make release_repo`）校验已有 custom tag 后触发 GitHub 的 Binary 发布构建，不使用上游 Fury/goreleaser 配置。
- 原生客户端签名与打包仍需要各平台真实构建验证；不要把 shell/YAML 检查等同于发布构建成功。

## 修改后的验证

- 修改发布逻辑时运行 `.github/tests/test_release_scripts.py`，覆盖版本推导、发布来源、不可移动 tag、渠道和 APK 版本转换。
- 用 actionlint 验证修改后的 workflow，用 `bash -n` 检查修改过的 shell 脚本。
- 功能代码变更运行适当的 Go 测试；纯分支、文档、CI 变更无需全量构建所有平台。
- 在本地交互式终端执行命令时遵循用户的 RTK 约定，命令以 `rtk` 开头；CI runner 内不要求安装 RTK。
