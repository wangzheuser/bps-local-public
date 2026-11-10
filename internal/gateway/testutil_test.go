package gateway

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeAuthFile writes a minimal valid OAuth file and returns its path. The
// access token does not need to be a real JWT: expiry parsing yields zero and
// the store treats the token as non-expiring.
func fakeAuthFile(t *testing.T, token, accountID string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth-"+accountID+".json")
	doc := `{"auth_mode":"chatgpt","tokens":{"access_token":` + quote(token) + `,"account_id":` + quote(accountID) + `}}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	return path
}

func quote(s string) string {
	b := make([]byte, 0, len(s)+2)
	b = append(b, '"')
	for _, c := range []byte(s) {
		switch c {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		default:
			b = append(b, c)
		}
	}
	return string(append(b, '"'))
}
