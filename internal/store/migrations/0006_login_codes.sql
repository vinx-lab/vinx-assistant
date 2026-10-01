-- 微信验证码登录（spec 0004）：网页取码，主人在微信里把验证码发给 Bot，收件时确认，浏览器凭短期 cookie 换会话。
-- 只存验证码和浏览器随机值的 sha256（十六进制）；时间是 Unix 秒。confirmed_at 为空表示还没在微信里确认。
CREATE TABLE login_codes (
  id INTEGER PRIMARY KEY,
  code_hash TEXT NOT NULL,
  browser_hash TEXT NOT NULL UNIQUE,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  confirmed_at INTEGER,
  polls INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX login_codes_code ON login_codes (code_hash);
