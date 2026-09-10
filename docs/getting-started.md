# 运行与接线指南

版本：2026-09-10 首个原型。架构基线是 v0.6；本文件描述实际代码和边界。

## 1. 当前可以运行的部分

一个 Go module，一个 `relaydock` 可执行程序，提供 `hub`、`worker`、`chat`、`channel` 和生成本地配置的 `init`。Hub 内含模型路由、工具协调和 SQLite；Worker 内含本机工具、tmux/psmux 命令和 pi 文件桥接。没有额外数据库服务、消息队列或后台调度 Agent。

`extensions/relaydock.ts` 运行在每个任务 pi 内，`extensions/wecom-gateway.ts` 加载到已有的企微操作 pi 中。两者各司其职，Gateway 不承担任务执行。

| 部分 | 本版实现 |
| --- | --- |
| 自然语言入口 | OpenAI-compatible Chat Completions + JSON decision；无模型配置时明确报错 |
| Hub 协调 | 查看机器、创建任务、续接会话、状态/最近结果、终端快照、中断、关闭；有歧义时可追问 |
| Worker 工具 | `host.inspect`、`session.list/inspect/create/close`、`agent.submit/interrupt`、`terminal.capture` |
| Agent | 交互式 pi，显式加载扩展；其他 Agent 尚未实现 |
| 本地人工操作 | 直接 attach、输入；相关输入和输出继续同步，无接管状态 |
| 多 Session | 不同目录可并行；一个实际目录或其上下级目录只分配一个活跃 Session |
| 恢复 | 同调用 ID 去重、旧回执查询、事件补报、Hub 未完成消息不自动重执行 |
| 聊天适配 | console 与文件 spool；企微 pi 使用四个 Gateway 工具接入现有桌面收发流程 |

`agent.submit` 只接受空闲 pi 的自然语言文本，不接受 pi slash command。运行中远程补充消息、任意 UI 确认回复暂未开放；这不影响用户在本地 pi 中正常输入。`completed` 只表示 pi 正常结束了该轮响应，不证明代码、测试或业务目标成功。

## 2. 本地启动

从仓库根目录执行，先准备一个可由本机用户访问的项目目录：

```sh
go build -o build/relaydock ./cmd/relaydock
./build/relaydock init --dir .relaydock --workspace /path/to/project \
  --model-url http://model-host:8000/v1 --model your-model
```

`init` 生成三份配置和随机独立 token，拒绝覆盖已有目录。它不会修改 pi 设置或系统网络。配置中的相对路径相对于配置文件目录解析；`bridge` 是本项目扩展的绝对路径。

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

上面是配置片段，合入生成的 `hub.json`。模型需要支持 `response_format: {"type":"json_object"}`。Hub 的模型只协调；任务 pi 使用自己的账号、provider、model 和项目授权设置。默认不关闭 pi 自带的项目/工具授权。可在 Worker 的 `agents.pi.args` 中配置正常使用的 pi 参数。

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

## 4. 接入现有企微 pi

