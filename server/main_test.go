package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testAppID  = "11111111-2222-4333-8444-555555555555"
	testSecret = "0123456789abcdef0123456789abcdef"
)

func newTestServer(t *testing.T) (*server, *httptest.Server) {
	t.Helper()
	s := &server{
		accessTTL: time.Minute, refreshTTL: time.Hour, releasesDir: t.TempDir(),
		devices: map[string]*device{}, access: map[string]token{}, refresh: map[string]token{},
		sumCache: map[string]release{}, price: 649_990,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/login", s.handleLogin)
	mux.HandleFunc("POST /api/v1/token/refresh", s.handleRefresh)
	mux.HandleFunc("GET /api/v1/product", s.handleProduct)
	mux.HandleFunc("GET /api/v1/update", s.handleUpdate)
	mux.HandleFunc("POST /register", s.handleRegisterSubmit)
	mux.HandleFunc("POST /api/admin/devices/{id}/revoke", s.handleAdminRevoke)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return s, ts
}

func do(t *testing.T, method, url, body, bearer string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if strings.HasPrefix(body, "{") {
		req.Header.Set("Content-Type", "application/json")
	} else if body != "" {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// Полный сценарий: 401 → login pending → привязка → токены → товар → refresh → отзыв.
func TestDeviceFlow(t *testing.T) {
	_, ts := newTestServer(t)
	login := `{"app_id":"` + testAppID + `","device_secret":"` + testSecret + `"}`

	if code, _ := do(t, "GET", ts.URL+"/api/v1/product", "", ""); code != 401 {
		t.Fatalf("product без токена: %d, ждали 401", code)
	}
	if code, _ := do(t, "POST", ts.URL+"/api/v1/login", login, ""); code != 202 {
		t.Fatalf("login до привязки: %d, ждали 202", code)
	}
	wrong := `{"app_id":"` + testAppID + `","device_secret":"ffffffffffffffffffffffffffffffff"}`
	if code, _ := do(t, "POST", ts.URL+"/api/v1/login", wrong, ""); code != 403 {
		t.Fatalf("login с чужим секретом: %d, ждали 403", code)
	}
	if code, _ := do(t, "POST", ts.URL+"/register", "app_id="+testAppID+"&name=test", ""); code != 303 {
		t.Fatalf("привязка: %d, ждали 303", code)
	}
	code, pair := do(t, "POST", ts.URL+"/api/v1/login", login, "")
	if code != 200 || pair["access_token"] == nil || pair["refresh_token"] == nil {
		t.Fatalf("login после привязки: %d %v", code, pair)
	}
	access, refresh := pair["access_token"].(string), pair["refresh_token"].(string)

	code, product := do(t, "GET", ts.URL+"/api/v1/product", "", access)
	if code != 200 || product["price"] == nil || product["specs"] == nil {
		t.Fatalf("product: %d %v", code, product)
	}

	code, pair2 := do(t, "POST", ts.URL+"/api/v1/token/refresh", `{"refresh_token":"`+refresh+`"}`, "")
	if code != 200 {
		t.Fatalf("refresh: %d", code)
	}
	if code, _ := do(t, "POST", ts.URL+"/api/v1/token/refresh", `{"refresh_token":"`+refresh+`"}`, ""); code != 401 {
		t.Fatalf("повторный refresh: %d, ждали 401 (refresh одноразовый)", code)
	}
	if code, _ := do(t, "GET", ts.URL+"/api/v1/product", "", access); code != 401 {
		t.Fatalf("старый access после ротации: %d, ждали 401", code)
	}

	if code, _ := do(t, "POST", ts.URL+"/api/admin/devices/"+testAppID+"/revoke", "", ""); code != 204 {
		t.Fatalf("revoke: %d", code)
	}
	if code, _ := do(t, "GET", ts.URL+"/api/v1/product", "", pair2["access_token"].(string)); code != 401 {
		t.Fatalf("product после отзыва: %d, ждали 401", code)
	}
	if code, _ := do(t, "POST", ts.URL+"/api/v1/token/refresh", `{"refresh_token":"`+pair2["refresh_token"].(string)+`"}`, ""); code != 401 {
		t.Fatalf("refresh после отзыва: %d, ждали 401", code)
	}
}

func TestAccessTokenExpires(t *testing.T) {
	s, ts := newTestServer(t)
	s.accessTTL = time.Millisecond
	s.devices[testAppID] = &device{AppID: testAppID, Status: statusApproved}
	s.mu.Lock()
	pair := s.issue(testAppID)
	s.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	if code, _ := do(t, "GET", ts.URL+"/api/v1/product", "", pair.AccessToken); code != 401 {
		t.Fatalf("просроченный access: %d, ждали 401", code)
	}
	if code, _ := do(t, "POST", ts.URL+"/api/v1/token/refresh", `{"refresh_token":"`+pair.RefreshToken+`"}`, ""); code != 200 {
		t.Fatalf("refresh просроченного access: %d, ждали 200", code)
	}
}

func TestUpdatePicksNewestForPlatform(t *testing.T) {
	s, ts := newTestServer(t)
	for _, name := range []string{
		"showcase-1.1.0-linux-x86_64",
		"showcase-1.10.0-linux-x86_64", // 1.10 > 1.9 — сравнение числовое, не строковое
		"showcase-1.9.0-linux-x86_64",
		"showcase-2.0.0-windows-x86_64.exe",
		"showcase-3.0.0-macos-aarch64",
		"README.txt",
	} {
		if err := os.WriteFile(filepath.Join(s.releasesDir, name), []byte(name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	code, upd := do(t, "GET", ts.URL+"/api/v1/update?os=linux&arch=x86_64&version=1.0.0", "", "")
	if code != 200 || upd["version"] != "1.10.0" || upd["url"] != "/download/showcase-1.10.0-linux-x86_64" {
		t.Fatalf("update linux: %d %v", code, upd)
	}
	if sum, _ := upd["sha256"].(string); len(sum) != 64 {
		t.Fatalf("sha256: %q", sum)
	}
	if code, upd := do(t, "GET", ts.URL+"/api/v1/update?os=macos&arch=aarch64&version=1.0.0", "", ""); code != 200 || upd["version"] != "3.0.0" {
		t.Fatalf("update macos: %d %v", code, upd)
	}
	if code, _ := do(t, "GET", ts.URL+"/api/v1/update?os=linux&arch=x86_64&version=1.10.0", "", ""); code != 204 {
		t.Fatalf("актуальная версия: %d, ждали 204", code)
	}
}

func TestCmpVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0}, {"1.0.1", "1.0.0", 1}, {"1.9.0", "1.10.0", -1}, {"2.0.0", "1.99.99", 1}, {"1.0", "1.0.0", 0},
	}
	for _, c := range cases {
		got := cmpVersion(c.a, c.b)
		if (got > 0) != (c.want > 0) || (got < 0) != (c.want < 0) {
			t.Errorf("cmpVersion(%s, %s) = %d, want sign %d", c.a, c.b, got, c.want)
		}
	}
}

func TestUpdateFromGitHubReleases(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/showcase/releases" {
			http.NotFound(w, r)
			return
		}
		asset := func(name, digest string) map[string]any {
			return map[string]any{"name": name, "size": 100, "digest": digest, "browser_download_url": "https://github.example/dl/" + name}
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"draft": true, "assets": []any{asset("showcase-9.0.0-linux-x86_64", "sha256:"+sum)}},
			{"prerelease": true, "assets": []any{asset("showcase-8.0.0-linux-x86_64", "sha256:"+sum)}},
			{"assets": []any{
				asset("showcase-2.0.0-linux-x86_64", "sha256:"+sum),
				asset("showcase-2.0.0-macos-aarch64", ""), // без digest — пропускаем
				asset("SHA256SUMS", "sha256:"+sum),
			}},
			{"assets": []any{asset("showcase-1.5.0-macos-aarch64", "sha256:"+sum)}},
		})
	}))
	defer gh.Close()

	s, ts := newTestServer(t)
	s.github = newGitHubSource("acme/showcase", "")
	s.github.api = gh.URL
	rels, err := s.github.fetch(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	s.github.rels = rels
	if len(rels) != 2 {
		t.Fatalf("ждали 2 файла (без draft, prerelease, без digest и SHA256SUMS), получили %d: %+v", len(rels), rels)
	}

	code, upd := do(t, "GET", ts.URL+"/api/v1/update?os=linux&arch=x86_64&version=1.0.0", "", "")
	if code != 200 || upd["version"] != "2.0.0" || upd["url"] != "https://github.example/dl/showcase-2.0.0-linux-x86_64" || upd["sha256"] != sum {
		t.Fatalf("update linux из GitHub: %d %v", code, upd)
	}
	if code, upd := do(t, "GET", ts.URL+"/api/v1/update?os=macos&arch=aarch64&version=1.0.0", "", ""); code != 200 || upd["version"] != "1.5.0" {
		t.Fatalf("macos: файл 2.0.0 без digest должен быть пропущен: %d %v", code, upd)
	}

	// Локальный файл той же версии важнее GitHub — отдаётся с нашего сервера.
	if err := os.WriteFile(filepath.Join(s.releasesDir, "showcase-2.0.0-linux-x86_64"), []byte("local"), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, upd := do(t, "GET", ts.URL+"/api/v1/update?os=linux&arch=x86_64&version=1.0.0", "", ""); code != 200 || upd["url"] != "/download/showcase-2.0.0-linux-x86_64" {
		t.Fatalf("локальный приоритет: %d %v", code, upd)
	}
}
