-- 0004 起「需要登录」由开关决定。升级前已设密码的部署自动打开，保持 0003 的行为（设了密码就要登录）。
INSERT INTO settings (key, value)
SELECT 'login_required', '1'
WHERE EXISTS (SELECT 1 FROM settings WHERE key = 'auth')
  AND NOT EXISTS (SELECT 1 FROM settings WHERE key = 'login_required');
