package store_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jobs-build/amber-store-core/fstree"
	"github.com/jobs-build/amber-store-core/key"
	"github.com/jobs-build/amber-store-core/packstore"
	"github.com/jobs-build/amber-store-core/reference"

	"github.com/amber-store/oci-amber/store"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestPublishResolveDeleteRoundTrip(t *testing.T) {
	st, _ := openStore(t)
	root := putBlob(t, st, []byte("blob root"))
	name := "oci/blob/" + digestA

	before := time.Now().Add(-time.Second)
	if err := st.Publish(name, root); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	got, err := st.Resolve(name)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != root {
		t.Fatalf("Resolve = %s, want %s", got, root)
	}

	// The stored record carries the oci-amber user and the root key.
	raw, err := st.Refs.Get(name)
	if err != nil {
		t.Fatalf("Refs.Get: %v", err)
	}
	rec, err := reference.Decode(raw)
	if err != nil {
		t.Fatalf("reference.Decode: %v", err)
	}
	if rec.Name != name || rec.User != store.RefUser || store.RefUser != "oci-amber" {
		t.Fatalf("record = %+v, want name %q user %q", rec, name, "oci-amber")
	}
	if k, err := key.Parse(rec.Key); err != nil || k != root {
		t.Fatalf("record key = %x (%v), want %s", rec.Key, err, root)
	}
	createdAt := time.Unix(0, rec.CreatedAt)
	if createdAt.Before(before) || createdAt.After(time.Now().Add(time.Second)) {
		t.Fatalf("CreatedAt = %v, not around now", createdAt)
	}

	refs, err := st.ListRefs("")
	if err != nil {
		t.Fatalf("ListRefs: %v", err)
	}
	if len(refs) != 1 || refs[0].Name != name || refs[0].Key != root {
		t.Fatalf("ListRefs = %+v, want one entry %s -> %s", refs, name, root)
	}
	if !refs[0].CreatedAt.Equal(createdAt) {
		t.Fatalf("ListRefs CreatedAt = %v, want %v", refs[0].CreatedAt, createdAt)
	}

	if err := st.DeleteRef(name); err != nil {
		t.Fatalf("DeleteRef: %v", err)
	}
	if _, err := st.Resolve(name); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Resolve after delete = %v, want ErrNotFound", err)
	}
	refs, err = st.ListRefs("")
	if err != nil {
		t.Fatalf("ListRefs after delete: %v", err)
	}
	if len(refs) != 0 {
		t.Fatalf("ListRefs after delete = %+v, want none", refs)
	}
	// The object itself is untouched; only the reference is gone.
	if has, err := st.Has(root); err != nil || !has {
		t.Fatalf("Has(root) after DeleteRef = %v, %v; want true", has, err)
	}
}

func TestPublishTreeRoot(t *testing.T) {
	st, _ := openStore(t)
	// A two-blob FileNode: PrepareRef walks the interior node (Get) and
	// checks both leaves (Has) before the record is stored.
	a, err := fstree.EncodeBlob([]byte("chunk a"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := fstree.EncodeBlob([]byte("chunk b"))
	if err != nil {
		t.Fatal(err)
	}
	node, err := fstree.EncodeFileNode([]key.Key{a.Key, b.Key})
	if err != nil {
		t.Fatal(err)
	}
	seq := func(yield func(packstore.Object, error) bool) {
		for _, o := range []fstree.Object{a, b, node} {
			if !yield(packstore.Object{Key: o.Key, Data: o.Bytes}, nil) {
				return
			}
		}
	}
	if err := st.Objects.WriteBatch(seq); err != nil {
		t.Fatalf("WriteBatch: %v", err)
	}
	name := "oci/manifest/library/app/" + digestA
	if err := st.Publish(name, node.Key); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	got, err := st.Resolve(name)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != node.Key {
		t.Fatalf("Resolve = %s, want %s", got, node.Key)
	}
}

func TestResolveAndDeleteAbsent(t *testing.T) {
	st, _ := openStore(t)
	if _, err := st.Resolve("oci/tag/library/app:missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Resolve(absent) = %v, want ErrNotFound", err)
	}
	if err := st.DeleteRef("oci/tag/library/app:missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DeleteRef(absent) = %v, want ErrNotFound", err)
	}
}

func TestPublishOverwriteLastWriterWins(t *testing.T) {
	st, _ := openStore(t)
	first := putBlob(t, st, []byte("first"))
	second := putBlob(t, st, []byte("second"))
	name := "oci/tag/library/app:latest"

	if err := st.Publish(name, first); err != nil {
		t.Fatalf("Publish first: %v", err)
	}
	if err := st.Publish(name, second); err != nil {
		t.Fatalf("Publish second: %v", err)
	}
	got, err := st.Resolve(name)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != second {
		t.Fatalf("Resolve = %s, want the second root %s", got, second)
	}
	refs, err := st.ListRefs("oci/tag/")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("ListRefs = %d entries, want 1 (overwrite, not append)", len(refs))
	}
}

