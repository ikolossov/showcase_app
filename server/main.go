// Демо-сервер для showcase-клиента: регистрация устройств по QR, выдача
// пары access/refresh токенов, информация о товаре и раздача обновлений.
// Всё состояние хранится в памяти — для стенда этого достаточно.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // scratch-образ без системной tzdata
)

//go:embed web/*.html
var webFS embed.FS

var tmpl = template.Must(template.ParseFS(webFS, "web/*.html"))

const (
	statusPending  = "pending"
	statusApproved = "approved"
)

type device struct {
	AppID      string    `json:"app_id"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	Version    string    `json:"version"`
	Platform   string    `json:"platform"`
	FirstSeen  time.Time `json:"first_seen"`
	LastSeen   time.Time `json:"last_seen"`
	secretHash [32]byte
}

type token struct {
	appID string
	exp   time.Time
}

type event struct {
	At    time.Time `json:"at"`
	AppID string    `json:"app_id"`
	Text  string    `json:"text"`
}

type release struct {
	Version string `json:"version"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	File    string `json:"file"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	URL     string `json:"url,omitempty"` // абсолютная ссылка (GitHub); пусто — /download/<File>
	Source  string `json:"source"`        // local | github
	modTime time.Time
}

type server struct {
	accessTTL   time.Duration
	refreshTTL  time.Duration
	releasesDir string
	github      *githubSource // nil — только локальный каталог

	mu       sync.Mutex
	devices  map[string]*device
	access   map[string]token
	refresh  map[string]token
	events   []event
	price    int64
	priceAt  time.Time
	sumCache map[string]release
}

func main() {
	addr := flag.String("addr", envOr("ADDR", ":8080"), "адрес HTTP-сервера")
	releases := flag.String("releases", envOr("RELEASES_DIR", "./releases"), "каталог с бинарниками релизов")
	accessTTL := flag.Duration("access-ttl", envDur("ACCESS_TTL", 15*time.Minute), "время жизни access-токена")
	refreshTTL := flag.Duration("refresh-ttl", envDur("REFRESH_TTL", 30*24*time.Hour), "время жизни refresh-токена")
	ghRepo := flag.String("github-repo", os.Getenv("GITHUB_REPO"), "owner/name: брать релизы ещё и из GitHub Releases")
	ghPoll := flag.Duration("github-poll", envDur("GITHUB_POLL", 5*time.Minute), "как часто перечитывать GitHub Releases")
	flag.Parse()

	s := &server{
		accessTTL:   *accessTTL,
		refreshTTL:  *refreshTTL,
		releasesDir: *releases,
		devices:     map[string]*device{},
		access:      map[string]token{},
		refresh:     map[string]token{},
		sumCache:    map[string]release{},
		price:       649_990,
	}
	if *ghRepo != "" {
		s.github = newGitHubSource(*ghRepo, os.Getenv("GITHUB_TOKEN"))
		go s.github.run(context.Background(), *ghPoll)
	}

	mux := http.NewServeMux()
	// API клиента
	mux.HandleFunc("POST /api/v1/login", s.handleLogin)
	mux.HandleFunc("POST /api/v1/token/refresh", s.handleRefresh)
	mux.HandleFunc("GET /api/v1/product", s.handleProduct)
	mux.HandleFunc("GET /api/v1/update", s.handleUpdate)
	mux.Handle("GET /download/", http.StripPrefix("/download/", http.FileServer(http.Dir(s.releasesDir))))
	// Страница регистрации (открывается по QR)
	mux.HandleFunc("GET /register", s.handleRegisterPage)
	mux.HandleFunc("POST /register", s.handleRegisterSubmit)
	// Админка стенда
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/admin", http.StatusFound) })
	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) { render(w, "admin.html", nil) })
	mux.HandleFunc("GET /api/admin/state", s.handleAdminState)
	mux.HandleFunc("POST /api/admin/devices/{id}/approve", s.handleAdminApprove)
	mux.HandleFunc("POST /api/admin/devices/{id}/revoke", s.handleAdminRevoke)

	slog.Info("server started", "addr", *addr, "releases", s.releasesDir, "access_ttl", s.accessTTL)
	if err := http.ListenAndServe(*addr, logRequests(mux)); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

// ---------- клиентский API ----------

type tokenPair struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// handleLogin поллится клиентом, пока на экране QR. Пока устройство не
// привязано — 202, после привязки — 200 с парой токенов.
// device_secret знает только сам клиент (в QR его нет), поэтому, зная лишь
// AppID из QR, получить токены чужого устройства нельзя.
func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AppID        string `json:"app_id"`
		DeviceSecret string `json:"device_secret"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil || !uuidRe.MatchString(req.AppID) || len(req.DeviceSecret) < 32 {
		writeErr(w, http.StatusBadRequest, "app_id (uuid) и device_secret обязательны")
		return
	}
	hash := sha256.Sum256([]byte(req.DeviceSecret))

	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[req.AppID]
	if !ok {
		d = &device{AppID: req.AppID, Status: statusPending, FirstSeen: time.Now(), secretHash: hash}
		s.devices[req.AppID] = d
		s.logEvent(d.AppID, "устройство ожидает регистрации (показан QR)")
	}
	if subtle.ConstantTimeCompare(d.secretHash[:], hash[:]) != 1 {
		writeErr(w, http.StatusForbidden, "device_secret не совпадает")
		return
	}
	s.touch(d, r)
	if d.Status != statusApproved {
		writeJSON(w, http.StatusAccepted, map[string]string{"status": statusPending})
		return
	}
	s.logEvent(d.AppID, "выдана пара access/refresh по /login")
	writeJSON(w, http.StatusOK, s.issue(d.AppID))
}

