-- 0009 标题来源：'' 表示未设置或由 AI 生成，'manual' 表示用户在详情页手动改过（AI 整理不再覆盖）
ALTER TABLE items ADD COLUMN title_by TEXT NOT NULL DEFAULT '';
