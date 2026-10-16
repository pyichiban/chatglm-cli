# glm2cli 设计（Go 命令行客户端）

> 目标读者：既要给**人**在终端敲，也要给 **agent** 在无头环境按契约调用。
> 依据：`glm2api/`（Python 实证实现）+ `docs/PROTOCOL.md` + `docs/生图协议与实操.md` + `docs/HANDOFF.md`。
> 本文只做设计与计划，代码未落地；带 **D#** 的条目是待用户拍板的决策项。

---

## 0. 定位与边界

一句话：把已实证的清言私有协议移植成**单个静态二进制** `glm2cli`，对话用 **JSONL 事件流**输出，
多轮用 **conversation_id** 续接，会话增删改查 + 用户信息 + 生图/改图全部可用命令行完成。

| 做 | 不做（继承既有边界） |
|---|---|
| 签名/凭据/刷新落盘/退避重试/SSE 聚合（从 Python 侧逐条移植实证结论） | ❌ 登录流程逆向（凭据仍由用户从浏览器取） |
| 人 & agent 共用一套**稳定契约**（JSONL + 退出码 + `schema` 自描述） | ❌ 工具调用 / agent loop |
| 本地会话存档（给「查历史」兜底 + 别名续接） | ❌ OpenAI 兼容 HTTP server（那是另一层） |
| 文生图 / 参考图改图 / 产物下载 | ❌ 多账号池、粘性路由、签到积分 |

依赖原则：**`CGO_ENABLED=0` 单文件**，核心零第三方库；是否引入压缩/cobra 见 D11/D13。

---

## 1. 上游能力 → 命令映射（实证度标注）

| CLI 命令 | 上游端点 | 实证度 |
|---|---|---|
| `auth check/refresh` | `POST user-api/user/refresh` | ✅ 签名+轮换铁律 |
| `user` / `whoami` | `GET user-api/user/info` | ✅（`member_info.left_score` 单位「分」） |
| `models` | `GET agent-api/operation/detail?tag=available_models` | ✅（无费率、无上下文长度） |
| `agents` | 内置常量表（PROTOCOL §4） | ✅ 5 个 assistant_id |
| `chat` / `ask` | `POST backend-api/assistant/stream` | ✅ 首轮 `conversation_id=""` → 首帧回传 |
| `image` | 同上 + `assistant_id=65a232c0…` + `meta_data.cogview` | ✅ 一次 4 图；`style/scene` 已失效 |
| `edit` | `POST drawing-api/v1/image/reference` | ✅ 只收**上游可达的公网 URL**；输出比例跟随输入图；单图 |
| `conv list` | `POST mainchat-api/conversation/recent_list` | ✅ 纯页码翻页 |
| `conv create` | **无创建端点**：发首条消息 + （可选）`modify_title` | ⚠️ 派生语义，见 D6 |
| `conv history` | `GET agent-api/conversation/page_messages_v2` | ❌ **上游 5 次全返回空 `list`** → 靠本地存档兜底，见 D5 |
| `conv rename` | `POST mainchat-api/conversation/modify_title` | ✅ |
| `conv title` | `GET mainchat-api/conversation/title` | ✅ 读自动生成的标题 |
| `conv delete` | `POST mainchat-api/conversation/bulk_delete` | ✅ 单删也走 bulk；**agent_id 必须匹配，否则静默无效** |
| `conv read` | `POST mainchat-api/stream/update_status` | ✅ |
| `conv stop` | `POST mainchat-api/stream/stop_stream` | ✅ `history_id` == 帧 `parts[].id` |
| （不做）`upload` | `productivity-api/file/chat_upload` | ❌ 不带 `refer__991` 时 100% 40012，见 D9 |

---

## 2. 全局契约（人和 agent 都遵守的规则）

1. **stdout 只放数据，stderr 只放日志**，永不混流。`--quiet` 关日志，`-v/-vv` 升 verbosity（`-vv` 打脱敏请求头与原始帧）。
2. **`--format`** 三档：`jsonl`（默认，非 TTY）/ `json`（缩进，TTY 友好）/ `text`（纯正文，给人看）。
   agent 一律显式写 `--format jsonl`，不依赖 TTY 探测。
3. **编码**：始终 UTF-8，行尾 `\n`，一事件一行；机器模式无 ANSI（`--color never|always|auto`）。
   Windows 中文控制台需 `chcp 65001`，或重定向到文件再读（见 D15）。
4. **零交互**：任何情况下不 prompt、不读 `/dev/tty`、不打印进度条。缺参数即 `exit 2` + 机器可读 error 事件。
5. **提示词输入**三级优先：`--prompt/-p` > `--prompt-file PATH` > `stdin`（`-p -` 显式强制 stdin）。
   超长/多行/中文一律 `--prompt-file`，避免 shell 引号地狱。
