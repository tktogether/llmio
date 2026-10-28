# 维护流程

本文档说明本 fork（`tktogether/llmio`）与上游（`atopos31/llmio`）并行维护时的分支职责、提交路径、同步方式、冲突处理规则和发布流程。

适用于三类工作：

1. 只交给上游的修复；
2. 只保留在本 fork 的功能（如转发透传）；
3. 两边都要的改动。

---

## 1. 前提

| 项 | 值 |
|---|---|
| `origin` | `tktogether/llmio`（本 fork） |
| `upstream` | `atopos31/llmio`（上游） |
| 本 fork 的生产部署 | `beta` 分支 push 后由 `beta.yml` 构建镜像并更新 Sealos 上的 `statefulset/llmio` |
| 上游的发布方式 | `cicd.yml` 在 tag 推送时构建并推送 Docker 镜像，同时重启上游自己的生产集群 |

上游的接受习惯（依据历史 PR）：只接受小颗粒、单一目的的改动；大而全的 PR 会被关闭。转发透传功能（`handler/provider_route.go`、`providers/forward.go`、`webui/src/routes/forwarding/*`）已明确拒绝，属于本 fork 的专有功能，不再整体提交上游。

上游目前仍在密集发版（`v0.10.x`），因此需要定期同步，不能长期分叉。

### 1.1 上游权限的核实

截至 2026 年 10 月，本账号对上游持有 `push` 权限（`admin: false`），上游 `master` 没有分支保护，也没有 ruleset。核实命令：

```bash
gh api repos/atopos31/llmio --jq '.permissions'
gh api repos/atopos31/llmio/branches/master/protection
gh api repos/atopos31/llmio/rulesets
```

这意味着技术上可以直接推送上游 `master`，没有任何服务端拦截。误操作会立即生效，因此第 8 节的红线必须靠流程和执行纪律保证。

`admin: false` 表示不能修改上游的仓库设置、secrets 和 Actions 配置，但可以推送分支、推送 tag、创建并合并 PR。

---

## 2. 分支职责

| 分支 | 所在仓库 | 职责 | 写入规则 |
|---|---|---|---|
| `master` | fork | 上游镜像 | 只允许合并 `upstream/master`，以及 `beta.yml` 等 fork 专属 CI 提交。因存在这些提交，与上游分叉后无法快进，使用普通 merge |
| `beta` | fork | 本 fork 的功能发布线 | 只从功能分支或同步分支合并。push 即构建镜像并部署 |
| `fix/*`、`feat/*`（送上游） | fork | 单个上游改动 | 从 `upstream/master` 切出。不得包含 `beta.yml` 或透传代码 |
| `feat/*`、`fix/*`（fork 专有） | fork | 透传等功能 | 从 `origin/beta` 切出。PR 目标为 `beta` |
| `sync/upstream-<日期>` | fork | 一次性同步分支 | 从 `origin/beta` 切出，合并 `upstream/master`，测试通过后合入 `beta` 并删除 |

不建立长期的 fork 专用集成分支，也不向上游推送任何 fork 分支。

---

## 3. 一次性配置

```bash
# 关闭 upstream 的推送，防止误推 master 或误推 tag
git remote set-url --push upstream no-push

# 冲突解法复用：相同的冲突只需要解一次
git config rerere.enabled true
```

恢复 upstream 的推送能力（通常只在确实需要时）：

```bash
git remote set-url --push upstream git@github.com:atopos31/llmio.git
```

向 upstream 提交 PR 不需要推送分支到 upstream，`--head` 使用本 fork 的分支即可，因此关闭推送不影响正常的回贡流程。

所有 `gh` 命令必须显式带 `-R`。本仓库没有配置默认远程，不带 `-R` 时 `gh` 会解析到上游，写入类命令会直接作用于上游仓库。

---

## 4. 三类改动的流程

### 4.1 只交给上游的修复

```bash
git fetch upstream origin --prune
git switch -c fix/<描述> upstream/master

# 修改代码并补充测试

gofmt -l .                      # 必须为空
go vet ./...
go test -count=1 ./...
cd webui && pnpm run lint && pnpm run test:coverage && pnpm run build && cd ..

git add -A
git commit
git push -u origin fix/<描述>

gh pr create -R atopos31/llmio --base master --head tktogether:fix/<描述> \
  --title "<type>(<scope>): <摘要>" --body "<说明背景、改动与验证>"
```

提交前检查改动范围：

```bash
git diff --stat upstream/master...HEAD
```

输出只能包含本功能相关的文件。如果出现 `.github/workflows/beta.yml`，说明分支基线错误，需要重新从 `upstream/master` 切出。

PR 的 `test` 检查通过后自行合并。合并方式使用 merge commit，与上游现有历史保持一致，不要 squash。

