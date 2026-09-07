package blob

import (
	"io"

	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/upload"
)

// Stage is one phase of a blob's finalization, in the order Put runs them.
// Analyze always comes first and, for a tar candidate, covers the
// speculative decompose that stages the layer's parts while the engine
// search runs; a prism continues with commit and, when VerifyRoundTrip is
// set, verify; a raw decision or a downgrade ends with raw.
type Stage string

const (
	StageAnalyze Stage = "analyze" // zrecipe pass one and the engine elimination
	StageConfirm Stage = "confirm" // recompress the content once while the tar is taken apart
	StageCommit  Stage = "commit"  // inserting the staged pack into the store
	StageVerify  Stage = "verify"  // round-trip check
	StageRaw     Stage = "raw"     // storing the bytes verbatim
)

// Observer receives finalization progress from Put. BlobStage says d
// entered s; BlobProgress says n bytes of d, counted against its
// compressed size, have been handled in the current stage. n never
// decreases within a stage. In analyze it is the spool's sequential read
// position, which reaches the size when pass one is done and then holds
// while the engine search reads through ReadAt; in commit it is the staged
// pack's bytes read so far, scaled to the size; in verify the
// bytes recompressed so far; in raw the bytes stored so far. A dedup
// hit reports nothing. Methods are called from the goroutines running
// Put, concurrently for different digests.
type Observer interface {
	BlobStage(d oci.Digest, s Stage)
	BlobProgress(d oci.Digest, n int64)
}

// observeStage reports a stage transition when an observer is configured.
func (b *Store) observeStage(d oci.Digest, s Stage) {
	if b.opts.Observer != nil {
		b.opts.Observer.BlobStage(d, s)
	}
}

// observeProgress reports n bytes of d handled in the current stage when
// an observer is configured.
func (b *Store) observeProgress(d oci.Digest, n int64) {
	if b.opts.Observer != nil {
		b.opts.Observer.BlobProgress(d, n)
	}
}

// observeReader wraps r so that its highest sequential read position is
// reported for d; ReadAt does not count. Without an observer r is
// returned as is. The wrapper does not own r: the caller keeps closing
// the original.
func (b *Store) observeReader(d oci.Digest, r upload.ReaderAtSeeker) upload.ReaderAtSeeker {
	if b.opts.Observer == nil {
		return r
	}
	obs := b.opts.Observer
	return &progressReader{ReaderAtSeeker: r, report: func(n int64) { obs.BlobProgress(d, n) }}
}

// observeWriter wraps w so that the bytes written through it are reported
// for d. Without an observer w is returned as is.
func (b *Store) observeWriter(d oci.Digest, w io.Writer) io.Writer {
	if b.opts.Observer == nil {
		return w
	}
	obs := b.opts.Observer
	return &progressWriter{w: w, report: func(n int64) { obs.BlobProgress(d, n) }}
}

// observeConfirmWriter wraps w so the content handed to the stager during
// the confirming pass is reported for d, scaled from the uncompressed size
// down to the compressed size so the confirm bar ends at the size like the
// other stages. Without an observer, or with an unknown uncompressed size,
// w is returned as is.
func (b *Store) observeConfirmWriter(d oci.Digest, w io.Writer, compressed, uncompressed int64) io.Writer {
	if b.opts.Observer == nil || uncompressed <= 0 {
		return w
	}
	obs := b.opts.Observer
	scale := float64(compressed) / float64(uncompressed)
	return &progressWriter{w: w, report: func(n int64) {
		obs.BlobProgress(d, min(compressed, int64(float64(n)*scale)))
	}}
}

// progressReader tracks the position of sequential reads over a seekable
// reader and reports every new high-water mark.
type progressReader struct {
	upload.ReaderAtSeeker
	pos, high int64
	report    func(int64)
}

func (p *progressReader) Read(buf []byte) (int, error) {
	n, err := p.ReaderAtSeeker.Read(buf)
	p.pos += int64(n)
	if p.pos > p.high {
		p.high = p.pos
		p.report(p.high)
	}
	return n, err
}

func (p *progressReader) Seek(offset int64, whence int) (int64, error) {
	pos, err := p.ReaderAtSeeker.Seek(offset, whence)
	if err == nil {
		p.pos = pos
	}
	return pos, err
}

// progressWriter counts the bytes written through it.
type progressWriter struct {
	w      io.Writer
	n      int64
	report func(int64)
}

func (p *progressWriter) Write(buf []byte) (int, error) {
	n, err := p.w.Write(buf)
	p.n += int64(n)
	p.report(p.n)
	return n, err
}
