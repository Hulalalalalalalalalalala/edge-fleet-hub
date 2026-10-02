package fleet

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"
)

// encodeLegacyCursorForTest mints a pre-retention index-based cursor ("p").
func encodeLegacyCursorForTest(t *testing.T, device string, highWater, scanPos int64) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"d": device,
		"h": highWater,
		"p": scanPos,
	})
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, cursorSecret)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(append(mac.Sum(nil), payload...))
}
