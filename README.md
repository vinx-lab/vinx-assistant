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

**安全提示**：第一期网页没有密码，请只在可信网络内监听。
