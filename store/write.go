package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"runtime"
	"sync"

	"github.com/amber-store/core/amberpack"
	"github.com/amber-store/core/cborx"
	"github.com/amber-store/core/chunkers"
	"github.com/amber-store/core/fstree"
	"github.com/amber-store/core/key"
	"github.com/amber-store/core/packstore"
)

// Stats is the accounting result of one Writer, as defined by the design
// spec's "Accounting and logging / Per blob" section. Every byte count is
// over the objects offered to the Writer, not over what the caller streamed
// in: content chunks, file index nodes, directory leaves and nodes all
// count.
type Stats struct {
	// LogicalBytes is the sum of the encoded size of every object offered,
	// duplicates included.
	LogicalBytes int64 `json:"logicalBytes"`
	// NewLogicalBytes is the encoded size of the objects that were actually
	// appended to the store (packstore.WriteStats.BytesStored).
	NewLogicalBytes int64 `json:"newLogicalBytes"`
	// DedupedBytes is LogicalBytes - NewLogicalBytes.
	DedupedBytes int64 `json:"dedupedBytes"`
	// DiskBytes is the number of bytes appended to pack segments: for every
	// key that Has reported absent when it was first offered, the stored
	// (compressed) payload size plus the record header.
	DiskBytes int64 `json:"diskBytes"`
	// ObjectsNew and ObjectsDeduped are packstore.WriteStats.Stored and
	// Deduped; ObjectsDeduped also counts the objects a pack Writer left
	// out because the store already held them (see AddPack), so a blob's
	// counts do not depend on when its duplicates were found.
	ObjectsNew     int `json:"objectsNew"`
	ObjectsDeduped int `json:"objectsDeduped"`
}

// Add returns the field-wise sum of a and b.
func (a Stats) Add(b Stats) Stats {
	return Stats{
		LogicalBytes:    a.LogicalBytes + b.LogicalBytes,
		NewLogicalBytes: a.NewLogicalBytes + b.NewLogicalBytes,
		DedupedBytes:    a.DedupedBytes + b.DedupedBytes,
		DiskBytes:       a.DiskBytes + b.DiskBytes,
		ObjectsNew:      a.ObjectsNew + b.ObjectsNew,
		ObjectsDeduped:  a.ObjectsDeduped + b.ObjectsDeduped,
	}
}

var (
	// errAborted is the cause a Writer's context is cancelled with by Abort;
	// Close returns it (wrapped) after an Abort.
	errAborted = errors.New("store: writer aborted")
	// errWriterClosed is returned by Emit after Close or Abort.
	errWriterClosed = errors.New("store: writer closed")
)

// emitBuffer bounds the objects queued between producers and the store
// writers (each is at most one max-size chunk, so about 8 MiB in flight).
const emitBuffer = 8

// item is one object queued for the backend: a built object (Data) or a
// record staged in a pack (Record), with the payload length the
// accounting charges for it. With skipped set it is a key alone, one a
// pack Writer left out because the store held it: AddPack queues it for
// the accounting and it never reaches the backend.
type item struct {
	obj     packstore.Object
	logical int64
	skipped bool
}

// Writer builds CAS objects and hands them to a backend. The live backend
// (NewWriter) streams them into the store through one
// packstore.WriteParallel call and accounts for them; the pack backend
// (NewPackWriter) encodes them into a pack file for a later AddPack.
// Objects are offered with Emit (directly, or through PutStream, PutBytes
// and Dir) from any number of goroutines; Close waits for every offered
// object to be durable, or staged, and returns the Stats. The Writer's
// context bounds its whole life: once it is cancelled, Emit fails and
// Close returns the context's error.
type Writer struct {
	s      *Store
	ctx    context.Context
	cancel context.CancelCauseFunc
	ic     chunkers.ItemChunker
	ch     chan item
	done   chan struct{} // closed when the backend has returned
	pack   *Pack         // the pack backend's file; nil for the live backend

	mu     sync.RWMutex // held shared by emit for the send; exclusively to close ch
	closed bool

	// Written only by the backend goroutines; read after done is closed.
	logical        int64
	seen           map[key.Key]bool // live backend: every key offered -> Has reported it absent
	skippedDeduped int              // live backend: skipped keys of added packs, found still present
	wstats         packstore.WriteStats
	werr           error

	once   sync.Once
	result Stats
	rerr   error
	sealed bool // pack backend: Close succeeded and Pack() is valid
}

// newWriter builds a Writer bound to ctx without starting a backend.
func (s *Store) newWriter(ctx context.Context) *Writer {
	ctx, cancel := context.WithCancelCause(ctx)
	return &Writer{
		s:      s,
		ctx:    ctx,
		cancel: cancel,
		ic:     chunkers.NewItemChunker(ItemBits),
		ch:     make(chan item, emitBuffer),
		done:   make(chan struct{}),
		seen:   make(map[key.Key]bool),
	}
}

