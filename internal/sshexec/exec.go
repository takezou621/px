// Package sshexec runs commands on the PVE node over SSH.
// The PVE REST API has no container-exec verb, so runner commands go through
// `pct exec` on the node.
package sshexec

import (
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/term"
)

// Config selects how to authenticate to the node.
type Config struct {
	Host    string // PVE node host
	Port    int    // default 22
	User    string // default root
	KeyPath string // optional private key path; falls back to ssh-agent
}

type Executor struct {
	cfg    Config
	client *ssh.Client
}

func Dial(ctxDialTimeout time.Duration, cfg Config) (*Executor, error) {
	if cfg.User == "" {
		cfg.User = "root"
	}
	if cfg.Port == 0 {
		cfg.Port = 22
	}
	auths, err := authMethods(cfg.KeyPath)
	if err != nil {
		return nil, err
	}
	client, err := ssh.Dial("tcp", fmt.Sprintf("%s:%d", cfg.Host, cfg.Port), &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            auths,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // TODO(M3): known_hosts pinning
		Timeout:         ctxDialTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("ssh %s@%s:%d: %w", cfg.User, cfg.Host, cfg.Port, err)
	}
	return &Executor{cfg: cfg, client: client}, nil
}

func (e *Executor) Close() error {
	if e.client != nil {
		return e.client.Close()
	}
	return nil
}

// Run executes a command on the node, returning combined output and exit code.
func (e *Executor) Run(cmd string, timeout time.Duration) (string, int, error) {
	sess, err := e.client.NewSession()
	if err != nil {
		return "", -1, err
	}
	defer sess.Close()
	if timeout > 0 {
		t := time.AfterFunc(timeout, func() { _ = sess.Close() })
		defer t.Stop()
	}
	out, err := sess.CombinedOutput(cmd)
	code := 0
	if err != nil {
		if ee, ok := err.(*ssh.ExitError); ok {
			code = ee.ExitStatus()
			err = nil
		} else {
			return string(out), -1, err
		}
	}
	return string(out), code, nil
}

func authMethods(keyPath string) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	if keyPath != "" {
		key, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, err
		}
		var signer ssh.Signer
		if pass, ok := askPassphraseOnce(key); ok {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(key, pass)
		} else {
			signer, err = ssh.ParsePrivateKey(key)
		}
		if err != nil {
			return nil, fmt.Errorf("parse key %s: %w", keyPath, err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
		return methods, nil
	}
	// Fall back to ssh-agent.
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		conn, err := net.Dial("unix", sock)
		if err == nil {
			ag := agent.NewClient(conn)
			methods = append(methods, ssh.PublicKeysCallback(ag.Signers))
		}
	}
	if len(methods) == 0 {
		return nil, fmt.Errorf("no ssh auth: set ssh.key or SSH_AUTH_SOCK")
	}
	return methods, nil
}

func askPassphraseOnce(key []byte) ([]byte, bool) {
	if _, err := ssh.ParsePrivateKey(key); err == nil {
		return nil, false // not encrypted
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, false
	}
	fmt.Fprintf(os.Stderr, "Enter passphrase for ssh key: ")
	pass, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return nil, false
	}
	return pass, true
}
