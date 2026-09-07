package blob

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	tarprism "github.com/draganm/tar-prism"
	zrecipe "github.com/draganm/zrecipe"
	cpformat "github.com/draganm/zrecipe/format"
	"github.com/jobs-build/amber-store-core/key"
	"lukechampine.com/blake3"

	"github.com/amber-store/oci-amber/oci"
	"github.com/amber-store/oci-amber/store"
	"github.com/amber-store/oci-amber/upload"
)

// decision is the outcome of pass one: how the blob will be stored.
type decision struct {
	kind     Kind
	reason   RawReason         // set for KindRaw
	err      error             // for KindRaw, the detail behind reason: zrecipe's error, if there is one
	analysis *zrecipe.Analysis // set for KindPrism: the decompressed content and the settled candidate, awaiting the confirming pass
	src      io.Closer         // set for KindPrism: the open upload spool the confirming pass reads back for its byte comparison
	format   string            // "gzip" | "zstd" | "none", always set
}

// close releases a prism decision's analysis (the zrecipe spool) and the
// upload reader it compares against. Safe on a raw decision and more than
// once.
func (d decision) close() {
	if d.analysis != nil {
		d.analysis.Close()
	}
	if d.src != nil {
		d.src.Close()
	}
}

// staged is what the speculative decompose left behind (spec "Blob
// orchestration"): the pack holding the recipe, index and file contents
// as records, the keys that name them inside the pack, and the facts
// about the decompressed stream. err is the staging failure, classified
// as pass two's used to be: *readError for the stream (only possible
// when Analyze itself failed), *sinkError for the pack writer,
// *decomposeError for tar-prism rejecting the archive.
type staged struct {
	pack                 *store.Pack
	recipe, index, blobs key.Key
	entries              int
	diffID               oci.Digest
	blake3               string
	size                 int64
	err                  error
}

// drop releases the pack, if any. Safe on nil and more than once.
func (s *staged) drop() {
	if s != nil && s.pack != nil {
		s.pack.Close()
		s.pack = nil
	}
}

// check reports why the staged result cannot be committed: the staging
// error, or a decompressed stream that differs from what Analyze hashed,
// which can only mean a bug and is treated like a decompose failure.
func (s *staged) check(params *zrecipe.Params) error {
	if s.err != nil {
		return s.err
	}
	if s.blake3 != params.Uncompressed.Blake3 || s.size != params.Uncompressed.Size {
		return &decomposeError{fmt.Errorf("staged stream is %s/%d, analyze recorded %s/%d", s.blake3, s.size, params.Uncompressed.Blake3, params.Uncompressed.Size)}
	}
	return nil
}

// stagePipeSlots is how many writes Analyze may be ahead of the stager
// through the pipe that carries the decompressed stream (spec "Budgets").
const stagePipeSlots = 8

// tarHeaderSize is one tar block; the first block of a tar is a header.
const tarHeaderSize = 512

// maxZstdWindow is the largest zstd window this store will decompress. Both
// this check and newDecompressor's zstd.WithDecoderMaxWindow enforce it, so
// a frame that declares a bigger window is never decompressed at all.
const maxZstdWindow = 128 << 20

// spoolDir is the directory zrecipe spills its decompressed spool to.
func (b *Store) spoolDir() string { return filepath.Join(b.opts.WorkDir, spoolDirName) }

