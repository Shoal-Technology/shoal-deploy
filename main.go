package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

var version = "0.1.12"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("shoal-deploy " + version)
		return
	}
	listen := flag.String("listen", envOr("SHOALDEPLOY_LISTEN", "127.0.0.1:7700"), "address for the portal")
	dataDir := flag.String("data-dir", envOr("SHOALDEPLOY_DATA", defaultDataDir()), "state, builds and Caddyfile")
	caddyBin := flag.String("caddy", envOr("SHOALDEPLOY_CADDY", ""), "caddy binary (auto-detected when empty)")
	tlsInternal := flag.Bool("tls-internal", os.Getenv("SHOALDEPLOY_TLS_INTERNAL") == "1", "issue certificates from caddy's own CA (no public dns)")
	flag.Parse()

	log.SetFlags(log.Ltime)
	store, err := openStore(*dataDir)
	if err != nil {
		log.Fatalf("state: %v", err)
	}
	if err := bootstrapAdmin(store, *dataDir); err != nil {
		log.Fatalf("admin: %v", err)
	}

	dep := &Deployer{store: store, dataDir: *dataDir, caddy: findCaddy(*caddyBin), tlsInternal: *tlsInternal}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	dockerOK := dockerAvailable(ctx)
	dockerVer := dockerVersion(ctx)
	cancel()
	if !dockerOK {
		log.Printf("docker is not reachable: builds and deploys will fail until it is")
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           (&Server{store: store, dep: dep, host: hostname()}).routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("shoal-deploy %s on http://%s (data %s, docker %s, caddy %v)", version, *listen, *dataDir, dockerVer, dep.caddy != "")

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}

// bootstrapAdmin creates the first account once and leaves the generated
// passphrase in the data dir, so no credential ever lives in the repo or flags.
func bootstrapAdmin(store *Store, dir string) error {
	var empty bool
	store.View(func(st *State) { empty = len(st.Users) == 0 })
	if !empty {
		return nil
	}
	pw := randHex(8)
	hash, err := hashPassword(pw)
	if err != nil {
		return err
	}
	if err := store.Update(func(st *State) { st.Users["admin"] = &User{Name: "admin", Hash: hash} }); err != nil {
		return err
	}
	path := filepath.Join(dir, "initial-passphrase")
	if err := os.WriteFile(path, []byte(pw+"\n"), 0o600); err != nil {
		return err
	}
	log.Printf("first run: user admin, passphrase written to %s (change it after login)", path)
	return nil
}

func findCaddy(explicit string) string {
	if explicit != "" {
		return explicit
	}
	for _, p := range []string{"/usr/bin/caddy", "/usr/local/bin/caddy", "/usr/sbin/caddy", "/sbin/caddy"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("caddy"); err == nil {
		return p
	}
	return ""
}

func defaultDataDir() string {
	if os.Geteuid() == 0 {
		return "/var/lib/shoal-deploy"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "./shoal-deploy-data"
	}
	return filepath.Join(home, ".shoal-deploy")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "this host"
	}
	return h
}
