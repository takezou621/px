// Package sshexec runs commands on the PVE node over SSH.
// The PVE REST API has no container-exec verb, so runner commands go through
// `pct exec` on the node.
package sshexec

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/term"
)

// Config selects how to authenticate to the node.
type Config struct {
	Host        string // PVE node host
	Port        int    // default 22
	User        string // default root
	KeyPath     string // optional private key path; falls back to ssh-agent
	HostKeyPath string // optional file of pinned host public keys; empty accepts any key
}

// Executor holds one SSH connection to the node and re-dials lazily if the
// connection drops, so a network blip does not kill the control plane until
// restart.
type Executor struct {
	cfg         Config
	dialTimeout time.Duration
	hostKeys    []ssh.PublicKey // pinned keys; empty accepts any host key

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
	if cfg.HostKeyPath != "" {
		data, err := os.ReadFile(cfg.HostKeyPath)
		if err != nil {
			return nil, err
		}
		keys, err := parseHostKeys(data)
		if err != nil {
			return nil, fmt.Errorf("parse host keys %s: %w", cfg.HostKeyPath, err)
		}
		e.hostKeys = keys
	}
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
	var hostKeyCallback ssh.HostKeyCallback = ssh.InsecureIgnoreHostKey()
	var hostKeyAlgos []string
	if len(e.hostKeys) > 0 {
		hostKeyCallback = pinnedHostKeyCallback(e.hostKeys)
		hostKeyAlgos = pinnedAlgos(e.hostKeys)
	}
	client, err := ssh.Dial("tcp", fmt.Sprintf("%s:%d", e.cfg.Host, e.cfg.Port), &ssh.ClientConfig{
		User:              e.cfg.User,
		Auth:              auths,
		HostKeyCallback:   hostKeyCallback,
		HostKeyAlgorithms: hostKeyAlgos,
		Timeout:           e.dialTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("ssh %s@%s:%d: %w", e.cfg.User, e.cfg.Host, e.cfg.Port, err)
	}
	return client, nil
}

// pinnedAlgos restricts the handshake's host key algorithms to the types
// actually pinned. Without it the negotiated key is whatever the server
// prefers (often ECDSA next to a pinned ED25519) and the pin never matches.
func pinnedAlgos(keys []ssh.PublicKey) []string {
	seen := make(map[string]bool)
	var algos []string
	for _, k := range keys {
		for _, a := range algoNames(k) {
			if !seen[a] {
				seen[a] = true
				algos = append(algos, a)
			}
		}
	}
	return algos
}

// algoNames lists the host key algorithms that can present this key on the
// wire. A pinned RSA key must also be advertised as rsa-sha2-256/512: modern
// sshd (PVE ships one) no longer negotiates bare ssh-rsa, while rsa-sha2
// host keys decode to the same public key blob the pin holds.
func algoNames(k ssh.PublicKey) []string {
	if k.Type() == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSA}
	}
	return []string{k.Type()}
}

// parseHostKeys reads public host keys, one per line: both authorized_keys
// format (`ssh-ed25519 AAAA... comment`) and ssh-keyscan output
// (`host ssh-ed25519 AAAA...`) parse. Blank lines and `#` comments are
// skipped, and several keys are allowed so a rotation window keeps working.
// Only the key bytes are pinned — the line's hostname field, if any, is not
// compared, because px talks to exactly one node.
func parseHostKeys(data []byte) ([]ssh.PublicKey, error) {
	var keys []ssh.PublicKey
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "@") {
			// @cert-authority / @revoked markers pin CA trust, not a host key;
			// accepting them as plain keys would silently never match.
			return nil, fmt.Errorf("line %q: @-marker lines are not supported (pin the host's public key itself)", line)
		}
		if _, _, pub, _, _, err := ssh.ParseKnownHosts([]byte(line)); err == nil {
			keys = append(keys, pub)
			continue
		}
		pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			return nil, fmt.Errorf("line %q: %w", line, err)
		}
		keys = append(keys, pub)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no host keys found")
	}
	return keys, nil
}

// pinnedHostKeyCallback accepts only the given keys; a mismatch fails the
// handshake with the offending key's fingerprint, so a wrong pin or a
// man-in-the-middle is diagnosable from the error alone.
func pinnedHostKeyCallback(keys []ssh.PublicKey) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, k ssh.PublicKey) error {
		got := k.Marshal()
		for _, pinned := range keys {
			if bytes.Equal(pinned.Marshal(), got) {
				return nil
			}
		}
		return fmt.Errorf("host key %s is not pinned (check -ssh-host-key)", ssh.FingerprintSHA256(k))
	}
}

func (e *Executor) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.client != nil {
		return e.client.Close()
	}
	return nil
}

// lockedWriter is a concurrency-safe buffer: one writer receiving both session
// streams is written from two goroutines (ssh copies stdout and stderr on
// separate ones), so plain bytes.Buffer would race.
type lockedWriter struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *lockedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// Run executes a command on the node, returning combined output and exit code.
// A dead connection is re-dialed once before giving up; a remote command that
// exits non-zero is not a connection failure and is not retried.
func (e *Executor) Run(cmd string, timeout time.Duration) (string, int, error) {
	var out lockedWriter
	code, err := e.RunStreams(cmd, timeout, &out, &out)
	return out.String(), code, err
}

// RunStreams is Run with stdout and stderr routed to separate writers (the
// same single writer receives both streams interleaved when it is passed
// twice), so a caller can keep the streams apart — `px exec` does.
func (e *Executor) RunStreams(cmd string, timeout time.Duration, stdout, stderr io.Writer) (int, error) {
	code, err := e.runStreamsOnce(cmd, timeout, stdout, stderr)
	if err != nil && e.redial() {
		code, err = e.runStreamsOnce(cmd, timeout, stdout, stderr)
	}
	return code, err
}

func (e *Executor) runStreamsOnce(cmd string, timeout time.Duration, stdout, stderr io.Writer) (int, error) {
	e.mu.Lock()
	client := e.client
	e.mu.Unlock()
	if client == nil {
		return -1, fmt.Errorf("ssh not connected")
	}
	sess, err := client.NewSession()
	if err != nil {
		return -1, err
	}
	defer sess.Close()
	if timeout > 0 {
		t := time.AfterFunc(timeout, func() { _ = sess.Close() })
		defer t.Stop()
	}
	// Leaving a stream unset would wire the session to os.Stdout of this
	// process; everything must land in the caller's writers.
	sess.Stdout = io.Discard
	sess.Stderr = io.Discard
	if stdout != nil {
		sess.Stdout = stdout
	}
	if stderr != nil {
		sess.Stderr = stderr
	}
	err = sess.Run(cmd)
	if err != nil {
		if ee, ok := err.(*ssh.ExitError); ok {
			return ee.ExitStatus(), nil
		}
		return -1, err
	}
	return 0, nil
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
