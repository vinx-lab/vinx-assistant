package ilink

import (
	"bytes"
	"crypto/aes"
	"errors"
)

var errPadding = errors.New("ilink: 解密后填充不合法（密钥可能不对）")

// DecryptECB 是微信 CDN 媒体用的 AES-128-ECB + PKCS7。
func DecryptECB(data, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("ilink: 密文长度不是 16 的倍数")
	}
	out := make([]byte, len(data))
	for i := 0; i < len(data); i += aes.BlockSize {
		block.Decrypt(out[i:i+aes.BlockSize], data[i:i+aes.BlockSize])
	}
	n := int(out[len(out)-1])
	if n == 0 || n > aes.BlockSize {
		return nil, errPadding
	}
	if !bytes.Equal(out[len(out)-n:], bytes.Repeat([]byte{byte(n)}, n)) {
		return nil, errPadding
	}
	return out[:len(out)-n], nil
}

// EncryptECB 只给测试和假后端用。
func EncryptECB(plain, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	n := aes.BlockSize - len(plain)%aes.BlockSize
	buf := append(append([]byte{}, plain...), bytes.Repeat([]byte{byte(n)}, n)...)
	for i := 0; i < len(buf); i += aes.BlockSize {
		block.Encrypt(buf[i:i+aes.BlockSize], buf[i:i+aes.BlockSize])
	}
	return buf, nil
}
