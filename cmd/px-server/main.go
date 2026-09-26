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
	"sync"
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
		listen          = flag.String("listen", "127.0.0.1:7420", "HTTP listen address (loopback by default; without -token-file the API is unauthenticated)")
		dbPath          = flag.String("db", "px.db", "SQLite database path")
		tokenFile       = flag.String("token-file", os.Getenv("PX_TOKEN_FILE"), "require API bearer token read from this file (clients pass it via -token/env PX_TOKEN; /healthz stays open)")
		pveEndpoint     = flag.String("pve-endpoint", os.Getenv("PX_PVE_ENDPOINT"), "Proxmox VE API endpoint (https://host:8006)")
		pveNode         = flag.String("pve-node", os.Getenv("PX_PVE_NODE"), "Proxmox VE node name (omit to schedule across all online nodes holding the template)")
		pveToken        = flag.String("pve-token", os.Getenv("PX_PVE_TOKEN"), "PVE API token: user@realm!tokenid=secret")
		tlsInsecure     = flag.Bool("tls-insecure", os.Getenv("PX_PVE_TLS_INSECURE") == "1", "skip TLS verification of the PVE endpoint (for PVE's default self-signed node cert)")
		sshUser         = flag.String("ssh-user", "root", "SSH user on the PVE nodes")
		sshKey          = flag.String("ssh-key", "", "SSH private key path (defaults to ssh-agent)")
		sshHostKey      = flag.String("ssh-host-key", os.Getenv("PX_SSH_HOST_KEY"), "file of pinned SSH host public keys, one per line (authorized_keys or ssh-keyscan format; get one with `ssh-keyscan -t ed25519 HOST`). Without it any host key is accepted")
		sshHostOverride = flag.String("ssh-host-override", os.Getenv("PX_SSH_HOST_OVERRIDE"), "SSH host per PVE node as \"node=host,...\" (PVE node names usually do not resolve in DNS). Ignored for -pve-node: single-node mode always SSHes to the -pve-endpoint host")
		interval        = flag.Duration("reconcile-interval", 2*time.Second, "controller reconcile interval")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if *pveEndpoint == "" || *pveToken == "" {
		fmt.Fprintln(os.Stderr, "error: --pve-endpoint and --pve-token are required")
		os.Exit(2)
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Error("open store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	pve := proxmox.New(*pveEndpoint, *pveNode, *pveToken, *tlsInsecure)

	hosts, err := parseHostOverrides(*sshHostOverride)
	if err != nil {
		log.Error("parse -ssh-host-override", "err", err)
		os.Exit(2)
	}
	// One endpoint and token reach every node (the PVE API proxies
	// cross-node requests), so a node-scoped client differs only in the node
	// its paths address. Created on first use and cached.
	ncMu := &sync.Mutex{}
	ncCache := map[string]*proxmox.Client{}
	nodePVE := func(node string) *proxmox.Client {
		ncMu.Lock()
		defer ncMu.Unlock()
		if c, ok := ncCache[node]; ok {
			return c
		}
		c := proxmox.New(*pveEndpoint, node, *pveToken, *tlsInsecure)
		ncCache[node] = c
		return c
	}

	sshPool := sshexec.NewPool(10*time.Second, sshexec.Config{User: *sshUser, KeyPath: *sshKey, HostKeyPath: *sshHostKey}, hosts)
	defer sshPool.Close()
	if *pveNode != "" {
		// Single-node mode (the pre-cluster flag set): keep the old behavior
		// exactly — SSH straight to the endpoint host, dial it up front so a
		// bad config fails fast, and route everything through the one client.
		hosts[*pveNode] = hostOf(*pveEndpoint)
		nodePVE = func(string) *proxmox.Client { return pve }
		if _, err := sshPool.Executor(*pveNode); err != nil {
			// Fail fast: without node SSH access the provisioner cannot boot
			// runners.
			log.Error("ssh dial failed; px-server needs SSH access to the PVE node", "host", hostOf(*pveEndpoint), "err", err)
			os.Exit(1)
		}
		log.Info("single-node mode", "node", *pveNode, "ssh-host", hostOf(*pveEndpoint))
	} else {
		// Cluster mode: tasks land on any online node holding spec.image, so
		// SSH dials lazily on first use — nodes never chosen cost nothing.
		log.Info("cluster mode", "nodes", "scheduled per task; ssh hosts from -ssh-host-override")
	}
	if *sshHostKey != "" {
		log.Info("ssh host key pinning enabled", "host-key-file", *sshHostKey)
	} else {
		log.Warn("ssh host key is not pinned: any host key is accepted (set -ssh-host-key)")
	}

	prov := controller.NewProvisioner(pve, nodePVE, sshPool, *pveNode)
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

	if *pveNode != "" {
		log.Info("px-server listening", "addr", *listen, "node", *pveNode)
	} else {
		log.Info("px-server listening", "addr", *listen, "mode", "cluster")
	}
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

// parseHostOverrides parses "node=host,node=host" into the pool's override
// map. An empty spec yields an empty map (not nil), so callers can assign
// into it unconditionally.
func parseHostOverrides(spec string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(spec) == "" {
		return out, nil
	}
	for _, pair := range strings.Split(spec, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("%q: want node=host", pair)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
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
