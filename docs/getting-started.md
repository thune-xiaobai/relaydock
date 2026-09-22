# 运行与接线指南

版本：2026-09-22，架构 v0.9；本文件描述实际代码和边界。

## 1. 当前可以运行的部分

一个 Go module，一个 `relaydock` 可执行程序，提供 `hub`、`worker`、`chat`、`channel`、交互终端 `shell` 和生成本地配置的 `init`。Hub 使用 pi SDK 进行多步协调，Go 保存权限、回执和 SQLite 状态；Worker 内含本机工具、tmux/psmux 命令和 pi 文件桥接。Hub 的 Node 子进程通过本地 JSON-lines 通道调用 Go 工具，不需要额外网络服务。

`extensions/relaydock.ts` 运行在每个任务 pi 内。企微 Gateway 改为 Go 固定流程，直接调用 pi-computer-use 原生 Windows helper；旧 `extensions/wecom-gateway.ts` 已移除。

| 部分 | 本版实现 |
| --- | --- |
| 自然语言入口 | pi SDK agent loop + 流式工具调用；无模型配置时明确报错 |
| Hub 协调 | 查看机器、创建任务、续接会话、状态/最近结果、终端快照、中断、关闭、Run 完成后的一次性后续；有歧义时可追问 |
| Worker 工具 | `host.inspect`、`session.list/inspect/create/close`、`agent.submit/interrupt`、`terminal.capture`、可选 `remote.exec/status/cancel` |
| Agent | 交互式 pi，显式加载扩展；其他 Agent 尚未实现 |
| 本地人工操作 | 直接 attach、输入；相关输入和输出继续同步，无接管状态 |
| 远程人工操作 | 本地 `relaydock shell` 连接 Linux/macOS PTY，内部自行 tmux attach；Windows 待实现 |
| 多 Session | 不同目录可并行；一个实际目录或其上下级目录只分配一个活跃 Session |
| 恢复 | 同调用 ID 去重、旧回执查询、事件补报、Hub 已开始的未完成消息不自动重执行；未开始的消息按序恢复 |
| 聊天适配 | console、文件 spool、固定 WeCom Gateway；三者共用 Channel 传输 |

`agent.submit` 只接受空闲 pi 的自然语言文本，不接受 pi slash command。运行中远程补充消息、任意 UI 确认回复暂未开放；这不影响用户在本地 pi 中正常输入。`completed` 只表示 pi 正常结束了该轮响应，不证明代码、测试或业务目标成功。

## 2. 本地启动

Hub 需要 Node.js 和已安装的 pi SDK，提供交互 Agent 的 Worker 需要 tmux / psmux 和任务 pi；shell-only Worker 只需要可执行程序和本机 shell。当前验证版本为 pi 0.85.1。

从仓库根目录执行，先准备一个可由本机用户访问的项目目录：

```sh
go build -o build/relaydock ./cmd/relaydock
./build/relaydock init --dir .relaydock --workspace /path/to/project \
  --model-url http://model-host:8000/v1 --model your-model
```

`init` 生成三份配置和随机独立 token，拒绝覆盖已有目录。新 Worker 默认启用 remote shell；添加 `--shell-only` 可生成不需要 pi / mux 的 Worker。旧配置需要显式增加 `"shell": {"enabled": true}`；并发限制、超时、输出分页、Windows PowerShell 策略与恢复语义见 [remote shell 指南](remote-shell.md)。它不会修改 pi 设置或系统网络。配置中的相对路径相对于配置文件目录解析；`bridge` 是本项目扩展的绝对路径。

Hub 模型支持如下配置；`url` 填 base URL，程序会追加 `/chat/completions`：

```json
{
  "model": {
    "url": "http://model-host:8000/v1",
    "model": "your-model",
    "key": "env:RELAYDOCK_MODEL_KEY"
  }
}
```

上面是配置片段，合入生成的 `hub.json`。模型需要支持流式 Chat Completions、`tools` 和 `tool_calls`。当前自定义模型按 64k context / 4k max output 配置。Hub 的模型只协调；任务 pi 使用自己的账号、provider、model 和项目授权设置。默认不关闭 pi 自带的项目/工具授权。可在 Worker 的 `agents.pi.args` 中配置正常使用的 pi 参数。

Hub 自动定位本机 pi 安装。对于自定义路径、Windows npm 包装器或没有源码 SDK 的独立 pi 二进制，补充：

```json
{
  "coordinator": {
    "node": "C:/Program Files/nodejs/node.exe",
    "package": "C:/path/to/node_modules/@earendil-works/pi-coding-agent"
  }
}
```