6. **重试纪律**：只对「尚未向 stdout 写出任何字节」的建连阶段做 40012 指数退避（`--retries`，默认 4，
   `delay=base*2^n*jitter`）；流一旦开始输出**绝不重试**，避免重复计费/重复消息。
7. **超时纪律**（三条硬规则之一）：流式请求**无总超时**；只提供可选墙钟上限 `--deadline`（默认 0=不限），
   触发时按 `stream_truncated` 报错而不是伪造收尾（R36）。非流式默认读超时 60s（`image/reference` 17~22s 够用）。
8. **自描述**：`glm2cli schema` 输出全部子命令、flag、事件类型、退出码的 JSON；agent 可在运行时读取契约而不必猜。
9. **幂等**：`--dry-run` 打印将发送的方法/URL/脱敏头/请求体，不发请求；写操作（rename/delete）支持 `--dry-run`。
10. **凭据绝不入日志**：token 只显示前 8 位 + 长度；`--cred` 文件权限在 README 里提示。

### 2.1 全局 flag

```
--cred PATH            凭据文件；默认 $GLM_CREDENTIAL_FILE → ~/.glm2cli/credential.json → ./glm_credential.json
--base URL             上游基址（默认 https://chatglm.cn）
--proxy URL            代理（默认 $GLM_PROXY，如 http://127.0.0.1:10809）
--state-dir DIR        状态目录（默认 ~/.glm2cli）
--retries N            40012 退避重试次数（默认 4）
--timeout SEC          非流式读超时（默认 60）
--deadline SEC         整条流的墙钟上限（默认 0 = 不限）
--refer-991 STR        画图族 webview 风控参数（默认 $GLM_REFER_991 / 状态文件）
--format FMT           jsonl|json|text
--color WHEN           auto|never|always
--quiet, -v, -vv       日志级别
--no-save-cred         不把轮换后的 refresh_token 写回文件（env 模式下自动禁用）
--dry-run              只打印请求
-V, --version          版本（含 commit/go 版本，`--version --format jsonl` 可被 agent 读）
```

---

## 3. 子命令契约

命名：**两级分组 + 扁平别名**（`conv list` 与 `list-conversations` 都收，见 D2）。
只有一条命令带位置参数：`chat/ask/image/edit` 的 prompt。

### 3.1 `chat`（核心）

```
glm2cli chat [prompt] [flags]

-p, --prompt STR         提示词；省略则读 stdin
    --prompt-file PATH   从 UTF-8 文件读
-c, --cid ID             续接会话：24 位 hex | @last（上一次）| @别名；省略 = 新建会话
    --new                强制新建（与 -c 互斥）
-s, --session NAME       本地别名：绑定到本次 cid（不存在则新建并绑定）
-a, --agent ID|NAME      main(默认) | drawing | 任意 24hex assistant_id
-m, --model NAME         selected_model（默认 glm-5.3-flash；`models` 可查枚举）
-e, --effort LEVEL       fast(→"") | thinking | deep_thinking     （深度/极致）
    --mode STR           chat_mode 原样透传（deep_research 等）
    --web / --no-web     is_networking（默认 web=on）
    --meta JSON          追加/覆盖 meta_data（逃生门，渠道私有参数直通）
    --stream-mode M      delta(默认) | frame | none
      delta  归一化事件流：meta/text/reasoning/image/done
      frame  上游原始帧逐条透传 {"type":"frame","raw":{...}}（仍做收尾校验）
      none   不输出中间事件，只在末尾输出一个 done 聚合对象
    --no-reasoning       抑制 reasoning 增量（只保留 done 里的 reasoning 全文）
    --save-images DIR    把 image 事件里的直链下载到本地（生图 URL 会过期，建议下载）
    --png                下载时追加 ?image_process=format,png 转真 PNG
    --cid-file PATH      额外把 conversation_id 单独写进文件（给 shell 管道用）
    --keep-alive         退出前不 flush?? （无：默认每行 flush，保证 agent 实时看到）
```

**人类用法**

```bash
glm2cli ask "用一句话介绍你自己"                        # 纯文本，聚合
glm2cli chat "从 1 数到 5" | jq -r 'select(.type=="text").delta' | tr -d '\n'
glm2cli chat -c @last "我上一个问题是什么"
printf '很长的中文提示词\n第二行' | glm2cli chat -s demo -e thinking
```

**agent 用法（无需 TTY，全靠契约）**

```bash
CID=$(glm2cli chat --format jsonl --prompt-file q.txt | tee out.jsonl |
      jq -r 'select(.type=="meta").conversation_id')
glm2cli chat --format jsonl --cid "$CID" --prompt "继续"   # 续接
grep '"type":"error"' out.jsonl                             # 判错，不用解析 stderr
```

### 3.2 `image` / `edit`

