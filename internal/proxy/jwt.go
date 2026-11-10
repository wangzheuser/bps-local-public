package proxy

import (
	"encoding/base64"
	"strings"
)

// decodeRawURL decodes base64url with or without padding.
func decodeRawURL(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	pad := strings.Repeat("=", (4-len(s)%4)%4)
	return base64.URLEncoding.DecodeString(s + pad)
}
