package store

import (
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"

	"github.com/amber-store/core/key"
	"github.com/amber-store/core/reference"
	"github.com/amber-store/core/refstore"
)

// RefUser is the creator recorded in every reference oci-amber publishes.
const RefUser = "oci-amber"

// Ref is one published reference: a name pointing at a root key.
type Ref struct {
	Name      string
	Key       key.Key
	CreatedAt time.Time
}

// refLock returns the stripe mutex for name.
func (s *Store) refLock(name string) *sync.Mutex {
	h := fnv.New32a()
	h.Write([]byte(name))
	return &s.refLocks[h.Sum32()%refStripes]
}

// Publish points name at root, overwriting any previous target (last writer
// wins). The name must satisfy reference.ValidateName ('/' and ':' are
// allowed, '@' is not). Every object reachable from root must already be in
// the store: the collector's PrepareRef walks the tree and a missing object
// fails the publish with a *fstree.MissingObjectError (or the wrapped
// packstore read error) in its chain; nothing is published in that case.
func (s *Store) Publish(name string, root key.Key) error {
	if err := reference.ValidateName(name); err != nil {
		return fmt.Errorf("store: publishing %q: %w", name, err)
	}
	rec := reference.Reference{
		Name:      name,
		Key:       root[:],
		User:      RefUser,
		CreatedAt: time.Now().UnixNano(),
	}
	raw, err := rec.Encode()
	if err != nil {
		return fmt.Errorf("store: publishing %q: %w", name, err)
	}
	mu := s.refLock(name)
	mu.Lock()
	defer mu.Unlock()
	commit, abort, err := s.GC.PrepareRef(root)
	if err != nil {
		return fmt.Errorf("store: publishing %q: %w", name, err)
	}
	if err := s.Refs.Put(name, raw); err != nil {
		abort()
		return fmt.Errorf("store: publishing %q: %w", name, err)
	}
	commit()
	return nil
}

// Resolve returns the root key name points at, or ErrNotFound.
func (s *Store) Resolve(name string) (key.Key, error) {
	raw, err := s.Refs.Get(name)
	if errors.Is(err, refstore.ErrNotFound) {
		return key.Key{}, fmt.Errorf("%w: reference %q", ErrNotFound, name)
	}
	if err != nil {
		return key.Key{}, fmt.Errorf("store: resolving %q: %w", name, err)
	}
	ref, err := decodeRef(name, raw)
	if err != nil {
		return key.Key{}, err
	}
	return ref.Key, nil
}

// DeleteRef removes name, or returns ErrNotFound when it is absent. Objects
// only reachable from the old root become garbage for the collector.
func (s *Store) DeleteRef(name string) error {
	mu := s.refLock(name)
	mu.Lock()
	defer mu.Unlock()
	err := s.Refs.Delete(name)
	if errors.Is(err, refstore.ErrNotFound) {
		return fmt.Errorf("%w: reference %q", ErrNotFound, name)
	}
	if err != nil {
		return fmt.Errorf("store: deleting %q: %w", name, err)
	}
	return nil
}

// ListRefs returns every reference whose name starts with prefix, in
// lexicographic name order (the refstore iterates its keys bytewise). An
// empty prefix lists everything; no match yields an empty, non-nil slice.
func (s *Store) ListRefs(prefix string) ([]Ref, error) {
	recs, err := s.Refs.All()
	if err != nil {
		return nil, fmt.Errorf("store: listing references: %w", err)
	}
	refs := make([]Ref, 0, len(recs))
	for _, r := range recs {
		if !strings.HasPrefix(r.Name, prefix) {
			continue
		}
		ref, err := decodeRef(r.Name, r.Data)
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// decodeRef parses a stored reference record.
func decodeRef(name string, raw []byte) (Ref, error) {
	rec, err := reference.Decode(raw)
	if err != nil {
		return Ref{}, fmt.Errorf("store: reference %q: %w", name, err)
	}
	k, err := key.Parse(rec.Key)
	if err != nil {
		return Ref{}, fmt.Errorf("store: reference %q: %w", name, err)
	}
	return Ref{Name: name, Key: k, CreatedAt: time.Unix(0, rec.CreatedAt).UTC()}, nil
}
