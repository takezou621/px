// Package sshexec runs commands on the PVE node over SSH.
// The PVE REST API has no container-exec verb, so runner commands go through
// `pct exec` on the node.
package sshexec

import (
	"bytes"
	"context"
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

// pinnedKey is one parsed pin-file line. Hosts names the hosts the key
// applies to (known_hosts-format lines carry a hostname field); an empty
// list means every host (authorized_keys-format lines), which is what keeps
// a single-node pin file working unchanged in cluster mode. Matching is by
// exact hostname string — px dials nodes by their name or an overridden
// address, and no wildcard resolution is worth the ambiguity here.
type pinnedKey struct {
	hosts []string
	key   ssh.PublicKey
}

// Executor holds one SSH connection to a host and re-dials lazily if the
// connection drops, so a network blip does not kill the control plane until
// restart. A Pool creates one Executor per PVE node.
type Executor struct {
	cfg         Config
	dialTimeout time.Duration
	hostKeys    []pinnedKey // pinned keys; empty accepts any host key

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
	keys := keysForHost(e.hostKeys, e.cfg.Host)
	switch {
	case len(e.hostKeys) > 0 && len(keys) == 0:
		// A pin file is in use but names no key for this host: dialing
		// insecurely here would silently drop verification for a whole node
		// just because its line is missing (or misspelled) from the file.
		return nil, fmt.Errorf("no pinned host key for %s (check -ssh-host-key)", e.cfg.Host)
	case len(keys) > 0:
		hostKeyCallback = pinnedHostKeyCallback(keys)
		plain := make([]ssh.PublicKey, len(keys))
		for i, k := range keys {
			plain[i] = k.key
		}
		hostKeyAlgos = pinnedAlgos(plain)
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
// A known_hosts-format line's hostname field (comma-separated, exact match
// only) scopes the pin to those hosts; a line without one applies to every
// host px dials. Hashed-hostname lines cannot be scoped, so they are
// rejected at parse time rather than silently never matching.
func parseHostKeys(data []byte) ([]pinnedKey, error) {
	var keys []pinnedKey
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
		// authorized_keys format first (the key type itself is field one);
		// anything else is tried as a known_hosts line with a hostname
		// scope. The scope is split by hand instead of via
		// ssh.ParseKnownHosts: which of its string-vs-slice returns a given
		// call site sees has proven toolchain-dependent, and the field is a
		// plain comma-separated list anyway.
		if pub, err := parseKeyLine(line); err == nil {
			keys = append(keys, pinnedKey{key: pub})
			continue
		}
		hosts, pub, err := parseScopedKeyLine(line)
		if err != nil {
			return nil, fmt.Errorf("line %q: %w", line, err)
		}
		keys = append(keys, pinnedKey{hosts: hosts, key: pub})
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no host keys found")
	}
	return keys, nil
}

// parseKeyLine parses an authorized_keys-format line (no hostname field).
// The returned pin applies to every host px dials. ParseAuthorizedKey
// tolerates a leading options field, which would swallow a known_hosts
// line's hostname — so field one must equal the key's own type, or the
// caller re-tries the line as scoped.
func parseKeyLine(line string) (ssh.PublicKey, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return nil, fmt.Errorf("empty line")
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return nil, err
	}
	if fields[0] != pub.Type() {
		return nil, fmt.Errorf("key type %q does not match the line's first field %q", pub.Type(), fields[0])
	}
	return pub, nil
}

// parseScopedKeyLine parses a known_hosts-format line: a comma-separated
// hostname list is field one, the key follows. Hostname matching is by exact
// string, so a hashed hostname can never match and is rejected up front.
func parseScopedKeyLine(line string) ([]string, ssh.PublicKey, error) {
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return nil, nil, fmt.Errorf("too few fields")
	}
	names := strings.Split(fields[0], ",")
	for _, h := range names {
		if strings.HasPrefix(h, "|1|") {
			return nil, nil, fmt.Errorf("hashed hostname %q is not supported (add the host in plain form)", h)
		}
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.Join(fields[1:], " ")))
	if err != nil {
		return nil, nil, err
	}
	return names, pub, nil
}

// keysForHost narrows the pinned keys to those that apply to one host —
// the callback and the handshake algorithm list must not consider keys
// scoped to other nodes, or a foreign pin would change this host's
// negotiation (or accept a key it was never pinned for).
func keysForHost(keys []pinnedKey, host string) []pinnedKey {
	var out []pinnedKey
	for _, pk := range keys {
		if len(pk.hosts) == 0 || containsHost(pk.hosts, host) {
			out = append(out, pk)
		}
	}
	return out
}

func containsHost(hosts []string, host string) bool {
	for _, h := range hosts {
		if h == host {
			return true
		}
	}
	return false
}