func TestListRefsPrefixAndOrder(t *testing.T) {
	st, _ := openStore(t)
	root := putBlob(t, st, []byte("shared root"))
	// Published deliberately out of order; listing must sort by name.
	names := []string{
		"oci/tag/library/app:v2",
		"oci/manifest/library/app/" + digestA,
		"oci/tag/library/app:latest",
		"oci/blob/" + digestB,
		"oci/tag/library/apple:v1",
		"oci/tag/library/app:v1",
		"oci/referrer/library/app/" + digestA + "/" + digestB,
	}
	for _, n := range names {
		if err := st.Publish(n, root); err != nil {
			t.Fatalf("Publish %q: %v", n, err)
		}
	}
	listNames := func(prefix string) []string {
		t.Helper()
		refs, err := st.ListRefs(prefix)
		if err != nil {
			t.Fatalf("ListRefs(%q): %v", prefix, err)
		}
		out := make([]string, len(refs))
		for i, r := range refs {
			if r.Key != root {
				t.Fatalf("ListRefs(%q)[%d].Key = %s, want %s", prefix, i, r.Key, root)
			}
			out[i] = r.Name
		}
		return out
	}

	// A repository's tags: prefix "oci/tag/<repo>:" excludes the sibling
	// repository "library/apple" whose name merely starts with "library/app".
	got := listNames("oci/tag/library/app:")
	want := []string{"oci/tag/library/app:latest", "oci/tag/library/app:v1", "oci/tag/library/app:v2"}
	if !slices.Equal(got, want) {
		t.Fatalf("ListRefs(tags of library/app) = %v, want %v", got, want)
	}

	got = listNames("oci/tag/")
	want = []string{"oci/tag/library/app:latest", "oci/tag/library/app:v1", "oci/tag/library/app:v2", "oci/tag/library/apple:v1"}
	if !slices.Equal(got, want) {
		t.Fatalf("ListRefs(all tags) = %v, want %v", got, want)
	}

	got = listNames("")
	sorted := slices.Clone(names)
	slices.Sort(sorted)
	if !slices.Equal(got, sorted) {
		t.Fatalf("ListRefs(\"\") = %v, want %v", got, sorted)
	}
	if !slices.IsSorted(got) {
		t.Fatalf("ListRefs(\"\") is not sorted: %v", got)
	}

	if got := listNames("oci/nothing/"); len(got) != 0 {
		t.Fatalf("ListRefs(no match) = %v, want empty", got)
	}
	refs, err := st.ListRefs("oci/nothing/")
	if err != nil || refs == nil {
		t.Fatalf("ListRefs(no match) = %v, %v; want an empty non-nil slice", refs, err)
	}
}

func TestPublishMissingRootFails(t *testing.T) {
	st, _ := openStore(t)

	// A Blob root that was never written.
	blob, err := fstree.EncodeBlob([]byte("never stored"))
	if err != nil {
		t.Fatal(err)
	}
	err = st.Publish("oci/blob/"+digestA, blob.Key)
	if err == nil {
		t.Fatal("Publish accepted a root that is not in the store")
	}
	var missing *fstree.MissingObjectError
	if !errors.As(err, &missing) || missing.Key != blob.Key {
		t.Fatalf("Publish error = %v, want a *fstree.MissingObjectError for %s", err, blob.Key)
	}
	if _, err := st.Resolve("oci/blob/" + digestA); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Resolve after failed Publish = %v, want ErrNotFound", err)
	}

	// A stored FileNode whose child blob is missing: the walk reaches the
	// leaf and reports it.
	node, err := fstree.EncodeFileNode([]key.Key{blob.Key})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Objects.Put(node.Key, node.Bytes); err != nil {
		t.Fatal(err)
	}
	err = st.Publish("oci/blob/"+digestB, node.Key)
	if !errors.As(err, &missing) || missing.Key != blob.Key {
		t.Fatalf("Publish(node with missing child) = %v, want a *fstree.MissingObjectError for %s", err, blob.Key)
	}

	// An interior node that was never written fails on the read, not the
	// leaf check: the packstore's not-found error is in the chain.
	absentNode, err := fstree.EncodeFileNode([]key.Key{node.Key})
	if err != nil {
		t.Fatal(err)
	}
	err = st.Publish("oci/blob/"+digestB, absentNode.Key)
	if !errors.Is(err, packstore.ErrNotFound) {
		t.Fatalf("Publish(absent interior node) = %v, want packstore.ErrNotFound in the chain", err)
	}

	if refs, err := st.ListRefs(""); err != nil || len(refs) != 0 {
		t.Fatalf("ListRefs after failed publishes = %v, %v; want none", refs, err)
	}
}

func TestPublishRejectsInvalidNames(t *testing.T) {
	st, _ := openStore(t)
	root := putBlob(t, st, []byte("root"))
	for _, name := range []string{"", "oci/tag/library/app@sha256:abc", "oci/tag/bad\x00name", strings.Repeat("n", reference.MaxNameLen+1)} {
		if err := st.Publish(name, root); err == nil {
			t.Fatalf("Publish(%q) succeeded, want an error", name)
		}
	}
	if refs, err := st.ListRefs(""); err != nil || len(refs) != 0 {
		t.Fatalf("ListRefs = %v, %v; want none", refs, err)
	}
}

func TestPublishConcurrent(t *testing.T) {
	st, _ := openStore(t)
	const n = 16
	roots := make([]key.Key, n)
	for i := range roots {
		roots[i] = putBlob(t, st, fmt.Appendf(nil, "root %d", i))
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2*n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := st.Publish(fmt.Sprintf("oci/tag/library/app:t%02d", i), roots[i]); err != nil {
				errs <- err
			}
			if err := st.Publish("oci/tag/library/shared:x", roots[i]); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("Publish: %v", err)
	}
	refs, err := st.ListRefs("oci/tag/")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != n+1 {
		t.Fatalf("ListRefs = %d entries, want %d", len(refs), n+1)
	}
	shared, err := st.Resolve("oci/tag/library/shared:x")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(roots, shared) {
		t.Fatalf("shared resolves to %s, which no goroutine published", shared)
	}
}
