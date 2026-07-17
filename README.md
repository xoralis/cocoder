# cocoder

`cocoder`（命令名 `ccd`）是一个角色驱动的本地 AI 编程 CLI 编排器。它把 Claude Code、Codex、Gemini、Grok 等工具映射为架构师、后端、前端、文档或审查者等角色：规划角色先把需求拆成任务 DAG，随后 `ccd` 按依赖顺序调用对应 CLI 完成工作。

`ccd` 不托管模型，也不代理 API。它复用本机已经安装并完成认证的 AI CLI，在当前工作区执行任务，并将计划、状态、事件和原始日志持久化到 `.ccd/`。

```text
需求
 └─ planner_role（只读）→ Architecture + Plan DAG → 计划校验
                                                    ↓
                                         按依赖顺序逐项执行
                                                    ↓
                                    role → adapter → 本地 AI CLI
                                                    ↓
                                  工作区改动 + .ccd/ 运行状态
```

## 主要能力

- **基于角色的分工**：每个角色可以选择不同 CLI、模型、权限、文件范围、超时和重试策略。
- **自动规划与校验**：生成架构约定和任务 DAG，校验角色、任务 ID、依赖、循环依赖及潜在的文件范围冲突。
- **可恢复执行**：任务状态原子落盘；失败、中断或预算停止后可继续运行。
- **上下文传递**：通过持久化黑板把架构约定、前置任务摘要和改动文件传给后续任务。
- **安全边界**：支持 `read-only`、`edits`、`full` 三档权限，以及超时、成本预算、人工确认和 `file_scope` 越界检查。
- **完整可观测性**：提供运行状态、原始日志、会话 ID、耗时、可报告成本及 Git 变更摘要。
- **可扩展适配器**：内置常用 CLI 适配器，也可通过 YAML 接入输出纯文本的其他 CLI。

> 当前调度器理解任务依赖关系，但使用单 worker，依赖就绪的任务会按稳定拓扑顺序**串行执行**。

## 环境要求

- Go 1.25 或更高版本；仅运行已构建二进制时不需要 Go。
- Git 位于 `PATH`。非 Git 目录也能执行任务，但无法生成可靠的改动报告或检查 `file_scope`。
- 至少一个受支持的 AI 编程 CLI 已安装、完成认证并位于 `PATH`。

内置支持情况：

| CLI | 适配器 | 续会话 | 美元成本报告 | 备注 |
| --- | --- | :---: | :---: | --- |
| Claude Code | `claude` | 是 | 是 | JSON 事件和结构化输出 |
| Codex CLI | `codex` | 是 | 否 | JSON 事件和结构化输出 |
| Gemini CLI | `gemini` | 是 | 否 | 流式事件格式按 CLI 文档实现 |
| Grok CLI | `grok` | 是 | 否 | 流式 JSON 事件 |
| Antigravity | `agy` | 否 | 否 | 未验证的 generic 文本回退 |
| 其他 CLI | `generic` | 可选 | 否 | 纯文本输出，以退出码判断成败 |

外部 CLI 的参数可能随版本变化。先运行 `ccd doctor` 检查环境；必要时可在 `ccd.yaml` 的 `clis` 中覆盖命令、参数和权限映射。

## 构建与安装

在本仓库根目录执行：

```bash
go install ./cmd/ccd
ccd --version
```

请确保 Go 的可执行文件目录已加入 `PATH`。也可以只在当前目录构建：

```bash
# macOS / Linux
go build -o ccd ./cmd/ccd

# Windows PowerShell
go build -o ccd.exe ./cmd/ccd
```

不安装时可直接运行：

```bash
go run ./cmd/ccd --help
```

## 快速开始

进入需要协作开发的目标项目，然后初始化配置：

```bash
ccd init
```

该命令会生成带完整注释的 `ccd.yaml`，并尽力将 `.ccd/` 幂等追加到目标项目的 `.gitignore`。已有配置不会被覆盖，除非显式使用 `ccd init --force`。

编辑 `ccd.yaml`，为角色选择本机可用的 CLI，然后检查环境：