func (s *server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req)

	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.refresh[req.RefreshToken]
	if !ok || time.Now().After(t.exp) || s.devices[t.appID] == nil || s.devices[t.appID].Status != statusApproved {
		writeErr(w, http.StatusUnauthorized, "refresh token недействителен")
		return
	}
	s.touch(s.devices[t.appID], r)
	s.logEvent(t.appID, "токены обновлены по refresh (ротация)")
	writeJSON(w, http.StatusOK, s.issue(t.appID))
}

func (s *server) handleProduct(w http.ResponseWriter, r *http.Request) {
	tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")

	s.mu.Lock()
	t, ok := s.access[tok]
	if !ok || time.Now().After(t.exp) {
		s.logEvent(r.Header.Get("X-App-ID"), "GET /product → 401")
		s.mu.Unlock()
		writeErr(w, http.StatusUnauthorized, "access token недействителен")
		return
	}
	s.touch(s.devices[t.appID], r)
	price := s.nextPrice()
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"sku":       "MBA13-M4-16-512",
		"name":      "Ноутбук Apple MacBook Air 13 M4 16/512GB Midnight",
		"price":     price,
		"old_price": 749_990,
		"currency":  "тг",
		"in_stock":  3 + time.Now().Minute()%9,
		"specs": []map[string]string{
			{"name": "Процессор", "value": "Apple M4, 10 ядер CPU"},
			{"name": "Графика", "value": "10 ядер GPU"},
			{"name": "Оперативная память", "value": "16 ГБ"},
			{"name": "Накопитель", "value": "SSD 512 ГБ"},
			{"name": "Экран", "value": "13.6\" Liquid Retina, 2560×1664"},
			{"name": "Автономность", "value": "до 18 ч"},
			{"name": "Вес", "value": "1.24 кг"},
		},
		"updated_at": time.Now().Format("15:04:05"),
	})
}

