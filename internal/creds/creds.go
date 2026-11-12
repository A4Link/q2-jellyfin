// Package creds keeps the Jellyfin sign-in (its access token, never the password) in the add-on's
// folder on the card, encrypted (C3) with a key kept in the Q2's own storage, so the card on its
// own (lost, lent, read over USB) doesn't give the sign-in away.
package creds

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/A4Link/q2-jellyfin/internal/jellyfin"
	"github.com/A4Link/q2-jellyfin/internal/q2"
)

const (
	name = "credentials"
	// magic starts an encrypted file: then a 12-byte nonce and the JSON sealed with AES-256-GCM.
	// A file without it is the plain JSON written before C3, or where no key could be kept.
	magic = "q2-jellyfin credentials v1\n"
	// label is the additional data: fixed, not the path, so the card reads the same mounted anywhere.
	label = "q2-jellyfin credentials v1"
)

// KeyPath is the key's file, in the Q2's internal storage (the firmware keeps its EQ presets in
// /data), never on the card. Tests point it elsewhere.
var KeyPath = "/data/q2-jellyfin.key"

// Logf receives why the sign-in can't be read or kept encrypted: reasons and paths, never a
// secret. The app points it at the card's log, which outlives a reboot.
var Logf = log.Printf

var (
	keyMu   sync.Mutex // one key made, even by two sign-ins at once
	logMu   sync.Mutex
	lastLog string
)

// logOnce logs each new reason once: Load runs on every request while signed out.
func logOnce(format string, a ...any) {
	s := fmt.Sprintf(format, a...)
	logMu.Lock()
	defer logMu.Unlock()
	if s != lastLog {
		lastLog = s
		Logf("%s", s)
	}
}

// Load reads the saved sign-in; false when there is none, it is incomplete, or it can't be
// decrypted (another Q2's card, /data cleared): then the user signs in again, which writes over it.
// A plain file is encrypted at once, so no new sign-in is needed after the update.
func Load(dir string) (jellyfin.Creds, bool) {
	p := filepath.Join(dir, name)
	b, err := os.ReadFile(p)
	if err != nil {
		return jellyfin.Creds{}, false
	}
	plain := !bytes.HasPrefix(b, []byte(magic))
	if !plain {
		k, err := key(false)
		if err == nil {
			b, err = open(k, b[len(magic):])
		}
		if err != nil {
			logOnce("credentials: can't decrypt %s with the key at %s (%v): signed out", p, KeyPath, err)
			return jellyfin.Creds{}, false
		}
	}
	var c jellyfin.Creds
	if json.Unmarshal(b, &c) != nil || c.Server == "" || c.User == "" {
		return jellyfin.Creds{}, false
	}
	if plain {
		if k, err := key(true); err != nil {
			logOnce("credentials: no key at %s (%v): kept unencrypted", KeyPath, err)
		} else {
			q2.Wipe(p) // the encrypted file replaces it, and FAT would keep the plain token's clusters
			if err := save(dir, c, k); err != nil {
				logOnce("credentials: can't encrypt %s: %v", p, err)
				save(dir, c, nil) // the sign-in kept plain, as it was
			}
		}
	}
	return c, true
}

// Save writes the sign-in encrypted, with mode 0600. Without a key (/data can't be written) it is
// written plain, as before C3, so signing in always works.
func Save(dir string, c jellyfin.Creds) error {
	k, err := key(true)
	if err != nil {
		logOnce("credentials: no key at %s (%v): stored unencrypted", KeyPath, err)
	}
	return save(dir, c, k)
}

// save writes c sealed with k, or plain when k is nil.
func save(dir string, c jellyfin.Creds, k []byte) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if k != nil {
		if b, err = seal(k, b); err != nil {
			return err
		}
	}
	return writePrivate(filepath.Join(dir, name), b)
}

// Remove signs out; a missing file is fine. The file is overwritten with zeros first: FAT keeps a
// deleted file's bytes, and the key to them stays in /data (audit 3 S13).
func Remove(dir string) error {
	p := filepath.Join(dir, name)
	q2.Wipe(p) // a missing file has nothing to wipe; a failed wipe still deletes
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// key reads the 32-byte key, making one first when create is set and there is none it can use (a
// damaged or unreadable one decrypts nothing anyway). A key lost to a power cut as it is made means only a new
// sign-in.
func key(create bool) ([]byte, error) {
	keyMu.Lock()
	defer keyMu.Unlock()
	k, err := os.ReadFile(KeyPath)
	switch {
	case err == nil && len(k) == 32:
		return k, nil
	case err == nil:
		err = errors.New("key file damaged")
	}
	if !create {
		return nil, err
	}
	k = make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	if err := writePrivate(KeyPath, k); err != nil {
		return nil, err
	}
	return k, nil
}

// writePrivate writes b with mode 0600. The temp file is made 0600 first, as q2.WriteFile keeps the
// mode of a file it truncates.
func writePrivate(p string, b []byte) error {
	if err := os.WriteFile(p+".tmp", nil, 0o600); err != nil {
		return err
	}
	if err := q2.WriteFile(p, b); err != nil {
		return err
	}
	return os.Chmod(p, 0o600)
}

func gcm(k []byte) (cipher.AEAD, error) {
	blk, err := aes.NewCipher(k)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(blk)
}

// seal is magic, a random nonce, and b sealed.
func seal(k, b []byte) ([]byte, error) {
	g, err := gcm(k)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(magic)+g.NonceSize(), len(magic)+g.NonceSize()+len(b)+g.Overhead())
	copy(out, magic)
	nonce := out[len(magic):]
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return g.Seal(out, nonce, b, []byte(label)), nil
}

// open undoes seal, given what follows magic.
func open(k, b []byte) ([]byte, error) {
	g, err := gcm(k)
	if err != nil {
		return nil, err
	}
	if len(b) < g.NonceSize() {
		return nil, errors.New("file too short")
	}
	return g.Open(nil, b[:g.NonceSize()], b[g.NonceSize():], []byte(label))
}
