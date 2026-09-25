// px-server is the px control plane: REST API + task controller in one binary.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kawai/px/internal/controller"
	"github.com/kawai/px/internal/proxmox"
	"github.com/kawai/px/internal/server"
	"github.com/kawai/px/internal/sshexec"
	"github.com/kawai/px/internal/store"
)

func main() {
	var (
		listen      = flag.String("listen", "127.0.0.1:7420", "HTTP listen address (loopback by default; without -token-file the API is unauthenticated)")
		dbPath      = flag.String("db", "px.db", "SQLite database path")
		tokenFile   = flag.String("token-file", os.Getenv("PX_TOKEN_FILE"), "require API bearer token read from this file (clients pass it via -token/env PX_TOKEN; /healthz stays open)")
		pveEndpoint = flag.String("pve-endpoint", os.Getenv("PX_PVE_ENDPOINT"), "Proxmox VE API endpoint (https://host:8006)")
		pveNode     = flag.String("pve-node", os.Getenv("PX_PVE_NODE"), "Proxmox VE node name")
		pveToken    = flag.String("pve-token", os.Getenv("PX_PVE_TOKEN"), "PVE API token: user@realm!tokenid=secret")
		tlsInsecure = flag.Bool("tls-insecure", os.Getenv("PX_PVE_TLS_INSECURE") == "1", "skip TLS verification of the PVE endpoint (for PVE's default self-signed node cert)")
		sshUser     = flag.String("ssh-user", "root", "SSH user on the PVE node")
		sshKey      = flag.String("ssh-key", "", "SSH private key path (defaults to ssh-agent)")
		interval    = flag.Duration("reconcile-interval", 2*time.Second, "controller reconcile interval")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if *pveEndpoint == "" || *pveNode == "" || *pveToken == "" {
		fmt.Fprintln(os.Stderr, "error: --pve-endpoint, --pve-node and --pve-token are required")
		os.Exit(2)
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Error("open store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	pve := proxmox.New(*pveEndpoint, *pveNode, *pveToken, *tlsInsecure)
	ssh, err := sshexec.Dial(10*time.Second, sshexec.Config{Host: hostOf(*pveEndpoint), User: *sshUser, KeyPath: *sshKey})
	if err != nil {
		// Fail fast: without node SSH access the provisioner cannot boot
		// runners and would panic on the first Create.
		log.Error("ssh dial failed; px-server needs SSH access to the PVE node", "host", hostOf(*pveEndpoint), "err", err)
		os.Exit(1)
	}
	defer ssh.Close()

	prov := controller.NewProvisioner(pve, ssh)
	ctl := controller.New(st, prov, log)
	ctl.Tick = *interval

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go ctl.Run(ctx)

	handler := server.New(st, ctl, prov, log).Handler()
	if *tokenFile != "" {
		token, err := loadToken(*tokenFile)
		if err != nil {
			log.Error("load api token", "err", err)
			os.Exit(1)
		}
		handler = server.RequireBearer(token, handler)
		log.Info("api token auth enabled", "token-file", *tokenFile)
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Info("px-server listening", "addr", *listen, "node", *pveNode)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("server", "err", err)
		os.Exit(1)
	}
	log.Info("px-server stopped")
}

func hostOf(endpoint string) string {
	s := endpoint
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		return host
	}
	return s
}

// loadToken reads a single-line token, trimming surrounding whitespace so
// editors that add a trailing newline don't corrupt it.
func loadToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", fmt.Errorf("%s: empty token", path)
	}
	return token, nil
}