合并后清理：

```bash
git push origin --delete fix/<描述>
git switch master
git merge upstream/master    # 不能用 --ff-only：master 上有 beta.yml 等 fork 提交
git push origin master
```

`master` 每次跟随上游前进都会产生一个 merge commit，这是预期行为，不要为了保持直线历史而 rebase 或 force-push。

### 4.2 fork 专有功能

```bash
git fetch origin --prune
git switch -c feat/<描述> origin/beta

# 修改代码并补充测试

git push -u origin feat/<描述>
gh pr create -R tktogether/llmio --base beta --head feat/<描述> \
  --title "<type>(<scope>): <摘要>" --body "<说明>"
```

合并后 `beta.yml` 自动构建镜像并部署。这一步不涉及 upstream。

新增 fork 专有功能时，同步更新本文档第 6.1 节的接缝文件清单。

### 4.3 两边都要的改动

拆成两个提交，顺序固定：

1. 从 `upstream/master` 切分支，完成完整改动，提交上游 PR 并合并；
2. 从 `master` 切分支，`git cherry-pick <上游提交>`，提交到 `beta`。

顺序不能颠倒。先合上游再 cherry-pick，能保证两边是同一份实现；反过来会产生两套写法，下次同步必然冲突。

---

## 5. 上游同步到 beta

不要直接在 `beta` 上合并上游。`beta.yml` 在 push 到 `beta` 时立即构建并部署，在 `beta` 上解决冲突的中间状态一旦推送就会上线。另外 `beta.yml` 的 `deploy-beta` 只依赖 `build-push-beta`，不依赖 `test.yml`，测试失败也不会阻止部署。

完整流程：

```bash
# 1. 获取最新引用
git fetch upstream origin --prune

# 2. 从 beta 切一次性同步分支
git switch -c sync/upstream-$(date +%Y%m%d) origin/beta

# 3. 合并上游
git merge upstream/master

# 4. 解决冲突（见第 6 节），然后完整验证
gofmt -l .
go vet ./...
go test -count=1 ./...
cd webui && pnpm run lint && pnpm run test:coverage && pnpm run build && cd ..
go test -coverprofile=coverage.out ./...
go tool cover -func=coverage.out | tail -1
rm -f coverage.out

# 5. 推送同步分支（不会触发部署）
git push -u origin sync/upstream-20261029

# 6. 对 beta 开 PR，等待 test 检查通过
gh pr create -R tktogether/llmio --base beta --head sync/upstream-20261029 \
  --title "chore: 同步上游 <上游短哈希>" \
  --body "同步 upstream/master 截至 <短哈希> 的改动。本地已通过 go test、前端 lint/测试/构建。"

# 7. 合并 PR。这一动作触发构建与部署
# 8. 部署后确认服务正常，删除同步分支
git push origin --delete sync/upstream-20261029
```

执行完后，`beta` 上只包含测试通过且已经上线的提交，`push beta` 与"发布"保持同一语义。

### 5.1 判断某个改动是否已在上游

```bash
# 上游已有等价补丁的提交会以 "-" 标记
git cherry -v upstream/master beta

# 该分支相对上游独有的提交
git log --oneline upstream/master..beta

# 文件级差异
git diff --stat upstream/master...beta
```

---

## 6. 冲突处理

先确认 `git config rerere.enabled` 为 `true`。

### 6.1 接缝文件

以下文件上游也会修改，是冲突的主要来源。当前清单：

| 文件 | fork 改动量 | 处理方式 |
|---|---|---|
| `main.go` | 约 26 行 | 保留上游的路由分组结构，把 fork 的路由注册追加到对应分组末尾 |
| `handler/api.go` | 约 56 行 | 新增的 handler 注册追加到函数尾部 |
| `middleware/auth.go` | 约 32 行 | fork 的鉴权分支放在函数最前面的独立 `if`，不插入现有判断链 |
| `models/init.go` | 约 55 行 | 只保留追加的 `AutoMigrate` 调用，上游语句原样保留 |
| `models/model.go` | 约 25 行 | 新结构体放在文件末尾，不修改上游字段 |
| `consts/consts.go` | 约 14 行 | 追加常量，不修改上游常量值 |
| `handler/chat.go` | 约 25 行 | fork 分支放在上游主流程之后，不重排现有语句 |
| `service/chat.go` | 约 66 行 | 同上 |
| `go.mod`、`go.sum` | 约 2 行 | 不手工解决。执行 `git checkout --theirs go.mod go.sum && go mod tidy && go mod verify` |
| `webui/src/routes/providers.tsx` | 中等 | 接受上游版本，fork 的转发入口保留在独立的 `forwarding.tsx` |
| `webui/src/routes/providers/provider-form-dialog.tsx` | 中等 | 同上 |

