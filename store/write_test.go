package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/amber-store/core/amberpack"
	"github.com/amber-store/core/cborx"
	"github.com/amber-store/core/fstree"
	"github.com/amber-store/core/key"
	"github.com/amber-store/core/packstore"
)

// openWriterStore opens a fresh store in a temp dir and closes it at cleanup.
func openWriterStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

// pseudoRandomBytes returns n deterministic pseudo-random bytes (seeded so a
// failure reproduces).
func pseudoRandomBytes(t *testing.T, n int, seed uint64) []byte {
	t.Helper()
	b := make([]byte, n)
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

// readFileContent reads a file object back through fstree.WriteContent.
func readFileContent(t *testing.T, s *Store, k key.Key) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := fstree.WriteContent(&buf, k, s.Get); err != nil {
		t.Fatalf("WriteContent(%s): %v", k, err)
	}
	return buf.Bytes()
}

func TestStatsAdd(t *testing.T) {
	a := Stats{LogicalBytes: 1, NewLogicalBytes: 2, DedupedBytes: 3, DiskBytes: 4, ObjectsNew: 5, ObjectsDeduped: 6}
	b := Stats{LogicalBytes: 10, NewLogicalBytes: 20, DedupedBytes: 30, DiskBytes: 40, ObjectsNew: 50, ObjectsDeduped: 60}
	got := a.Add(b)
	want := Stats{LogicalBytes: 11, NewLogicalBytes: 22, DedupedBytes: 33, DiskBytes: 44, ObjectsNew: 55, ObjectsDeduped: 66}
	if got != want {
		t.Fatalf("Add = %+v, want %+v", got, want)
	}
	if (Stats{}).Add(Stats{}) != (Stats{}) {
		t.Fatal("zero + zero != zero")
	}
}

func TestPutBytesSmallReturnsBlob(t *testing.T) {
	s := openWriterStore(t)
	w := s.NewWriter(context.Background())

	cases := map[string][]byte{
		"empty": {},
		"hello": []byte("hello, world"),
		"20k":   pseudoRandomBytes(t, 20_000, 1),
	}
	keys := map[string]key.Key{}
	for name, data := range cases {
		k, err := w.PutBytes(data)
		if err != nil {
			t.Fatalf("%s: PutBytes: %v", name, err)
		}
		if k.Type() != key.Blob {
			t.Errorf("%s: root type = %s, want Blob", name, k.Type())
		}
		if k.Length() != uint64(len(data)) {
			t.Errorf("%s: Length() = %d, want %d", name, k.Length(), len(data))
		}
		keys[name] = k
	}
	if _, err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for name, data := range cases {
		if got := readFileContent(t, s, keys[name]); !bytes.Equal(got, data) {
			t.Errorf("%s: content read back differs (%d bytes vs %d)", name, len(got), len(data))
		}
	}
}

func TestPutStreamLargeReturnsFileNode(t *testing.T) {
	s := openWriterStore(t)
	const size = 3 << 20
	data := pseudoRandomBytes(t, size, 2)

	w := s.NewWriter(context.Background())
	k, err := w.PutStream(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("PutStream: %v", err)
	}
	if k.Type() != key.FileNode {
		t.Fatalf("root type = %s, want FileNode", k.Type())
	}
	if k.Length() != size {
		t.Fatalf("Length() = %d, want %d", k.Length(), size)
	}
	st, err := w.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := readFileContent(t, s, k); !bytes.Equal(got, data) {
		t.Fatal("content read back differs from input")
	}

	// First write of unique data: everything is new.
	if st.LogicalBytes < size {
		t.Errorf("LogicalBytes = %d, want >= %d", st.LogicalBytes, size)
	}
	if st.NewLogicalBytes != st.LogicalBytes {
		t.Errorf("NewLogicalBytes = %d, want == LogicalBytes %d", st.NewLogicalBytes, st.LogicalBytes)
	}
	if st.DedupedBytes != 0 {
		t.Errorf("DedupedBytes = %d, want 0", st.DedupedBytes)
	}
	if st.DiskBytes <= 0 {
		t.Errorf("DiskBytes = %d, want > 0", st.DiskBytes)
	}
	if st.ObjectsDeduped != 0 {
		t.Errorf("ObjectsDeduped = %d, want 0", st.ObjectsDeduped)
	}
	// chunks (>= 3 for 3 MiB with a 1 MiB max chunk) + one FileNode
	if st.ObjectsNew < 4 {
		t.Errorf("ObjectsNew = %d, want >= 4", st.ObjectsNew)
	}
	// Random data does not compress: each new record costs its payload
	// plus a header, and no more than that.
	minDisk := st.NewLogicalBytes + int64(st.ObjectsNew)*amberpack.RecHeaderSize
	if st.DiskBytes > minDisk {
		t.Errorf("DiskBytes = %d, want <= payload + headers = %d", st.DiskBytes, minDisk)
	}
	if st.DiskBytes < int64(st.ObjectsNew)*amberpack.RecHeaderSize {
		t.Errorf("DiskBytes = %d, smaller than %d record headers", st.DiskBytes, st.ObjectsNew)
	}
}

