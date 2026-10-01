# ilink 与上游的对应关系

`internal/ilink` 是微信 ClawBot（iLink）协议的 Go 实现，跟随腾讯官方插件 [Tencent/openclaw-weixin](https://github.com/Tencent/openclaw-weixin)。官方插件是 OpenClaw 的 TypeScript 频道插件，不能直接引用，所以协议部分在这里按它的协议文档和源码重写。当前对齐的版本和文件指纹见 `upstream.lock`。

## 文件对应

| 本包 | 上游 | 内容 |
|---|---|---|
| `client.go` | `src/api/api.ts`、`src/api/session-guard.ts`（常量） | 请求头、`base_info`、`getupdates`、`sendmessage`、`notifystart/stop`、错误码 |
| `login.go` | `src/auth/login-qr.ts` | 二维码登录与各状态 |
| `aes.go`、`client.go` 的 `Download` | `src/cdn/aes-ecb.ts`、`src/cdn/cdn-url.ts`、`src/cdn/pic-decrypt.ts` | CDN 下载与 AES-128-ECB |
| `types.go`、`drift.go` | `src/api/types.ts` | 消息结构；已知字段清单 |
| `../ingest/poller.go` | `src/monitor/monitor.ts` | 长轮询、重试与退避、游标 |
| `../session/session.go` | `src/api/session-guard.ts` | `-14` 暂停 1 小时 |
| `../ingest/service.go` 的 `resolveRef` | `src/messaging/quote-store.ts` | 只带 `svr_id` 的引用：用自己发过的消息还原 |
| — | `docs/protocol_zh_CN.md` | 协议说明，同步时优先读它的变化 |

## 与上游一致的行为

- 请求头：`iLink-App-Id: bot`，`iLink-App-ClientVersion` 按 `0x00MMNNPP` 编码 `ChannelVersion`；POST 带 `AuthorizationType`、随机 `X-WECHAT-UIN`；二维码状态轮询只带应用头。
- `getupdates`：超时视为空结果；按响应里的 `longpolling_timeout_ms` 调整下次超时（默认 35 秒）；只有返回的游标非空才保存。
- 失败重试：2 秒后重试，连续 3 次失败退避 30 秒。
- `-14`（`ret` 或 `errcode`）：暂停该账号的全部收发 1 小时，到点自动恢复。
- 登录：最多上送最近 10 个 `bot_token`；状态查询请求失败按 `wait` 处理；`scaned_but_redirect` 切换地址。
- 媒体：优先 `full_url`，否则用 `encrypt_query_param` 拼 CDN 地址；图片优先十六进制 `aeskey`；语音、文件、视频缺 `aes_key` 时跳过，不当明文。
- `sendmessage` 非零 `ret` 视为失败；成功时记下返回的 `message_id`，用来还原以后只带 `svr_id` 的引用。

## 与上游不同的地方

- 一条消息解析失败（进 `Updates.Undecodable`，日志 WARN）不影响同批其他消息；上游整批失败。
- 本批处理失败时不推进游标，连续 5 批失败才跳过；上游不保留。
- `sendmessage` 在 `ret==0` 但 `errcode≠0` 时也视为失败；上游只看 `ret`。
- 「处理失败不推进游标」依赖服务端按旧游标重发这一批：**实测：用同一个旧游标连续两次 getupdates 都返回同一条消息**。
- 只接收凭证里 `ilink_user_id` 对应的主人消息，其他来源拒收。

- 实测新版引用只带 `message_item{type:0,msg_id}`，`msg_id` 即被引用消息的服务端 message_id；我们用它查 `sent_msgs` 还原（查不到再查 items，最后才用 `svr_id`）。type 0 是上游 `MessageItemType.NONE`，不算协议漂移。

## 有意不实现的部分

上传媒体（`getuploadurl`）、输入状态（`sendtyping`、`getconfig`）、工具调用进度消息（类型 11、12）、SILK 转码、引用缓存里的媒体保留。第一期不需要；需要时按上表找上游文件对照实现。

## 怎么发现上游变了

1. **定期检查**：`make check-upstream`（先编译 `tools/check-upstream` 再运行；不用 `go run`，它会把退出码 2 折成 1）。对比 `upstream.lock` 里的版本和协议相关文件的 blob SHA，有变化时输出报告（版本、改动的文件、新的变更日志段落、对比链接），退出码 1。仓库在 GitHub 上时，`.github/workflows/upstream-check.yml` 每周一自动跑，有变化就开或更新一个 issue。
2. **运行时告警**：收到 `drift.go` 已知字段之外的字段或消息类型时，日志打 WARN，记进 kv `ilink.drift`，网页状态栏提示。
3. **样例测试**：`testdata/messages/` 下是脱敏后的真实消息，`TestFixtures` 保证它们能解析、没有未知字段、确实已脱敏。

## 同步流程

1. 跑 `make check-upstream`，按报告里的对比链接看改动，先读 `docs/protocol_zh_CN.md` 的变化，再看对应源码。
2. 只是 OpenClaw 宿主适配、协议没变：直接到第 5 步。
3. 协议变了：按「文件对应」改本包（和 poller、session），`ChannelVersion` 改成上游新版本号；新字段加进 `drift.go`；有真实样例的话用 `tools/sanitize-fixture` 生成并人工检查后加进 `testdata/messages/`；补测试。
4. `make test`，再在真实微信上冒烟一次（收文字、图片、语音、文件、引用回复）。
5. `make check-upstream ARGS=-update` 更新 `upstream.lock`，和代码一起提交：`chore(ilink): 同步上游 <版本>`，提交说明里写清楚协议改了什么、我们改了什么。
