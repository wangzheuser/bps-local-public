package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestDeviceFlow(t *testing.T) {
	for _, mode := range []string{"success", "denied", "bad_pkce", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			polls, exchanges := 0, 0
			verifier := strings.Repeat("x", 43)
			digest := sha256.Sum256([]byte(verifier))
			challenge := base64.RawURLEncoding.EncodeToString(digest[:])
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" {
					t.Error("expected POST")
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/accounts/deviceauth/usercode":
					var v map[string]string
					_ = json.NewDecoder(r.Body).Decode(&v)
					if v["client_id"] != ExcelClientID {
						t.Error("wrong client")
					}
					ioJSON(w, map[string]any{"device_auth_id": "device", "user_code": "user", "interval": "5"})
				case "/api/accounts/deviceauth/token":
					polls++
					var v map[string]string
					_ = json.NewDecoder(r.Body).Decode(&v)
					if v["device_auth_id"] != "device" || v["user_code"] != "user" || v["client_id"] != ExcelClientID {
						t.Error("wrong poll fields")
					}
					if polls == 1 || mode == "denied" {
						w.WriteHeader(403)
						code := "deviceauth_authorization_pending"
						if mode == "denied" {
							code = "access_denied"
						}
						ioJSON(w, map[string]any{"error": map[string]string{"code": code}})
						return
					}
					c := challenge
					if mode == "bad_pkce" {
						c = "wrong"
					}
					ioJSON(w, map[string]string{"authorization_code": "code", "code_verifier": verifier, "code_challenge": c})
				case "/oauth/token":
					exchanges++
					_ = r.ParseForm()
					if r.Form.Get("redirect_uri") != deviceRedirect || r.Form.Get("code_verifier") != verifier || r.Form.Get("code") != "code" || r.URL.Query().Get("unified") != "true" {
						t.Error("wrong exchange")
					}
					ioJSON(w, map[string]any{"access_token": "opaque", "refresh_token": "refresh", "token_type": "Bearer", "expires_in": 3600})
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer srv.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := &DeviceClient{HTTP: srv.Client(), base: srv.URL, wait: func(ctx context.Context, d time.Duration) error {
				if d != 5*time.Second {
					t.Error("interval not honored")
				}
				return ctx.Err()
			}}
			creds, err := c.Login(ctx, func(p DevicePrompt) error {
				if p.UserCode != "user" || !strings.Contains(p.URL, "client_id="+ExcelClientID) {
					t.Error("wrong prompt")
				}
				// The authorize page needs user_code in the query string; without
				// it the endpoint degrades into the regular OAuth flow and is
				// rejected with deviceauth_disabled.
				if !strings.Contains(p.URL, "user_code="+p.UserCode) {
					t.Errorf("authorize URL missing user_code: %s", p.URL)
				}
				if mode == "cancel" {
					cancel()
				}
				return nil
			})
			if mode != "success" {
				if err == nil || exchanges != 0 {
					t.Fatal("failure must not exchange token")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if polls != 2 || exchanges != 1 {
				t.Fatal("bad flow counts")
			}
			dest := filepath.Join(t.TempDir(), "new", "oauth.json")
			if err = SaveDeviceCredentials(dest, creds); err != nil {
				t.Fatal(err)
			}
			st, _ := os.Stat(dest)
			// Windows does not expose POSIX permission bits; os.Stat reports
			// 0666 for regular files there, so the strict check runs only on
			// Unix where the 0600 mode is meaningful.
			if runtime.GOOS != "windows" && st.Mode().Perm() != 0600 {
				t.Fatal("unsafe mode")
			}
			store, err := Load(dest)
			if err != nil {
				t.Fatal(err)
			}
			token, _, err := store.Credentials()
			if err != nil || token != "opaque" {
				t.Fatal("saved credentials incompatible")
			}
			if err = SaveDeviceCredentials(dest, creds); !errors.Is(err, os.ErrExist) {
				t.Fatal("must not overwrite")
			}
		})
	}
}
func ioJSON(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }

func TestDeviceSaveRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "existing")
	_ = os.WriteFile(target, []byte("unchanged"), 0600)
	link := filepath.Join(dir, "oauth.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skip(err)
	}
	if SaveDeviceCredentials(link, &DeviceCredentials{AccessToken: "new"}) == nil {
		t.Fatal("replaced symlink")
	}
	b, _ := os.ReadFile(target)
	if string(b) != "unchanged" {
		t.Fatal("modified target")
	}
}