// NewWriter starts a live Writer over s bound to ctx. It launches the
// store's parallel writer immediately; the caller must end the Writer with
// Close or Abort.
func (s *Store) NewWriter(ctx context.Context) *Writer {
	w := s.newWriter(ctx)
	go w.run()
	return w
}

// writers is the WriteParallel worker count: GOMAXPROCS/2, at least 1.
func writers() int {
	return max(1, runtime.GOMAXPROCS(0)/2)
}

// run feeds WriteParallel from the accounting iterator and records its
// result.
func (w *Writer) run() {
	defer close(w.done)
	w.wstats, w.werr = w.s.Objects.WriteParallel(w.objects(), packstore.WriteOpts{
		Writers: writers(),
		Verify:  true,
	})
}

// objects is the accounting iterator: it forwards every object received on
// ch to WriteParallel, summing encoded sizes and remembering which keys the
// store did not have when they were first offered. A skipped key from an
// added pack is accounted here and not forwarded. A cancelled context
// stops the stream with the cancellation cause.
func (w *Writer) objects() iter.Seq2[packstore.Object, error] {
	return func(yield func(packstore.Object, error) bool) {
		for {
			select {
			case <-w.ctx.Done():
				yield(packstore.Object{}, context.Cause(w.ctx))
				return
			case it, ok := <-w.ch:
				if !ok {
					if err := context.Cause(w.ctx); err != nil {
						yield(packstore.Object{}, err)
					}
					return
				}
				if it.skipped {
					if err := w.accountSkipped(it); err != nil {
						yield(packstore.Object{}, err)
						return
					}
					continue
				}
				if err := w.account(it); err != nil {
					yield(packstore.Object{}, err)
					return
				}
				if !yield(it.obj, nil) {
					return
				}
			}
		}
	}
}

// account records one offered item. It runs on the iterator goroutine
// only, so it needs no lock.
func (w *Writer) account(it item) error {
	w.logical += it.logical
	if _, seen := w.seen[it.obj.Key]; seen {
		return nil
	}
	has, err := w.s.Objects.Has(it.obj.Key)
	if err != nil {
		return err
	}
	w.seen[it.obj.Key] = !has
	return nil
}

// accountSkipped records one key a pack Writer left out because the store
// held it when the pack was staged. The store is asked again, observe then
// Has like the pack Writer and WriteParallel: the time between staging and
// commit is longer than the live writer's between its own decision and the
// publication, so the earlier answer is not trusted, and a key that has
// disappeared fails the Writer rather than let the caller publish a root
// that dangles. A present key counts as a deduplicated object with its
// logical bytes and is remembered as present, so it never adds to
// DiskBytes; a key this Writer stored itself earlier keeps its entry. Runs
// on the iterator goroutine only, like account.
func (w *Writer) accountSkipped(it item) error {
	k := it.obj.Key
	w.s.Objects.ObserveKeys([]key.Key{k})
	has, err := w.s.Objects.Has(k)
	if err != nil {
		return err
	}
	if !has {
		return fmt.Errorf("store: staged chunk %s is no longer in the store", k)
	}
	w.logical += it.logical
	if _, seen := w.seen[k]; !seen {
		w.seen[k] = false
	}
	w.skippedDeduped++
	return nil
}

// Emit offers one built object to the backend. It is safe for concurrent
// use and blocks only while the pipeline is full. It fails once the Writer
// is closed or aborted, its context is cancelled, or the backend has
// stopped with an error.
func (w *Writer) Emit(o fstree.Object) error {
	return w.emit(item{obj: packstore.Object{Key: o.Key, Data: o.Bytes}, logical: int64(len(o.Bytes))})
}

// emit is Emit for a queued item.
func (w *Writer) emit(it item) error {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		return w.stoppedErr()
	}
	if err := context.Cause(w.ctx); err != nil {
		return err
	}
	select {
	case w.ch <- it:
		return nil
	case <-w.ctx.Done():
		return context.Cause(w.ctx)
	case <-w.done:
		if w.werr != nil {
			return w.werr
		}
		return w.stoppedErr()
	}
}

// stoppedErr is the error Emit reports once the Writer no longer accepts
// objects: the cancellation cause (errAborted after Abort, the parent's
// error after a context cancellation, errWriterClosed after Close), or
// errWriterClosed while a Close is still in progress.
func (w *Writer) stoppedErr() error {
	if err := context.Cause(w.ctx); err != nil {
		return err
	}
	return errWriterClosed
}

// byteOpts returns the store's content-defined chunking parameters. A fresh
// value is returned per call because the chunker keeps the pointer.
func (s *Store) byteOpts() *chunkers.ByteOpts {
	return s.Config().Chunking.ByteOpts()
}

