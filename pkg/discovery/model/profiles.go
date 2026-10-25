package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
)

// ProfileDataPath assigns legacy state to its first cluster and retains the
// original file. Later cluster changes select independent state and identity.
func ProfileDataPath(legacy, cluster string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(legacy), 0700); err != nil {
		return "", err
	}
	marker := legacy + ".profile-owner"
	f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		_, err = f.WriteString(cluster)
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
	} else if errors.Is(err, os.ErrExist) {
		err = nil
	}
	if err != nil {
		return "", err
	}
	owner, err := os.ReadFile(marker)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(cluster))
	dir := filepath.Join(filepath.Dir(legacy), "profiles", hex.EncodeToString(sum[:]))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	target := filepath.Join(dir, filepath.Base(legacy))
	_, migratedErr := os.Stat(target + ".migrated")
	if string(owner) == cluster && errors.Is(migratedErr, os.ErrNotExist) {
		if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
			data, err := os.ReadFile(legacy)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return "", err
			}
			if err == nil {
				if err := os.WriteFile(target, data, 0600); err != nil {
					return "", err
				}
			}
		}
		if _, err := os.Stat(target + ".wal"); errors.Is(err, os.ErrNotExist) {
			data, err := os.ReadFile(legacy + ".wal")
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return "", err
			}
			if err == nil {
				if err = os.WriteFile(target+".wal", data, 0600); err != nil {
					return "", err
				}
			}
		}

	}
	if err := os.WriteFile(target+".migrated", []byte("done"), 0600); err != nil {
		return "", err
	}
	return target, nil
}