```
glm2cli image [prompt]  [chat 的通用 flag]
    --ratio R          1:1|3:4|4:3|9:16|16:9（默认 1:1）。非法值上游会**静默降级 1:1**
                       ⇒ CLI 侧默认直接 exit 2 拒绝，--allow-unknown-ratio 才透传（D8）
    --style / --scene  实测已失效，仅透传，help 里明确「不承诺效果」
    --pick N           一次返回 4 图，只保留前 N 张（默认 4）
    --rm-watermark     cogview.rm_label_watermark（取值效果 [未知]）
    --save-dir DIR / --png / --no-save   默认下载（URL 会过期）

glm2cli edit [prompt] \
    --image-url URL    ★ 唯一必需输入：**上游服务器可达的公网 URL**
                       （自家 CDN / 境内图床 / ghproxy 中转 OK；data URI、境外直连 = status 500）
    --image-file PATH  本地文件 → 需先托管：--upload-cmd "!cmd!" 外部命令回显 URL（D9）
    --title T          默认 "【智能编辑】 " + prompt[:40]（照抄前端约定）
    --rm-watermark / --save-dir / --png
    --ratio R          ⚠️ help 里写明「编辑模式强制跟随输入图比例，此参数无效」
```

### 3.3 会话管理（增删改查）

```
glm2cli conv list   [--page N --size N | --all] [--agent ID] [--since TIME] [--grep KW]
                    [--fields cid,title,updated,total] [--sort updated|created]
glm2cli conv create --prompt STR|--prompt-file P [--title T] [--agent ID] [-e LEVEL]
                    # = 发首条消息（服务端懒创建）+ 可选 rename；见 D6
glm2cli conv show   CID [--tail N]      # 元数据（recent_list 命中项）+ 最近 N 条本地历史
glm2cli conv history CID [--source auto|upstream|local] [--page-size 30] [--limit N]
                    [--role user|assistant|both] [--tail N]
                    # upstream = page_messages_v2 原样透传（已知返空 list，不写死字段）
                    # local    = 读 ~/.glm2cli/store/<cid>.jsonl；auto = upstream 空则回落 local
glm2cli conv rename  CID --title T [--title-file P]      # --yes 不需要（非破坏）
glm2cli conv title   CID                                  # 读上游自动标题
glm2cli conv delete  CID... [--agent ID] [--purge-local] [--yes]
                    # ⚠️ 生图会话必须 --agent drawing，否则上游静默无效
                    # ⚠️ 非 TTY 时缺 --yes 直接 exit 2（拒绝执行），不提供交互确认
glm2cli conv read    HISTORY_ID          # update_status
glm2cli conv stop    HISTORY_ID          # stop_stream（也可被 chat --deadline 内部复用）
```

### 3.4 其它

```
glm2cli auth check      解析凭据：uid/device_id/access exp/refresh exp/是否需刷新（不发请求）
glm2cli auth refresh    强制刷新并落盘（回读校验 refresh_token 已轮换）
glm2cli user            完整 user/info（--fields 精简；whoami 为别名）
glm2cli models          模型 + reasoning_efforts 枚举（--format text 打表）
glm2cli agents          内置 assistant 表（main/drawing/read/video/study）
glm2cli sessions        列本地别名/最近会话（--forget NAME）
glm2cli schema          机器可读契约（命令/flag/事件/退出码）
```

---

## 4. JSONL 事件模式（`v:1`，前向兼容）

每个事件一行；首事件恒为 `meta`，末事件恒为 `done` **或** `error`（二者互斥，绝不同时出现）。

```jsonl
{"v":1,"type":"meta","request_id":"3f2a…","conversation_id":"6abbaf3a…","message_id":"6abbaf3a…",
 "agent_id":"65940acf…","model":"glm-5.3-flash","effort":"thinking","new_conversation":true,"ts":1790644023}
{"v":1,"type":"reasoning","delta":"先算 3+5…","part":"2f2d0f54…"}
{"v":1,"type":"text","delta":"答案是 ","part":"6ad0…_1"}
{"v":1,"type":"image","url":"https://sfile.chatglm.cn/testpath/6ad0…_1_0.jpg",
 "polished_prompt":"一幅扁平插画…","index":1,"part":"6ad0…_2"}
{"v":1,"type":"frame","raw":{…}}                      // 仅 --stream-mode frame
{"v":1,"type":"warn","code":"unknown_ratio","message":"21:9 会被上游静默降级为 1:1"}
{"v":1,"type":"done","conversation_id":"…","message_id":"…","text":"全文（按 logic_id 顺序聚合）",
 "reasoning":"…","images":["…"],"parts":[{"logic_id":"…","text":"…","thinking":"…"}],
 "frames":57,"elapsed_ms":9231,"finished":true,"usage":null,
 "local":{"store":"~/.glm2cli/store/6abb….jsonl","session":"demo"}}
{"v":1,"type":"error","code":"stream_truncated","message":"共收 12 帧，末帧 status=init",
 "retryable":true,"http_status":0,"biz_status":0,"detail":"","request_id":"…","conversation_id":"…"}
```