用户已确认入口：安装 [injaneity/pi-computer-use](https://github.com/injaneity/pi-computer-use)，打开普通交互式 pi 会话，直接用自然语言要求“看消息 / 发消息”。Gateway 就采用这一方式：同一个 pi 同时加载桌面操作扩展与本项目的消息桥接扩展。

尚未安装时按上游 README 执行；目标 Windows 已经可用的安装可直接复用，无需为了接线升级：

```powershell
pi install npm:@injaneity/pi-computer-use
```

本次审阅的上游包版本为 0.5.1，扩展注册了 `find_roots`、`observe_ui`、`search_ui`、`read_text`、`act_ui`、`wait_for` 等工具。pi 自行组合这些工具读取/操作普通聊天窗口；RelayDock 不需要企业微信 API 或额外的收发脚本。实际工具参数以目标 Windows 已安装版本为准。

1. 为此聊天生成/配置独立的 Channel ID 和 token，写入 Hub 的 `channels` 及 Gateway 的 `channel.json`。`spool` 指向一个普通用户可写的本地目录。
2. 在 Windows 启动 `relaydock.exe channel --config channel.json`。
3. 在启动企微操作 pi 的同一 PowerShell 中设置绑定并追加扩展：

```powershell
$env:RELAYDOCK_SPOOL = "C:\Users\you\relaydock\spool"
$env:RELAYDOCK_CHAT_REF = "企业微信中与张三的单聊（按实际聊天填写）"
$env:RELAYDOCK_SENDER_REF = "张三（按实际身份补全以区分同名者）"
pi --name relaydock-gateway `
  --session-dir C:\Users\you\relaydock\gateway-sessions `
  -e C:\Tools\relaydock\extensions\wecom-gateway.ts
```

用 `pi install` 安装并启用的 pi-computer-use 会正常自动加载，因此这里只需额外指定本项目扩展；不要加 `--no-extensions` 将已安装扩展屏蔽。保留原有可用的模型配置。`RELAYDOCK_SPOOL` 必须与 `channel.json` 的 `spool` 指向同一目录。普通 `chat` 与 Gateway `channel` 不同时使用同一 Channel 身份和 state directory。

两个 `*_REF` 是预先配置的聊天/发送者定位描述，不要求企微 API ID，也不是界面临时的 `@r` / `@e` 引用。Gateway pi 依据描述定位并核对窗口中的实际身份。接收消息时，桥接工具会校验引用与配置相同。

本项目扩展会在保留原有 system prompt 的基础上追加中继职责、配置的聊天绑定和工具协作规则。打开后仍直接用自然语言，例如：

```text
开始做企微中继：查看绑定聊天的新消息并交给 Hub，再把 Hub 待回复的内容发回该聊天。
```

也可以分别说“检查一下新消息”“把待回复的消息发出去”。这些操作由同一个 pi 调用两组工具完成。

Gateway pi 的工作约定：

- 按原先可工作的方式读取指定企微聊天，只将允许发送者的新入站消息交给 `relaydock_receive`。参数带聊天引用、发送者引用、消息发生标识和原文。
- 同一次消息被再次观察时，复用 `observation_id`；用户再次发送相同文字是新的 occurrence，应使用新 ID。`relaydock_receive` 保存发生标识、原文及可选 `source_context`（可见时间、前后文等定位依据）。`relaydock_outbox` 同时返回最近 8 条入站观察记录，pi 重启后也能核对。较长原文只返回 1,000 字符预览并标记截断。
- pi-computer-use 的 `stateId`、`@r` 和 `@e` 对应界面观察及其引用，不是稳定聊天消息 ID。Gateway pi 需要结合可见消息顺序与已保存的观察记录识别同一条消息，不能每次观察都生成新 ID。无法确认新旧时继续核对；这些工具仍依赖模型正确读取 UI，不能单凭参数校验证明来源识别和消息去重已在目标界面通过验收。
- 调用 `relaydock_outbox` 取按顺序待发送的回复；调用 `relaydock_claim_output` 持久记录发送开始，然后用已验证的 pi-computer-use 收发能力向绑定的原会话发送返回的准确文字。
- 在 UI 中确认出现后调用 `relaydock_finish_output(status="sent")`；无法确定则记录 `unknown`。`sending/unknown` 不能盲目重发，先核对聊天记录。程序自身的回复不得当成新的用户任务再次上报。

这四个工具衔接消息，界面动作由同一 pi 的 pi-computer-use 工具完成。入口和安装方式已经明确，后续联调检查的是这两组工具在目标企微聊天中的协作，不再等待额外的收发脚本或 API。

第一步可直接验证一次完整收发。用户要求持续中继时，pi 可在当前运行轮次继续观察、等待和收发，但本版尚无程序定时唤醒 Gateway pi 的循环；`wait_for` 等待一次 UI 条件，不等于后台消息订阅。pi 本轮结束后不会自行重新开始。因此一次自然语言收发成功与无人值守持续运行仍分开验收。

Hub 收到的 `output_ack` 表示 Channel 已接收并持久保存回复，**不表示用户已读或企微已发送成功**。发送状态保存在 Gateway 的 `delivery` 文件中；本版尚未将它映射成 Hub 的平台投递状态。

## 5. 状态、存储与恢复

每个角色有独立 `state_dir`，SQLite 使用 WAL 和进程锁。不要让两个进程共用一个 state directory。生产配置、token 和会话文本保存在用户本机，配置支持从环境变量读取凭据。

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

扩展投递输入前先记录结果未知，成功调用后再记录已接受；崩溃窗口内不会盲目重试。Worker 将每个事件及读取位置一起写入 SQLite，Hub 将事件去重、会话状态和待发送回复一起写入 SQLite。事件次序使用本机日志位置，避免时钟或跨实例序号造成状态倒退。

Worker 重启后重新读取原 Session，不重建 pi。重连时 Hub 只通过 `call_status` 查询未确认的旧调用，Worker 不因查询执行工具。如果 Hub 在解释或派发过程中重启，未完成的聊天请求标为待核对，并回复不确定性说明；用户查看状态后再继续。

没有自动重试任务、自动复活 pi、跨机器迁移或机器重启后自动恢复执行。pi 心跳超过 8 秒未更新时显示 `unknown`，不能仅凭 mux/pane 存在认定 pi 可用。`session.create` 启动失败但结果未知时可能保留待核对 Session；先检查其终端/日志，不能盲目再次创建。

第一版保守地给每个活跃 Session 保留整个实际目录，`max_running` 因而限制的是活跃 Session 数量。关闭 Session 后释放目录；已退出 pi 不再占用该目录。外部编辑器或未受管进程不属于操作系统隔离范围。

单次远程文本和转发的单条 Agent 输出限制为约 64 KiB，终端快照保留最近 100 行并截断到 64 KiB；完整内容可在 pi 原生会话中查看。事件文件达到 64 MiB 后不再接受新远程任务；正在执行和本地交互的事件仍追加以保留结束信息。当前没有自动归档/保留期清理，使用中需关注磁盘，关闭 Session 后归档完整 state directory。不要在运行中删除事件或回执文件。

## 6. 验证边界与后续接线

本机环境为 macOS，Go 1.27.0、tmux 3.7c、pi 0.85.1。`go test -race ./...` 检查核心状态逻辑；真实 runtime 集成测试使用本地固定响应模型、隔离 pi 配置、两个项目目录，不使用用户模型凭据，不发送真实聊天。

```sh
go test -race ./...
RELAYDOCK_TEST_PI=1 go test ./internal/hub -run TestRealPiRuntime -v -count=1
node scripts/test-gateway.mjs /absolute/path/to/installed/pi-coding-agent
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o build/relaydock-windows-amd64.exe ./cmd/relaydock
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o build/relaydock-linux-amd64 ./cmd/relaydock
```

Gateway 测试通过 pi 的实际扩展加载器执行四个工具，覆盖中继提示词追加、来源绑定、入站观察记录重载、发生标识去重、发送顺序和不确定发送保护，不调用桌面收发。pi-computer-use 的能力依据来自本次上游 README / 源码审阅；目标企微基础收发可行性来自用户既有验证。Linux/Windows 交叉编译与 macOS runtime 测试分开记录；两组扩展的实际协作、目标 Windows 的 psmux 和企业微信连续收发仍需在实际配置下联调。

扩展入口保持在消息协议、`hub.Router`、`internal/channel` 和 Worker 的 pi/mux 适配代码。出现第二个真实 Agent 后再提取它们共有的接口；目前不为假设的插件创建空框架。
