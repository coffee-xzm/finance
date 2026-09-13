package store

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
)

func mkdirAll(dir string) error { return os.MkdirAll(dir, 0o755) }

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