要点（都是实证换来的，勿凭直觉改）：

- **`meta` 里的 `conversation_id` 就是续接句柄**（首轮由服务端懒创建、首帧回传，是唯一来源）。
- 非流式类命令（`user`/`conv list`/…）用同一信封：`{"v":1,"type":"result","data":{…}}` + `done`，
  这样 agent 只需一套解析器（见 D3）。
- `init` 帧 = 增量、`finish` 帧 = 该段全文 ⇒ 聚合器按 `logic_id` 累加、遇 `finish` **覆盖**；
  `done.text` 是权威全文，消费方拼 `text.delta` 结果应与之一致（不一致即丢帧，报 `warn`）。
- 上游**不发** `[DONE]`；正常收尾 = 顶层 `status:"finish"`。
- 生图流末尾有一行**不带 `data:` 前缀的裸 JSON 信封** `{"status":0,…,"rid":…}` ⇒ 解析器忽略它，
  但 `frame` 透传模式必须**过滤掉**，不能带给下游。
- `usage` 恒为 `null`：上游整条流没有任何 token/计费字段（实证），**不要伪造**。

---

## 5. 会话 id 语义与本地状态

### 5.1 三个 id，别混

| id | 含义 | 来源 | 用法 |
|---|---|---|---|
| `conversation_id` | **续接句柄**（一个会话 = (assistant_id, conversation_id) 二元组） | 上游首帧回传 | `--cid` 传它 |
| `message_id` (`history_id`) | 助手消息主键，== 帧 `parts[].id` | 上游帧 | `conv stop/read` |
| `request_id` | 本 CLI 每次调用的追踪号 | 本地生成 32hex，同时作为 `X-Request-Id` 上游 | 日志/排错关联 |

> D1：你说的「内部维持一个请求 id」，按协议应是 **conversation_id** 承担续接；`request_id` 只做日志。
> 我实现为「meta 事件里三个都给 + `-c @别名/@last` 语法糖」，让 agent 既可靠又省事。

### 5.2 状态目录（唯一有状态的东西）

```
~/.glm2cli/
  credential.json        # 与 Python 侧同格式（refresh_token/access_token/device_id/user_id/refer_991）
  cookies.json           # WAF cookie（acw_tc/cdn_sec_tc）跨进程复用，见 D12
  last_cid               # 最近一次 conversation_id（@last 的来源）
  index.json             # 别名表 + cid 摘要（title/agent/updated_at/turns）
  store/<cid>.jsonl      # 每轮追加：{"turn":1,"role":"user","text":…}/{"role":"assistant","text":…,"images":[…],"message_id":…}
```

- 只记录**经过本 CLI** 的对话 ⇒ `conv history --source local` 不是账号全量历史（网页端聊的不在这），
  这是 D5 的核心权衡。
- `credential.json` 与 `glm_credential.json` 双向兼容：Go 读 Python 的文件、写回同格式 ⇒ 换 token 不会互相打断
  （**refresh 会轮换 refresh_token，两个客户端不能同时持有同一份陈旧副本** ⇒ D10：是否加写入前的 mtime/版本号乐观锁）。
- 写 `index.json`/`store` 用 tmp+rename 原子替换；多进程并发用 `state/.lock` 文件锁（Windows `LockFileEx`/unix `flock`）。

---

## 6. 退出码（agent 只靠它 + error 事件判状态）

| code | 含义 | 对应 Python 异常 | 可重试 |
|---|---|---|---|
| 0 | 成功 | — | — |
| 1 | 上游业务/HTTP 错误、响应非 JSON（WAF 拦截页） | `GlmUpstreamError` | 视情况 |
| 2 | 用法错误（未知 flag/缺参数/非法 ratio/非 TTY 缺 `--yes`） | — | 否 |
| 3 | 凭据/鉴权失败（HTTP 401、业务 40102/40103、refresh 被拒、访客账号） | `GlmAuthError` | 否（需重新取凭据） |
| 4 | 风控瞬时拒绝，已耗尽退避（40012） | `GlmTransientRejection` | 是（降速） |
| 5 | 流被截断（未收到 finish/error），或 `--deadline` 触发 | `GlmStreamTruncated` | 是（幂等重试） |
| 6 | 被限流（HTTP 429） | `GlmRateLimitError` | 是（退避） |
| 7 | 能力未实现/未实证（如 `conv history --source upstream` 需要补抓包时） | — | 否 |

错误也**同时**以 `{"type":"error",...}` 写 stdout（`--errors-to-stdout` 默认开），
stderr 只留人读的一行摘要 ⇒ agent 不必分流两个 fd。

---

## 7. Go 工程结构与技术选型

