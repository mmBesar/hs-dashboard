package main

// services.go — loads config.jsonc, finds logos, and checks which services are up.

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Title    string    `json:"title"`    // big heading; empty = real hostname
	Logo     string    `json:"logo"`     // server logo next to the heading (file in logos/); empty = logo.* or built-in
	Favicon  string    `json:"favicon"`  // browser tab icon (file in logos/); empty = favicon.*, else the logo, else built-in
	Accent   string    `json:"accent"`   // Catppuccin colour name, e.g. "mauve"
	Accent2  string    `json:"accent2"`  // second colour of the heading gradient
	Locale   string    `json:"locale"`   // e.g. "en-GB", "ar-EG"; empty = browser default
	Timezone string    `json:"timezone"` // e.g. "Africa/Cairo"; empty = browser timezone
	Hour12   *bool     `json:"hour12"`   // true = 12-hour clock, false = 24-hour; omit = locale default
	NewTab   bool      `json:"newTab"`   // open service links in a new tab
	Services []Service `json:"services"`
}

type Service struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Group string `json:"group"`
	Desc  string `json:"desc"`
	Icon  string `json:"icon"`  // logo file name, or an emoji / short text
	Check string `json:"check"` // optional address to test instead of url
}

type Status struct {
	State string // "up", "down" or "unknown"
	MS    int    // how long the check took
}

// What the browser receives for each service.
type ServiceView struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Group string `json:"group"`
	Desc  string `json:"desc"`
	Icon  string `json:"icon"` // text/emoji icon (empty when a logo file is used)
	Logo  string `json:"logo"` // URL of the logo image (empty when there is none)
	State string `json:"state"`
	MS    int    `json:"ms"`
}

var trailingComma = regexp.MustCompile(`,(\s*[}\]])`)

// parseConfig is forgiving on purpose: full-line // comments and trailing commas
// are allowed. Comment lines are blanked (not removed) so error line numbers match the file.
func parseConfig(b []byte) (Config, error) {
	lines := strings.Split(string(b), "\n")
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "//") {
			lines[i] = ""
		}
	}
	text := trailingComma.ReplaceAllString(strings.Join(lines, "\n"), "$1")
	var c Config
	if err := json.Unmarshal([]byte(text), &c); err != nil {
		return Config{}, errors.New(withLine(text, err))
	}
	return c, nil
}

func withLine(text string, err error) string {
	off := int64(-1)
	var se *json.SyntaxError
	var te *json.UnmarshalTypeError
	switch {
	case errors.As(err, &se):
		off = se.Offset
	case errors.As(err, &te):
		off = te.Offset
	}
	if off < 0 {
		return err.Error()
	}
	if off > int64(len(text)) {
		off = int64(len(text))
	}
	return fmt.Sprintf("line %d: %v", 1+strings.Count(text[:off], "\n"), err)
}

// ---- logos ----

var logoExt = map[string]int{ // allowed extensions; lower number = preferred when several exist
	".svg": 0, ".png": 1, ".webp": 2, ".avif": 3, ".jpg": 4, ".jpeg": 4, ".gif": 5, ".ico": 6,
}

type logoFile struct {
	file string
	mod  int64
}

// norm lowercases and keeps only letters and digits, so "Paperless-ngx",
// "paperless_ngx" and "paperlessngx" all match each other.
func norm(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ---- store ----

type Store struct {
	path      string
	logosDir  string
	warned    bool
	lastError string

	mu     sync.RWMutex
	cfg    Config
	status []Status
	err    string
	byNorm map[string]logoFile // normalised file name -> file
	byFile map[string]logoFile // exact lowercase file name -> file
}

func newStore(path, logosDir string) *Store {
	s := &Store{path: path, logosDir: logosDir, byNorm: map[string]logoFile{}, byFile: map[string]logoFile{}}
	if cfg, err := s.load(); err == nil {
		s.cfg = cfg
	} else {
		s.err = err.Error()
	}
	s.byNorm, s.byFile = s.scanLogos()
	return s
}

// load reads the config file, or the built-in example if there is no file.
func (s *Store) load() (Config, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if !s.warned {
			log.Printf("config: %v — using the built-in example (mount a /config folder to customise)", err)
			s.warned = true
		}
		b = defaultConfig
	}
	cfg, perr := parseConfig(b)
	if perr != nil {
		if perr.Error() != s.lastError {
			log.Printf("config: %s: %v (keeping the previous config)", s.path, perr)
			s.lastError = perr.Error()
		}
		return Config{}, perr
	}
	s.lastError = ""
	return cfg, nil
}

