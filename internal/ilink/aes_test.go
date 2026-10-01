package ilink

import (
	"bytes"
	"testing"
)

func TestECBRoundTrip(t *testing.T) {
	key := []byte("0123456789abcdef")
	for _, n := range []int{0, 1, 15, 16, 17, 1000} {
		plain := bytes.Repeat([]byte{'x'}, n)
		enc, err := EncryptECB(plain, key)
		if err != nil {
			t.Fatal(err)
		}
		if len(enc)%16 != 0 || len(enc) <= n {
			t.Fatalf("n=%d enc len %d", n, len(enc))
		}
		dec, err := DecryptECB(enc, key)
		if err != nil || !bytes.Equal(dec, plain) {
			t.Fatalf("n=%d dec mismatch err=%v", n, err)
		}
	}
}

func TestDecryptRejectsBadInput(t *testing.T) {
	key := []byte("0123456789abcdef")
	if _, err := DecryptECB([]byte("short"), key); err == nil {
		t.Fatal("want error for non-block length")
	}
	enc, _ := EncryptECB([]byte("hello"), key)
	if _, err := DecryptECB(enc, []byte("fedcba9876543210")); err == nil {
		t.Fatal("want padding error with wrong key")
	}
}