```
chatglm-cli/                              # 模块 github.com/pyichiban/chatglm-cli（go 1.26）
  go.mod  README.md  LICENSE  .gitignore
  cmd/glm2cli/main.go                     # 入口（所以 go install 产出的二进制就叫 glm2cli）
  internal/glm/                           # 协议层：与 CLI 无关，可被别的 Go 程序复用
    constants.go  sign.go  cred.go  client.go  sse.go  api.go  history.go  event.go  errors.go
    testdata/                             # 真实帧回放：首轮(仅 finish 帧)/abort/生图 6 帧+裸信封/历史响应
  internal/cli/                           # 参数契约层
    cli.go  conv.go  raw.go  schema.go  console_windows.go  console_other.go
  docs/glm2cli设计.md                     # 本设计文档（协议文档在兄弟仓 chatglm2api/docs/）
  scripts/build.sh                        # win+linux 单文件到 dist/
```

> 命名约定：**仓库名 `chatglm-cli`，命令名 `glm2cli`**（历史沿用，文档与脚本里的 `glm2cli` 均指本仓）。

| 主题 | 决定 |
|---|---|
| HTTP | `net/http` + `net/http/cookiejar`；**不设 `http.Client.Timeout`**（流式无超时硬规则）；非流式用 `context.WithTimeout` |
| 压缩 | 最终只发 `Accept-Encoding: gzip, deflate` + stdlib 手工解压（`compress/gzip`、`compress/flate`），**零依赖**；实测上游并不压缩 SSE，全部线上请求通过（D11 定稿）。要完全一指纹则需 `klauspost/compress` + `andybalholm/brotli` |
| SSE | 手写行解析（`bufio.Scanner` + `Buffer(make([]byte,0,1<<20))` 放宽长帧），语义与 `sse.py` 逐条对齐 |
| 聚合 | `logic_id` 优先，退回 `id`/序号（同 Python `_part_key`）；`done` 前校验见过 finish/error，否则 `exit 5` |
| 参数解析 | 推荐 **stdlib `flag` + 20 行 alias 包装**（零依赖，D13）；要更好用的 help/补全则上 `cobra` |
| 凭据 | JWT 只 base64 解 payload（不验签）；refresh 后**必须** tmp+rename 落盘，落盘失败立刻报错退出 |
| 头指纹 | 逐头照抄 `sign.py`（IDE Electron 指纹：UA/sec-ch-ua/X-Exp-Groups/Sec-Fetch-*）；`X-Device-Id` == JWT.device_id；**Cookie 交给 jar，绝不设显式 Cookie 头** |
| 构建 | `CGO_ENABLED=0 GOOS=windows/linux GOARCH=amd64 go build -ldflags "-s -w"`；release 追求单文件小体积 |
| 测试 | 离线：帧回放锁死聚合语义（`testdata/`）+ 签名复算（PROTOCOL §2 的 6/6 命中样例）；线上：`scripts/smoke.sh` 手动跑 |

---

## 8. 实现计划

| 里程碑 | 内容 | 产出/验收 | 估量 |
|---|---|---|---|
| **M0** | 拍板 D1~D18（尤其 D5/D11/D13） | 本文档定稿 | 本轮对话 |
| **M1** | 协议内核：constants/sign/profile/credential/transport/sse | `go test ./internal/glm` 离线全绿：签名复算命中、抓包帧聚合 == Python `as_result()` 逐字段一致 | ~700 行，1 个会话 |
| **M2** | `chat` 闭环：delta/frame/none 三档、stdin/-p/--prompt-file、`-c/--cid` 续接、`@last/@别名`、退出码、meta 首行 | `conv list` 之外**只发 1 次线上请求**冒烟：新会话 → 记下 cid → 续接问「我上一句说了什么」→ `done.text` 与 delta 拼接一致 | ~400 行 |
| **M3** | `auth/user/whoami/models/agents` + `conv list/show/rename/title/delete/read/stop/create` | 全部 `--dry-run` 单测 + 线上只读命令各 1 次；delete 用 M2 产生的测试会话验证「agent_id 不匹配则静默无效」这条实证 | ~450 行 |
| **M4** | `image`（4 图 + polished prompt + 下载 + `--png` + ratio 校验）/ `edit`（公网 URL、比例跟随提示） | 各 1 次线上冒烟（间隔 ≥5s，不触发 40012）；产物落地文件名 `<history_id>_<n>.png` | ~250 行 |
| **M5** | 状态层：`store/*.jsonl` 存档、别名、`conv history --source auto` 兜底、cookie 持久化、`sessions` | 断网也能 `conv history --source local`；模拟陈旧 refresh 验证轮换落盘 | ~300 行 |
| **M6** | `schema` 子命令、`README.md`、README 快速上手（取凭据步骤）、Windows 编码提示、跨平台构建脚本 | 验收清单 §9 全绿；`go vet`/`gofmt` 干净；仓库 grep 不到明文凭据 | ~200 行 |

建议交付顺序即上表；M2 结束就能满足「多轮对话 + JSONL 流式」的主诉求，M3/M4 补齐 CRUD 与生图。

---

## 9. 验收清单