func (s *Store) scanLogos() (map[string]logoFile, map[string]logoFile) {
	byNorm, byFile := map[string]logoFile{}, map[string]logoFile{}
	entries, err := os.ReadDir(s.logosDir)
	if err != nil {
		return byNorm, byFile
	}
	best := map[string]int{}
	for _, e := range entries {
		ext := strings.ToLower(filepath.Ext(e.Name()))
		rank, ok := logoExt[ext]
		info, ierr := e.Info()
		if !ok || ierr != nil || strings.HasPrefix(e.Name(), ".") || info.IsDir() {
			continue
		}
		lf := logoFile{file: e.Name(), mod: info.ModTime().Unix()}
		byFile[strings.ToLower(e.Name())] = lf
		n := norm(strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())))
		if n == "" {
			continue
		}
		if r, seen := best[n]; !seen || rank < r {
			best[n], byNorm[n] = rank, lf
		}
	}
	return byNorm, byFile
}

// run re-reads the config and logos and re-checks every service, forever.
func (s *Store) run() {
	for {
		cfg, err := s.load()
		s.mu.RLock()
		if err != nil {
			cfg = s.cfg // keep the last working config
		}
		s.mu.RUnlock()

		res := make([]Status, len(cfg.Services))
		var wg sync.WaitGroup
		slots := make(chan struct{}, 16) // at most 16 checks at once, so 60+ services do not hit the reverse proxy in one burst
		for i, svc := range cfg.Services {
			wg.Add(1)
			go func(i int, svc Service) {
				defer wg.Done()
				slots <- struct{}{}
				defer func() { <-slots }()
				res[i] = probe(svc)
			}(i, svc)
		}
		wg.Wait()
		byNorm, byFile := s.scanLogos()

		s.mu.Lock()
		s.cfg, s.status, s.byNorm, s.byFile = cfg, res, byNorm, byFile
		s.err = ""
		if err != nil {
			s.err = err.Error()
		}
		s.mu.Unlock()
		time.Sleep(20 * time.Second)
	}
}

func logoURL(l logoFile) string {
	return fmt.Sprintf("logos/%s?v=%d", url.PathEscape(l.file), l.mod)
}

// pickIcon decides between a logo image and a text/emoji icon.
//
//	icon: "jellyfin.svg"  -> that file
//	icon: "jellyfin"      -> any file called jellyfin.*
//	icon: "🎬"            -> shown as text
//	no icon               -> a file matching the service name, if one exists
//
// Callers must hold s.mu.
func (s *Store) pickIcon(svc Service) (logo, text string) {
	icon := strings.TrimSpace(svc.Icon)
	if isImageURL(icon) {
		return icon, ""
	}
	if icon == "" {
		if n := norm(svc.Name); n != "" {
			if l, ok := s.byNorm[n]; ok {
				return logoURL(l), ""
			}
		}
		return "", ""
	}
	if l, ok := s.byFile[strings.ToLower(icon)]; ok {
		return logoURL(l), ""
	}
	if n := norm(strings.TrimSuffix(icon, filepath.Ext(icon))); n != "" {
		if l, ok := s.byNorm[n]; ok {
			return logoURL(l), ""
		}
	}
	return "", icon
}

func isImageURL(v string) bool {
	return strings.HasPrefix(v, "https://") || strings.HasPrefix(v, "http://")
}

