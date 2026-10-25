package state

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/levmv/skot/internal/privatefs"
)

// This is obfuscation, not a boundary against readers of the file: its key is
// stored alongside the ciphertext. It keeps tokens out of plaintext searches
// without a password, external key store, or machine-dependent recovery.
// Format: version byte, 32-byte key, then AES-GCM's nonce and sealed JSON.
// Each write uses a new key and nonce; the version is authenticated as AAD.
const (
	credentialFileVersion = 1
	credentialKeySize     = 32
)

func credentialCipher(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithRandomNonce(block)
}

func (store *Store) saveCredentials(credentials credentialData) error {
	raw, err := json.Marshal(credentials, json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encode credentials: %w", err)
	}
	key := make([]byte, credentialKeySize)
	_, _ = rand.Read(key)
	aead, err := credentialCipher(key)
	if err != nil {
		return fmt.Errorf("initialize credential store encryption: %w", err)
	}
	data := append([]byte{credentialFileVersion}, key...)
	data = append(data, aead.Seal(nil, nil, raw, []byte{credentialFileVersion})...)
	// Only the sealed representation reaches a temporary file or the store.
	return writeStateAtomic(store.dir, store.credentialPath, "store", data)
}

func (store *Store) loadCredentials() (credentialData, error) {
	data, err := os.ReadFile(store.credentialPath)
	if errors.Is(err, os.ErrNotExist) {
		return credentialData{
			Profiles: make(map[string]CredentialProfile),
			Defaults: make(map[string]string),
		}, nil
	}
	if err != nil {
		return credentialData{}, fmt.Errorf("read credential store: %w", err)
	}
	if len(data) < 1+credentialKeySize {
		return credentialData{}, errors.New("credential store is truncated")
	}
	if data[0] != credentialFileVersion {
		return credentialData{}, fmt.Errorf("unsupported credential store version %d", data[0])
	}
	aead, err := credentialCipher(data[1 : 1+credentialKeySize])
	if err != nil {
		return credentialData{}, fmt.Errorf("initialize credential store decryption: %w", err)
	}
	raw, err := aead.Open(nil, nil, data[1+credentialKeySize:], data[:1])
	if err != nil {
		return credentialData{}, errors.New("credential store is corrupt or incomplete")
	}
	return decodeCredentials(raw)
}

func decodeCredentials(raw []byte) (credentialData, error) {
	var credentials credentialData
	if err := json.Unmarshal(raw, &credentials); err != nil {
		return credentialData{}, fmt.Errorf("decode credentials: %w", err)
	}
	if credentials.Profiles == nil {
		credentials.Profiles = make(map[string]CredentialProfile)
	}
	if credentials.Defaults == nil {
		credentials.Defaults = make(map[string]string)
	}
	return credentials, nil
}

func (store *Store) migrateCredentials() error {
	legacyPath := filepath.Join(store.dir, "auth.json")
	if _, err := os.Lstat(legacyPath); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect legacy credential store: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	lock, err := acquireStateLock(ctx, filepath.Join(store.dir, "store.lock"), "credential store")
	if err != nil {
		return err
	}
	defer releaseStateLock(lock)
	if err := privatefs.InspectRegularFile(legacyPath, "legacy credential store"); err != nil {
		return err
	}
	if _, err := os.Stat(store.credentialPath); errors.Is(err, os.ErrNotExist) {
		raw, err := os.ReadFile(legacyPath)
		if err != nil {
			return fmt.Errorf("read legacy credential store: %w", err)
		}
		credentials, err := decodeCredentials(raw)
		if err != nil {
			return err
		}
		if err := store.saveCredentials(credentials); err != nil {
			return err
		}
	} else if err != nil {
		return fmt.Errorf("inspect credential store: %w", err)
	} else if _, err := store.loadCredentials(); err != nil {
		return err
	}
	// A valid store.bin is authoritative if a previous migration stopped after
	// publishing it. Never replace it with stale tokens from the legacy file.
	// Another process may have completed migration before we acquired the lock.
	if err := os.Remove(legacyPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove legacy credential store: %w", err)
	}
	return syncDirectory(store.dir)
}