// analyze runs zrecipe's first pass under the analyze deadline while the
// speculative decompose stages the stream (spec "Speculative decompose"),
// and classifies the result. It returns an error only for failures that
// must fail the upload: the request context ended, an I/O error, an
// unexpected zrecipe error, a pack file that could not be created. Every
// fallback case is a raw decision carrying its reason; a prism decision
// carries the staged pack, which the caller must drop or commit.
func (b *Store) analyze(ctx context.Context, sp *upload.Spool) (decision, error) {
	if err := ctx.Err(); err != nil {
		return decision{}, fmt.Errorf("blob: analyze: %w", err)
	}
	r, err := sp.Open()
	if err != nil {
		return decision{}, fmt.Errorf("blob: opening spool: %w", err)
	}
	// The confirming pass reads this reader back for its byte comparison,
	// so a prism decision keeps it open and closes it through decision.close;
	// every other return closes it here. srcCloser is nil'd once the prism
	// decision has taken ownership.
	srcCloser, _ := r.(io.Closer)
	defer func() {
		if srcCloser != nil {
			srcCloser.Close()
		}
	}()
	r = b.observeReader(sp.Digest(), r)

	// Detect first so a raw fallback still records the container format.
	f, err := zrecipe.Detect(r)
	if err != nil {
		return decision{}, fmt.Errorf("blob: detecting format: %w", err)
	}
	format := string(f)

	// A zstd frame that declares a window past what this store will ever
	// decompress is decided from its header alone, before anything is
	// decompressed: parsing costs a handful of bytes, where letting
	// newDecompressor's bounded decoder discover the same fact mid-probe or
	// mid-Analyze would mean it already started reading compressed data
	// under a window budget it cannot honor.
	if f == zrecipe.FormatZstd {
		windowSize, err := zstdWindowSize(r)
		if err == nil && windowSize > maxZstdWindow {
			b.log.Info("zstd window exceeds limit",
				"window_size", windowSize, "limit", int64(maxZstdWindow))
			return decision{
				kind:   KindRaw,
				reason: ReasonUnsupported,
				err:    fmt.Errorf("zstd window %d exceeds the %d byte limit", windowSize, int64(maxZstdWindow)),
				format: format,
			}, nil
		}
		// A header that cannot be parsed decides nothing: Analyze runs and
		// reports its own corrupt/unsupported classification.
	}

	// A compressed blob whose first decompressed block is not a tar header
	// can never become a prism, so decide it here rather than after a full
	// engine search and a staging that tar-prism is bound to reject (spec
	// "Blob finalization" step 5). The probe decompresses one 512-byte
	// block through klauspost; nothing is spooled.
	if f != zrecipe.FormatNone {
		ok, err := startsWithCompressedTarHeader(r, f)
		if err == nil && !ok {
			return decision{kind: KindRaw, reason: ReasonNotTar, format: format}, nil
		}
		// A probe that could not read the stream decides nothing: Analyze
		// runs and reports a corrupt or unsupported stream with its own
		// reason, or fails the upload on a spool I/O error.
	}

	// A stream Detect reports as none is a tar candidate only if it starts
	// with a tar header; decide that before anything is staged so that a
	// config blob never touches the pack writer and keeps reason not-tar.
	if f == zrecipe.FormatNone {
		ok, err := startsWithTarHeader(r)
		if err != nil {
			return decision{}, fmt.Errorf("blob: reading tar header: %w", err)
		}
		if !ok {
			return decision{kind: KindRaw, reason: ReasonNotTar, format: format}, nil
		}
	}

	// Every remaining stream is a tar candidate. Find its compression by
	// decompressing once and eliminating the candidates, under the analyze
	// deadline (spec "Single-pass confirm"). Nothing is staged here: the
	// confirming pass in finalizePrism takes the tar apart while it
	// recompresses the content, so the layer is read and recompressed once.
	actx, cancel := context.WithTimeout(ctx, b.opts.AnalyzeTimeout)
	defer cancel()
	a, err := zrecipe.Start(actx, r, &zrecipe.Options{
		TempDir:     b.spoolDir(),
		MaxInMemory: b.opts.MaxInMemory,
		Parallelism: b.opts.AnalyzeParallelism,
		VerifyLimit: b.opts.VerifyLimit,
	})
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return decision{}, fmt.Errorf("blob: analyze: %w", ctx.Err())
		case errors.Is(err, context.DeadlineExceeded):
			// Only the child deadline can have expired here.
			return decision{kind: KindRaw, reason: ReasonAnalyzeTimeout, err: err, format: format}, nil
		case errors.Is(err, zrecipe.ErrNotReproducible):
			return decision{kind: KindRaw, reason: ReasonNotReproducible, err: err, format: format}, nil
		case errors.Is(err, zrecipe.ErrUnsupported):
			return decision{kind: KindRaw, reason: ReasonUnsupported, err: err, format: format}, nil
		case errors.Is(err, zrecipe.ErrCorrupt):
			return decision{kind: KindRaw, reason: ReasonCorrupt, err: err, format: format}, nil
		default:
			return decision{}, fmt.Errorf("blob: analyze: %w", err)
		}
	}
	dec := decision{kind: KindPrism, analysis: a, src: srcCloser, format: format}
	srcCloser = nil // ownership passes to dec.close
	return dec, nil
}

