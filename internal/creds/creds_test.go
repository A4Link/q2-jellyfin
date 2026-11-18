package creds

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/q2"
)

// useKey points KeyPath at a temp folder for the test, never the machine's /mnt/data.
func useKey(t *testing.T) string {
	t.Helper()
	old := KeyPath
	KeyPath = filepath.Join(t.TempDir(), "q2-jellyfin.key")
	t.Cleanup(func() { KeyPath = old })
	return KeyPath
}

// The key is kept where the Q2 can write, beside stock's settings: the Q2 has no /data, and
// a key that can't be made leaves the sign-in plain on the card.
func TestKeyPathInQ2Storage(t *testing.T) {
	if filepath.Dir(KeyPath) != filepath.Dir(q2.ConfigPath) {
		t.Fatalf("KeyPath %s not in %s", KeyPath, filepath.Dir(q2.ConfigPath))
	}
}

func TestCredsMode0600(t *testing.T) {
	useKey(t)
	dir := t.TempDir()
	want := jellyfin.Creds{Server: "https://nd.example", User: "a", Password: "p w"}
	if err := Save(dir, want); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "credentials"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("stat %v mode %v", err, fi.Mode())
	}
	if got, ok := Load(dir); !ok || got != want {
		t.Fatalf("load %v %v", got, ok)
	}
	if err := Remove(dir); err != nil {
		t.Fatal(err)
	}
	if _, ok := Load(dir); ok {
		t.Fatal("still loads after Remove")
	}
	if err := Remove(dir); err != nil {
		t.Fatalf("Remove of missing file: %v", err)
	}
}

// I23: an API-key sign-in keeps only the key, never a password field.
func TestCredsTokenOnly(t *testing.T) {
	useKey(t)
	dir := t.TempDir()
	want := jellyfin.Creds{Server: "https://nd.example", User: "a", Token: "k"}
	b, _ := json.Marshal(want)
	if strings.Contains(string(b), "Password") {
		t.Fatalf("json %s", b)
	}
	if err := Save(dir, want); err != nil {
		t.Fatal(err)
	}
	if got, ok := Load(dir); !ok || got != want {
		t.Fatalf("load %v %v", got, ok)
	}
}

// C3: the card holds only ciphertext; the key is made once, 0600, outside the card.
func TestCredsEncrypted(t *testing.T) {
	kp := useKey(t)
	dir := t.TempDir()
	c := jellyfin.Creds{Server: "https://music.example", User: "alice", Token: "tok-secret-123", UserID: "u1"}
	if err := Save(dir, c); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "credentials"))
	if !bytes.HasPrefix(b, []byte(magic)) {
		t.Fatalf("no header: %q", b)
	}
	for _, s := range []string{"music.example", "alice", "tok-secret-123", "u1", "Token"} {
		if bytes.Contains(b[len(magic):], []byte(s)) {
			t.Errorf("%q readable on the card", s)
		}
	}
	fi, err := os.Stat(kp)
	if err != nil || fi.Mode().Perm() != 0o600 || fi.Size() != 32 {
		t.Fatalf("key %v %v", err, fi)
	}
	k1, _ := os.ReadFile(kp)
	if err := Save(dir, c); err != nil {
		t.Fatal(err)
	}
	if k2, _ := os.ReadFile(kp); !bytes.Equal(k1, k2) {
		t.Fatal("key made again")
	}
	if b2, _ := os.ReadFile(filepath.Join(dir, "credentials")); bytes.Equal(b, b2) {
		t.Fatal("nonce reused")
	}
	if got, ok := Load(dir); !ok || got != c {
		t.Fatalf("load %v %v", got, ok)
	}
}

// A plain file from before C3 (or kept plain while no key could be made) still signs in, and is
// encrypted at once.
func TestCredsPlainMigrated(t *testing.T) {
	useKey(t)
	dir := t.TempDir()
	c := jellyfin.Creds{Server: "https://music.example", User: "alice", Token: "tok-secret-123"}
	b, _ := json.Marshal(c)
	os.WriteFile(filepath.Join(dir, "credentials"), b, 0o644)
	if got, ok := Load(dir); !ok || got != c {
		t.Fatalf("load %v %v", got, ok)
	}
	b, _ = os.ReadFile(filepath.Join(dir, "credentials"))
	if !bytes.HasPrefix(b, []byte(magic)) || bytes.Contains(b, []byte("tok-secret-123")) {
		t.Fatalf("not encrypted: %q", b)
	}
	if got, ok := Load(dir); !ok || got != c {
		t.Fatalf("load after migration %v %v", got, ok)
	}
}

