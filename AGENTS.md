# Vinx 助手 agent 协作约定

适用于所有在本仓库工作的 agent 和贡献者。

## 定位

Vinx 助手（`vinx-assistant`）是单用户的微信收集箱：在微信里把内容发给一个 ClawBot，后台定时用 AI 归类、打标签、写摘要，待办到期在微信里提醒，网页上逐条查看。单个 Go 二进制、单进程、SQLite。需求见 `docs/specs/`。

不得把拟定的方案称为已实现、已验证。

## 构建与测试

- `make test`：全部单元测试和假后端链路测试。
- `make vet`：静态检查。
- `make build`：构建 `dist/vinx-assistant`。
- 测试只用 `t.TempDir()` 和 `httptest`，不连真实微信和真实 AI 服务。
- 提交信息：`<type>(<包名>): <摘要>`，如 `feat(ingest): 前缀解析`。

## 文件与数据边界

- 运行数据在数据目录（`--data`），不进仓库；仓库里的 `data/`、`*.db` 已被忽略。
- 数据库迁移放 `internal/store/migrations/NNNN_名称.sql`，只新增，不修改已有文件。
- 日志和错误里不得出现 API 密钥、`bot_token`、`context_token`，统一用 `internal/redact`。

## 需求与记录

- 较大的改动先在 `docs/specs/` 写说明（格式见 `docs/specs/README.md`），完成后在同一文件补结果。
- 这些文件会公开：不写本机路径、主机名、内网地址、个人信息。

## 验证与交接

区分单元测试、假后端链路测试和真实微信验收；只有对应验证完成才报告通过。交接时说明修改的文件、执行过的验证、未验证项。