```bash
ccd doctor
ccd roles
```

绕过规划器，直接运行一个角色任务：

```bash
ccd assign backend "为用户接口增加参数校验和单元测试"
```

让规划角色拆解并执行完整需求：

```bash
ccd run --confirm "增加用户注册与登录功能，并补充测试和文档"
```

查看最新运行及日志：

```bash
ccd status
ccd logs plan
ccd logs t1
ccd logs t1 --follow
```

中断或失败后继续：

```bash
ccd resume
```

如果任务保存了可恢复的 CLI 会话，还可以追加要求；如需继续执行被阻塞的后续任务，再运行 `resume`：

```bash
ccd followup t1 "补充边界条件测试，并重新运行测试"
ccd resume
```

## 配置

`ccd init` 生成的模板是完整配置参考。一个精简示例如下：

```yaml
version: 1

defaults:
  task_timeout: 30m
  retries: 1
  plan_retries: 2
  budget_usd: 0
  planner_role: architect
  builder_role: backend
  scope_violation: warn

roles:
  architect:
    cli: claude
    permission: read-only
    system_prompt: |
      You are a pragmatic software architect. Decide interfaces first.

  backend:
    cli: codex
    permission: edits
    file_scope: ["cmd/", "internal/"]

  docs:
    cli: claude
    permission: edits
    file_scope: ["docs/", "README.md"]

clis:
  claude:
    command: claude
  codex:
    command: codex
```

### 默认配置

| 字段 | 含义 |
| --- | --- |
| `task_timeout` | 单个任务的默认超时，使用 Go duration，例如 `30s`、`10m`、`1h30m` |
| `retries` | 任务失败后的重试次数 |
| `plan_retries` | 计划提取或校验失败后的修正次数 |
| `budget_usd` | 一次运行的美元成本上限；`0` 表示不限 |
| `planner_role` | 负责需求拆解的角色 |
| `builder_role` | `--degrade` 降级为单任务时使用的角色 |
| `scope_violation` | 越界改动策略：`warn` 或 `fail` |

每个 `roles.<name>` 可配置 `cli`、`model`、`permission`、`system_prompt`、`file_scope`、`timeout`、`retries` 和 `extra_args`。

权限级别：

- `read-only`：用于读取、检索和规划。
- `edits`：自动允许文件编辑；命令执行仍遵循底层 CLI 的审批策略。
- `full`：允许高度自动化执行，可能映射为绕过审批或沙箱的危险参数，仅应在受信任的仓库中使用。

### 接入其他 CLI

通用适配器支持 `{prompt}`、`{model}`、`{workdir}`、`{session}` 占位符：

```yaml
roles:
  backend:
    cli: mycli
    permission: edits

clis:
  mycli:
    adapter: generic
    command: mycli
    run_args: ["-p", "{prompt}"]
    resume_args: ["-r", "{session}", "-p", "{prompt}"]
    prompt_via: arg
    output: text
```

支持标准输入的 CLI 建议使用 `prompt_via: stdin`。generic 适配器不会自动把 `permission` 映射为目标 CLI 的安全参数，安全边界需通过 `run_args`、`resume_args` 或目标 CLI 自身配置实现。

## 命令概览

| 命令 | 作用 | 主要选项 |
| --- | --- | --- |
| `ccd init` | 生成 `ccd.yaml` | `--force` |
| `ccd doctor` | 检查配置、Git 和所需 CLI | — |
| `ccd roles` | 查看角色、CLI、模型、权限、范围和超时 | — |
| `ccd assign <role> <task...>` | 单角色执行任务，不调用规划器 | `--scope`、`--timeout` |
| `ccd run <requirement...>` | 自动规划并执行需求 | `--confirm`、`--plan`、`--budget`、`--timeout`、`--degrade` |
| `ccd status [run-id]` | 查看指定或最新运行 | — |
| `ccd logs <task-id>` | 查看任务原始日志；规划器任务 ID 为 `plan` | `--run`、`-f/--follow` |
| `ccd resume [run-id]` | 继续指定或最新未完成的 `run` | `--budget`、`--timeout`、`--degrade` |
| `ccd followup <task-id> <instruction...>` | 继续某任务保存的 CLI 会话 | `--run`、`--timeout` |