func TestSecondWriteIsFullyDeduplicated(t *testing.T) {
	s := openWriterStore(t)
	const size = 3 << 20
	data := pseudoRandomBytes(t, size, 3)

	w1 := s.NewWriter(context.Background())
	k1, err := w1.PutStream(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("first PutStream: %v", err)
	}
	st1, err := w1.Close()
	if err != nil {
		t.Fatalf("first Close: %v", err)
	}

	w2 := s.NewWriter(context.Background())
	k2, err := w2.PutStream(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("second PutStream: %v", err)
	}
	st2, err := w2.Close()
	if err != nil {
		t.Fatalf("second Close: %v", err)
	}

	if k1 != k2 {
		t.Fatalf("keys differ: %s vs %s", k1, k2)
	}
	if st2.LogicalBytes != st1.LogicalBytes {
		t.Errorf("LogicalBytes = %d, want %d (same as first write)", st2.LogicalBytes, st1.LogicalBytes)
	}
	if st2.NewLogicalBytes != 0 {
		t.Errorf("NewLogicalBytes = %d, want 0", st2.NewLogicalBytes)
	}
	if st2.DedupedBytes != st2.LogicalBytes {
		t.Errorf("DedupedBytes = %d, want == LogicalBytes %d", st2.DedupedBytes, st2.LogicalBytes)
	}
	if st2.DiskBytes != 0 {
		t.Errorf("DiskBytes = %d, want 0", st2.DiskBytes)
	}
	if st2.ObjectsNew != 0 {
		t.Errorf("ObjectsNew = %d, want 0", st2.ObjectsNew)
	}
	if st2.ObjectsDeduped != st1.ObjectsNew {
		t.Errorf("ObjectsDeduped = %d, want %d (every object of the first write)", st2.ObjectsDeduped, st1.ObjectsNew)
	}
}

func TestDuplicateWithinOneWriterCountsDiskOnce(t *testing.T) {
	s := openWriterStore(t)
	data := []byte("the same small object, offered twice")

	w := s.NewWriter(context.Background())
	k1, err := w.PutBytes(data)
	if err != nil {
		t.Fatalf("PutBytes 1: %v", err)
	}
	k2, err := w.PutBytes(data)
	if err != nil {
		t.Fatalf("PutBytes 2: %v", err)
	}
	if k1 != k2 {
		t.Fatalf("keys differ: %s vs %s", k1, k2)
	}
	st, err := w.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	n := int64(len(data))
	if st.LogicalBytes != 2*n {
		t.Errorf("LogicalBytes = %d, want %d (both offers count)", st.LogicalBytes, 2*n)
	}
	if st.NewLogicalBytes != n {
		t.Errorf("NewLogicalBytes = %d, want %d", st.NewLogicalBytes, n)
	}
	if st.DedupedBytes != n {
		t.Errorf("DedupedBytes = %d, want %d", st.DedupedBytes, n)
	}
	if st.ObjectsNew != 1 || st.ObjectsDeduped != 1 {
		t.Errorf("ObjectsNew/Deduped = %d/%d, want 1/1", st.ObjectsNew, st.ObjectsDeduped)
	}
	size, found, err := s.Objects.StoredSize(k1)
	if err != nil || !found {
		t.Fatalf("StoredSize: found=%v err=%v", found, err)
	}
	if want := int64(size) + amberpack.RecHeaderSize; st.DiskBytes != want {
		t.Errorf("DiskBytes = %d, want exactly one record = %d", st.DiskBytes, want)
	}
}