// stage takes the decompressed tar apart on its own goroutine: it reads
// the stream from p, hashes it with BLAKE3 and sha256, splits it with the
// amber sink over the pack writer w and returns what it left behind. It
// always reads p to its end, so the confirming pass, which writes p, is
// never blocked or failed by the stager; after a failure the rest of the
// stream is discarded and the pack writer aborted.
func (b *Store) stage(ctx context.Context, p *pipe, w *store.Writer) *staged {
	s := &staged{}
	b3 := blake3.New(32, nil)
	s256 := sha256.New()
	counter := &byteCounter{r: io.TeeReader(&streamReader{r: p}, io.MultiWriter(b3, s256))}
	sink := newAmberSink(w)
	defer sink.closeRecipe()
	err := tarprism.DecomposeTo(counter, sink)
	sink.closeRecipe()
	// Read whatever is left of the stream so Analyze never blocks on a
	// full pipe: after a failure the bytes are discarded; after a success
	// there are none, tar-prism reads to EOF, and if it ever stopped short
	// the hashes below would not match params and check would say so.
	io.Copy(io.Discard, p)
	if err == nil {
		s.recipe, s.blobs, err = sink.finish()
	}
	if err == nil {
		if _, cerr := w.Close(); cerr != nil {
			err = &sinkError{cerr}
		}
	}
	if err != nil {
		w.Abort()
		s.err = classifyDecomposeError(ctx, err)
		return s
	}
	s.pack = w.Pack()
	s.index = sink.index
	s.entries = sink.entries
	s.diffID = oci.DigestFromSum(s256.Sum(nil))
	s.blake3 = hex.EncodeToString(b3.Sum(nil))
	s.size = counter.n
	return s
}

// startsWithCompressedTarHeader reports whether the first 512 decompressed
// bytes of r are a tar header block or the stream is an empty archive (see
// probeTar). It rewinds r and reads through a klauspost decompressor for f,
// which pulls only as much of the compressed stream as the probe needs;
// nothing is written to disk. Analyze rewinds r itself (its Detect does),
// so the position left behind does not matter. A stream with fewer than 512
// decompressed bytes is not a tar; any other failure returns an error and
// decides nothing.
func startsWithCompressedTarHeader(r io.ReadSeeker, f zrecipe.Format) (bool, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	dec, err := newDecompressor(f, r)
	if err != nil {
		return false, err
	}
	defer dec.Close()
	return probeTar(dec)
}

// zstdWindowSize parses the zstd frame header of r and returns its declared
// window size, without decompressing anything. It rewinds r first; Analyze
// and the other probes rewind it themselves before use, so the position
// left behind does not matter.
func zstdWindowSize(r io.ReadSeeker) (uint64, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	h, err := cpformat.ParseZstdFrameHeader(bufio.NewReader(r))
	if err != nil {
		return 0, err
	}
	return h.WindowSize, nil
}

// startsWithTarHeader reports whether r begins with a tar header block
// carrying a valid checksum or is an empty archive (see probeTar).
func startsWithTarHeader(r io.ReadSeeker) (bool, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	return probeTar(r)
}

// emptyArchiveMax is the longest all-zero stream accepted as an empty tar
// archive: two zero blocks end an archive and GNU tar pads to its 10 KiB
// record size.
const emptyArchiveMax = 10240

// probeTar reports whether r starts like a tar archive: a header block with
// a valid checksum, or an empty archive, which is nothing but zero blocks
// and at most emptyArchiveMax bytes long. A stream shorter than one block
// is not a tar; a read error decides nothing.
func probeTar(r io.Reader) (bool, error) {
	var hdr [tarHeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return false, nil
		}
		return false, err
	}
	if isTarHeader(hdr[:]) {
		return true, nil
	}
	if !bytes.Equal(hdr[:], make([]byte, tarHeaderSize)) {
		return false, nil
	}
	rest, err := io.ReadAll(io.LimitReader(r, emptyArchiveMax-tarHeaderSize+1))
	if err != nil {
		return false, err
	}
	if len(rest) > emptyArchiveMax-tarHeaderSize {
		return false, nil
	}
	for _, c := range rest {
		if c != 0 {
			return false, nil
		}
	}
	return true, nil
}

// isTarHeader verifies the checksum of a 512-byte tar header: the sum of
// all bytes with the checksum field (148..155) taken as spaces, accepting
// the signed variant some old archivers wrote.
func isTarHeader(b []byte) bool {
	if len(b) < tarHeaderSize {
		return false
	}
	var unsigned, signed int64
	for i, c := range b[:tarHeaderSize] {
		if i >= 148 && i < 156 {
			c = ' '
		}
		unsigned += int64(c)
		signed += int64(int8(c))
	}
	field := strings.Trim(string(b[148:156]), " \x00")
	if field == "" {
		return false
	}
	want, err := strconv.ParseInt(field, 8, 64)
	if err != nil {
		return false
	}
	return want == unsigned || want == signed
}