- [ ] `CGO_ENABLED=0` 交叉编译 windows/linux amd64 单文件，无运行期依赖
- [ ] 流式长回答（>60s）**不被超时掐断**（无总超时；`--deadline` 关闭）
- [ ] 断流场景产出 `stream_truncated` + exit 5，**无伪造 finish**
- [ ] refresh 轮换后磁盘上是新 refresh_token；`--no-save-cred` 时内存可用但不落盘且给出警告
- [ ] `--format jsonl` 每行都是合法 JSON（`while read -r l; do jq . <<<"$l"; done` 全过）
- [ ] `chat --stream-mode frame` 输出与浏览器抓包帧**逐帧一致**，且不含末尾裸 JSON 信封
- [ ] `conv delete` 对生图会话需 `--agent drawing` 才真删（验证静默无效这条实证）
- [ ] 离线测试覆盖：签名复算、init-delta/finish-全文、生图 6 帧、`one_to_more_finish` 空对象跳过、error 收尾、裸 JSON 信封
- [ ] agent 侧：只靠 `--format jsonl` + 退出码 + `schema` 即可完成「新会话 → 续接 → 查历史 → 改名 → 删除」全链路，不读 stderr
- [ ] 全仓 grep 不到 token/cookie 明文

---

## 10. 待讨论项（已定稿 ⇒ 见 §11；本节保留作为当时的问题清单与选项记录）

| # | 问题 | 选项 | **我的推荐** |
|---|---|---|---|
| **D1** | 「内部维持的请求 id」到底指什么？上游只有 `conversation_id` 能续接 | (a) 只用 cid (b) cid + 本地别名/`@last` | **(b)**：meta 回传 cid 是主契约，别名/`@last` 是省事的糖 |
| **D2** | 命令命名风格 | (a) 扁平 `chat/list/rename/delete` (b) 两级 `conv list/create/rename/delete` | **(b) 两级 + 保留扁平别名**（`list`→`conv list`），破坏性操作集中在 `conv` 下更好约束 |
| **D3** | 非流式命令（user/list/history…）也走同一 JSONL 信封？还是各自原样输出上游 JSON | (a) 统一 `{type:"result",data}` (b) 原样透传 | **(a)** 一套解析器；同时 `--raw` 可拿上游原样 JSON |
| **D4** | `done` 是否总是带全文 `text`（长回答 stdout 翻倍） | (a) 总是带 (b) 仅 `none` 模式带 (c) `--emit-full-text` 开关 | **(c) 默认 delta 模式不带全文**（省 token/流量），`--emit-full-text` 或 `none` 模式才带 |
| **D5** | 「查单对话历史」：上游 `page_messages_v2` 已知返空 list（5 次 + 2 个备选端点都不通） | (a) 只做 upstream 并如实报错 (b) 建本地 jsonl 存档当主数据源 (c) `--source auto` 回落 | **(c)**：默认 upstream→local 回落，并在 README 标注「local 只含经本 CLI 的对话」；**能否安排一次「打开真实会话并翻页」的抓包补齐实证？** |
| **D6** | 「增」：上游无创建端点 | (a) `conv create --prompt` = 发首条消息（真实消耗一次对话） (b) 不提供 create，只允许 `chat` 懒创建 | **(a)** 但 help 里写明「会真发一条消息」；空壳会话（`history_total:0`）无法用 API 造 |
| **D7** | 破坏性操作保护 | 非 TTY 缺 `--yes` 时：(a) exit 2 拒绝 (b) 仍执行（agent 更省事） | **(a)**：agent 加一个 flag 成本极低，误删代价高 |
| **D8** | 非法 `--ratio`（上游静默降级 1:1） | (a) CLI 直接拒 (b) 透传 + warn 事件 (c) 静默 | **(a)** + `--allow-unknown-ratio`；4:3 虽不在预设但实测可用 ⇒ 白名单里放行 |
| **D9** | `edit --image-file` 本地文件怎么变公网 URL | (a) 不提供，只收 `--image-url` (b) 内置 GitHub 临时仓库 + ghproxy 托管 (c) `--upload-cmd` 外部命令钩子 | **(c) + (a) 兜底**：托管策略是账号/网络相关的，CLI 不背；文档里给一段 ghproxy 配方脚本 |
| **D10** | Go/Python 共用同一份凭据文件：refresh 轮换互相打脸 | (a) 各自独立文件（默认同目录不同名） (b) 共用 + 写前乐观锁（mtime/版本） | **(b)**：读 Python 的 `glm_credential.json` 兼容，写回同格式；写前比对 mtime，冲突则先重读合并再写 |
| **D11** | `Accept-Encoding` 与压缩依赖 | (a) 原样发 `gzip, deflate, br, zstd` + 引 2 个压缩库（指纹完全一致） (b) 只发 `gzip, deflate` + 纯 stdlib（Go 自动解压） | **(a)** 更稳（WAF 校验的是头组合指纹）；但 (b) 依赖为零，**可否允许我用 1 次线上请求实测 (b) 是否被拒？** 实测通过就选 (b) |
| **D12** | WAF cookie（`acw_tc`/`cdn_sec_tc`）跨进程持久化 | (a) 落盘 `cookies.json` 复用 (b) 每次进程全新（与 Python 长驻 session 行为不同） | **(a)**：Python 侧靠 requests jar 在同进程内串起 cookie 链；CLI 是一次性进程，不落盘等于每次冷启动，POST 类 40012 概率上升。同时持久化 `refer__991` |
| **D13** | 参数库 | (a) stdlib `flag` + alias 包装（零依赖） (b) spf13/cobra（help/补全/分组更好） | **(a)** 起步，`schema` 自描述补齐 agent 友好度；若你更在意人手 help 体验再换 cobra |
| **D14** | 是否需要 `batch` 子命令（stdin 收 `{"cid","prompt"}` 多任务，串行/并发，输出带 job id 的 jsonl） | (a) M5 之后加 (b) 不做 | **(a)**：多 agent/批量改写提示词场景收益大，但**并发上限要做**（同账号连发易触发 40012） |
| **D15** | Windows 中文输出乱码 | (a) 文档要求 `chcp 65001` (b) 启动时 `SetConsoleOutputCP(65001)`（需 `golang.org/x/sys`） (c) 只保证重定向到文件是 UTF-8 | **(c) + (b)**：默认写 UTF-8 字节 + 可选 x/sys 一行调用（x/sys 不算重量级） |
| **D16** | 二进制放哪 / 怎么装 | (a) `go install` (b) 产出 `glm2cli.exe` 拷到 `D:/dev/Git/cmd/`（与 rg/fd/jq 同列） (c) 两者 | **(c)** |
| **D17** | 深度思考/联网等默认值要不要跟随网页版（`model=glm-5.3-flash`, `effort=""`, `is_networking=true`） | (a) 与 Python 侧一致 (b) 为 agent 场景改「快省」默认 | **(a)**：契约与既有实证一致，行为可预测；agent 显式传 `-e/--no-web` |
| **D18** | 与 Python 库的关系 | (a) Go 独立实现协议，常量两边各写一份（+ 快照测试比对） (b) 把 `glm2api` 退役 (c) Go 调 Python（不现实） | **(a)**：`internal/glm/constants_test.go` 里硬编码期望值，另加一条脚本 `scripts/check_consts.py` 导出 Python 常量做比对，防漂移 |