// handleUpdate отдаёт самый свежий релиз под os/arch клиента, если он новее
// текущей версии; иначе 204.
func (s *server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cur := q.Get("version")
	rels := s.releases()
	var best *release
	for i := range rels {
		rel := &rels[i]
		if rel.OS == q.Get("os") && rel.Arch == q.Get("arch") && cmpVersion(rel.Version, cur) > 0 &&
			(best == nil || cmpVersion(rel.Version, best.Version) > 0) {
			best = rel
		}
	}
	if best == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.mu.Lock()
	s.logEvent(r.Header.Get("X-App-ID"), fmt.Sprintf("предложено обновление %s → %s", cur, best.Version))
	s.mu.Unlock()
	url := best.URL
	if url == "" {
		url = "/download/" + best.File
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version": best.Version,
		"url":     url,
		"sha256":  best.SHA256,
		"size":    best.Size,
	})
}

// ---------- регистрация по QR ----------

func (s *server) handleRegisterPage(w http.ResponseWriter, r *http.Request) {
	appID := r.URL.Query().Get("app_id")
	s.mu.Lock()
	var d device
	known := false
	if p, ok := s.devices[appID]; ok {
		d, known = *p, true
	}
	s.mu.Unlock()
	render(w, "register.html", map[string]any{"AppID": appID, "Known": known, "Device": d})
}

func (s *server) handleRegisterSubmit(w http.ResponseWriter, r *http.Request) {
	appID := r.FormValue("app_id")
	name := strings.TrimSpace(r.FormValue("name"))
	if err := s.approve(appID, name); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	http.Redirect(w, r, "/register?app_id="+appID, http.StatusSeeOther)
}

func (s *server) approve(appID, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[appID]
	if !ok {
		return fmt.Errorf("устройство %q не найдено: запустите приложение, чтобы оно показало QR", appID)
	}
	if name == "" {
		name = "Устройство " + appID[:8]
	}
	d.Name, d.Status = name, statusApproved
	s.logEvent(appID, "устройство привязано: "+name)
	return nil
}

// ---------- админка ----------

