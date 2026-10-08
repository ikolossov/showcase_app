package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// githubSource — релизы из GitHub Releases. Сервер по-прежнему решает, какую
// версию отдать клиенту, но сам файл клиент качает с GitHub (browser_download_url).
// SHA-256 берётся из поля digest, которое GitHub считает для каждого файла релиза.
type githubSource struct {
	api   string // https://api.github.com (подменяется в тестах)
	repo  string // owner/name
	token string // необязательно: лимит без токена — 60 запросов в час
	http  *http.Client

	mu   sync.RWMutex
	rels []release
	err  error
	at   time.Time
}

func newGitHubSource(repo, token string) *githubSource {
	return &githubSource{api: "https://api.github.com", repo: repo, token: token, http: &http.Client{Timeout: 15 * time.Second}}
}

// run обновляет кэш релизов раз в every, пока не отменён ctx.
func (g *githubSource) run(ctx context.Context, every time.Duration) {
	for {
		rels, err := g.fetch(ctx)
		g.mu.Lock()
		if err == nil {
			g.rels = rels
		}
		g.err, g.at = err, time.Now()
		g.mu.Unlock()
		if err != nil {
			slog.Warn("github releases", "repo", g.repo, "err", err)
		} else {
			slog.Info("github releases", "repo", g.repo, "files", len(rels))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// list возвращает последний удачный снимок; при ошибке API — предыдущий.
func (g *githubSource) list() []release {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.rels
}

func (g *githubSource) status() map[string]any {
	g.mu.RLock()
	defer g.mu.RUnlock()
	st := map[string]any{"repo": g.repo, "checked_at": g.at, "files": len(g.rels)}
	if g.err != nil {
		st["error"] = g.err.Error()
	}
	return st
}

func (g *githubSource) fetch(ctx context.Context) ([]release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/repos/%s/releases?per_page=30", g.api, g.repo), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET releases: HTTP %d", resp.StatusCode)
	}
	var payload []struct {
		Draft      bool `json:"draft"`
		Prerelease bool `json:"prerelease"`
		Assets     []struct {
			Name   string `json:"name"`
			Size   int64  `json:"size"`
			Digest string `json:"digest"`
			URL    string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	var out []release
	for _, r := range payload {
		if r.Draft || r.Prerelease {
			continue
		}
		for _, a := range r.Assets {
			m := releaseRe.FindStringSubmatch(a.Name)
			sum, ok := strings.CutPrefix(a.Digest, "sha256:")
			if m == nil || !ok || len(sum) != 64 {
				continue // без контрольной суммы файл клиенту не предлагаем
			}
			out = append(out, release{Version: m[1], OS: m[2], Arch: m[3], File: a.Name, Size: a.Size, SHA256: sum, URL: a.URL, Source: "github"})
		}
	}
	return out, nil
}