## 11. 决策定稿与实施状态（2026-10-16）

### 11.1 本轮定下的决策（用户 8 条指示 → 落到实现）

| # | 决定 | 实现位置 |
|---|---|---|
| 1 | **不做 image/edit 命令**（它们只是对话的包装） | 生图走 `chat --agent drawing --ratio`；§3.2 的两条命令标为「本版未做」 |
| 2 | 单会话历史到底能不能拿 | ✅ **能**，但必须换端点：`mainchat-api/conversation/messages`（§3.5.1 已补实证）；`page_messages_v2` 确认为死路 |
| 3 | **彻底放弃 WEB 通道** ⇒ 不需要 `refer__991`、**不做退避重试** | `constants.go` 只留 IDE 指纹；`client.go` 无重试循环，40012 直接 `transient_rejection` + exit 4 |
| 4 | **只支持流式**（输入 → JSONL 持续输出） | 删掉了 `--stream-mode none/frame`、聚合模式与 `ask` 命令 |
| 5 | 每次对话一个进程 ⇒ 是否需要持久化 | 只持久化**凭据文件**（refresh 轮换必须落盘）；不持久化 cookie/会话/别名（实测 IDE 通道一次 POST 不需 acw_tc 链路） |
| 6 | 不要 `@last` 这类语法糖 ⇒ **必须显式传 conversation_id** | `--cid` 严格校验 `^[0-9a-f]{24}$`，否则 exit 2 |
| 7 | 首版适当简化参数（不做输出格式选择） | 取消 `--format/--color/--stream-mode/--pick/--save-dir` 等；**所有命令统一输出 JSONL** |
| 8 | 落盘什么数据？多会话交叉会不会乱 | 只落 `credential.json`（tmp+rename 原子写）⇒ 无本地会话状态，**不可能交叉污染**；唯一的并发风险是多进程同时刷凭据，已用「写前比对磁盘」堵住 |

### 11.2 其它已定的契约细节

- **flag 可写在任意位置**：自定义解析把 argv 拆成「已知 flag 项」与「位置参数」（`internal/cli/cli.go:parse`），
  未知 flag 直接 exit 2；flag 顺序无关（`--cid X chat` ≡ `chat --cid X`）。
- **凭据来源**：`--cred` > `$GLM_CREDENTIAL_FILE` > `./glm_credential.json` > `~/.glm2cli/credential.json` > env；
  文件兼容 Python 侧格式，**未知键原样保留**（两边共用一个文件不丢字段）。
