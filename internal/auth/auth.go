// Package auth 是网页密码的哈希、校验、会话令牌和防暴力破解计数。只用标准库。
//
// 密码记录存在 settings 表的 "auth" 键里（JSON，见 Record），只存 PBKDF2-SHA256 的结果和盐；
// 会话令牌只把 sha256 存进数据库（见 store 的 web_sessions），cookie 里才是令牌本身。
package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"unicode/utf8"
)

const (
	// Algorithm 是记录里的算法名。
	Algorithm = "pbkdf2-sha256"
	// DefaultIterations 是新密码的迭代次数（OWASP 2023 对 PBKDF2-HMAC-SHA256 的建议值）。
	DefaultIterations = 600000
	saltLen           = 16
	keyLen            = 32
	// maxIterations 防止库里被改成离谱的值，一次校验就把 CPU 占满。
	maxIterations = 10_000_000

	// MaxLen 是密码长度（按字符计）的上限，只为防止超长输入；下限只要求不为空（spec 0004）。
	MaxLen = 256
)

// Record 是保存下来的密码记录。不含明文；也不要把它写进日志。
type Record struct {
	Alg  string `json:"alg"`
	Iter int    `json:"iter"`
	Salt []byte `json:"salt"` // JSON 里是 base64
	Hash []byte `json:"hash"`
}

// CheckNew 检查新密码是否合格，不合格时返回给用户看的中文说明。
func CheckNew(pw string) error {
	n := utf8.RuneCountInString(pw)
	switch {
	case n == 0:
		return errors.New("密码不能为空")
	case n > MaxLen:
		return fmt.Errorf("密码最多 %d 位", MaxLen)
	}
	return nil
}

// Hash 用随机盐和 iter 次迭代生成记录；iter <= 0 时用 DefaultIterations。
func Hash(pw string, iter int) (Record, error) {
	if iter <= 0 {
		iter = DefaultIterations
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return Record{}, err
	}
	k, err := pbkdf2.Key(sha256.New, pw, salt, iter, keyLen)
	if err != nil {
		return Record{}, err
	}
	return Record{Alg: Algorithm, Iter: iter, Salt: salt, Hash: k}, nil
}

// Verify 校验密码，比较用常数时间。记录不完整或算法不认识时一律返回 false。
func (r Record) Verify(pw string) bool {
	if r.Alg != Algorithm || r.Iter < 1 || r.Iter > maxIterations || len(r.Salt) == 0 || len(r.Hash) == 0 {
		return false
	}
	if len(pw) > MaxLen*utf8.UTFMax {
		return false
	}
	k, err := pbkdf2.Key(sha256.New, pw, r.Salt, r.Iter, len(r.Hash))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(k, r.Hash) == 1
}

// Encode 把记录编成存进数据库的 JSON。
func (r Record) Encode() (string, error) {
	b, err := json.Marshal(r)
	return string(b), err
}

// Decode 解析数据库里的记录。
func Decode(raw string) (Record, error) {
	var r Record
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return Record{}, errors.New("auth: 密码记录格式不对")
	}
	if r.Alg == "" || len(r.Hash) == 0 {
		return Record{}, errors.New("auth: 密码记录不完整")
	}
	return r, nil
}

// NewToken 生成 32 字节随机会话令牌（base64url，放进 cookie）。
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// TokenHash 是令牌在数据库里的样子：sha256 的十六进制。
func TokenHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// CodeLen 是微信登录验证码的位数。
const CodeLen = 6

// NewCode 生成一个 6 位数字验证码（crypto/rand，均匀分布，可以有前导 0）。
func NewCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// CodeHash 是验证码在数据库里的样子。
func CodeHash(code string) string { return TokenHash("login-code:" + code) }

// ParseCode 识别微信里发来的验证码：去掉首尾空白后正好是 6 位数字，或者「登录 123456」（中间空白可有可无）。
func ParseCode(text string) (string, bool) {
	t := strings.TrimSpace(text)
	if rest, ok := strings.CutPrefix(t, "登录"); ok {
		t = strings.TrimSpace(rest)
	}
	if len(t) != CodeLen {
		return "", false
	}
	for i := 0; i < len(t); i++ {
		if t[i] < '0' || t[i] > '9' {
			return "", false
		}
	}
	return t, true
}
