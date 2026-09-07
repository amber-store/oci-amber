package dockerarchive

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/amber-store/oci-amber/oci"
)

// stager rebuilds the plan's blobs into files under a directory of its
// own, several at once and ahead of the archive writer, which takes them
// back in write order. A feeder hands the blobs in write order to workers
// and queues them in that order for the writer; the queue's capacity
// bounds how far the rebuilding runs ahead, and so how many staged files
// exist at once (the queue plus the one the writer holds).
type stager struct {
	plan   *savePlan
	dir    string
	ctx    context.Context
	cancel context.CancelFunc
	queue  chan *staged
	wg     sync.WaitGroup

	mu  sync.Mutex
	err error // the first failure, which cancelled the rest
}

// staged is one blob on its way through the stager.
type staged struct {
	digest oci.Digest
	size   int64 // what the descriptor says
	path   string
	n      int64 // what the source produced
	err    error
	done   chan struct{}
}

// stage starts rebuilding blobs, in order, on workers goroutines, into a
// fresh directory under workDir ("" is the OS temp directory). ctx and
// cancel must belong together: the stager cancels it on a failure so the
// other workers stop.
func (p *savePlan) stage(ctx context.Context, cancel context.CancelFunc, blobs []oci.Digest, workers int, workDir string) (*stager, error) {
	dir, err := os.MkdirTemp(workDir, "oci-amber-save-")
	if err != nil {
		return nil, fmt.Errorf("dockerarchive: creating the staging directory: %w", err)
	}
	st := &stager{plan: p, dir: dir, ctx: ctx, cancel: cancel, queue: make(chan *staged, 2*workers)}
	jobs := make(chan *staged)
	for range workers {
		st.wg.Add(1)
		go func() {
			defer st.wg.Done()
			for s := range jobs {
				st.run(s)
				close(s.done)
			}
		}()
	}
	st.wg.Add(1)
	go func() {
		defer st.wg.Done()
		defer close(jobs)
		for _, d := range blobs {
			s := &staged{digest: d, size: p.items[d].size, path: filepath.Join(dir, d.Hex()), done: make(chan struct{})}
			select {
			case st.queue <- s:
			case <-ctx.Done():
				return
			}
			select {
			case jobs <- s:
			case <-ctx.Done():
				return
			}
		}
	}()
	return st, nil
}

// run rebuilds one blob into its file, counting the bytes into the plan's
// progress; a failure is recorded and stops the stager.
func (st *stager) run(s *staged) {
	if err := st.ctx.Err(); err != nil {
		s.err = err
		return
	}
	s.err = st.rebuild(s)
	if s.err != nil {
		os.Remove(s.path)
		st.fail(s.err)
	}
}

func (st *stager) rebuild(s *staged) error {
	f, err := os.Create(s.path)
	if err != nil {
		return fmt.Errorf("dockerarchive: staging blob %s: %w", s.digest, err)
	}
	st.plan.startBlob(s.digest, s.size)
	defer st.plan.finishBlob(s.digest)
	err = st.plan.src.Blob(st.ctx, s.digest, &stageWriter{s: s, plan: st.plan, f: f})
	if cerr := f.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("dockerarchive: rebuilding blob %s: %w", s.digest, err)
	}
	return nil
}

// fail records the first failure and cancels the rest.
func (st *stager) fail(err error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.err == nil {
		st.err = err
		st.cancel()
	}
}

// next returns the next blob in write order, rebuilt or failed, or the
// context's error once the stager was stopped.
func (st *stager) next() (*staged, error) {
	var s *staged
	select {
	case s = <-st.queue:
	case <-st.ctx.Done():
		return nil, st.ctx.Err()
	}
	select {
	case <-s.done:
		return s, nil
	case <-st.ctx.Done():
		return nil, st.ctx.Err()
	}
}

// failure is the error the writer reports for err, which it met on its
// way: the caller's cancellation when there was one, else the failure
// that stopped the stager, else err itself.
func (st *stager) failure(outer context.Context, err error) error {
	if cerr := outer.Err(); cerr != nil {
		return cerr
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.err != nil {
		return st.err
	}
	return err
}

// close stops the workers, waits for them and removes the staged files.
func (st *stager) close() {
	st.cancel()
	st.wg.Wait()
	os.RemoveAll(st.dir)
}

// stageWriter writes a blob's bytes to its staged file, counting them into
// the plan's progress.
type stageWriter struct {
	s    *staged
	plan *savePlan
	f    *os.File
}

func (w *stageWriter) Write(b []byte) (int, error) {
	n, err := w.f.Write(b)
	if n > 0 {
		w.s.n += int64(n)
		w.plan.advance(w.s.digest, int64(n))
	}
	return n, err
}
