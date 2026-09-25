package sshexec

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func testPubKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

func pubKeyLine(pub ssh.PublicKey) string {
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(pub.Marshal())
}

func TestParseHostKeys(t *testing.T) {
	pub := testPubKey(t)
	line := pubKeyLine(pub)

	// authorized_keys format, with surrounding comments and blank lines.
	got, err := parseHostKeys([]byte("# my pve host\n\n" + line + " root@pve\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !bytes.Equal(got[0].key.Marshal(), pub.Marshal()) {
		t.Fatalf("want exactly the pinned key, got %d keys", len(got))
	}
	// A line without a hostname field applies to every host px dials.
	if hosts := got[0].hosts; len(hosts) != 0 {
		t.Fatalf("unscoped pin carries hosts %v, want none", hosts)
	}

	// ssh-keyscan output: the hostname prefix must not break parsing.
	got, err = parseHostKeys([]byte("pve.example.com " + line))
	if err != nil {
		t.Fatalf("ssh-keyscan line rejected: %v", err)
	}
	if len(got) != 1 || !bytes.Equal(got[0].key.Marshal(), pub.Marshal()) {
		t.Fatalf("want exactly the pinned key, got %d keys", len(got))
	}
	if !slices.Equal(got[0].hosts, []string{"pve.example.com"}) {
		t.Fatalf("scoped pin carries hosts %v, want [pve.example.com]", got[0].hosts)
	}

	// Several keys (a rotation window) all pin.
	got, err = parseHostKeys([]byte(pubKeyLine(testPubKey(t)) + "\n" + line + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 keys, got %d", len(got))
	}
}

func TestParseHostKeysInvalid(t *testing.T) {
	if _, err := parseHostKeys(nil); err == nil {
		t.Fatal("expected error for no keys")
	}
	if _, err := parseHostKeys([]byte("# only a comment\n\n")); err == nil {
		t.Fatal("expected error for no keys")
	}
	if _, err := parseHostKeys([]byte("zzz no base64 here")); err == nil {
		t.Fatal("expected error for a garbage line")
	}
}

func TestPinnedHostKeyCallback(t *testing.T) {
	pinned := testPubKey(t)
	other := testPubKey(t)

	cb := pinnedHostKeyCallback([]pinnedKey{{key: pinned}})
	if err := cb("pve", nil, pinned); err != nil {
		t.Fatalf("pinned key rejected: %v", err)
	}
	if err := cb("pve", nil, other); err == nil {
		t.Fatal("unpinned key accepted")
	}
}

// A pin scoped to one hostname must not accept (or steer the handshake of) a
// different node: cluster mode pins per node, and a foreign node must keep
// its own trust decision.
func TestKeysForHostScoping(t *testing.T) {
	mine := testPubKey(t)
	foreign := testPubKey(t)
	keys := []pinnedKey{
		{hosts: []string{"third", "192.168.2.100"}, key: mine},
		{hosts: []string{"second"}, key: foreign},
	}
	// ssh.PublicKey values wrap uncomparable structs, so pins are compared
	// by their marshalled bytes — the same form the handshake check uses.
	if got := keysForHost(keys, "third"); len(got) != 1 || !bytes.Equal(got[0].key.Marshal(), mine.Marshal()) {
		t.Fatalf("third must see only its own pin, got %d keys", len(got))
	}
	// known_hosts lines scope by comma-separated hostname list — px dials
	// the node by name or by an overridden address, and both must match.
	if got := keysForHost(keys, "192.168.2.100"); len(got) != 1 || !bytes.Equal(got[0].key.Marshal(), mine.Marshal()) {
		t.Fatalf("an overridden address must still match its scoped pin, got %d keys", len(got))
	}
	if got := keysForHost(keys, "second"); len(got) != 1 || !bytes.Equal(got[0].key.Marshal(), foreign.Marshal()) {
		t.Fatalf("second must see only its own pin, got %d keys", len(got))
	}
	// A node with no scoped pin sees nothing — never another node's pin.
	if got := keysForHost(keys, "forth"); len(got) != 0 {
		t.Fatalf("forth must see no pins, got %d keys", len(got))
	}
	// An unscoped pin keeps applying to every host.
	if got := keysForHost([]pinnedKey{{key: mine}}, "anything"); len(got) != 1 {
		t.Fatalf("unscoped pin must match every host, got %d keys", len(got))
	}
}

// A scoped pin must actually gate the handshake: dialing that host with a
// mismatched key fails the callback, not silently proceeds.
func TestScopedPinGatesHandshake(t *testing.T) {
	mine := testPubKey(t)
	foreign := testPubKey(t)
	cb := pinnedHostKeyCallback(keysForHost([]pinnedKey{{hosts: []string{"third"}, key: mine}}, "third"))
	if err := cb("third", nil, mine); err != nil {
		t.Fatalf("third rejected its pinned key: %v", err)
	}
	if err := cb("third", nil, foreign); err == nil {
		t.Fatal("third accepted a key it never pinned")
	}
}

// testPrivateKey writes an RSA private key PEM the executor can parse.
func testPrivateKey(t *testing.T) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "id_rsa")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)})
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// With a pin file in use, a host the file never names must fail closed at
// dial time: falling back to InsecureIgnoreHostKey would silently drop
// verification for a whole node just because its line is missing (or
// misspelled) from the file.
func TestDialFailsClosedForUnpinnedHost(t *testing.T) {
	mine := testPubKey(t)
	pinPath := filepath.Join(t.TempDir(), "hostkeys")
	pin := "third " + pubKeyLine(mine) + "\n"
	if err := os.WriteFile(pinPath, []byte(pin), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Dial(2*time.Second, Config{
		Host:        "unpinned.invalid",
		User:        "root",
		KeyPath:     testPrivateKey(t),
		HostKeyPath: pinPath,
	})
	if err == nil || !strings.Contains(err.Error(), "no pinned host key for unpinned.invalid") {
		t.Fatalf("want a fail-closed pin error, got %v", err)
	}
}

// An authorized_keys-format pin (no hostname field) applies to every host,
// so dialing must get past the pin gate — the failure, if any, is the
// connection itself, not verification being dropped.
func TestDialUnscopedPinGatesEveryHost(t *testing.T) {
	mine := testPubKey(t)
	pinPath := filepath.Join(t.TempDir(), "hostkeys")
	if err := os.WriteFile(pinPath, []byte(pubKeyLine(mine)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Dial(2*time.Second, Config{
		Host:        "unreachable.invalid",
		User:        "root",
		KeyPath:     testPrivateKey(t),
		HostKeyPath: pinPath,
	})
	if err == nil {
		t.Fatal("dialing an unreachable host should fail")
	}
	if strings.Contains(err.Error(), "no pinned host key") {
		t.Fatalf("an unscoped pin must cover every host, got %v", err)
	}
}

func TestPinnedAlgos(t *testing.T) {
	one := testPubKey(t)
	same := testPubKey(t) // same type, different key
	got := pinnedAlgos([]ssh.PublicKey{one, same})
	if len(got) != 1 || got[0] != one.Type() {
		t.Fatalf("want one %s, got %v", one.Type(), got)
	}
}

// Keys of different types each advertise their own algorithm.
func TestPinnedAlgosMixedTypes(t *testing.T) {
	ed := testPubKey(t)
	ecPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecPub, err := ssh.NewPublicKey(&ecPriv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	got := pinnedAlgos([]ssh.PublicKey{ed, ecPub})
	want := []string{ssh.KeyAlgoECDSA256, ssh.KeyAlgoED25519}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}

// A pinned RSA key must also advertise rsa-sha2-256/512: modern sshd no
// longer negotiates bare ssh-rsa, so pinning RSA without them never matches.
func TestAlgoNamesRSA(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ssh.NewPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	got := algoNames(pub)
	want := []string{ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSA}
	if !slices.Equal(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}

	// Non-RSA keys stay single-algorithm.
	ed := testPubKey(t)
	if got := algoNames(ed); !slices.Equal(got, []string{ed.Type()}) {
		t.Fatalf("ed25519 should list only itself, got %v", got)
	}
}

// @cert-authority / @revoked lines pin CA trust, not a host key; accepting
// them as plain pins would silently never match.
func TestParseHostKeysRejectsMarkers(t *testing.T) {
	line := pubKeyLine(testPubKey(t))
	for _, marker := range []string{
		"@cert-authority * " + line,
		"@revoked pve.example.com " + line,
	} {
		if _, err := parseHostKeys([]byte(marker)); err == nil {
			t.Errorf("@-marker line accepted as a pin: %q", marker)
		}
	}
}

// Hashed hostnames cannot be scoped by exact match, so they are rejected at
// parse time — a pin that silently never matches looks exactly like a pin
// that works, right up to the man-in-the-middle.
func TestParseHostKeysRejectsHashedHostname(t *testing.T) {
	line := pubKeyLine(testPubKey(t))
	if _, err := parseHostKeys([]byte("|1|salt1|salt2 " + line)); err == nil {
		t.Fatal("hashed hostname accepted as a scoped pin")
	}
}

// ---- in-process SSH server ----

// fakeServer is a real (in-process) SSH server for exercising the executor's
// wire paths — dial, redial, timeout, ctx cancel, stream separation — that
// the parsing tests above cannot reach. cmdFn runs per exec request; its
// return value becomes the session's exit status (negative: the handler
// already dropped the whole connection, so no exit-status is sent).
type fakeServer struct {
	ln      net.Listener
	client  ssh.PublicKey // the key the server accepts
	hostKey ssh.Signer
	cmdFn   func(cmd string, stdout, stderr io.Writer) int

	mu         sync.Mutex
	failConns  int    // accept-and-immediately-close this many connections
	closeOnCmd string // an exec payload containing this kills the connection mid-run
	commands   []string
	conns      int // TCP connections accepted, failed or not — redial tests count dials
}

func newFakeServer(t *testing.T, cmdFn func(cmd string, stdout, stderr io.Writer) int) *fakeServer {
	t.Helper()
	hostSigner, err := ssh.NewSignerFromKey(testHostPrivKey(t))
	if err != nil {
		t.Fatal(err)
	}
	clientPub, err := ssh.NewPublicKey(testClientPrivKey(t).Public())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{ln: ln, client: clientPub, hostKey: hostSigner, cmdFn: cmdFn}
	go s.acceptLoop()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func testHostPrivKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

var testClientKeyOnce sync.Once
var testClientKey ed25519.PrivateKey

// testClientPrivKey is the executor's auth key, generated once per process:
// key generation is the slow part of standing the fake server up.
func testClientPrivKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	testClientKeyOnce.Do(func() {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			panic(err)
		}
		testClientKey = priv
	})
	return testClientKey
}

func (s *fakeServer) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns++
		fail := s.failConns > 0
		if fail {
			s.failConns--
		}
		s.mu.Unlock()
		if fail {
			_ = conn.Close()
			continue
		}
		go s.serveConn(conn)
	}
}

func (s *fakeServer) serveConn(conn net.Conn) {
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), s.client.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, fmt.Errorf("unknown client key")
		},
	}
	cfg.AddHostKey(s.hostKey)
	sconn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer sconn.Close()
	go ssh.DiscardRequests(reqs)
	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		go s.handleSession(sconn, ch, chReqs)
	}
}