`package` 是含 `package.json` 和 `dist/index.js` 的包目录，不是 `pi.cmd`。Node 应满足所安装 pi 的运行要求。SDK 脚本内嵌在 Go 二进制中，启动时写到 Hub 的 state directory，无需另行复制脚本。Hub 对话历史独立于任务 pi；每个 Channel 保留一份原生 SDK 会话，未加载 Hub 主机的 shell 工具、扩展或项目指令。

分别运行：

```sh
./build/relaydock hub --config .relaydock/hub.json
./build/relaydock worker --config .relaydock/worker.json
./build/relaydock chat --config .relaydock/channel.json
```

用户输入例如：

```text
有哪些机器可以用？
在本机的 project 项目检查测试失败的原因
现在怎么样了？
继续修复刚才发现的问题
停止这个任务
```

查看/进入本机终端：

```sh
tmux -L relaydock list-sessions
tmux -L relaydock attach-session -t <上一步的会话名>
```

退出聊天或 Worker 不会关闭 pi。关闭 Session 是显式操作；中断 Run 只调用 pi 的 abort。每个新 Session 的原始 pane ID 单独记录，终端快照不依赖当前焦点。

若需要从本地终端进入远端 Linux/macOS 主机，运行：

```sh
./build/relaydock shell --config .relaydock/channel.json --node local --cwd project
```

将 `local` 改成配置中的 Worker ID。进入后直接执行上述 tmux 命令；Ctrl+C 传到远端，行首 `~.` 断开。复用 Channel 认证与节点权限，允许聊天入口继续在线。此命令不打开 Channel 状态目录；普通终端字节不会发送到模型或企微。配置、断线语义和 Windows 接口交接见 [交互终端指南](interactive-shell.md)。

## 3. 内网部署与 Windows

Hub 放在可被其他节点主动访问的主机。生产网络连接采用 WSS；普通 WS 仅允许 loopback。Worker 和 Gateway 都主动连接 Hub，Windows 无需监听内网端口，无需创建系统服务或管理员规则。

Hub 配置片段：

```json
{
  "listen": "0.0.0.0:7331",
  "tls_cert": "certs/hub.crt",
  "tls_key": "certs/hub.key",
  "workers": {
    "linux-dev": {"token": "env:LINUX_WORKER_TOKEN"},
    "windows-dev": {"token": "env:WINDOWS_WORKER_TOKEN"}
  },
  "channels": {
    "wecom-personal": {
      "token": "env:WECOM_CHANNEL_TOKEN",
      "nodes": ["linux-dev", "windows-dev"]
    }
  }
}
```

每个 token 至少 24 字符且唯一，实际建议用随机 32 字节以上。客户端的 `id` 必须与 Hub 配置键一致；凭据不能跨角色或节点使用。一个 Channel 凭据对应一个绑定的聊天/发送者范围。

Worker / Channel 设置 `hub: "wss://hub.internal:7331/ws"`。如果使用内部 CA，配置 `ca` 为客户端可读的 PEM 文件；证书应覆盖连接时使用的主机名。程序不会关闭证书验证，也不会修改企业防火墙。

Windows Worker 使用：

```json
{
  "backend": "psmux",
  "mux": "C:/Tools/psmux.exe",
  "namespace": "relaydock",
  "bridge": "C:/Tools/relaydock/extensions/relaydock.ts",
  "workspaces": {"project": "C:/Work/project"},
  "agents": {
    "pi": {
      "executable": "C:/Program Files/nodejs/node.exe",
      "args": ["C:/path/to/installed/pi-coding-agent/dist/cli.js"]
    }
  }
}
```

这也是需要合入完整 Worker 配置的片段。按实际已安装的 pi 路径填写；npm 的 `pi.cmd` 是 shell 包装器，首版建议配置真正的 `node.exe + pi cli.js`，避免 `.cmd` 启动和引用规则。若已有原生 pi 可执行文件，也可直接配置。

Windows 下运行 `relaydock.exe worker --config worker.json`。`psmux` 当前复用上游提供的 `-L`、命名 Session、多参数直接启动、`list-panes`、`capture-pane`、`kill-session` 子集；**尚未在目标 Windows 上验收**。所有清理都指定受管 Session，不调用全局 `kill-server`。

受限 Windows 仍需验证本机允许执行这些程序、允许 psmux 的本机通信，以及可以主动连接选定 Hub。这些验证不要求改变管理员策略。

## 4. 配置固定企微 Gateway

