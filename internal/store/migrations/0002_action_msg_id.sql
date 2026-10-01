-- 指令记录带上来源消息的 msg_id，用于重放时按 msg_id 查重。
ALTER TABLE actions ADD COLUMN msg_id TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX actions_msg_id_once ON actions(msg_id) WHERE msg_id <> '';
