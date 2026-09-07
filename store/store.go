// Package store embeds an amber-store-core store: a packstore for objects, a
// refstore for named roots and the mark-and-sweep collector that guards
// reference publication. It pins the store parameters oci-amber depends on
// (chunk sizes, segment size), records them in the store directory on first
// open and refuses to open a store that was created with different ones.
package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/amber-store/core/chunkers"
	"github.com/amber-store/core/gc"
	"github.com/amber-store/core/key"
	"github.com/amber-store/core/packstore"
	"github.com/amber-store/core/refstore"
)

// ConfigFile is the name of the parameter record inside the store directory.
const ConfigFile = "oci-amber.json"

// ItemBits is the item-chunker average run (2^ItemBits items) used for every
// file index and directory node, amber's own default.
const ItemBits = 7

// Modes of the directory entries oci-amber writes. UID, GID and Mtime are
// always zero, so identical content always yields identical keys.
const (
	ModeFile uint64 = 0o100644
	ModeDir  uint64 = 0o040755
)

// ErrNotFound reports an absent object, directory entry or reference.
var ErrNotFound = errors.New("store: not found")

// ErrInUse reports a store directory that another process has open. The
// packstore takes an exclusive flock on its directory, so a second Open
// (say `oci-amber browse` while `serve` runs) fails with EWOULDBLOCK.
var ErrInUse = errors.New("store: in use by another process")

// refStripes is the number of mutexes reference names are hashed onto.
// Publication is serialized per name because refstore.Put is an
// unconditional overwrite; striping bounds the lock table.
const refStripes = 64

// Chunking is the content-defined byte chunker configuration.
type Chunking struct {
	MinSize    int `json:"minSize"`
	NormalSize int `json:"normalSize"`
	MaxSize    int `json:"maxSize"`
}

// ByteOpts returns the chunker options to hand to chunkers.SplitBytes.
func (c Chunking) ByteOpts() *chunkers.ByteOpts {
	return &chunkers.ByteOpts{MinSize: c.MinSize, NormalSize: c.NormalSize, MaxSize: c.MaxSize}
}

// Config is the parameter record stored in ConfigFile. Every field is fixed
// for the lifetime of a store.
type Config struct {
	Version     int      `json:"version"`
	Chunking    Chunking `json:"chunking"`
	SegmentSize int64    `json:"segmentSize"`
}

// DefaultConfig returns the parameters this binary is compiled for: min
// 32 KiB, normal 512 KiB, max 1 MiB chunks and 2 GiB pack segments.
func DefaultConfig() Config {
	return Config{
		Version:     1,
		Chunking:    Chunking{MinSize: 32768, NormalSize: 524288, MaxSize: 1048576},
		SegmentSize: 2147483648,
	}
}

// Options configure Open.
type Options struct {
	// GCInterval starts amber's collector in the background with a cycle per
	// interval; zero disables background cycles.
	GCInterval time.Duration
	// Logger receives the store's log lines; nil means slog.Default().
	Logger *slog.Logger
}

// Store is an open amber store. Objects, Refs and GC are the embedded amber
// components; the rest of the package builds on them.
type Store struct {
	Objects *packstore.Store
	Refs    *refstore.Store
	GC      *gc.Collector

	cfg Config
	log *slog.Logger

	// refLocks serialize reference publication per name (see refs.go).
	refLocks [refStripes]sync.Mutex

	closeOnce sync.Once
	closeErr  error
}

// Open opens the store rooted at dir, creating it when absent. The layout is
// <dir>/packstore, <dir>/refs, <dir>/gc and <dir>/oci-amber.json. On the
// first open the parameter record is written with DefaultConfig; afterwards
// a record that differs from DefaultConfig refuses the open, because changing
// chunk boundaries silently defeats deduplication against existing content.
func Open(dir string, opts Options) (*Store, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("store: creating %s: %w", dir, err)
	}
	cfg, err := loadOrWriteConfig(filepath.Join(dir, ConfigFile))
	if err != nil {
		return nil, err
	}
	objects, err := packstore.Open(filepath.Join(dir, "packstore"),
		packstore.WithSync(true), packstore.WithSegmentSize(cfg.SegmentSize))
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("%w: %s", ErrInUse, dir)
		}
		return nil, fmt.Errorf("store: %w", err)
	}
	refs, err := refstore.Open(filepath.Join(dir, "refs"), true)
	if err != nil {
		objects.Close()
		return nil, fmt.Errorf("store: %w", err)
	}
	coll, err := gc.Open(filepath.Join(dir, "gc"), objects, refs, gc.Options{Interval: opts.GCInterval})
	if err != nil {
		refs.Close()
		objects.Close()
		return nil, fmt.Errorf("store: %w", err)
	}
	log.Info("store opened", "dir", dir, "gc_interval", opts.GCInterval)
	return &Store{Objects: objects, Refs: refs, GC: coll, cfg: cfg, log: log}, nil
}

// loadOrWriteConfig reads the parameter record at path, writing DefaultConfig
// when it does not exist yet. A record that cannot be parsed or that differs
// from DefaultConfig is an error mentioning "store parameters"; the file is
// left exactly as found.
func loadOrWriteConfig(path string) (Config, error) {
	want := DefaultConfig()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := writeConfig(path, want); err != nil {
			return Config{}, err
		}
		return want, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("store: reading store parameters %s: %w", path, err)
	}
	var got Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		return Config{}, fmt.Errorf("store: parsing store parameters %s: %w", path, err)
	}
	if got != want {
		return Config{}, fmt.Errorf("store: %s records store parameters %s but this binary requires %s; refusing to open",
			path, encodeConfig(got), encodeConfig(want))
	}
	return want, nil
}

// encodeConfig is the compact JSON form of cfg (no trailing newline).
func encodeConfig(cfg Config) []byte {
	data, err := json.Marshal(cfg)
	if err != nil {
		panic(fmt.Sprintf("store: encoding config: %v", err)) // ints only; cannot fail
	}
	return data
}

// writeConfig writes cfg to path durably: temp file, fsync, rename. The
// on-disk form is one compact JSON object followed by a newline.
func writeConfig(path string, cfg Config) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("store: writing store parameters: %w", err)
	}
	if _, err := f.Write(append(encodeConfig(cfg), '\n')); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("store: writing store parameters: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("store: writing store parameters: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("store: writing store parameters: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("store: writing store parameters: %w", err)
	}
	return nil
}

// Close stops the collector, then closes the refs DB and the packstore,
// returning every error joined. Closing an already closed store is a no-op
// that returns the first Close's result (pebble panics on a double close).
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = errors.Join(s.GC.Close(), s.Refs.Close(), s.Objects.Close())
	})
	return s.closeErr
}

// Config returns the store parameters, always equal to DefaultConfig.
func (s *Store) Config() Config {
	return s.cfg
}

// Get returns the bytes stored under k, or ErrNotFound.
func (s *Store) Get(k key.Key) ([]byte, error) {
	data, err := s.Objects.Get(k)
	if errors.Is(err, packstore.ErrNotFound) {
		return nil, fmt.Errorf("%w: object %s", ErrNotFound, k)
	}
	if err != nil {
		return nil, fmt.Errorf("store: reading %s: %w", k, err)
	}
	return data, nil
}

// Has reports whether an object is stored under k.
func (s *Store) Has(k key.Key) (bool, error) {
	return s.Objects.Has(k)
}
