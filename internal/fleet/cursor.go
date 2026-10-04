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
	DeviceID  string     `json:"d"`
	From      *time.Time `json:"f,omitempty"`
	To        *time.Time `json:"t,omitempty"`
	HighWater int64      `json:"h"`
	StartSeq  int64      `json:"s"`           // first receive sequence the next page scans
	IID       []byte     `json:"i,omitempty"` // data-directory instance scope
	Version   int        `json:"v,omitempty"` // cursor format version; 0 = legacy index cursor
}

var cursorSecret = []byte("edge-fleet-hub-history-cursor-v1")

// encodeCursor signs the cursor with an HMAC and renders it as base64rawurl.
func encodeCursor(c pageCursor) string {
	c.Version = 2
	payload, _ := json.Marshal(c)
	mac := hmac.New(sha256.New, cursorSecret)
	mac.Write(payload)
	token := append(mac.Sum(nil), payload...)
	return base64.RawURLEncoding.EncodeToString(token)
}

// decodeCursor verifies the signature and parses the cursor. Legacy cursors
// written with index coordinates are translated to sequence coordinates.
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
	switch cursor.Version {
	case 0:
		// Cursors issued before retention used a 0-based array index ("p").
		// Those predate trimming, so event base was 0 and index p maps to
		// sequence p+1.
		var legacy struct {
			DeviceID  string     `json:"d"`
			From      *time.Time `json:"f,omitempty"`
			To        *time.Time `json:"t,omitempty"`
			HighWater int64      `json:"h"`
			ScanPos   int64      `json:"p"`
			IID       []byte     `json:"i,omitempty"`
		}
		if err := json.Unmarshal(payload, &legacy); err != nil {
			return pageCursor{}, errors.New("invalid cursor")
		}
		if legacy.ScanPos < 0 {
			return pageCursor{}, errors.New("invalid cursor")
		}
		cursor.DeviceID = legacy.DeviceID
		cursor.From = legacy.From
		cursor.To = legacy.To
		cursor.HighWater = legacy.HighWater
		cursor.IID = legacy.IID
		cursor.StartSeq = legacy.ScanPos + 1
	case 2:
		if cursor.StartSeq < 1 {
			return pageCursor{}, errors.New("invalid cursor")
		}
	default:
		return pageCursor{}, errors.New("invalid cursor")
	}
	return cursor, nil
}
