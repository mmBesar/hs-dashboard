package main

// services.go — loads config.jsonc and checks which services are up.

import (
	"crypto/tls"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Title    string    `json:"title"`
	Services []Service `json:"services"`
}

type Service struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Group string `json:"group"`
	Desc  string `json:"desc"`
	Icon  string `json:"icon"`
	Check string `json:"check"`
}

type Status struct {
	State string // "up", "down" or "unknown"
	MS    int    // how long the check took
}

// What the browser receives for each tile.
type ServiceView struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Group string `json:"group"`
	Desc  string `json:"desc"`
	Icon  string `json:"icon"`
	State string `json:"state"`
	MS    int    `json:"ms"`
}

// parseConfig drops full-line // comments, then reads normal JSON.
func parseConfig(b []byte) (Config, error) {
	var kept []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		kept = append(kept, line)
	}
	var c Config
	err := json.Unmarshal([]byte(strings.Join(kept, "\n")), &c)
	return c, err
}

type Store struct {
	path   string
	warned bool

	mu     sync.RWMutex
	cfg    Config
	status []Status
}

func newStore(path string) *Store {
	s := &Store{path: path}
	if cfg, ok := s.load(); ok {
		s.cfg = cfg
	}
	return s
}

// load reads the config file, or the built-in example if there is no file.
func (s *Store) load() (Config, bool) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if !s.warned {
			log.Printf("config: %v — using the built-in example (mount a /config folder to customise)", err)
			s.warned = true
		}
		b = defaultConfig
	}
	cfg, err := parseConfig(b)
	if err != nil {
		log.Printf("config: %s: %v (keeping the previous config)", s.path, err)
		return Config{}, false
	}
	return cfg, true
}

// run re-reads the config and re-checks every service, forever.
func (s *Store) run() {
	for {
		cfg, ok := s.load()
		if !ok {
			s.mu.RLock()
			cfg = s.cfg
			s.mu.RUnlock()
		}
		res := make([]Status, len(cfg.Services))
		var wg sync.WaitGroup
		for i, svc := range cfg.Services {
			wg.Add(1)
			go func(i int, svc Service) {
				defer wg.Done()
				res[i] = probe(svc)
			}(i, svc)
		}
		wg.Wait()

		s.mu.Lock()
		s.cfg, s.status = cfg, res
		s.mu.Unlock()
		time.Sleep(20 * time.Second)
	}
}

func (s *Store) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := struct {
		Title    string        `json:"title"`
		Services []ServiceView `json:"services"`
	}{Title: s.cfg.Title, Services: []ServiceView{}}
	for i, sv := range s.cfg.Services {
		st := Status{State: "unknown"}
		if i < len(s.status) {
			st = s.status[i]
		}
		out.Services = append(out.Services, ServiceView{sv.Name, sv.URL, sv.Group, sv.Desc, sv.Icon, st.State, st.MS})
	}
	writeJSON(w, out)
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
