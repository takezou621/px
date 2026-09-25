package sshexec

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
