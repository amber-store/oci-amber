package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/amber-store/oci-amber/blob"
	"github.com/amber-store/oci-amber/image"
	"github.com/amber-store/oci-amber/store"
)

// checkStoreExists fails when dir holds no store: the read-only commands
// must never create one.
func checkStoreExists(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, store.ConfigFile)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("no oci-amber store at %s", dir)
		}
		return fmt.Errorf("checking store %s: %w", dir, err)
	}
	return nil
}

// readOnlyStore is a store opened by a command that never writes: browse,
// ls and save. The blob store is read-only and the work directory is not
// touched.
type readOnlyStore struct {
	st     *store.Store
	blobs  *blob.Store
	images *image.Store
}

// openReadOnly opens an existing store without touching its work
// directory. A store that serve (or another command) has open is reported
// as in use.
func openReadOnly(dir string, log *slog.Logger) (*readOnlyStore, error) {
	if err := checkStoreExists(dir); err != nil {
		return nil, err
	}
	st, err := store.Open(dir, store.Options{Logger: log})
	if errors.Is(err, store.ErrInUse) {
		return nil, fmt.Errorf("store %s is in use by another process", dir)
	}
	if err != nil {
		return nil, fmt.Errorf("opening store %s: %w", dir, err)
	}
	blobs := blob.NewReadOnly(st, log)
	return &readOnlyStore{st: st, blobs: blobs, images: image.New(st, blobs, log)}, nil
}

// Close closes the store.
func (r *readOnlyStore) Close() error {
	if err := r.st.Close(); err != nil {
		return fmt.Errorf("closing store: %w", err)
	}
	return nil
}
