// hs-dashboard — a tiny self-hosted home server dashboard.
//
// One static Go binary, standard library only (no dependencies), so it
// cross-compiles to amd64, arm64 and riscv64 without any extra work.
//
// Files in this folder:
//
//	main.go      – web server, routes, privilege drop (PUID/PGID)
//	stats.go     – reads CPU/RAM/disk/network/temperature from /host/proc and /host/sys
//	services.go  – loads config.jsonc and checks which services are up
//	web/         – the page itself (index.html, style.css, app.js) – no build step
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	_ "time/tzdata" // bundles the timezone database so TZ=Africa/Cairo works in a scratch image
)

// The web page is baked into the binary.
//
//go:embed web
var webFS embed.FS

// The example config is baked in too, used when no config file is mounted.
//
//go:embed config.jsonc
var defaultConfig []byte

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// healthcheck is used by the Docker HEALTHCHECK (the image has no shell or curl).
func healthcheck(port string) {
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/api/health")
	if err != nil || resp.StatusCode != 200 {
		os.Exit(1)
	}
	os.Exit(0)
}

// dropPrivileges implements PUID/PGID: start as root, then become that user.
// Needs the SETUID and SETGID capabilities (see compose.yaml).
func dropPrivileges() {
	uid, errU := strconv.Atoi(os.Getenv("PUID"))
	gid, errG := strconv.Atoi(os.Getenv("PGID"))
	if errU != nil || errG != nil || uid == 0 || os.Getuid() != 0 {
		return // not requested, or already non-root
	}
	_ = syscall.Setgroups([]int{})
	if err := syscall.Setgid(gid); err != nil {
		log.Fatalf("setgid(%d): %v", gid, err)
	}
	if err := syscall.Setuid(uid); err != nil {
		log.Fatalf("setuid(%d): %v", uid, err)
	}
	log.Printf("running as uid=%d gid=%d", uid, gid)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

// noCache makes edits to web/ show up on a normal refresh.
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

func main() {
	hc := flag.Bool("healthcheck", false, "probe /api/health and exit (used by Docker HEALTHCHECK)")
	flag.Parse()

	port := env("PORT", "8080")
	if *hc {
		healthcheck(port)
	}

	dropPrivileges()

	sampler := newSampler()
	go sampler.run()

	cfgPath := env("CONFIG", "/config/config.jsonc")
	// Optional logos live next to the config: /config/logos/jellyfin.svg, immich.png ...
	store := newStore(cfgPath, env("LOGOS", filepath.Join(filepath.Dir(cfgPath), "logos")))
	go store.run()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
	mux.HandleFunc("GET /api/info", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, sampler.info) })
	mux.HandleFunc("GET /api/history", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, sampler.History()) })
	mux.HandleFunc("GET /api/stream", sampler.stream)
	mux.HandleFunc("GET /api/services", store.handle)
	mux.HandleFunc("GET /logos/{name}", store.serveLogo)

	site, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}
	mux.Handle("GET /", noCache(http.FileServerFS(site)))

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second, // no WriteTimeout on purpose: /api/stream stays open
	}
	log.Printf("hs-dashboard listening on :%s", port)
	log.Fatal(srv.ListenAndServe())
}