原则：接缝处只保留"一行调用加一个追加块"。逻辑主体放在新文件（如 `handler/provider_route.go`、`providers/forward.go`），新文件本身不会产生冲突。

### 6.2 新增文件

一般不会冲突。如果上游也创建了同名文件，将 fork 的文件改名，并同步修改 import 与引用。不要用 `--ours` 覆盖上游文件。

### 6.3 i18n 与前端 API 客户端

不要逐行合并。先接受上游版本，再追加 fork 新增的部分：

```bash
git checkout --theirs webui/src/i18n/locales/zh-CN/<文件>.json
# 追加 fork 新增的 key
cd webui && pnpm run lint
```

`webui/src/lib/api.ts` 同理：取上游版本后，把 fork 的转发相关函数整段追加到文件末尾。

### 6.4 数据库

fork 只能新增表或字段，不能修改上游已有字段。`db-compat.yml` 的工作方式是：用历史 tag 的代码建库，再用当前代码升级。修改上游字段会破坏这条升级链路。

### 6.5 冲突后的验证顺序

```bash
gofmt -l .                  # 必须为空
go vet ./...                # 无输出
go test -count=1 ./...      # 全部通过
cd webui && pnpm run build  # 类型检查在此执行
```

`models/compat_db_test.go` 需要设置 `LLMIO_COMPAT_DB` 才会运行，本地不设置会自动跳过；这组用例由 `db-compat.yml` 在涉及数据库层改动时按历史 tag 触发。

---

## 7. 发布与回滚

### 7.1 日常发布

`beta.yml` 在每次 push 到 `beta` 时推送两个 tag：

- 可移动的渠道 tag，默认 `beta`；
- 固定的可回溯 tag，格式为 `beta-<UTC 日期>-<短哈希>`，同时作为镜像内版本号，`/api/version` 可以查到当前运行的构建。

部署只使用固定 tag，回滚时指定上一个固定 tag 即可。

### 7.2 正式版本 tag

只有在需要对外发布 Release 时才打 tag。tag 只能打在 fork 上：

```bash
# 确认提交不属于上游分支
git branch -r --contains <commit>

git tag v0.10.2-fork.1 <commit>
git push origin v0.10.2-fork.1
```

`docker/metadata-action` 的 `type=semver` 能解析 prerelease 段。tag 名需要避开上游已有的版本号。

### 7.3 回滚

```bash
kubectl -n ns-7qijmoae rollout undo statefulset/llmio
```

或者将镜像指回上一个 `beta-<日期>-<哈希>` 固定 tag。

---

## 8. 红线

1. 不在 upstream 推送任何 tag，不在 upstream 执行 `gh release create`。
   `cicd.yml` 的 `deploy` 作业带有 `if: github.repository == 'atopos31/llmio'`，在上游成立：会用上游的 `DOCKERHUB_TOKEN` 推送镜像，并用上游的 `KUBE_CONFIG` 重启上游生产集群。`releaser.yml` 会在上游创建 Release。
2. 不执行 `git push upstream master`。上游没有分支保护，不会拦截。
3. 不对 upstream 的任何分支执行 force-push。
4. 不把 `beta.yml`、转发透传相关代码提交到 upstream。
5. 所有 `gh` 命令显式带 `-R`，写操作前确认仓库名是 `tktogether/llmio` 还是 `atopos31/llmio`。
6. tag 一旦触发过 Release 就不再移动。移动会导致 `releaser.yml` 重新上传资产并在 `422 already_exists` 处失败。需要调整历史时，在打 tag 之前完成。

---

## 9. 可选自动化

### 9.1 同步预演

需要 Git 2.38 及以上版本。定时检查 `beta` 与 `upstream/master` 是否还能干净合并：

```bash
git fetch upstream origin --prune
git merge-tree --write-tree --name-only origin/beta upstream/master
```

退出码非 0 表示存在冲突，输出中会列出冲突文件。

### 9.2 beta 的 PR 门禁

为 fork 的 `beta` 分支设置分支保护，要求 `test` 状态检查通过后才能合并。这是唯一能保证"测试未通过不部署"的机制，因为 `beta.yml` 的 `deploy-beta` 只依赖 `build-push-beta`，不依赖 `test.yml`。

### 9.3 接缝审计

同步前检查上游最近改动是否涉及接缝文件：

```bash
git log --oneline --since="1 month ago" upstream/master -- \
  main.go handler/api.go middleware/auth.go models/init.go models/model.go \
  consts/consts.go handler/chat.go service/chat.go go.mod \
  webui/src/routes/providers.tsx webui/src/routes/providers/provider-form-dialog.tsx
```

有输出时，本次同步的冲突风险较高，预留足够时间并优先处理这些文件。
