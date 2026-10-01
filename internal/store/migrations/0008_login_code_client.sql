-- 记录发起登录的来源（spec 0004 安全审查 F1）：Bot 确认时把设备和地址告诉主人，便于察觉别人诱骗发码。
-- client_ua 只存「浏览器 / 系统」这样的摘要，不存完整 UA。
ALTER TABLE login_codes ADD COLUMN client_ip TEXT NOT NULL DEFAULT '';
ALTER TABLE login_codes ADD COLUMN client_ua TEXT NOT NULL DEFAULT '';