复用 [pi-computer-use](https://github.com/injaneity/pi-computer-use) 的 Windows native helper。本次对照上游 0.5.1 的协议和平台适配代码，实现 JSON-lines protocol v3；运行时会验证协议版本。Gateway 不需要模型或另一个 pi 会话。

如果目标 Windows 已通过 pi-computer-use 操作过企微，可先查找现有 helper。默认位置为 `%USERPROFILE%\.pi\agent\helpers\pi-computer-use\windows-bridge.exe`；其他路径可通过 `wecom.helper` 显式指定。此程序作为当前用户的子进程启动，不创建服务或监听端口。若还没有 helper，按上游正常方式安装并完成原有 pi-computer-use 的初始化：

```powershell
pi install npm:@injaneity/pi-computer-use
```

安装扩展不等于 native helper 已就绪，需确认上述文件存在且 `wecom-inspect` 的 diagnostics 校验通过。协议不同会明确报错，不自动升级用户安装。

复制 [完整配置模板](../examples/wecom-channel.json) 到自己的 `channel.json`，设置独立 Channel ID、token、Hub 地址及 CA。不要同时设置 `spool`。模板中的控件、窗口标题和消息格式都是**待替换示例**，不能直接用于真实企微。

先打开指定聊天，在普通 PowerShell 执行只读检查：

```powershell
.\relaydock.exe wecom-inspect --config channel.json
```

此命令只列出原生窗口。用观察到的 PID 和 rootRef 继续检查，例如：

```powershell
.\relaydock.exe wecom-inspect --config channel.json --pid 1234 --root-ref "实际窗口引用"
```

命令不会聚焦、输入或发送。它返回原生 outline；配置 `wecom.row` 后还会在同一次 look 内读取匹配消息行的 UIA 原文，输出到 `row_text`，便于编写提取规则。最多读取 200 行。窗口引用仅用于这次检查，不存成消息 ID。

配置项：

| 字段 | 含义 |
| --- | --- |
| `app` / `window_title` | 精确选择唯一可见窗口，进程名忽略大小写及 `.exe` 后缀 |
| `chat` / `chat_title` | 聊天标题控件及其 UIA 精确文本，用于发送前核对 |
| `messages` / `row` | 消息容器的唯一选择器及其消息行选择器；行顺序须是旧到新 |
| `input` / `send` | 输入框与发送按钮，各匹配一个控件 |
| `message_pattern` | 应覆盖完整消息行文本的 Go 正则；命名捕获 `sender`、`text`，可选 `id` |
| `peer` / `self` | 从消息行提取出的获准发送者及当前企微账号，必须不同 |
| `poll_ms` | 轮询间隔；省略或小于 500 时使用 2,000 毫秒 |

选择器可组合 `role`、`identifier`、`title`、`description`，均为 UIA outline 精确匹配；不要填写每次 look 改变的 ref。模板里的 `id|sender|text` 只是测试格式，必须用目标消息行的实际 UIA 原文替换。若稳定消息身份不可得，删除 `id` 捕获；若发送者无法从消息行中可靠区分，当前提取器还不能用于该界面，应先调整 Desktop 适配，不能把所有文本默认为用户指令。

固定流程只操作**当前已打开的指定聊天**，不自动切换联系人，不使用截图模型、OCR 或坐标点击。窗口不唯一、切到别的聊天、控件不可读、历史连续性丢失时暂停，保留旧检查点。若当前企微仅能通过截图读消息，新的 UIA 实现需要补充读取适配后才能部署；用户之前的视觉 Agent 收发验证不证明 UIA 路径已可用。

校准后启动：

```powershell
.\relaydock.exe channel --config channel.json
```

第一次成功观察只保存历史基线，日志显示 `baseline established` 后再发送新任务。每个观察的新消息先取得稳定本地输入 ID，再进入 Channel 队列；只有 `peer` 的新文本发给 Hub。自己的消息仍参与序列锚定，但不会被当成新任务。

没有平台稳定消息 ID 时使用连续文本锚点，不能保证长期离线、快速滚屏或外观完全相同的新旧窗口下的无损识别。若连续性丢失，先恢复可对齐的聊天视图。确实无法恢复、决定跳过当前可见历史时，停止 Channel 后显式建立新基线，再手动补发尚未处理的用户请求：

```powershell
.\relaydock.exe wecom-baseline --config channel.json
```

发送前持久记录 `sending`，在新观察中看到唯一新增的 self 完整文本回显才记为 `sent`。10 秒内无法确认则为 `unknown`，阻塞后续发送，仍尝试接收入站。草稿不为空时不覆盖它，但这次发送仍保守保留为待核对；排除草稿或界面问题后再处理。helper 退出 / 超时后需要重启 Channel；旧 sending 只能观察确认，不自动重发。

处理不确定发送时，先查看原聊天，然后停止 Channel，用日志里的 output ID 标记结果：

```powershell
# 已核对消息出现，或者已手动发出相同回复
.\relaydock.exe wecom-resolve --config channel.json --output out_xxx --delivery sent
# 已核对没有发出，明确要求重新尝试
.\relaydock.exe wecom-resolve --config channel.json --output out_xxx --delivery retry
```

这些是本机维护命令；聊天用户仍输入自然语言。重试会真实再次尝试发送，不能仅凭超时选择 retry。维护命令与 Gateway 共用状态锁，需先停止 Channel。

`output_ack` 代表 Gateway 已在本地持久保存回复，不代表企微已发送或用户已读。真实投递状态保存在 `state_dir/wecom/state.db`，当前不反向同步到 Hub。首版仅支持可提取的文本，未实现附件、平台文本分片或聊天自动导航；超过企微实际单条限制会导致写入 / 回显核对失败，需要现场确定限制。

从旧原型迁移时，停止原企微操作 pi 中的 RelayDock Gateway 扩展，移除旧 `spool` 配置，使用新的 WeCom 配置。旧 spool 的未发送消息需要先人工核对处理，不会自动导入成新待发送内容。Gateway state directory 绑定 Channel、聊天及发送者；切换聊天应使用独立目录，避免把旧回复发给新联系人。

## 5. 状态、存储与恢复

每个角色有独立 `state_dir`，SQLite 使用 WAL 和进程锁。不要让两个进程共用一个 state directory。生产配置、token 和会话文本保存在用户本机，配置支持从环境变量读取凭据。

Hub 的 SDK 原生历史位于 `hub-state/coordinator/<channel-id>/sessions/<授权节点集合的哈希>/`，与 Go 的业务回执分别保存。节点授权变化后使用对应范围的历史，Go 也会清除包含已撤权节点的对话上下文。`session_watch` 保存一次性后续；完成事件只入队一次，注册晚于完成也能处理。后续仍经过同样的权限、调用上限和不确定性检查。

每个任务 Session 的文件位于 `worker-state/sessions/<session-id>/`：

```text
launch.json            固定的 pi 启动配置，不含任务文本
state.json             pi 心跳、实例、原生会话、当前 Run
requests/<call>.json    原子提交的输入/中断请求
receipts/<call>.json    pi 的持久回执
events.ndjson          pi 单写者的完整事件记录
native/                pi 原生会话文件
exit.json              独立 launcher 记录的 pi 退出事实
```

扩展投递输入前先记录结果未知，仅在 pi 实际启动并接纳对应用户消息后记录已接受，不等待整轮完成。缺少模型、凭据等可确认的预检失败返回 `rejected` 并恢复空闲。pi 0.85.1 的扩展发送 API 不返回 Promise；投递后若 5 秒没有可关联的启动事实（例如其他扩展拦截、异步校验失败或延迟），状态转为 `unknown`，不会误报仍在运行，也不会自动重发。可以查看原 pi，必要时显式关闭或在本地切换原生会话后继续；不相关的本地运行不会替旧请求确认接受。

Worker 将每个事件及读取位置一起写入 SQLite，Hub 将事件去重、会话状态和待发送回复一起写入 SQLite。关闭会话后仍接收待补报的输出和结束事件，保持 `closed`，不触发已取消的后续安排。事件次序使用本机日志位置，避免时钟或跨实例序号造成状态倒退。

Worker 重启后重新读取原 Session，不重建 pi。重连时 Hub 只通过 `call_status` 查询未确认的旧调用，Worker 不因查询执行工具。如果 Hub 在解释或派发过程中重启，已开始却未完成的聊天请求标为待核对，并回复不确定性说明；尚未开始的请求按原顺序继续。旧版本没有 started / sequence 的未完成记录保守地标为待核对。

Hub 保存最终回复的事务失败时，保留本次结果并退避重试数据库写入，同时打印错误；不会再次运行协调器或工具。读取对话失败也会等待恢复。持续故障会暂停后续协调；若这期间关闭或崩溃，未落盘的请求仍按重启时的不确定性规则处理，不能保证找回内存中的最终回复。

没有自动重试任务、自动复活 pi、跨机器迁移或机器重启后自动恢复执行。pi 心跳超过 8 秒未更新时显示 `unknown`，不能仅凭 mux/pane 存在认定 pi 可用。启动失败的 Session 可以显式 `session.close`：未初始化桥接也能清理自己创建的受管终端，确认关闭或不存在后释放目录；权限错误、mux 不可用等不会当作不存在。psmux 的列表会跳过未响应会话，因此 Windows 使用 `PSMUX_DATA_DIR` 或 `USERPROFILE/.psmux` 内的命名空间登记文件保守判断存在性；无法确定该目录时拒绝清理，不能猜测路径。已初始化的活动会话仍要求匹配当前实例和原生会话。

Hub 入队和实际下发输出时都重新检查节点授权，涵盖 pi 消息、shell 完成通知和可能汇总多节点的最终回复。撤权后仍保存 Worker 状态并确认事件，以便恢复与去重，但不再向该 Channel 发送。已经交付到 Channel / Gateway 的内容无法由 Hub 撤回。

空白或超长聊天输入被永久拒绝，Hub 通过 `chat_reject` 返回原因，Channel 持久记录在 `rejected` 并移出 pending；旧版本遗留的空白 pending 也会清理。Gateway 将无效消息保留在 `rejected_inputs` 后继续接收，文件 spool 将无效文件移到 `rejected/`。数据库、文件权限或网络等暂时错误仍保留请求重试，不会误当作永久拒绝。

升级到此版本时一并更新 Hub、Channel、Worker 和 pi 扩展（已有 pi 可在本地 `/reload`）。旧版没有完整授权来源记录的 Hub 待发输出不再自动发送，日志会记录丢弃的 ID；原始会话和结果仍保留，可重新查询。旧 SDK 历史文件保留但不自动加载，避免撤权后旧上下文继续泄漏；建议升级前先让旧待发队列排空。

第一版保守地给每个活跃 Session 保留整个实际目录，`max_running` 因而限制的是活跃 Session 数量。关闭 Session 后释放目录；已退出 pi 不再占用该目录。外部编辑器或未受管进程不属于操作系统隔离范围。

单次远程文本和转发的单条 Agent 输出限制为约 64 KiB，终端快照保留最近 100 行并截断到 64 KiB；完整内容可在 pi 原生会话中查看。事件文件达到 64 MiB 后不再接受新远程任务；正在执行和本地交互的事件仍追加以保留结束信息。当前没有自动归档/保留期清理，使用中需关注磁盘，关闭 Session 后归档完整 state directory。不要在运行中删除事件或回执文件。

## 6. 验证边界与后续接线

本机验证环境为 macOS、Go 1.27.0、tmux 3.7c、pi 0.85.1。真实集成测试使用隔离的本地流式工具调用模型、独立 Worker 进程和两个项目目录，不使用用户模型凭据，不发送真实聊天。

```sh
go test -race ./...
go vet ./...
RELAYDOCK_TEST_PI=1 go test -race ./internal/hub -run 'TestRealPi(RemoteRuntime|Runtime|CoordinatorHistoryScope)$' -v -count=1
RELAYDOCK_TEST_PI=1 go test -race ./internal/worker -run TestRealPiBridgeReceipts -v -count=1
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o build/relaydock-windows-amd64.exe ./cmd/relaydock
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o build/relaydock-linux-amd64 ./cmd/relaydock
```

覆盖真正的 pi SDK 多步循环与结果驱动下一步、完成后唤醒、原生会话延续、并行任务、本地输入回传、Worker 重启、去重、中断和退出。Gateway 的本地协议 / 桌面夹具覆盖版本与 ID 检查、超时断开、源绑定、控件重新定位、聊天变化保护、重复文本 occurrence、检查点恢复、发送顺序及未知发送不重试。

shell 测试还覆盖本机进程输出、中文分页、并发/取消/超时、输出截断、WSS 断线完成补报、通知去重和 shell-only 运行。故障回归覆盖 pi 预检失败 / 无启动事件 / 延迟回调、失败启动的清理、撤权后消息与历史过滤、关闭后的消息补报、无效输入队列，以及 SQLite 写失败恢复时不重跑请求。

交叉编译只证明构建成功。目标 Windows 上 remote shell 还需验证 PowerShell 运行策略、本机程序退出码与编码、Job Object 的子进程清理。已有功能仍需验证 psmux 的实际命令兼容性、helper protocol v3、UIA 消息可读性与顺序、sender / self 区分、稳定 ID 或锚点可靠性、输入 / 发送能力、消息长度和长时间轮询。未完成这些验证之前，不把固定 Gateway 标为企微端到端可用。

可替换边界是 `hub.Coordinator`、`internal/channel`、`wecom.Desktop` 和 Worker 的 pi / mux 适配。出现第二个真实 Agent 后再提取共有接口；当前不构建通用插件平台。