func (s *fakeServer) handleSession(sconn *ssh.ServerConn, ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			_ = req.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
			_ = req.Reply(false, nil)
			return
		}
		_ = req.Reply(true, nil)
		s.mu.Lock()
		s.commands = append(s.commands, payload.Command)
		drop := s.closeOnCmd != "" && strings.Contains(payload.Command, s.closeOnCmd)
		s.closeOnCmd = "" // consume: only the first matching exec is dropped
		s.mu.Unlock()
		code := s.cmdFn(payload.Command, ch, ch.Stderr())
		if code < 0 {
			_ = sconn.Close()
			return
		}
		if drop {
			// Drop the session mid-run: the command's output is already on
			// the wire (channel close is ordered), but no exit-status ever
			// follows — what the executor sees is "the command died before
			// reporting", the signal Run's redial exists for.
			_ = ch.Close()
			return
		}
		_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(code)}))
		return
	}
}

func (s *fakeServer) setCloseOnCmd(substr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeOnCmd = substr
}

func (s *fakeServer) setFailConns(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failConns = n
}

func (s *fakeServer) sawCommands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...)
}

func (s *fakeServer) connectionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns
}

// executorFor dials the server with a throwaway client key — the same
// key-file auth path the real deployment uses.
func (s *fakeServer) executor(t *testing.T) *Executor {
	t.Helper()
	keyPath := filepath.Join(t.TempDir(), "id_ed25519")
	der, err := x509.MarshalPKCS8PrivateKey(testClientPrivKey(t))
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if err := os.WriteFile(keyPath, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := Dial(2*time.Second, Config{
		Host:    "127.0.0.1",
		Port:    s.ln.Addr().(*net.TCPAddr).Port,
		User:    "root",
		KeyPath: keyPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

// The executor must talk to a real server end to end: streams land on the
// right side, and the remote exit status comes back as an exit code.
func TestRunStreamsOnceFakeServer(t *testing.T) {
	srv := newFakeServer(t, func(cmd string, stdout, stderr io.Writer) int {
		fmt.Fprint(stdout, "OUT:"+cmd)
		fmt.Fprint(stderr, "ERR:"+cmd)
		return 3
	})
	e := srv.executor(t)

	var out, errb bytes.Buffer
	code, err := e.RunStreamsOnce(context.Background(), "probe", 5*time.Second, &out, &errb)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 3 {
		t.Fatalf("exit = %d, want 3", code)
	}
	if got := out.String(); got != "OUT:probe" {
		t.Fatalf("stdout = %q, want OUT:probe", got)
	}
	if got := errb.String(); got != "ERR:probe" {
		t.Fatalf("stderr = %q, want ERR:probe", got)
	}
}

// Run re-dials after the connection dies mid-command: the retry must be a
// fresh dial (two TCP connections, counted at the server), and the returned
// output must be the clean result of the second attempt — never the first
// attempt's partial output spliced back in.
func TestRunRedialsAfterMidRunDrop(t *testing.T) {
	srv := newFakeServer(t, func(cmd string, stdout, stderr io.Writer) int {
		fmt.Fprint(stdout, "ran-once")
		return 0
	})
	e := srv.executor(t)
	srv.setCloseOnCmd("die")

	got, code, err := e.Run(context.Background(), "die; live", 5*time.Second)
	if err != nil || code != 0 {
		t.Fatalf("run: code=%d err=%v", code, err)
	}
	if got != "ran-once" {
		t.Fatalf("output = %q, want the retry's clean result", got)
	}
	var ran int
	for _, c := range srv.sawCommands() {
		if strings.Contains(c, "die; live") {
			ran++
		}
	}
	if ran != 2 {
		t.Fatalf("command ran %d times across reconnects, want 2", ran)
	}
	if got := srv.connectionCount(); got != 2 {
		t.Fatalf("connection count = %d, want 2 (the retry must be a fresh dial)", got)
	}
}

// When the re-dial itself fails (node refusing new connections), Run gives up
// with the original error and the executor disconnects — later runs fail
// fast instead of silently running nowhere.
func TestRunRedialFailureDisconnectsExecutor(t *testing.T) {
	srv := newFakeServer(t, func(cmd string, stdout, stderr io.Writer) int {
		fmt.Fprint(stdout, "ran-once")
		return 0
	})
	e := srv.executor(t)
	srv.setCloseOnCmd("die")
	srv.setFailConns(1) // the redial's TCP connect lands, then the server drops it

	_, _, err := e.Run(context.Background(), "die", 5*time.Second)
	if err == nil {
		t.Fatal("a failed redial must surface the original error")
	}
	if got := srv.connectionCount(); got != 2 {
		t.Fatalf("connection count = %d, want 2 (initial dial + refused redial)", got)
	}
	if c := e.current(); c != nil {
		t.Fatal("a failed redial must leave the executor disconnected")
	}
	var out bytes.Buffer
	if _, err := e.RunStreamsOnce(context.Background(), "x", time.Second, &out, &out); err == nil || !strings.Contains(err.Error(), "ssh not connected") {
		t.Fatalf("post-failure run = %v, want the not-connected error", err)
	}
}

// redial only replaces the client the failed attempt ran on: a caller that
// re-dials elsewhere (a different attempt's failure) must not have its fresh,
// healthy client torn down by a third caller still reporting a stale one.
func TestRedialKeepsForeignClient(t *testing.T) {
	srv := newFakeServer(t, func(cmd string, stdout, stderr io.Writer) int {
		return 0
	})
	e := srv.executor(t)
	stale := e.current()

	if !e.redial(stale) {
		t.Fatal("redial after a real failure must succeed")
	}
	fresh := e.current()
	if fresh == stale || fresh == nil {
		t.Fatal("redial must install a new client")
	}

	// A third attempt still ran on the stale client; asking to redial it must
	// keep the current (foreign, healthy) client and must not dial again.
	if !e.redial(stale) {
		t.Fatal("a foreign re-dial counts as recovered")
	}
	if e.current() != fresh {
		t.Fatal("redialing a stale client must not replace the current client")
	}
	if got := srv.connectionCount(); got != 2 {
		t.Fatalf("connection count = %d, want 2 (the stale redial must not dial)", got)
	}
}

// RunStreamsOnce is the no-retry path a one-shot user command rides on: the
// mid-run drop surfaces as an error, and no second execution happens.
func TestRunStreamsOnceDoesNotRetry(t *testing.T) {
	srv := newFakeServer(t, func(cmd string, stdout, stderr io.Writer) int {
		fmt.Fprint(stdout, "partial")
		return 0
	})
	e := srv.executor(t)
	srv.setCloseOnCmd("once")

	var out, errb bytes.Buffer
	if _, err := e.RunStreamsOnce(context.Background(), "once", 5*time.Second, &out, &errb); err == nil {
		t.Fatal("a mid-run drop must fail the one-shot run")
	}
	if got := out.String(); got != "partial" {
		t.Fatalf("output = %q, want the partial bytes preserved", got)
	}
	var ran int
	for _, c := range srv.sawCommands() {
		if strings.Contains(c, "once") {
			ran++
		}
	}
	if ran != 1 {
		t.Fatalf("one-shot ran %d times, want exactly 1", ran)
	}
}

// The per-call timeout must tear the session down from the client side: a
// remote command that never returns releases the caller with an error.
func TestRunStreamsOnceTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv := newFakeServer(t, func(cmd string, stdout, stderr io.Writer) int {
		<-release
		return 0
	})
	e := srv.executor(t)

	var out, errb bytes.Buffer
	start := time.Now()
	_, err := e.RunStreamsOnce(context.Background(), "hang", 150*time.Millisecond, &out, &errb)
	if err == nil {
		t.Fatal("a hung command must hit the timeout error")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("timeout fired after %s, want near 150ms", d)
	}
}

// A canceled ctx must kill the in-flight command (the HTTP handler's request
// context rides down to here) and surface as a cancelation error — and a
// later call on the same executor still works, so the shared client must
// have survived the canceled session. The claim is client-side: the
// server-side handler keeps blocking until test cleanup, so remote-process
// termination itself is out of scope here (x/crypto/ssh has no exit report
// for a session the client tore down).
func TestRunStreamsOnceContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	defer close(release)
	srv := newFakeServer(t, func(cmd string, stdout, stderr io.Writer) int {
		if strings.Contains(cmd, "long") {
			<-release // only the canceled command hangs; later ones finish
		}
		return 0
	})
	e := srv.executor(t)

	var out, errb bytes.Buffer
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	_, err := e.RunStreamsOnce(ctx, "long", 30*time.Second, &out, &errb)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}

	// The shared client survives: the next command on the same executor runs.
	code, err := e.RunStreamsOnce(context.Background(), "after", 5*time.Second, &out, &errb)
	if err != nil || code != 0 {
		t.Fatalf("post-cancel run: code=%d err=%v", code, err)
	}
}

// An executor that was never dialed (or whose dial failed) must fail its runs
// instead of panicking on a nil client.
func TestRunStreamsOnceNotConnected(t *testing.T) {
	e := &Executor{}
	if _, err := e.RunStreamsOnce(context.Background(), "x", time.Second, io.Discard, io.Discard); err == nil {
		t.Fatal("want an error with no client dialed")
	}
}
