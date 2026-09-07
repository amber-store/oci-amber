package upload

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"hash"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amber-store/oci-amber/oci"
)

// ErrUnknown is returned for an upload session id that is not registered,
// which includes sessions that were removed, expired or closed with the
// manager.
var ErrUnknown = errors.New("upload: unknown session")

// Session is one in-progress blob upload. Bytes are appended in request
// order. They stay in a memory buffer while the total is at or below
// maxInMemory; the first append that would exceed it creates the session's
// file, writes the buffer out, drops the buffer and appends to the file from
// then on. A sha256 runs over every byte received.
//
// All methods are safe for concurrent use. Requests on the same session are
// serialized on its mutex, so overlapping appends see consistent offsets.
type Session struct {
	// ID is the session's random 128-bit id in lowercase hex.
	ID string

	path        string
	maxInMemory int64

	mu   sync.Mutex
	buf  bytes.Buffer
	file *os.File
	size int64
	hash hash.Hash

	// closed makes Append and Spool return ErrUnknown; it is set on the
	// first call to close, before the file is necessarily gone. removed
	// is set only once the file has actually been unlinked (or there was
	// none to unlink), which may take more than one call to close if an
	// earlier attempt to remove the file failed. See close.
	closed  bool
	removed bool

	// lastActive holds a UnixNano timestamp, updated by touch and read by
	// active without taking mu. Append can hold mu for the whole duration
	// of a large request body copy, so Manager.Sweep must never need mu to
	// find out whether a session is idle.
	lastActive atomic.Int64
}

func newSession(id, path string, maxInMemory int64, now time.Time) *Session {
	s := &Session{
		ID:          id,
		path:        path,
		maxInMemory: maxInMemory,
		hash:        sha256.New(),
	}
	s.lastActive.Store(now.UnixNano())
	return s
}

// Append reads r until EOF, appends its bytes to the session and returns the
// new total byte count. When r fails part way the bytes read before the
// failure stay in the session and the returned offset counts them. On a
// removed session it returns ErrUnknown.
//
// last-active is touched on entry, on every write of the copy (see write)
// and again right before Append returns, so a session that is receiving
// bytes never looks idle to the sweeper: a request body that takes longer
// than the idle timeout to arrive is neither swept mid-transfer nor the
// instant it finishes.
func (s *Session) Append(r io.Reader) (int64, error) {
	s.touch()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		s.touch()
		return s.size, ErrUnknown
	}
	_, err := io.Copy(sessionWriter{s}, r)
	s.touch()
	return s.size, err
}

// Offset is the number of bytes appended so far.
func (s *Session) Offset() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.size
}

// Spool returns a snapshot of everything appended so far together with its
// sha256 digest. The session keeps its data and stays registered so that a
// failed finalize can be retried; the caller removes the session through
// Manager.Remove once the blob is stored. A memory spool shares the
// session's buffer, which is only ever appended to, so later appends do not
// change what the spool reads. On a removed session it returns ErrUnknown.
func (s *Session) Spool() (*Spool, error) {
	s.touch()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrUnknown
	}
	sp := &Spool{
		size:   s.size,
		digest: oci.DigestFromSum(s.hash.Sum(nil)),
		touch:  s.touch,
	}
	if s.file != nil {
		sp.path = s.path
	} else {
		sp.mem = s.buf.Bytes()
	}
	return sp, nil
}

// sessionWriter adapts a session to io.Writer for io.Copy. The caller of
// Append holds the session mutex for the whole copy.
type sessionWriter struct{ s *Session }

func (w sessionWriter) Write(p []byte) (int, error) { return w.s.write(p) }

// write appends p, spilling the buffer to the file first when p would push
// the in-memory total over maxInMemory. The caller holds mu.
//
// Every write records activity: Append holds mu for the whole copy of a
// request body, so without a touch per write a body that takes longer than
// the idle timeout to arrive would look idle to Manager.Sweep from the
// moment its entry timestamp aged out until the copy finished.
func (s *Session) write(p []byte) (int, error) {
	s.touch()
	if s.file == nil && s.size+int64(len(p)) > s.maxInMemory {
		if err := s.spill(); err != nil {
			return 0, err
		}
	}
	if s.file != nil {
		n, err := s.file.Write(p)
		s.hash.Write(p[:n])
		s.size += int64(n)
		return n, err
	}
	n, _ := s.buf.Write(p)
	s.hash.Write(p[:n])
	s.size += int64(n)
	return n, nil
}

// spill creates the session's file, writes the buffered bytes to it and
// drops the buffer. The caller holds mu.
func (s *Session) spill() error {
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(s.buf.Bytes()); err != nil {
		f.Close()
		os.Remove(s.path)
		return err
	}
	s.file = f
	s.buf = bytes.Buffer{}
	return nil
}

// touch records activity on the session. It does not take mu, so it never
// blocks behind an in-progress Append (which holds mu for the duration of
// its copy) and never contributes to lock inversion with Manager.Sweep.
func (s *Session) touch() {
	s.lastActive.Store(time.Now().UnixNano())
}

// active returns the time of the last activity on the session. Like touch,
// it does not take mu: Manager.Sweep calls this while holding the manager's
// own mutex, and it must never also wait on a session's mutex that a
// concurrent Append might be holding for a long request body copy.
func (s *Session) active() time.Time {
	return time.Unix(0, s.lastActive.Load())
}

// removeFile is os.Remove, overridable in tests to simulate a filesystem
// that transiently refuses to unlink a spooled session's file.
var removeFile = os.Remove

// close releases the buffer and marks the session unusable for Append and
// Spool, which return ErrUnknown from their very next call onward. That
// much happens on the first call and is final.
//
// Removing the file is a separate, retryable step: close does not consider
// the file gone (and does not mark itself done) until removeFile actually
// succeeds, or the path never existed. If removeFile fails, close returns
// that error and a later call tries again from scratch — including
// retrying the file descriptor close if that also failed — until the
// unlink succeeds. Once it has, close is an idempotent no-op returning
// nil. Unlinking the file is safe even while a Spool.Open reader for this
// session is still open elsewhere: on Unix an open file descriptor keeps
// working after its directory entry is removed (see Spool.Open).
func (s *Session) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.removed {
		return nil
	}
	s.buf = bytes.Buffer{}
	var fdErr error
	if s.file != nil {
		if err := s.file.Close(); err != nil {
			fdErr = err
		} else {
			s.file = nil
		}
	}
	if err := removeFile(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.Join(fdErr, err)
	}
	s.removed = true
	return fdErr
}