func TestConcurrentPutStream(t *testing.T) {
	s := openWriterStore(t)
	const n = 4
	const size = 1 << 20
	inputs := make([][]byte, n)
	for i := range inputs {
		inputs[i] = pseudoRandomBytes(t, size+i*1000, uint64(10+i))
	}

	w := s.NewWriter(context.Background())
	keys := make([]key.Key, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			keys[i], errs[i] = w.PutStream(bytes.NewReader(inputs[i]))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: PutStream: %v", i, err)
		}
	}
	st, err := w.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	for i := range n {
		if keys[i].Length() != uint64(len(inputs[i])) {
			t.Errorf("goroutine %d: Length() = %d, want %d", i, keys[i].Length(), len(inputs[i]))
		}
		if got := readFileContent(t, s, keys[i]); !bytes.Equal(got, inputs[i]) {
			t.Errorf("goroutine %d: content differs", i)
		}
	}
	var total int64
	for _, in := range inputs {
		total += int64(len(in))
	}
	if st.LogicalBytes < total {
		t.Errorf("LogicalBytes = %d, want >= %d", st.LogicalBytes, total)
	}
	if st.NewLogicalBytes != st.LogicalBytes {
		t.Errorf("NewLogicalBytes = %d, want == LogicalBytes %d", st.NewLogicalBytes, st.LogicalBytes)
	}
}

func TestAbortAfterPartialWrites(t *testing.T) {
	s := openWriterStore(t)
	data := pseudoRandomBytes(t, 3<<20, 4)

	w := s.NewWriter(context.Background())
	if _, err := w.PutStream(bytes.NewReader(data)); err != nil {
		t.Fatalf("PutStream: %v", err)
	}
	w.Abort()
	w.Abort() // idempotent

	if _, err := w.PutBytes([]byte("after abort")); err == nil {
		t.Error("PutBytes after Abort succeeded, want error")
	}
	if _, err := w.Close(); !errors.Is(err, errAborted) {
		t.Errorf("Close after Abort: err = %v, want errAborted", err)
	}
	w.Abort() // safe after Close

	// The store is unaffected: a new Writer works.
	w2 := s.NewWriter(context.Background())
	k, err := w2.PutBytes([]byte("fresh"))
	if err != nil {
		t.Fatalf("PutBytes on new writer: %v", err)
	}
	if _, err := w2.Close(); err != nil {
		t.Fatalf("Close on new writer: %v", err)
	}
	if got := readFileContent(t, s, k); string(got) != "fresh" {
		t.Errorf("content = %q, want %q", got, "fresh")
	}
}

// endlessZeros never ends; it drives a PutStream that only Abort can stop.
type endlessZeros struct{}