// pinnedHostKeyCallback accepts only the given keys; a mismatch fails the
// handshake with the offending key's fingerprint, so a wrong pin or a
// man-in-the-middle is diagnosable from the error alone. Installed only when
// at least one key applies, so the zero-key case never reaches here.
func pinnedHostKeyCallback(keys []pinnedKey) ssh.HostKeyCallback {
	return func(host string, _ net.Addr, k ssh.PublicKey) error {
		got := k.Marshal()
		for _, pinned := range keys {
			if bytes.Equal(pinned.key.Marshal(), got) {
				return nil
			}
		}
		return fmt.Errorf("host %s key %s is not pinned (check -ssh-host-key)", host, ssh.FingerprintSHA256(k))
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

// Pool keeps one Executor per PVE node, dialed lazily: a node no task ever
// lands on is never SSH'd, so cluster mode costs no connections until a
// sandbox is provisioned there. The base Config supplies user/key/pins;
// each node's host comes from hosts (the px-server's node→host override
// map) or the node name itself — PVE node names are not guaranteed to
// resolve, which is what the override map exists for.
type Pool struct {
	base        Config
	dialTimeout time.Duration
	hosts       map[string]string

	mu    sync.Mutex
	execs map[string]*Executor
}

func NewPool(dialTimeout time.Duration, base Config, hosts map[string]string) *Pool {
	return &Pool{base: base, dialTimeout: dialTimeout, hosts: hosts, execs: map[string]*Executor{}}
}

// Executor returns the (dialed-on-first-use) executor for a node.
func (p *Pool) Executor(node string) (*Executor, error) {
	p.mu.Lock()
	e, ok := p.execs[node]
	p.mu.Unlock()
	if ok {
		return e, nil
	}
	cfg := p.base
	cfg.Host = node
	if p.hosts != nil {
		if h, ok := p.hosts[node]; ok {
			cfg.Host = h
		}
	}
	e, err := Dial(p.dialTimeout, cfg)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	if existing, ok := p.execs[node]; ok {
		p.mu.Unlock()
		e.Close()
		return existing, nil
	}
	p.execs[node] = e
	p.mu.Unlock()
	return e, nil
}

func (p *Pool) Close() error {
	p.mu.Lock()
	execs := p.execs
	p.execs = map[string]*Executor{}
	p.mu.Unlock()
	var first error
	for _, e := range execs {
		if err := e.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
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
// exits non-zero is not a connection failure and is not retried. Each attempt
// starts from a fresh buffer: a retry means the previous attempt died
// mid-flight, and its partial output must not splice into the final result.
// A canceled ctx is never a redial — the connection is fine, only the caller
// is gone. The retry dials only if the client the failed attempt actually
// used is still current; another caller's re-dial is left alone.
func (e *Executor) Run(ctx context.Context, cmd string, timeout time.Duration) (string, int, error) {
	var out lockedWriter
	used, code, err := e.runStreams(ctx, cmd, timeout, &out, &out)
	if err != nil && ctx.Err() == nil && e.redial(used) {
		out = lockedWriter{}
		_, code, err = e.runStreams(ctx, cmd, timeout, &out, &out)
	}
	return out.String(), code, err
}

// RunStreamsOnce is Run with stdout and stderr routed to separate writers (the
// same single writer receives both streams interleaved when it is passed
// twice), and it never retries: the caller running a one-shot user command
// (`px exec`) must not have it executed a second time — a retry repeats side
// effects and splices the first attempt's partial output into the result. A
// failure here is final; the user re-runs the command. A canceled ctx closes
// the session (killing the remote command) but not the shared client, which
// other users of the node still hold.
func (e *Executor) RunStreamsOnce(ctx context.Context, cmd string, timeout time.Duration, stdout, stderr io.Writer) (int, error) {
	_, code, err := e.runStreams(ctx, cmd, timeout, stdout, stderr)
	return code, err
}

// runStreams is RunStreamsOnce, also returning the client the attempt ran on
// so a retrying caller can redial exactly that connection.
func (e *Executor) runStreams(ctx context.Context, cmd string, timeout time.Duration, stdout, stderr io.Writer) (*ssh.Client, int, error) {
	e.mu.Lock()
	client := e.client
	e.mu.Unlock()
	if client == nil {
		return nil, -1, fmt.Errorf("ssh not connected")
	}
	// NewSession opens a channel on the live connection and can stall if the
	// node stops answering without dropping TCP, so race it against ctx: a
	// cancel returns promptly. A session that still arrives afterwards is
	// closed as soon as it opens — the shared client stays up.
	resCh := make(chan sessionResult, 1)
	go func() {
		sess, err := client.NewSession()
		resCh <- sessionResult{sess: sess, err: err}
	}()
	var sess *ssh.Session
	select {
	case res := <-resCh:
		if res.err != nil {
			return client, -1, res.err
		}
		sess = res.sess
	case <-ctx.Done():
		go func() {
			if res := <-resCh; res.sess != nil {
				_ = res.sess.Close()
			}
		}()
		return client, -1, fmt.Errorf("canceled: %w", ctx.Err())
	}
	defer sess.Close()
	if timeout > 0 {
		t := time.AfterFunc(timeout, func() { _ = sess.Close() })
		defer t.Stop()
	}
	if done := ctx.Done(); done != nil {
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			select {
			case <-done:
				_ = sess.Close()
			case <-stop:
			}
		}()
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
	err := sess.Run(cmd)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return client, -1, fmt.Errorf("canceled: %w", ctxErr)
		}
		if ee, ok := err.(*ssh.ExitError); ok {
			return client, ee.ExitStatus(), nil
		}
		return client, -1, err
	}
	return client, 0, nil
}

// sessionResult carries a NewSession outcome across the race goroutine.
type sessionResult struct {
	sess *ssh.Session
	err  error
}

// current reports the live client, or nil after a failed redial. A read-only
// helper for callers that need to observe the connection identity (tests, and
// nothing else).
func (e *Executor) current() *ssh.Client {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.client
}

// redial replaces the connection after a failed attempt. The caller passes
// the client its attempt actually ran on (runStreams returns it), so the
// only client ever closed is the one known to have failed: a concurrent
// caller may have already re-dialed between that attempt's failure and now,
// and their client is healthy — blindly closing whatever is current (the old
// behavior) would tear down sessions it just started.
func (e *Executor) redial(prev *ssh.Client) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.client != prev {
		return e.client != nil
	}
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