// PutStream chunks r with the store's content-defined chunker, emits one
// Blob per chunk, builds the FileNode index above them and returns the
// file's root key: a Blob for content that fits in one chunk (an empty
// reader yields the empty Blob), a FileNode otherwise. The root's Length is
// the byte length of the content. Safe for concurrent use.
func (w *Writer) PutStream(r io.Reader) (key.Key, error) {
	ib := fstree.NewFileIndexBuilder(w.ic)
	saw := false
	err := chunkers.SplitBytes(r, w.s.byteOpts(), func(chunk []byte) error {
		saw = true
		return w.addChunk(ib, chunk)
	})
	if err != nil {
		return key.Key{}, err
	}
	if !saw {
		if err := w.addChunk(ib, []byte{}); err != nil {
			return key.Key{}, err
		}
	}
	return ib.Finish(w.Emit)
}

// addChunk emits one content chunk and adds it to the file index.
func (w *Writer) addChunk(ib *fstree.IndexBuilder, chunk []byte) error {
	obj, err := fstree.EncodeBlob(chunk)
	if err != nil {
		return err
	}
	if err := w.Emit(obj); err != nil {
		return err
	}
	return ib.AddChild(w.Emit, obj.Key, nil)
}

// PutBytes is PutStream over an in-memory buffer.
func (w *Writer) PutBytes(b []byte) (key.Key, error) {
	return w.PutStream(bytes.NewReader(b))
}

// Close ends the object stream, waits for the backend to make everything
// durable (or, for a pack Writer, staged) and returns the accounting. It
// is idempotent: later calls return the same result. It returns the
// Writer's context error when the context was cancelled, errAborted after
// Abort, and the backend's error when a write failed; in those cases the
// objects appended so far are left for GC, and a pack Writer's file is
// released.
func (w *Writer) Close() (Stats, error) {
	w.closeStream()
	<-w.done
	w.settle()
	return w.result, w.rerr
}

// settle computes the result once the backend has returned: the Stats or
// the error and, for the pack backend, whether the pack survives (a
// failed run releases its file). It runs once; Close and Abort both call
// it, so Close after Abort reports errAborted and an Abort after a
// successful Close leaves the pack alone.
func (w *Writer) settle() {
	w.once.Do(func() {
		w.result, w.rerr = w.finish()
		w.cancel(errWriterClosed)
		if w.pack != nil {
			if w.rerr != nil {
				w.pack.f.Close()
			} else {
				w.sealed = true
			}
		}
	})
}

// closeStream marks the Writer closed and closes the object channel once.
func (w *Writer) closeStream() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.closed = true
	close(w.ch)
}

// finish computes the Stats after the backend has returned: the logical
// bytes alone for a pack, the full accounting for the live backend.
func (w *Writer) finish() (Stats, error) {
	err := w.werr
	if err == nil {
		err = context.Cause(w.ctx)
	}
	if err != nil {
		return Stats{}, err
	}
	if w.pack != nil {
		return Stats{LogicalBytes: w.logical}, nil
	}
	st := Stats{
		LogicalBytes:    w.logical,
		NewLogicalBytes: w.wstats.BytesStored,
		DedupedBytes:    w.logical - w.wstats.BytesStored,
		ObjectsNew:      w.wstats.Stored,
		ObjectsDeduped:  w.wstats.Deduped + w.skippedDeduped,
	}
	for k, absent := range w.seen {
		if !absent {
			continue
		}
		size, found, err := w.s.Objects.StoredSize(k)
		if err != nil {
			return Stats{}, err
		}
		if found {
			st.DiskBytes += int64(size) + amberpack.RecHeaderSize
		}
	}
	return st, nil
}

// Abort stops the Writer: in-flight and later Emit calls fail, the backend
// is stopped, and Close reports errAborted. Objects already appended stay
// in the store as unreachable garbage; a pack Writer's file is released.
// Safe to call more than once and after Close.
func (w *Writer) Abort() {
	w.cancel(errAborted)
	w.closeStream()
	<-w.done
	w.settle()
}

// XattrInlineMax is the largest canonical encoding of an extended-attribute
// set that is kept inline in a directory entry; a larger set is spilled to
// an XattrSet object. It is amber's own ingest default.
const XattrInlineMax = 256

// PutXattrs prepares m for a directory entry: the canonical encoding is
// returned as inline when it is at most XattrInlineMax bytes, otherwise an
// XattrSet object is emitted and its key returned. An empty m yields
// neither. Safe for concurrent use.
func (w *Writer) PutXattrs(m map[string][]byte) (inline []byte, spilled key.Key, err error) {
	if len(m) == 0 {
		return nil, key.Key{}, nil
	}
	enc := cborx.EncodeXattrs(m)
	if len(enc) <= XattrInlineMax {
		return enc, key.Key{}, nil
	}
	obj, err := fstree.EncodeXattrSet(m)
	if err != nil {
		return nil, key.Key{}, err
	}
	if err := w.Emit(obj); err != nil {
		return nil, key.Key{}, err
	}
	return nil, obj.Key, nil
}