func (endlessZeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestAbortUnblocksInFlightPutStream(t *testing.T) {
	s := openWriterStore(t)
	w := s.NewWriter(context.Background())

	res := make(chan error, 1)
	go func() {
		_, err := w.PutStream(endlessZeros{})
		res <- err
	}()
	time.Sleep(50 * time.Millisecond) // let the stream get going
	w.Abort()

	select {
	case err := <-res:
		if !errors.Is(err, errAborted) {
			t.Errorf("PutStream returned %v, want errAborted", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("PutStream did not return after Abort")
	}
	if _, err := w.Close(); !errors.Is(err, errAborted) {
		t.Errorf("Close: err = %v, want errAborted", err)
	}
}

func TestCloseWithCancelledContextReturnsContextError(t *testing.T) {
	s := openWriterStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	w := s.NewWriter(ctx)
	if _, err := w.PutBytes([]byte("before cancel")); err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	cancel()
	if _, err := w.PutBytes([]byte("after cancel")); !errors.Is(err, context.Canceled) {
		t.Errorf("PutBytes after cancel: err = %v, want context.Canceled", err)
	}
	st, err := w.Close()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Close: err = %v, want context.Canceled", err)
	}
	if st != (Stats{}) {
		t.Errorf("Close returned stats %+v with an error, want zero", st)
	}
	// Close is idempotent and keeps reporting the error.
	if _, err := w.Close(); !errors.Is(err, context.Canceled) {
		t.Errorf("second Close: err = %v, want context.Canceled", err)
	}
}

func TestCloseIsIdempotentAndEmitAfterCloseFails(t *testing.T) {
	s := openWriterStore(t)
	w := s.NewWriter(context.Background())
	if _, err := w.PutBytes([]byte("x")); err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	st1, err := w.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	st2, err := w.Close()
	if err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if st1 != st2 {
		t.Errorf("second Close returned %+v, want %+v", st2, st1)
	}
	obj, err := fstree.EncodeBlob([]byte("late"))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Emit(obj); !errors.Is(err, errWriterClosed) {
		t.Errorf("Emit after Close: err = %v, want errWriterClosed", err)
	}
	if _, err := w.PutBytes([]byte("late")); !errors.Is(err, errWriterClosed) {
		t.Errorf("PutBytes after Close: err = %v, want errWriterClosed", err)
	}
}

func TestVerifyRejectsMismatchedObject(t *testing.T) {
	s := openWriterStore(t)
	w := s.NewWriter(context.Background())
	obj, err := fstree.EncodeBlob([]byte("honest payload"))
	if err != nil {
		t.Fatal(err)
	}
	obj.Bytes = []byte("tampered payload") // key no longer matches
	if err := w.Emit(obj); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	_, err = w.Close()
	if !errors.Is(err, packstore.ErrVerify) {
		t.Fatalf("Close: err = %v, want packstore.ErrVerify", err)
	}
	has, err := s.Has(obj.Key)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("tampered object was stored")
	}
}

func TestWriterOnClosedStoreFails(t *testing.T) {
	s, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	w := s.NewWriter(context.Background())
	_, putErr := w.PutBytes([]byte("nowhere to go"))
	_, closeErr := w.Close()
	if putErr == nil && closeErr == nil {
		t.Fatal("writing to a closed store succeeded")
	}
	if closeErr != nil && !errors.Is(closeErr, packstore.ErrClosed) {
		t.Errorf("Close: err = %v, want packstore.ErrClosed", closeErr)
	}
}

func TestPutStreamPropagatesReaderError(t *testing.T) {
	s := openWriterStore(t)
	w := s.NewWriter(context.Background())
	boom := fmt.Errorf("disk on fire")
	r := io.MultiReader(bytes.NewReader(pseudoRandomBytes(t, 100_000, 5)), writeTestErrReader{boom})
	if _, err := w.PutStream(r); !errors.Is(err, boom) {
		t.Fatalf("PutStream: err = %v, want %v", err, boom)
	}
	if _, err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

type writeTestErrReader struct{ err error }

func (e writeTestErrReader) Read([]byte) (int, error) { return 0, e.err }

func TestPutXattrsInlineAndSpilled(t *testing.T) {
	s := openWriterStore(t)
	w := s.NewWriter(context.Background())
	defer w.Abort()
	inline, spilled, err := w.PutXattrs(nil)
	if err != nil || inline != nil || spilled != (key.Key{}) {
		t.Fatalf("PutXattrs(nil) = %v, %s, %v; want nothing", inline, spilled, err)
	}
	small := map[string][]byte{"security.capability": []byte("\x01\x00\x00\x02")}
	inline, spilled, err = w.PutXattrs(small)
	if err != nil {
		t.Fatal(err)
	}
	if spilled != (key.Key{}) || !bytes.Equal(inline, cborx.EncodeXattrs(small)) {
		t.Fatalf("small set: inline %x, spilled %s", inline, spilled)
	}
	large := map[string][]byte{"user.big": bytes.Repeat([]byte("x"), XattrInlineMax)}
	inline, spilled, err = w.PutXattrs(large)
	if err != nil {
		t.Fatal(err)
	}
	if inline != nil || spilled == (key.Key{}) || spilled.Type() != key.XattrSet {
		t.Fatalf("large set: inline %x, spilled %s", inline, spilled)
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := s.Get(spilled)
	if err != nil {
		t.Fatalf("spilled XattrSet not stored: %v", err)
	}
	got, err := cborx.DecodeXattrs(data)
	if err != nil || !bytes.Equal(got["user.big"], large["user.big"]) {
		t.Fatalf("decoded %v, %v", got, err)
	}
}
