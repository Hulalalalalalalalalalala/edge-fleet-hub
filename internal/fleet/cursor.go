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
	DeviceID  string    `json:"d"`
	From      time.Time `json:"f,omitempty"`
	To        time.Time `json:"t,omitempty"`
	HighWater int64     `json:"h"`
	ScanPos   int64     `json:"p"`           // 0-based event slot the next page starts at
	IID       []byte    `json:"i,omitempty"` // data-directory instance scope
}

var cursorSecret = []byte("edge-fleet-hub-history-cursor-v1")

// encodeCursor signs the cursor with an HMAC and renders it as base64rawurl.
func encodeCursor(c pageCursor) string {
	payload, _ := json.Marshal(c)
	mac := hmac.New(sha256.New, cursorSecret)
	mac.Write(payload)
	token := append(mac.Sum(nil), payload...)
	return base64.RawURLEncoding.EncodeToString(token)
}

// decodeCursor verifies the signature and parses the cursor.
func decodeCursor(raw string) (pageCursor, error) {
	token, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil || len(token) <= sha256.Size {
		return pageCursor{}, errors.New("invalid cursor")
	}
	signature, payload := token[:sha256.Size], token[sha256.Size:]
	mac := hmac.New(sha256.New, cursorSecret)
	mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return pageCursor{}, errors.New("invalid cursor")
	}
	var cursor pageCursor
	if err := json.Unmarshal(payload, &cursor); err != nil {
		return pageCursor{}, errors.New("invalid cursor")
	}
	return cursor, nil
}
