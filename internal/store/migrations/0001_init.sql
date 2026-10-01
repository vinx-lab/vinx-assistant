-- 0001 初始表结构（spec 0001）
CREATE TABLE items (
  id               INTEGER PRIMARY KEY,
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL,
  msg_id           TEXT    NOT NULL UNIQUE,
  raw_text         TEXT    NOT NULL DEFAULT '',
  url              TEXT    NOT NULL DEFAULT '',
  link_title       TEXT    NOT NULL DEFAULT '',
  link_desc        TEXT    NOT NULL DEFAULT '',
  category         TEXT    NOT NULL DEFAULT 'inbox'
                   CHECK (category IN ('inbox','research','later','todo','idea','archive')),
  category_by      TEXT    NOT NULL DEFAULT 'ai' CHECK (category_by IN ('prefix','ai','manual')),
  level            TEXT    NOT NULL DEFAULT 'light' CHECK (level IN ('light','medium','deep')),
  status           TEXT    NOT NULL DEFAULT 'new',
  title            TEXT    NOT NULL DEFAULT '',
  summary          TEXT    NOT NULL DEFAULT '',
  detail           TEXT    NOT NULL DEFAULT '',
  priority         TEXT    NOT NULL DEFAULT '' CHECK (priority IN ('','high','medium','low')),
  due_at           INTEGER,
  due_has_time     INTEGER NOT NULL DEFAULT 0,
  processed_level  TEXT    NOT NULL DEFAULT '' CHECK (processed_level IN ('','light','medium','deep')),
  process_error    TEXT    NOT NULL DEFAULT '',
  process_attempts INTEGER NOT NULL DEFAULT 0,
  tokens_used      INTEGER NOT NULL DEFAULT 0,
  raw_json         TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX items_category_status ON items(category, status);
CREATE INDEX items_due ON items(due_at) WHERE due_at IS NOT NULL;

CREATE TABLE seen_msgs (msg_id TEXT PRIMARY KEY, seen_at INTEGER NOT NULL);

CREATE TABLE attachments (
  id         INTEGER PRIMARY KEY,
  item_id    INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  kind       TEXT    NOT NULL CHECK (kind IN ('image','voice','file','video')),
  rel_path   TEXT    NOT NULL DEFAULT '',
  file_name  TEXT    NOT NULL DEFAULT '',
  size       INTEGER NOT NULL DEFAULT 0,
  md5        TEXT    NOT NULL DEFAULT '',
  state      TEXT    NOT NULL DEFAULT 'pending' CHECK (state IN ('ok','pending','failed')),
  attempts   INTEGER NOT NULL DEFAULT 0,
  last_error TEXT    NOT NULL DEFAULT '',
  media_json TEXT    NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX attachments_item ON attachments(item_id);

CREATE TABLE tags (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE);
CREATE TABLE item_tags (
  item_id INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  tag_id  INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
  PRIMARY KEY (item_id, tag_id)
);

CREATE TABLE reminders (
  id           INTEGER PRIMARY KEY,
  kind         TEXT    NOT NULL CHECK (kind IN ('digest','due')),
  item_id      INTEGER REFERENCES items(id) ON DELETE CASCADE,
  scheduled_at INTEGER NOT NULL,
  state        TEXT    NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','sent','deferred','dropped')),
  attempts     INTEGER NOT NULL DEFAULT 0,
  last_error   TEXT    NOT NULL DEFAULT '',
  body         TEXT    NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,
  sent_at      INTEGER
);
CREATE UNIQUE INDEX reminders_due_once ON reminders(item_id, scheduled_at) WHERE kind = 'due';
CREATE UNIQUE INDEX reminders_digest_once ON reminders(scheduled_at) WHERE kind = 'digest';

CREATE TABLE actions (
  id         INTEGER PRIMARY KEY,
  command    TEXT    NOT NULL,
  item_id    INTEGER REFERENCES items(id) ON DELETE SET NULL,
  before     TEXT    NOT NULL DEFAULT '{}',
  after      TEXT    NOT NULL DEFAULT '{}',
  undone     INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);

CREATE TABLE llm_usage (
  id                INTEGER PRIMARY KEY,
  day               TEXT    NOT NULL,         -- 'YYYY-MM-DD'（上海时间）
  level             TEXT    NOT NULL,         -- light/medium/deep/command
  provider          TEXT    NOT NULL DEFAULT '',
  model             TEXT    NOT NULL,
  prompt_tokens     INTEGER NOT NULL,
  completion_tokens INTEGER NOT NULL,
  item_id           INTEGER REFERENCES items(id) ON DELETE SET NULL,
  created_at        INTEGER NOT NULL
);
CREATE INDEX llm_usage_day ON llm_usage(day);

CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE kv (key TEXT PRIMARY KEY, value TEXT NOT NULL);

CREATE VIRTUAL TABLE items_fts USING fts5(title, raw_text, summary, detail, tags, link_title, tokenize = 'trigram');

-- 我们发出的消息：sendmessage 返回的 message_id → 正文。新版微信的引用只带 svr_id，靠这张表还原。
CREATE TABLE sent_msgs (msg_id TEXT PRIMARY KEY, body TEXT NOT NULL, sent_at INTEGER NOT NULL);
CREATE INDEX sent_msgs_at ON sent_msgs(sent_at);
