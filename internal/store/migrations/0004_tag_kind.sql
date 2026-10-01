-- 0004 标签分两种（spec 0002）：label 类别标签（关键词规则产生），topic 内容标签（AI 生成）
ALTER TABLE tags ADD COLUMN kind TEXT NOT NULL DEFAULT 'topic';
UPDATE tags SET kind = 'label' WHERE name IN ('待办', '点子', '待研究', '稍后看', '资料');
