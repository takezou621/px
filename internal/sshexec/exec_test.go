package sshexec

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"slices"
	"testing"

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
	if len(got) != 1 || !bytes.Equal(got[0].Marshal(), pub.Marshal()) {
		t.Fatalf("want exactly the pinned key, got %d keys", len(got))
	}

	// ssh-keyscan output: the hostname prefix must not break parsing.
	got, err = parseHostKeys([]byte("pve.example.com " + line))
	if err != nil {
		t.Fatalf("ssh-keyscan line rejected: %v", err)
	}
	if len(got) != 1 || !bytes.Equal(got[0].Marshal(), pub.Marshal()) {
		t.Fatalf("want exactly the pinned key, got %d keys", len(got))
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

	cb := pinnedHostKeyCallback([]ssh.PublicKey{pinned})
	if err := cb("pve", nil, pinned); err != nil {
		t.Fatalf("pinned key rejected: %v", err)
	}
	if err := cb("pve", nil, other); err == nil {
		t.Fatal("unpinned key accepted")
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
