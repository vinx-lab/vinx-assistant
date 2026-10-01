-- 需要 AI 翻译的指令：收件时先按 msg_id 落库再交给后台执行，进程重启后从这里续做。
-- done_at 为空表示还没处理完；处理完保留一段时间，用于重放时查重。
CREATE TABLE ai_commands (
  msg_id     TEXT PRIMARY KEY,
  text       TEXT NOT NULL,
  ref_text   TEXT NOT NULL DEFAULT '',
  has_word   INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  done_at    INTEGER,
  reply      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX ai_commands_pending ON ai_commands(created_at) WHERE done_at IS NULL;
