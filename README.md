# Vinx 助手

单用户的微信收集箱：在微信里发给 Bot 就算收下，后台定时用 AI 归类、打标签、写摘要，待办到期在微信里提醒，网页上逐条查看。

> 开发中，第一期需求见 [docs/specs/0001-wechat-inbox-mvp.md](docs/specs/0001-wechat-inbox-mvp.md)。

## 构建

需要 Go 1.27+。

    make build        # 生成 dist/vinx-assistant
    make test

## 使用

    vinx-assistant login     # 终端扫码登录微信 ClawBot
    vinx-assistant serve     # 常驻运行：收消息、定时整理、网页（默认 0.0.0.0:3100）
    vinx-assistant backup    # 运行中导出数据库快照和附件
    vinx-assistant version

参数：`--listen`（`VINX_LISTEN`，默认 `0.0.0.0:3100`）、`--data`（`VINX_DATA`，默认 `~/.local/share/vinx-assistant`）。需要代理时设置标准的 `HTTPS_PROXY`。

**凭证提示**：数据目录和 `backup` 导出的压缩包里含微信登录凭证（bot_token），请保持私有（目录权限 700），不要上传或分享。

## 网页

`serve` 启动后打开 `http://<监听地址>/`：

- 看板：按分类看条目，改状态、标记「深入研究」、「立即整理」；顶部显示逾期待办、微信连接、今日 token、下次整理时间。
- 详情：原文、AI 整理笔记、附件，可改分类、标签、截止时间、优先级。
- 搜索、用量、设置（AI 服务商与三档模型、整理时间、每日摘要时间、分类关键词和操作词）、微信扫码登录。

**AI 设置示例（DeepSeek）**：设置页「添加服务商」填名称 `DeepSeek`、API 地址 `https://api.deepseek.com`（程序会自动补 `/v1`）、密钥；再在三档模型里选这个服务商，模型填 `deepseek-chat`（可点「拉取模型列表」）。轻量档同时用于微信指令的 AI 兜底（如「完成材料那个」），这部分用量单独记为 command，不占每日整理的 token 上限。

**用域名访问**：网页只认 `localhost`、回环地址和本机网卡 IP 作为 Host（防 DNS rebinding）。用域名（如 Tailscale MagicDNS 名称）访问时，把域名写进环境变量 `VINX_ALLOWED_HOSTS`，多个用逗号分隔，例如 `VINX_ALLOWED_HOSTS=vinx.tailnet-xxxx.ts.net`。

**安全提示**：网页默认没有密码。跨站提交会被拒绝，但任何能访问这个端口的人都能查看和修改全部条目与设置，请只在可信网络内监听（如 `--listen 127.0.0.1:3100` 或只在 Tailscale 网内开放）。

## 微信指令与提醒

- 回复「完成 12」「推迟 12 明天」「改到 12 10-08 15:00」「取消 12」「列表」「撤销」直接操作待办；引用 Bot 发的提醒只回「完成」也行。
- 固定格式认不出的，在配置了轻量档 AI 时交给 AI 理解：先回「收到，正在理解…」，理解完再回结果；AI 认为不是指令的消息照常存成条目。
- 每天在设置的摘要时间发一份今日摘要，待办到期时发提醒。微信只能在你最近发过消息后回复，发不出去的提醒会在你下一次发消息时随回执补发。