- **三个 id**：`conversation_id`（续接句柄）/ `message_id`=`history_id`（stop/read）/ `request_id`（日志），
  全部在 `meta`/`done`/`error` 事件里给出。
- `meta` **拿到首帧后才发** ⇒ 里面的 `conversation_id` 一定是服务端懒创建的真实值（而不是空串）。
- **Ctrl-C / 断流**：发 `stream_truncated` + exit 5，并尽力调 `stop_stream(message_id)` 中止服务端生成。
- **`raw` 逃生门**：`glm2cli raw --method GET --path chatglm/… --param k=v` 直打任意端点，
  本次发现历史端点就是靠它而不是改代码重编。

### 11.3 验收结果

- [x] `CGO_ENABLED=0` 单文件；零第三方依赖（`compress/gzip`+`flate` 手工解压，不发 br/zstd）
- [x] Go 侧 32 个离线用例全绿（`go test ./...`）：签名复算、头形状、凭据解析/落盘/并发拒写、请求体形状、
      flag 顺序无关解析、真实帧回放（finish-only 短回答 / error 收尾 / 生图 4 图 / 截断 / 多行 data / 裸信封噪声）、
      历史结构解析；Python 侧 20 个用例同步全绿
- [x] 线上冒烟（IDE 通道、无 refer_991、无重试）：`user`/`models`/`conv list`/`chat`首轮/`chat --cid` 续接/
      `conv history`/`rename`/`title`/`show`/`delete`/`read`/`stop`/生图（4 图 12.0s）均成功
- [x] **delta 拼接 == 权威全文**：线上实测 text_chars=22 / reasoning_chars=91 与拼接结果逐字符相等
- [x] 复现两条关键实证：错 `--agent` 删除**静默无效**（返 ok 但会话仍在）；正确 agent 删除后 `found_in_recent:false`
- [x] `auth refresh` 后新凭据可继续使用（落盘链路通）；并记录了「refresh 非必然轮换」的实测异象
- [x] 全仓 grep 不到明文凭据（`auth` 输出里 token 被截成 `eyJhbGci…(len=364)`）

### 11.4 待做（下一轮）

| # | 项 | 备注 |
|---|---|---|
| N1 | 图产物下载（`--save-dir/--png`） | 目前只给直链；图链会过期，agent 需要自己下 |
| N2 | `edit`（参考图改图） | 单图、只收公网 URL；输出比例跟随输入图 |
| N3 | ~~Python 侧 `glm2api` 同步新历史端点~~ **已完成** | `glm2api.history()` 已改指 `mainchat-api/conversation/messages`，旧端点降为 `history_v2()`；`python -m glm2api history <cid>` 线上验证拉到 3 轮真实消息 |
| N4 | 常量防漂移比对脚本 | `scripts/check_consts.py` 导出 Python 常量与 Go 常量表对排 |
| N5 | `--effort/--mode` 映射实证 | 上游两套枚举并存，本版只透传；需一次受控对比实验 |
| N6 | `chat --title` 与 rename 的失败语义 | 改名失败目前直接 exit；可考虑降级成 warn |
| N7 | 非流式超时的真实覆盖 | 本版 `Do()` 已用 `context.WithTimeout`（之前 `--timeout` 只存不用的 bug 已修）；建议再跑一次弱网/挂起验证 |


> 补充说明：旧清单里的 D5（历史端点无实证）已由 PROTOCOL §3.5.1 的实测直接解决，不需要再补抓包；
> D11（压缩依赖）最终选“只发 `gzip, deflate` + stdlib 解压”，本轮全部线上请求已验证；
> D12（cookie 跳进程持久化）结论为不需要（一次性 POST 不依赖 acw_tc 链路，全部请求零 40012）。
> 另记一条实测异象：强制 `user/refresh` 只轮换了 access，refresh_token 原值返回 ⇒ **轮换非必然**；
> 实现按「响应里 refresh 非空就覆盖 + 只要有刷新就落盘」写，两种情况都安全。

### 10.2 已知的上游缺口（继承，不隐瞒）

| 项 | 状态 |
|---|---|
| `page_messages_v2` 的 `list` 元素结构 / 翻页参数名 | ✅ **已解决**：该端点确实是死路（始终空 list），可用端点是 `mainchat-api/conversation/messages`（无分页）⇒ 见 PROTOCOL §3.5.1 |
| `style` / `scene` 生图参数 | ⚠️ 实测已失效，仅透传不承诺效果 |
| `rm_label_watermark` 是否真去水印 | ❌ [未知] |
| 产物直链有效期 | ❌ [未知] ⇒ 需要长期保留请自行下载（本版未做 `--save-dir`） |
| `chat_upload` | ❌ 当前 100% 40012，不实现 |
| 多图输入（一次改图给多张参考图） | ❌ 上游单图；多角色请合并到一张参考图 |