// resolveImage turns a config value into an image URL.
//
//	value "mylogo.png" -> that file in logos/
//	value "mylogo"     -> any file called mylogo.* in logos/
//	value "https://..." -> used as is
//	empty value        -> the first file in logos/ named like one of autoNames (e.g. "logo")
//
// Callers must hold s.mu.
func (s *Store) resolveImage(value string, autoNames ...string) string {
	value = strings.TrimSpace(value)
	if isImageURL(value) {
		return value
	}
	if value != "" {
		if l, ok := s.byFile[strings.ToLower(value)]; ok {
			return logoURL(l)
		}
		if n := norm(strings.TrimSuffix(value, filepath.Ext(value))); n != "" {
			if l, ok := s.byNorm[n]; ok {
				return logoURL(l)
			}
		}
		return ""
	}
	for _, name := range autoNames {
		if l, ok := s.byNorm[norm(name)]; ok {
			return logoURL(l)
		}
	}
	return ""
}

func (s *Store) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	brand := s.resolveImage(s.cfg.Logo, "logo")
	favicon := s.resolveImage(s.cfg.Favicon, "favicon")
	if favicon == "" {
		favicon = brand // no favicon of its own: reuse the server logo
	}
	out := struct {
		Title    string        `json:"title"`
		Logo     string        `json:"logo"`
		Favicon  string        `json:"favicon"`
		Accent   string        `json:"accent"`
		Accent2  string        `json:"accent2"`
		Locale   string        `json:"locale"`
		Timezone string        `json:"timezone"`
		Hour12   *bool         `json:"hour12"`
		NewTab   bool          `json:"newTab"`
		Error    string        `json:"error"`
		Services []ServiceView `json:"services"`
	}{s.cfg.Title, brand, favicon, s.cfg.Accent, s.cfg.Accent2, s.cfg.Locale, s.cfg.Timezone, s.cfg.Hour12, s.cfg.NewTab, s.err, []ServiceView{}}
	for i, sv := range s.cfg.Services {
		st := Status{State: "unknown"}
		if i < len(s.status) {
			st = s.status[i]
		}
		logo, text := s.pickIcon(sv)
		out.Services = append(out.Services, ServiceView{sv.Name, sv.URL, sv.Group, sv.Desc, text, logo, st.State, st.MS})
	}
	writeJSON(w, out)
}

// serveLogo serves one image from the logos folder. Only plain file names with an
// image extension are allowed — no sub-folders, no listing.
func (s *Store) serveLogo(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := logoExt[strings.ToLower(filepath.Ext(name))]; !ok ||
		name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(s.logosDir, name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Cache-Control", "public, max-age=86400") // the ?v= in the URL changes when a file changes
	h.Set("X-Content-Type-Options", "nosniff")
	// An SVG opened directly must not be able to run scripts.
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	http.ServeContent(w, r, name, st.ModTime(), f)
}

// Internal services often use self-signed certs (e.g. Caddy internal TLS),
// so certificate errors are ignored — this only checks "is it answering?".
var httpClient = &http.Client{
	Timeout:       3 * time.Second,
	Transport:     &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, DisableKeepAlives: true},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func probe(svc Service) Status {
	target := svc.Check
	if target == "" {
		target = svc.URL
	}
	if target == "" || target == "none" {
		return Status{State: "unknown"}
	}
	start := time.Now()
	ok := false
	if addr, isTCP := strings.CutPrefix(target, "tcp://"); isTCP {
		if conn, err := net.DialTimeout("tcp", addr, 3*time.Second); err == nil {
			conn.Close()
			ok = true
		}
	} else if resp, err := httpClient.Get(target); err == nil {
		resp.Body.Close()
		ok = resp.StatusCode < 500
	}
	ms := int(time.Since(start).Milliseconds())
	if ok {
		return Status{State: "up", MS: ms}
	}
	return Status{State: "down", MS: ms}
}
