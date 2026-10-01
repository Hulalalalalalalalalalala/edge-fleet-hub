package fleet

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// pageCursor is the opaque, tamper-evident continuation token used by the
// history endpoint. It pins the device, filter and first-page high-water mark
// so a paged walk cannot see rows written after the first page.
type pageCursor struct {
	Instance  string    `json:"i,omitempty"`
	DeviceID  string    `json:"d"`
	From      time.Time `json:"f,omitempty"`
	To        time.Time `json:"t,omitempty"`
	HighWater int64     `json:"h"`
	ScanPos   int64     `json:"p"` // 0-based event slot the next page starts at
}

var cursorSecret = []byte("edge-fleet-hub-history-cursor-v1")

// cursorSigningKey derives the HMAC key for a data directory instance. A
// cursor minted in one directory (even for the same device id) cannot be
// validated in another directory. The empty instance is the in-memory mode,
// which keeps the original fixed key.
func cursorSigningKey(instance string) []byte {
	if instance == "" {
		return cursorSecret
	}
	return []byte("edge-fleet-hub-history-cursor-v1:" + instance)
}

// encodeCursor signs the cursor with an HMAC and renders it as base64rawurl.
func encodeCursor(c pageCursor) string {
	payload, _ := json.Marshal(c)
	mac := hmac.New(sha256.New, cursorSigningKey(c.Instance))
	mac.Write(payload)
	token := append(mac.Sum(nil), payload...)
	return base64.RawURLEncoding.EncodeToString(token)
}

// decodeCursor verifies the signature and parses the cursor, requiring it to
// belong to expectedInstance.
func decodeCursor(raw, expectedInstance string) (pageCursor, error) {
	token, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil || len(token) <= sha256.Size {
		return pageCursor{}, errors.New("invalid cursor")
	}
	signature, payload := token[:sha256.Size], token[sha256.Size:]
	mac := hmac.New(sha256.New, cursorSigningKey(expectedInstance))
	mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return pageCursor{}, errors.New("invalid cursor")
	}
	var cursor pageCursor
	if err := json.Unmarshal(payload, &cursor); err != nil {
		return pageCursor{}, errors.New("invalid cursor")
	}
	if cursor.Instance != expectedInstance {
		return pageCursor{}, errors.New("invalid cursor")
	}
	return cursor, nil
}
