-- 网页登录会话：只存令牌的 sha256（十六进制），令牌本身只在浏览器 cookie 里。时间是 Unix 秒。
CREATE TABLE web_sessions (
  token_hash TEXT PRIMARY KEY,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE INDEX web_sessions_expires ON web_sessions (expires_at);
