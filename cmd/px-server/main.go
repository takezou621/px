// px-server is the px control plane: REST API + task controller in one binary.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
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
		listen      = flag.String("listen", ":7420", "HTTP listen address")
		dbPath      = flag.String("db", "px.db", "SQLite database path")
		pveEndpoint = flag.String("pve-endpoint", os.Getenv("PX_PVE_ENDPOINT"), "Proxmox VE API endpoint (https://host:8006)")
		pveNode     = flag.String("pve-node", os.Getenv("PX_PVE_NODE"), "Proxmox VE node name")
		pveToken    = flag.String("pve-token", os.Getenv("PX_PVE_TOKEN"), "PVE API token: user@realm!tokenid=secret")
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

	pve := proxmox.New(*pveEndpoint, *pveNode, *pveToken)
	ssh, err := sshexec.Dial(10*time.Second, sshexec.Config{Host: hostOf(*pveEndpoint), User: *sshUser, KeyPath: *sshKey})
	if err != nil {
		log.Warn("ssh dial failed; runner exec unavailable", "err", err)
	}
	if ssh != nil {
		defer ssh.Close()
	}

	prov := controller.NewProvisioner(pve, ssh)
	ctl := controller.New(st, prov, log)
	ctl.Tick = *interval

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go ctl.Run(ctx)

	srv := &http.Server{
		Addr:              *listen,
		Handler:           server.New(st, ctl, prov, log).Handler(),
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
	// https://pve.example.com:8006 -> pve.example.com
	s := endpoint
	for _, p := range []string{"https://", "http://"} {
		if len(s) > len(p) && s[:len(p)] == p {
			s = s[len(p):]
			break
		}
	}
	if i := indexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	if i := indexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return s
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