// Encrypting a plain file writes the sealed bytes over the plain ones, in the same file: a second
// name for it (a hard link) sees the sealed copy, as the card's clusters would, and no other file
// is made.
func TestCredsPlainWipedWhenEncrypted(t *testing.T) {
	useKey(t)
	dir := t.TempDir()
	c := jellyfin.Creds{Server: "https://music.example", User: "alice", Token: "tok-secret-123"}
	b, _ := json.Marshal(c)
	p, other := filepath.Join(dir, "credentials"), filepath.Join(dir, "other-name")
	os.WriteFile(p, b, 0o644)
	if err := os.Link(p, other); err != nil {
		t.Skip("no hard links here:", err)
	}
	if got, ok := Load(dir); !ok || got != c {
		t.Fatalf("load %v %v", got, ok)
	}
	sealed, _ := os.ReadFile(p)
	if !bytes.HasPrefix(sealed, []byte(magic)) {
		t.Fatalf("not encrypted: %q", sealed)
	}
	if b, _ := os.ReadFile(other); !bytes.Equal(b, sealed) || bytes.Contains(b, []byte("tok-secret-123")) {
		t.Fatalf("plain bytes not overwritten: %q", b)
	}
	if es, _ := os.ReadDir(dir); len(es) != 2 {
		t.Fatalf("other files made: %v", es)
	}
}

// A full card takes no new file, but the plain file is still encrypted where it is (a folder that
// can't be written stands in for the full card).
func TestCredsPlainEncryptedOnFullCard(t *testing.T) {
	useKey(t)
	dir := t.TempDir()
	c := jellyfin.Creds{Server: "https://music.example", User: "alice", Token: "tok-secret-123"}
	b, _ := json.Marshal(c)
	p := filepath.Join(dir, "credentials")
	os.WriteFile(p, b, 0o644)
	os.Chmod(dir, 0o500)
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if f, err := os.Create(filepath.Join(dir, "probe")); err == nil {
		f.Close()
		t.Skip("the folder still takes new files here (root, or no permissions)")
	}
	if got, ok := Load(dir); !ok || got != c {
		t.Fatalf("load %v %v", got, ok)
	}
	if b, _ := os.ReadFile(p); !bytes.HasPrefix(b, []byte(magic)) {
		t.Fatalf("not encrypted: %q", b)
	}
	if got, ok := Load(dir); !ok || got != c {
		t.Fatalf("load after migration %v %v", got, ok)
	}
}

// When the file can't be written, the sign-in stays plain and readable, and the reason is logged.
func TestCredsPlainKeptWhenUnwritable(t *testing.T) {
	useKey(t)
	var got []string
	old := Logf
	Logf = func(format string, a ...any) { got = append(got, fmt.Sprintf(format, a...)) }
	t.Cleanup(func() { Logf = old })
	dir := t.TempDir()
	c := jellyfin.Creds{Server: "https://music.example", User: "alice", Token: "tok-secret-123"}
	b, _ := json.Marshal(c)
	p := filepath.Join(dir, "credentials")
	os.WriteFile(p, b, 0o400)
	if f, err := os.OpenFile(p, os.O_WRONLY, 0); err == nil {
		f.Close()
		t.Skip("a read-only file can still be written here (root)")
	}
	for i := 0; i < 2; i++ {
		if l, ok := Load(dir); !ok || l != c {
			t.Fatalf("load %v %v", l, ok)
		}
	}
	if b2, _ := os.ReadFile(p); !bytes.Equal(b2, b) {
		t.Fatalf("plain file changed: %q", b2)
	}
	if len(got) != 1 || !strings.Contains(got[0], p+" kept unencrypted") || strings.Contains(got[0], "tok-secret-123") {
		t.Fatalf("%q", got)
	}
}