全局选项：

```text
-c, --config <path>   配置文件，默认 ccd.yaml
-C, --chdir <dir>     执行前切换工作目录
    --verbose         显示 agent stderr 和更多细节
    --no-color        禁用彩色输出；也支持 NO_COLOR 环境变量
    --version         显示版本
```

更多示例：

```bash
ccd assign backend --scope internal/,cmd/ --timeout 20m "完成接口并运行测试"
ccd run --confirm --budget 5 --timeout 30m "实现功能并补齐测试"
ccd status 20260714-231502-a1b2
ccd logs t2 --run 20260714-231502-a1b2
```

## 使用预写计划

`--plan` 可以跳过规划角色，直接加载 JSON：

```bash
ccd run --plan plan.json
```

最小计划示例：

```json
{
  "goal": "增加健康检查接口",
  "architecture": "在现有 HTTP 层增加只读端点，并添加测试。",
  "tasks": [
    {
      "id": "t1",
      "role": "backend",
      "title": "实现健康检查",
      "description": "新增健康检查端点及单元测试。",
      "depends_on": [],
      "file_scope": ["internal/", "cmd/"],
      "acceptance": "go test ./... 通过"
    }
  ]
}
```

计划必须满足：角色已在配置中定义；任务 ID 唯一；依赖指向已有任务且不能成环；没有依赖顺序的任务不能声明相互重叠的 `file_scope`。

## 运行状态与恢复

每次运行保存在 `.ccd/runs/<run-id>/`：

```text
.ccd/runs/<run-id>/
├── meta.json
├── plan.json
├── architecture.md
├── blackboard.json
├── events.jsonl
├── task-<id>.state.json
├── planner-raw-<attempt>.txt
├── logs/
│   ├── plan.log
│   └── <task-id>.log
└── tmp/
```

第一次按 `Ctrl-C` 会请求优雅中断、终止代理子进程并刷新状态，之后可用 `ccd resume` 继续；第二次按 `Ctrl-C` 会强制退出。`resume` 只适用于 `ccd run` 创建的运行，不适用于 `ccd assign`。

`ccd` 只修改和报告工作区内容，**不会自动 commit 或 push**。

## 安全边界与当前限制

- 建议从干净的 Git 工作树开始运行，便于准确审查和归因变更。
- `file_scope` 是任务提示约束与运行后的 Git 检查，不是操作系统级文件沙箱；`fail` 会将越界任务判为失败，但不会回滚已经产生的改动。
- 完整的 `ccd run` 会执行越界策略；`ccd assign` 当前仅把 scope 写入提示词，`followup` 对越界改动只做警告。
- 成本预算是任务边界上的软上限：只统计能报告美元成本的 CLI，并在启动下一个任务前检查，因此当前任务可能使总额超过上限。
- 任务当前串行执行。计划中的 DAG 为依赖排序、失败阻塞和未来并发隔离提供依据。
- CLI 的权限和沙箱能力最终由对应外部工具实现；特别是 generic 适配器不提供权限硬隔离。
- `.ccd/` 中可能包含需求、代理输出、会话标识和日志，请按项目敏感级别妥善处理。

## 项目结构

```text
cmd/ccd/               CLI 入口
internal/cli/          Cobra 命令与参数解析
internal/config/       配置模型、默认值、严格校验与模板
internal/adapter/      Claude、Codex、Gemini、Grok 及 generic 适配器
internal/execx/        子进程启动、流处理与进程树终止
internal/plan/         计划生成、JSON 提取、校验与拓扑排序
internal/orchestrator/ 规划、调度、执行、重试、恢复、黑板与报告
internal/state/        运行状态、计划、事件和日志持久化
internal/gitx/         Git 快照、变更与 file_scope 检查
internal/ui/           终端输出
```

## 开发与验证

```bash
go test ./...
go vet ./...
go build ./cmd/ccd
```