func (s *server) handleAdminState(w http.ResponseWriter, r *http.Request) {
	rels := s.releases()
	s.mu.Lock()
	devs := make([]device, 0, len(s.devices))
	for _, d := range s.devices {
		devs = append(devs, *d)
	}
	evs := slices.Clone(s.events)
	s.mu.Unlock()
	slices.SortFunc(devs, func(a, b device) int { return b.FirstSeen.Compare(a.FirstSeen) })
	slices.Reverse(evs)
	state := map[string]any{
		"devices": devs, "events": evs, "releases": rels,
		"access_ttl": s.accessTTL.String(), "now": time.Now(),
	}
	if s.github != nil {
		state["github"] = s.github.status()
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *server) handleAdminApprove(w http.ResponseWriter, r *http.Request) {
	if err := s.approve(r.PathValue("id"), ""); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAdminRevoke отвязывает устройство и отзывает все его токены —
// клиент получит 401, refresh тоже не пройдёт, и он снова покажет QR.
func (s *server) handleAdminRevoke(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[r.PathValue("id")]
	if !ok {
		writeErr(w, http.StatusNotFound, "устройство не найдено")
		return
	}
	d.Status = statusPending
	s.revokeTokens(d.AppID)
	s.logEvent(d.AppID, "доступ отозван администратором")
	w.WriteHeader(http.StatusNoContent)
}

// ---------- внутреннее ----------

// issue выпускает новую пару и отзывает предыдущие токены устройства
// (одна активная сессия на устройство, refresh одноразовый). Вызывать под s.mu.
func (s *server) issue(appID string) tokenPair {
	s.revokeTokens(appID)
	now := time.Now()
	p := tokenPair{AccessToken: randHex(32), RefreshToken: randHex(32), ExpiresIn: int(s.accessTTL.Seconds())}
	s.access[p.AccessToken] = token{appID, now.Add(s.accessTTL)}
	s.refresh[p.RefreshToken] = token{appID, now.Add(s.refreshTTL)}
	return p
}

func (s *server) revokeTokens(appID string) {
	for _, m := range []map[string]token{s.access, s.refresh} {
		for k, t := range m {
			if t.appID == appID {
				delete(m, k)
			}
		}
	}
}

func (s *server) touch(d *device, r *http.Request) {
	if d == nil {
		return
	}
	d.LastSeen = time.Now()
	if v := r.Header.Get("X-App-Version"); v != "" {
		if d.Version != "" && d.Version != v {
			s.logEvent(d.AppID, fmt.Sprintf("клиент перезапустился на новой версии %s → %s", d.Version, v))
		}
		d.Version = v
	}
	if p := r.Header.Get("X-App-Platform"); p != "" {
		d.Platform = p
	}
}

// nextPrice — «живая» цена: случайное блуждание раз в 20 секунд. Вызывать под s.mu.
func (s *server) nextPrice() int64 {
	if time.Since(s.priceAt) > 20*time.Second {
		n, _ := rand.Int(rand.Reader, big.NewInt(9))
		s.price = min(max(s.price+(n.Int64()-4)*5_000, 599_990), 749_990)
		s.priceAt = time.Now()
	}
	return s.price
}

func (s *server) logEvent(appID, text string) {
	slog.Info("event", "app_id", appID, "text", text)
	s.events = append(s.events, event{At: time.Now(), AppID: appID, Text: text})
	if len(s.events) > 100 {
		s.events = s.events[len(s.events)-100:]
	}
}

// releases — все известные релизы: локальный каталог + GitHub Releases.
// При одинаковой версии под одну платформу выигрывает локальный файл.
func (s *server) releases() []release {
	rels := s.scanReleases()
	if s.github != nil {
		rels = append(rels, s.github.list()...)
	}
	slices.SortStableFunc(rels, func(a, b release) int { return cmpVersion(b.Version, a.Version) })
	return rels
}

var releaseRe = regexp.MustCompile(`^showcase-(\d+\.\d+\.\d+)-([a-z]+)-([a-z0-9_]+)(\.exe)?$`)

// scanReleases читает каталог релизов. Имя файла: showcase-<версия>-<os>-<arch>[.exe].
// Чтобы выпустить обновление, достаточно положить файл в каталог.
func (s *server) scanReleases() []release {
	entries, _ := os.ReadDir(s.releasesDir)
	var out []release
	for _, e := range entries {
		m := releaseRe.FindStringSubmatch(e.Name())
		info, err := e.Info()
		if m == nil || err != nil || !info.Mode().IsRegular() {
			continue
		}
		s.mu.Lock()
		rel, ok := s.sumCache[e.Name()]
		s.mu.Unlock()
		if !ok || rel.Size != info.Size() || !rel.modTime.Equal(info.ModTime()) {
			sum, err := fileSHA256(filepath.Join(s.releasesDir, e.Name()))
			if err != nil {
				continue
			}
			rel = release{Version: m[1], OS: m[2], Arch: m[3], File: e.Name(), Size: info.Size(), SHA256: sum, Source: "local", modTime: info.ModTime()}
			s.mu.Lock()
			s.sumCache[e.Name()] = rel
			s.mu.Unlock()
		}
		out = append(out, rel)
	}
	slices.SortFunc(out, func(a, b release) int { return cmpVersion(b.Version, a.Version) })
	return out
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// cmpVersion сравнивает версии вида 1.2.3.
func cmpVersion(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(pa), len(pb)) {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x - y
		}
	}
	return 0
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("render", "tmpl", name, "err", err)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) { r.code = code; r.ResponseWriter.WriteHeader(code) }

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		if !strings.HasPrefix(r.URL.Path, "/api/admin/") {
			slog.Info("http", "method", r.Method, "path", r.URL.Path, "code", rec.code, "app_id", r.Header.Get("X-App-ID"))
		}
	})
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return d
	}
	return def
}
