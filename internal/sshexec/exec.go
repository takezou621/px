// Package sshexec runs commands on the PVE node over SSH.
// The PVE REST API has no container-exec verb, so runner commands go through
// `pct exec` on the node.
package sshexec

import (
	"fmt"
	"net"
	"os"
	"sync"
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

// Executor holds one SSH connection to the node and re-dials lazily if the
// connection drops, so a network blip does not kill the control plane until
// restart.
type Executor struct {
	cfg        Config
	dialTimeout time.Duration

	mu     sync.Mutex
	client *ssh.Client
}

func Dial(dialTimeout time.Duration, cfg Config) (*Executor, error) {
	if cfg.User == "" {
		cfg.User = "root"
	}
	if cfg.Port == 0 {
		cfg.Port = 22
	}
	e := &Executor{cfg: cfg, dialTimeout: dialTimeout}
	client, err := e.dial()
	if err != nil {
		return nil, err
	}
	e.client = client
	return e, nil
}

func (e *Executor) dial() (*ssh.Client, error) {
	auths, err := authMethods(e.cfg.KeyPath)
	if err != nil {
		return nil, err
	}
	client, err := ssh.Dial("tcp", fmt.Sprintf("%s:%d", e.cfg.Host, e.cfg.Port), &ssh.ClientConfig{
		User:            e.cfg.User,
		Auth:            auths,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // TODO(M3): known_hosts pinning
		Timeout:         e.dialTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("ssh %s@%s:%d: %w", e.cfg.User, e.cfg.Host, e.cfg.Port, err)
	}
	return client, nil
}

func (e *Executor) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.client != nil {
		return e.client.Close()
	}
	return nil
}

// Run executes a command on the node, returning combined output and exit code.
// A dead connection is re-dialed once before giving up; a remote command that
// exits non-zero is not a connection failure and is not retried.
func (e *Executor) Run(cmd string, timeout time.Duration) (string, int, error) {
	out, code, err := e.runOnce(cmd, timeout)
	if err != nil && e.redial() {
		out, code, err = e.runOnce(cmd, timeout)
	}
	return out, code, err
}

func (e *Executor) runOnce(cmd string, timeout time.Duration) (string, int, error) {
	e.mu.Lock()
	client := e.client
	e.mu.Unlock()
	if client == nil {
		return "", -1, fmt.Errorf("ssh not connected")
	}
	sess, err := client.NewSession()
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

func (e *Executor) redial() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.client != nil {
		_ = e.client.Close()
	}
	client, err := e.dial()
	if err != nil {
		e.client = nil
		return false
	}
	e.client = client
	return true
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