// Another Q2's key, no key, or a damaged file: signed out, nothing deleted, no panic.
func TestCredsUndecryptable(t *testing.T) {
	kp := useKey(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "credentials")
	if err := Save(dir, jellyfin.Creds{Server: "https://music.example", User: "alice", Token: "t"}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(kp, bytes.Repeat([]byte{7}, 32), 0o600) // another Q2's
	if _, ok := Load(dir); ok {
		t.Fatal("loaded with the wrong key")
	}
	os.Remove(kp)
	if _, ok := Load(dir); ok {
		t.Fatal("loaded with no key")
	}
	if _, err := os.Stat(kp); err == nil {
		t.Fatal("Load made a key")
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("file deleted: %v", err)
	}
	for _, b := range []string{magic, magic + "short", magic + strings.Repeat("x", 64)} {
		os.WriteFile(p, []byte(b), 0o600)
		if _, ok := Load(dir); ok {
			t.Fatalf("loaded %q", b)
		}
	}
	// The next sign-in writes over it with a new key.
	c := jellyfin.Creds{Server: "https://music.example", User: "alice", Token: "t2"}
	if err := Save(dir, c); err != nil {
		t.Fatal(err)
	}
	if got, ok := Load(dir); !ok || got != c {
		t.Fatalf("load %v %v", got, ok)
	}
}

// Why the sign-in can't be decrypted reaches Logf once, with the paths and no secret.
func TestCredsUndecryptableLogged(t *testing.T) {
	kp := useKey(t)
	var got []string
	old := Logf
	Logf = func(format string, a ...any) { got = append(got, fmt.Sprintf(format, a...)) }
	t.Cleanup(func() { Logf = old })
	dir := t.TempDir()
	if err := Save(dir, jellyfin.Creds{Server: "https://music.example", User: "alice", Token: "tok-secret-123"}); err != nil {
		t.Fatal(err)
	}
	k, _ := os.ReadFile(kp)
	os.WriteFile(kp, bytes.Repeat([]byte{7}, 32), 0o600) // another Q2's
	Load(dir)
	Load(dir)
	if len(got) != 1 || !strings.Contains(got[0], "can't decrypt "+filepath.Join(dir, "credentials")) || !strings.Contains(got[0], kp) {
		t.Fatalf("%q", got)
	}
	if strings.Contains(got[0], "tok-secret-123") || strings.Contains(got[0], string(k)) {
		t.Fatalf("secret logged: %q", got[0])
	}
}

// Where no key can be kept (/mnt/data not writable), sign-in still works, stored plain as before
// C3.
func TestCredsNoKeyPlain(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, nil, 0o644)
	old := KeyPath
	KeyPath = filepath.Join(blocker, "q2-jellyfin.key") // under a file: can't be made, even as root
	t.Cleanup(func() { KeyPath = old })
	dir := t.TempDir()
	c := jellyfin.Creds{Server: "https://music.example", User: "alice", Token: "t"}
	if err := Save(dir, c); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "credentials")); bytes.HasPrefix(b, []byte(magic)) || !json.Valid(b) {
		t.Fatalf("%q", b)
	}
	for i := 0; i < 2; i++ {
		if got, ok := Load(dir); !ok || got != c {
			t.Fatalf("load %v %v", got, ok)
		}
	}
}

// Sign Out zeroes the file before deleting it: a second name for the same file (a hard link) sees
// only zeros afterwards, as the card's freed clusters would.
func TestRemoveWipesFirst(t *testing.T) {
	useKey(t)
	dir := t.TempDir()
	if err := Save(dir, jellyfin.Creds{Server: "https://music.example", User: "alice", Token: "tok-secret-123"}); err != nil {
		t.Fatal(err)
	}
	p, other := filepath.Join(dir, "credentials"), filepath.Join(dir, "other-name")
	if err := os.Link(p, other); err != nil {
		t.Skip("no hard links here:", err)
	}
	if err := Remove(dir); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(other)
	if len(b) == 0 || len(bytes.Trim(b, "\x00")) > 0 {
		t.Fatalf("not wiped: %q", b)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("not removed: %v", err)
	}
	if err := Remove(dir); err != nil { // signed out already: fine
		t.Fatal(err)
	}
}
