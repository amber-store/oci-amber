# Speculative Decompose Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Take a compressed layer apart while zrecipe is still searching for its compression parameters, staging the chunks in a temporary pack file that is inserted into the store as is once the search succeeds and dropped otherwise.

**Architecture:** zrecipe's single inflate is teed through a queued pipe into tar-prism, whose sink writes through a pack-backed `store.Writer` into an unlinked amberpack wire pack under the work directory. When `Analyze` returns params, a live `store.Writer` inserts the pack's records through a new pre-encoded record path in amber-store-core's packstore (dedup, verify, GC barrier and accounting unchanged), then comp.json, the round-trip check and the root proceed as today. Two repositories change: `jobs-build/amber-store-core` (record write path, record iterator) and this one (`store`, `blob`, the stage rename in `importer`/`tui`, docs).

**Tech Stack:** Go 1.26, amber-store-core (packstore, amberpack, fstree), zrecipe v0.4.0 (`Options.Uncompressed`), tar-prism sink API, klauspost zstd through amberpack.

**Spec:** `docs/superpowers/specs/2026-09-05-speculative-decompose-design.md`

## Global Constraints

- No `internal/` packages: every Go package is a flat top-level directory.
- oci-amber tests run inside the dev shell because zrecipe's cgo engines need zlib and zstd: `nix develop --command go test ./...` from `/Users/dragan/draganm/oci-amber`. amber-store-core tests run with plain `go test ./...` from `/Users/dragan/jobs-build/amber-store-core` (its flake provides Go; use `nix develop --command` there too if `go` is not on PATH).
- Every commit message ends with the two trailers below, separated from the body by a blank line:

  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01M2DSRzbKx4Cc2Fbwap4VPP
  ```
- oci-amber work happens on branch `speculative-decompose` (already created from `main`, holds the spec). amber-store-core work happens on a new branch `record-write-path` created from `origin/main` (commit `2d22eef`, which is exactly what oci-amber pins today).
- Delete any binary you build (`go build -o ...`) before finishing a task.
- Doc comments follow the repos' prose style: full sentences, say why, name the spec section when the behaviour comes from it.
- Every observable outcome of `blob.Store.Put` stays the same (kinds, raw reasons, meta.json fields, stats, error classes); the existing blob tests are the contract and must pass unchanged except for the stage rename.

---

### Task 1: amberpack record iterator (`amber-store-core`)

**Files:**
- Modify: `/Users/dragan/jobs-build/amber-store-core/amberpack/pack.go` (the `Reader` section, lines 111-186)
- Test: `/Users/dragan/jobs-build/amber-store-core/amberpack/pack_test.go`

**Interfaces:**
- Produces: `type RawRecord struct { Record; Bytes []byte }` and `func (r *Reader) Records() iter.Seq2[RawRecord, error]`. `All` is rebuilt on top of `Records`. Task 4 consumes `Records` to feed `packstore.Object.Record`.

- [ ] **Step 1: Create the branch**

```bash
cd /Users/dragan/jobs-build/amber-store-core
git checkout -b record-write-path origin/main
git log --oneline -1   # expect 2d22eef Merge pull request #3 ...
```

- [ ] **Step 2: Write the failing tests**

Append to `amberpack/pack_test.go`:

```go
// collectRecords drains Records, returning what it yielded and the error
// that ended it.
func collectRecords(t *testing.T, r *Reader) ([]RawRecord, error) {
	t.Helper()
	var out []RawRecord
	for rec, err := range r.Records() {
		if err != nil {
			return out, err
		}
		out = append(out, rec)
	}
	return out, nil
}

func TestReader_Records_RoundTrip(t *testing.T) {
	// Records yields every record's bytes exactly as EncodeRecord produced
	// them, with its parsed header, and without decoding: a consumer that
	// appends records verbatim (packstore's Object.Record) never touches
	// zstd. Feeding those bytes back through AddRecord must give a stream
	// All decodes to the original objects.
	objs := []fstree.Object{
		mkObj(t, []byte("alpha")),
		mkObj(t, []byte("")),
		mkObj(t, bytes.Repeat([]byte("amber"), 50_000)), // compressed on disk
		mkObj(t, incompressible(4000)),
	}
	var buf bytes.Buffer
	w := NewWriter(&buf)
	want := make([][]byte, len(objs))
	for i, o := range objs {
		rec, err := EncodeRecord(o.Key, o.Bytes)
		if err != nil {
			t.Fatalf("EncodeRecord: %v", err)
		}
		want[i] = rec
		if i%2 == 0 {
			err = w.Add(o)
		} else {
			err = w.AddRecord(rec)
		}
		if err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, err := collectRecords(t, NewReader(bytes.NewReader(buf.Bytes())))
	if err != nil {
		t.Fatalf("Records: %v", err)
	}
	if len(got) != len(objs) {
		t.Fatalf("got %d records, want %d", len(got), len(objs))
	}
	var again bytes.Buffer
	w2 := NewWriter(&again)
	for i, rec := range got {
		if !bytes.Equal(rec.Bytes, want[i]) {
			t.Errorf("record %d bytes differ from EncodeRecord's", i)
		}
		if rec.Key != objs[i].Key {
			t.Errorf("record %d key = %s, want %s", i, rec.Key, objs[i].Key)
		}
		if rec.Ulen != uint32(len(objs[i].Bytes)) {
			t.Errorf("record %d ulen = %d, want %d", i, rec.Ulen, len(objs[i].Bytes))
		}
		if len(rec.Bytes) != RecHeaderSize+int(rec.Slen) {
			t.Errorf("record %d is %d bytes, header says %d", i, len(rec.Bytes), RecHeaderSize+int(rec.Slen))
		}
		if err := w2.AddRecord(rec.Bytes); err != nil {
			t.Fatalf("AddRecord: %v", err)
		}
	}
	if err := w2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	decoded, err := collect(t, NewReader(&again))
	if err != nil {
		t.Fatalf("All over re-added records: %v", err)
	}
	for i, o := range objs {
		if decoded[i].Key != o.Key || !bytes.Equal(decoded[i].Bytes, o.Bytes) {
			t.Errorf("object %d differs after the record round trip", i)
		}
	}
}

func TestReader_Records_Truncated(t *testing.T) {
	// A stream cut before the end marker is malformed for Records exactly
	// as it is for All.
	o := mkObj(t, []byte("alpha"))
	rec, err := EncodeRecord(o.Key, o.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	got, err := collectRecords(t, NewReader(bytes.NewReader(wirePack(rec))))
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v, want ErrMalformed", err)
	}
	if len(got) != 1 {
		t.Fatalf("yielded %d records before the truncation, want 1", len(got))
	}
}

func TestReader_Records_CRCMismatch(t *testing.T) {
	o := mkObj(t, incompressible(64))
	rec, err := EncodeRecord(o.Key, o.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	rec[len(rec)-1] ^= 0x01
	body := append(rec, tagEnd)
	if _, err := collectRecords(t, NewReader(bytes.NewReader(wirePack(body)))); !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v, want ErrMalformed (record CRC mismatch)", err)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `cd /Users/dragan/jobs-build/amber-store-core && go test ./amberpack -run 'TestReader_Records' -v`
Expected: compile error, `undefined: RawRecord` / `r.Records undefined`.

- [ ] **Step 4: Implement `Records` and rebuild `All` on it**

Replace the `Reader` section of `amberpack/pack.go` (from `// Reader decodes a wire pack stream.` to the end of the file) with:

```go
// Reader decodes a wire pack stream.
type Reader struct {
	r io.Reader
}

// NewReader returns a Reader over r.
func NewReader(r io.Reader) *Reader {
	return &Reader{r: r}
}

// RawRecord is one record of a pack as it was read: its parsed header and
// its complete bytes, header and stored payload, exactly as EncodeRecord
// produced them. Bytes is caller-owned.
type RawRecord struct {
	Record
	Bytes []byte
}

// Records iterates over the records in the stream without decoding them.
// Every record is validated exactly as All validates it (framing, the
// size bound, CRC, key canonicality) but its payload stays as stored, so
// a consumer that appends records verbatim (packstore's Object.Record,
// Writer.AddRecord) skips the decompress/recompress round trip; it is the
// read-side counterpart of AddRecord. It yields exactly one error (and
// stops) on any structural problem; on a clean stream it yields every
// record and returns after the end marker. Records must be called at
// most once per Reader because the underlying stream position is not
// reset between calls.
func (r *Reader) Records() iter.Seq2[RawRecord, error] {
	return func(yield func(RawRecord, error) bool) {
		br := bufio.NewReader(r.r)
		var magic [len(packMagic)]byte
		if _, err := io.ReadFull(br, magic[:]); err != nil {
			yield(RawRecord{}, fmt.Errorf("%w: reading magic: %v", ErrMalformed, err))
			return
		}
		if string(magic[:]) != packMagic {
			yield(RawRecord{}, fmt.Errorf("%w: bad magic", ErrMalformed))
			return
		}
		for {
			tag, err := br.ReadByte()
			if err != nil {
				yield(RawRecord{}, fmt.Errorf("%w: truncated before end marker: %v", ErrMalformed, err))
				return
			}
			switch tag {
			case tagEnd:
				return
			case tagChunk:
				// Reassemble the full record — tag + remaining 45 header bytes +
				// slen payload bytes — then validate it with ParseRecord.
				var hdr [RecHeaderSize]byte
				hdr[0] = tag
				if _, err := io.ReadFull(br, hdr[1:]); err != nil {
					yield(RawRecord{}, fmt.Errorf("%w: truncated record header: %v", ErrMalformed, err))
					return
				}
				slen := binary.BigEndian.Uint32(hdr[38:42]) // stored-payload length field
				if slen > MaxPayload {
					yield(RawRecord{}, fmt.Errorf("%w: record payload %d exceeds limit %d", ErrMalformed, slen, MaxPayload))
					return
				}
				full := make([]byte, RecHeaderSize+int(slen))
				copy(full, hdr[:])
				if _, err := io.ReadFull(br, full[RecHeaderSize:]); err != nil {
					yield(RawRecord{}, fmt.Errorf("%w: truncated record payload: %v", ErrMalformed, err))
					return
				}
				rec, err := ParseRecord(full)
				if err != nil {
					yield(RawRecord{}, fmt.Errorf("%w: %v", ErrMalformed, err))
					return
				}
				if !yield(RawRecord{Record: rec, Bytes: full}, nil) {
					return
				}
			default:
				yield(RawRecord{}, fmt.Errorf("%w: bad record tag %#x", ErrMalformed, tag))
				return
			}
		}
	}
}

// All iterates over the objects in the stream: Records with every payload
// decoded. It yields exactly one error (and stops) on any structural
// problem; on a clean stream it yields every object and returns after the
// end marker. All must be called at most once per Reader because the
// underlying stream position is not reset between calls.
func (r *Reader) All() iter.Seq2[fstree.Object, error] {
	return func(yield func(fstree.Object, error) bool) {
		for raw, err := range r.Records() {
			if err != nil {
				yield(fstree.Object{}, err)
				return
			}
			payload, err := DecodePayload(raw.Flags, raw.Ulen, raw.Bytes[RecHeaderSize:])
			if err != nil {
				yield(fstree.Object{}, fmt.Errorf("%w: %v", ErrMalformed, err))
				return
			}
			if !yield(fstree.Object{Key: raw.Key, Bytes: payload}, nil) {
				return
			}
		}
	}
}
```

Also extend the package doc comment's sentence "The Reader validates framing, CRC, and key canonicality and decodes each payload" to "The Reader validates framing, CRC, and key canonicality and decodes each payload (All), or hands the validated records over undecoded (Records)".

- [ ] **Step 5: Run the amberpack tests**

Run: `cd /Users/dragan/jobs-build/amber-store-core && go test ./amberpack -v -run 'Reader|Writer'`
Expected: every test passes, including the pre-existing `TestReader_*` negative tests through the rebuilt `All`.

- [ ] **Step 6: Commit**

```bash
cd /Users/dragan/jobs-build/amber-store-core
gofmt -l ./amberpack   # expect no output
git add amberpack/pack.go amberpack/pack_test.go
git commit -m "amberpack: Reader.Records yields validated records undecoded

The read-side counterpart of Writer.AddRecord: a consumer that appends
records verbatim gets each record's bytes and parsed header without a
decompress/recompress round trip. All is now built on it.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01M2DSRzbKx4Cc2Fbwap4VPP"
```

---

### Task 2: packstore pre-encoded record write path (`amber-store-core`)

**Files:**
- Modify: `/Users/dragan/jobs-build/amber-store-core/packstore/segment.go:33-37` (the `Object` type)
- Create: `/Users/dragan/jobs-build/amber-store-core/packstore/prepare.go`
- Modify: `/Users/dragan/jobs-build/amber-store-core/packstore/parallel.go:109-150` (`runWriter`)
- Modify: `/Users/dragan/jobs-build/amber-store-core/packstore/packstore.go:466-512` (`WriteBatch`)
- Modify: `/Users/dragan/jobs-build/amber-store-core/packstore/gc.go:172-189` (`AppendRecord`)
- Modify: `/Users/dragan/jobs-build/amber-store-core/architecture/amberpack.md` (the "Two checks, two layers" paragraph)
- Test: `/Users/dragan/jobs-build/amber-store-core/packstore/record_test.go` (new)

**Interfaces:**
- Consumes: `amberpack.ParseRecord`, `amberpack.DecodePayload`, `amberpack.RecHeaderSize`, the existing `verifyObject`.
- Produces: `packstore.Object{Key, Data, Record}`; `WriteParallel` and `WriteBatch` accept objects with `Record` set. Task 4 relies on: a `Record` object is appended verbatim, deduped through `Has` and the in-stream seen set, counted in `WriteStats.BytesStored` at its `ulen`, verified (decoded and rehashed) when `WriteOpts.Verify` is on, and a bad record fails the run with an error wrapping `amberpack.ErrCorrupt` (`packstore.ErrCorrupt` is the same sentinel).

- [ ] **Step 1: Write the failing tests**

Create `packstore/record_test.go`:

```go
package packstore

import (
	"bytes"
	"errors"
	"testing"

	"github.com/amber-store/core/amberpack"
)

// recordObj is o as a pre-encoded record: what a caller that already holds
// the record bytes (a staged pack) offers instead of Data.
func recordObj(t *testing.T, o Object) Object {
	t.Helper()
	rec, err := amberpack.EncodeRecord(o.Key, o.Data)
	if err != nil {
		t.Fatal(err)
	}
	return Object{Key: o.Key, Record: rec}
}

func TestWriteParallelRecordsStoredAndReadable(t *testing.T) {
	s := openStore(t, t.TempDir(), WithSegmentSize(32<<10))
	objs := testObjects(t, 60)
	recs := make([]Object, len(objs))
	for i, o := range objs {
		recs[i] = recordObj(t, o)
	}
	batch := append(append([]Object{}, recs...), recs[:7]...) // in-stream dups
	stats, err := s.WriteParallel(objSeq(batch, -1), WriteOpts{Writers: 3, BatchSize: 8 << 10, Verify: true})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Stored != len(objs) || stats.Deduped != 7 {
		t.Fatalf("stats = %+v, want %d stored, 7 deduped", stats, len(objs))
	}
	var wantBytes int64
	for _, o := range objs {
		wantBytes += int64(len(o.Data))
	}
	if stats.BytesStored != wantBytes {
		t.Fatalf("BytesStored = %d, want %d (the records' ulen)", stats.BytesStored, wantBytes)
	}
	for i, o := range objs {
		data, err := s.Get(o.Key)
		if err != nil || !bytes.Equal(data, o.Data) {
			t.Fatalf("Get(%s): %v", o.Key, err)
		}
		// The record went in verbatim: the stored bytes are the offered ones.
		got, err := s.GetRecord(o.Key)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, recs[i].Record) {
			t.Fatalf("object %d: stored record differs from the offered one", i)
		}
	}
}

func TestWriteParallelRecordsDedupAgainstPresent(t *testing.T) {
	s := openStore(t, t.TempDir())
	objs := testObjects(t, 20)
	for _, o := range objs[:10] {
		if err := s.Put(o.Key, o.Data); err != nil {
			t.Fatal(err)
		}
	}
	recs := make([]Object, len(objs))
	for i, o := range objs {
		recs[i] = recordObj(t, o)
	}
	stats, err := s.WriteParallel(objSeq(recs, -1), WriteOpts{Verify: true})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Stored != 10 || stats.Deduped != 10 {
		t.Fatalf("stats = %+v, want 10/10", stats)
	}
}

func TestWriteParallelRecordVerifyCatchesWrongPayload(t *testing.T) {
	// The record is well formed (its CRC is right) but its payload does not
	// hash to its key: only Verify can tell, exactly as for Data.
	s := openStore(t, t.TempDir())
	objs := testObjects(t, 3)
	rec, err := amberpack.EncodeRecord(objs[0].Key, append(bytes.Clone(objs[0].Data), 0xFF))
	if err != nil {
		t.Fatal(err)
	}
	bad := Object{Key: objs[0].Key, Record: rec}
	_, err = s.WriteParallel(objSeq([]Object{recordObj(t, objs[1]), bad}, -1), WriteOpts{Verify: true})
	if !errors.Is(err, ErrVerify) {
		t.Fatalf("err = %v, want ErrVerify", err)
	}
	if has, _ := s.Has(objs[0].Key); has {
		t.Fatal("mismatching record was stored")
	}
	// Without Verify the record is taken on trust, as Data is.
	if _, err := s.WriteParallel(objSeq([]Object{bad}, -1), WriteOpts{}); err != nil {
		t.Fatalf("unverified write: %v", err)
	}
}

func TestWriteParallelRecordCorruptFails(t *testing.T) {
	s := openStore(t, t.TempDir())
	o := recordObj(t, testObjects(t, 1)[0])
	o.Record[len(o.Record)-1] ^= 0x01 // CRC no longer matches
	if _, err := s.WriteParallel(objSeq([]Object{o}, -1), WriteOpts{}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
	if has, _ := s.Has(o.Key); has {
		t.Fatal("corrupt record was stored")
	}
}

func TestWriteParallelRecordKeyMismatchFails(t *testing.T) {
	s := openStore(t, t.TempDir())
	objs := testObjects(t, 2)
	o := recordObj(t, objs[0])
	o.Key = objs[1].Key // record says objs[0], object says objs[1]
	if _, err := s.WriteParallel(objSeq([]Object{o}, -1), WriteOpts{}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

func TestWriteParallelDataAndRecordFails(t *testing.T) {
	s := openStore(t, t.TempDir())
	o := testObjects(t, 1)[0]
	both := recordObj(t, o)
	both.Data = o.Data
	if _, err := s.WriteParallel(objSeq([]Object{both}, -1), WriteOpts{}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

func TestWriteBatchRecords(t *testing.T) {
	s := openStore(t, t.TempDir())
	objs := testObjects(t, 30)
	if err := s.Put(objs[0].Key, objs[0].Data); err != nil {
		t.Fatal(err)
	}
	recs := make([]Object, 0, len(objs)+1)
	for _, o := range objs {
		recs = append(recs, recordObj(t, o))
	}
	recs = append(recs, recs[5]) // in-batch duplicate
	if err := s.WriteBatch(objSeq(recs, -1)); err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		data, err := s.Get(o.Key)
		if err != nil || !bytes.Equal(data, o.Data) {
			t.Fatalf("Get(%s): %v", o.Key, err)
		}
	}
	bad := recordObj(t, testObjects(t, 40)[39])
	bad.Record[len(bad.Record)-1] ^= 0x01
	if err := s.WriteBatch(objSeq([]Object{bad}, -1)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("WriteBatch of a corrupt record: err = %v, want ErrCorrupt", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd /Users/dragan/jobs-build/amber-store-core && go test ./packstore -run 'Record|WriteBatchRecords' -v`
Expected: compile error, `unknown field Record in struct literal`.

- [ ] **Step 3: Extend `Object` and add `prepare`**

In `packstore/segment.go` replace the `Object` type with:

```go
// Object is one CAS object to store: its key and either its serialized
// bytes (Data) or, for an object that was encoded elsewhere, the complete
// record as amberpack.EncodeRecord produced it (Record). Exactly one of
// the two is set. A Record is parsed (framing, CRC, canonical key, key
// equal to Key) and appended verbatim, so a caller that already holds
// encoded records, say a pack it staged on disk, skips the compression
// round trip; with WriteOpts.Verify its payload is decoded and rehashed
// like Data is.
type Object struct {
	Key    key.Key
	Data   []byte
	Record []byte
}
```

Create `packstore/prepare.go`:

```go
package packstore

import (
	"fmt"

	"github.com/amber-store/core/amberpack"
)

// prepare returns the record to append for obj and the payload length the
// write stats charge for it. For Data that is EncodeRecord's output after
// the optional verification; for a pre-encoded Record it is the record
// itself, after ParseRecord (framing, flags, length invariants, CRC,
// canonical key), a check that the record names obj.Key and is exactly one
// record long, and, with verify, a decode and rehash of the payload. Every
// rejection of a Record wraps ErrCorrupt, a verification failure ErrVerify.
func prepare(obj Object, verify bool) ([]byte, int64, error) {
	if obj.Record == nil {
		if verify {
			if err := verifyObject(obj); err != nil {
				return nil, 0, err
			}
		}
		rec, err := amberpack.EncodeRecord(obj.Key, obj.Data)
		if err != nil {
			return nil, 0, err
		}
		return rec, int64(len(obj.Data)), nil
	}
	if obj.Data != nil {
		return nil, 0, fmt.Errorf("%w: object %s carries both Data and Record", ErrCorrupt, obj.Key)
	}
	rec, err := amberpack.ParseRecord(obj.Record)
	if err != nil {
		return nil, 0, err
	}
	if rec.Key != obj.Key {
		return nil, 0, fmt.Errorf("%w: record key %s does not match %s", ErrCorrupt, rec.Key, obj.Key)
	}
	if len(obj.Record) != amberpack.RecHeaderSize+int(rec.Slen) {
		return nil, 0, fmt.Errorf("%w: record is %d bytes, want %d", ErrCorrupt, len(obj.Record), amberpack.RecHeaderSize+int(rec.Slen))
	}
	if verify {
		data, err := amberpack.DecodePayload(rec.Flags, rec.Ulen, obj.Record[amberpack.RecHeaderSize:])
		if err != nil {
			return nil, 0, err
		}
		if err := verifyObject(Object{Key: obj.Key, Data: data}); err != nil {
			return nil, 0, err
		}
	}
	return obj.Record, int64(rec.Ulen), nil
}
```

- [ ] **Step 4: Route the three write paths through `prepare`**

In `packstore/parallel.go`, `runWriter`, replace the block from `if verify {` through `bytesStored.Add(int64(len(obj.Data)))` with:

```go
			rec, ulen, err := prepare(obj, verify)
			if err != nil {
				return err
			}
			if err := s.append(obj.Key, rec, false); err != nil {
				return err
			}
			stored.Add(1)
			bytesStored.Add(ulen)
```

and update the doc comment of `WriteParallel` by appending the sentence: "An Object may carry a pre-encoded Record instead of Data; it is validated and appended as is (see Object)."

In `packstore/packstore.go`, `WriteBatch`, replace

```go
		rec, err := amberpack.EncodeRecord(obj.Key, obj.Data)
		if err != nil {
			return fail(err)
		}
```

with

```go
		rec, _, err := prepare(obj, false)
		if err != nil {
			return fail(err)
		}
```

In `packstore/gc.go`, replace the body of `AppendRecord` with:

```go
func (s *Store) AppendRecord(k key.Key, raw []byte) error {
	rec, _, err := prepare(Object{Key: k, Record: raw}, false)
	if err != nil {
		return err
	}
	return s.append(k, rec, false)
}
```

(`prepare` keeps AppendRecord's two error messages verbatim, so nothing that matched on them changes.) Remove the now-unused `amberpack` import from `gc.go` and `packstore.go` if the compiler reports it.

In `architecture/amberpack.md`, in the paragraph starting "Keeping the hash check out of the codec is deliberate", append: "A record read from a pack can also be appended to a store as it is (packstore's `Object.Record`): the store re-parses it, and with verification decodes and rehashes the payload, so the gate is the same whichever form an object arrives in."

- [ ] **Step 5: Run the packstore and gc tests**

Run: `cd /Users/dragan/jobs-build/amber-store-core && go test ./packstore ./gc ./amberpack`
Expected: all pass (the gc package exercises `AppendRecord` through compaction).

- [ ] **Step 6: Run the whole module and vet**

Run: `cd /Users/dragan/jobs-build/amber-store-core && go vet ./... && go test ./...`
Expected: no vet findings, all packages pass (the `cmd/amber-bench` smoke test takes about 8 s).

- [ ] **Step 7: Commit**

```bash
cd /Users/dragan/jobs-build/amber-store-core
gofmt -l ./packstore   # expect no output
git add packstore/segment.go packstore/prepare.go packstore/parallel.go packstore/packstore.go packstore/gc.go packstore/record_test.go architecture/amberpack.md
git commit -m "packstore: accept pre-encoded records in WriteParallel and WriteBatch

Object gains Record: a complete amberpack record appended verbatim after
ParseRecord and a key check, decoded and rehashed under Verify. The
dedup, barrier, write token and stats are the object path's, so a caller
that staged records in a pack inserts them without re-compressing.
AppendRecord shares the check.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01M2DSRzbKx4Cc2Fbwap4VPP"
```

---

### Task 3: Publish the upstream branch and point oci-amber at it

**Files:**
- Modify: `/Users/dragan/draganm/oci-amber/go.mod`, `go.sum`

**Interfaces:**
- Produces: oci-amber builds against the `record-write-path` branch through a temporary `replace`. Task 7 swaps the `replace` for a pseudo-version pin.

- [ ] **Step 1: Push the branch and open the PR**

```bash
cd /Users/dragan/jobs-build/amber-store-core
git push -u origin record-write-path
gh pr create --title "packstore: pre-encoded record write path; amberpack Reader.Records" --body "$(cat <<'EOF'
Two additions for an embedder that stages objects in a pack file before it knows whether they will be kept (oci-amber's speculative decompose):

- `amberpack.Reader.Records` yields each validated record undecoded, with its parsed header: the read-side counterpart of `Writer.AddRecord`. `All` is rebuilt on it.
- `packstore.Object.Record`: `WriteParallel` and `WriteBatch` accept a complete record and append it verbatim after `ParseRecord` and a key check; `Verify` decodes and rehashes the payload. Dedup, the GC barrier, the write token and the stats are unchanged. `AppendRecord` shares the check.

Tests: `packstore/record_test.go`, `amberpack` Records round trip and negative cases; `go test ./...` passes.

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01M2DSRzbKx4Cc2Fbwap4VPP
EOF
)"
```

Note the PR URL in the task report. Do not merge; merging and the final pin are the user's call (Task 7 pins the branch commit, which needs no merge).

- [ ] **Step 2: Point oci-amber at the local checkout for development**

```bash
cd /Users/dragan/draganm/oci-amber
go mod edit -replace github.com/amber-store/core=/Users/dragan/jobs-build/amber-store-core
nix develop --command go mod tidy
nix develop --command go build ./... && nix develop --command go test ./store ./blob
```

Expected: builds and the two packages' tests pass unchanged (nothing uses the new API yet). Do not commit the `replace` line: it is removed in Task 7. Keep it in the working tree meanwhile and exclude `go.mod`/`go.sum` from every commit until then (`git add` named files only, never `git add -A`).

---

### Task 4: Pack-backed writer and `AddPack` (`store`)

**Files:**
- Modify: `/Users/dragan/draganm/oci-amber/store/write.go`
- Create: `/Users/dragan/draganm/oci-amber/store/pack.go`
- Test: `/Users/dragan/draganm/oci-amber/store/pack_test.go` (new)

**Interfaces:**
- Consumes: `packstore.Object{Key, Data, Record}` (Task 2), `amberpack.Reader.Records` (Task 1), `amberpack.NewWriter`/`AddRecord`/`Close`, `amberpack.EncodeRecord`.
- Produces, for Task 6:
  - `func (s *Store) NewPackWriter(ctx context.Context, dir string) (*Writer, error)`
  - `func (w *Writer) Pack() *Pack` (non-nil only after a successful `Close`)
  - `type Pack`, `func (p *Pack) Size() int64`, `func (p *Pack) Close() error`
  - `func (w *Writer) AddPack(p *Pack, progress func(read int64)) error` on a live writer.
  - Unchanged: `NewWriter`, `Emit`, `PutStream`, `PutBytes`, `NewDir`, `PutXattrs`, `Close`, `Abort`, `Stats`.

- [ ] **Step 1: Write the failing tests**

Create `store/pack_test.go`:

```go
package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/amber-store/core/amberpack"
	"github.com/amber-store/core/fstree"
	"github.com/amber-store/core/key"
)

// packFixture is the content every pack test stages: one file that spans
// several chunks, one small file and one directory naming both.
type packFixture struct {
	big, small []byte
}

func newPackFixture(t *testing.T) packFixture {
	t.Helper()
	return packFixture{big: pseudoRandomBytes(t, 3<<20, 41), small: []byte("a small file")}
}

// stageFixture writes the fixture through w and returns the root keys.
func stageFixture(t *testing.T, w *Writer, fx packFixture) (big, small, dir key.Key) {
	t.Helper()
	var err error
	if big, err = w.PutStream(bytes.NewReader(fx.big)); err != nil {
		t.Fatalf("PutStream: %v", err)
	}
	if small, err = w.PutBytes(fx.small); err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	d := w.NewDir()
	if err := d.AddFile("big", big); err != nil {
		t.Fatal(err)
	}
	if err := d.AddFile("small", small); err != nil {
		t.Fatal(err)
	}
	if dir, err = d.Finish(); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return big, small, dir
}

// assertDirEmpty fails when dir holds any entry: pack files are unlinked
// at creation, so nothing may ever be visible there.
func assertDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("%d files left under %s: %v", len(entries), dir, entries)
	}
}

func TestPackWriterStagesThenAddPackStores(t *testing.T) {
	s := openWriterStore(t)
	dir := t.TempDir()
	fx := newPackFixture(t)

	pw, err := s.NewPackWriter(context.Background(), dir)
	if err != nil {
		t.Fatalf("NewPackWriter: %v", err)
	}
	if pw.Pack() != nil {
		t.Fatal("Pack() before Close is not nil")
	}
	big, small, root := stageFixture(t, pw, fx)
	st, err := pw.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if st.LogicalBytes <= int64(len(fx.big)) || st.NewLogicalBytes != 0 || st.DiskBytes != 0 || st.ObjectsNew != 0 {
		t.Fatalf("pack writer stats = %+v, want LogicalBytes only", st)
	}
	p := pw.Pack()
	if p == nil || p.Size() == 0 {
		t.Fatalf("Pack() = %v after Close, want a non-empty pack", p)
	}
	assertDirEmpty(t, dir)
	for _, k := range []key.Key{big, small, root} {
		if has, _ := s.Has(k); has {
			t.Fatalf("%s reached the store before AddPack", k)
		}
	}

	w := s.NewWriter(context.Background())
	var reports []int64
	if err := w.AddPack(p, func(n int64) { reports = append(reports, n) }); err != nil {
		t.Fatalf("AddPack: %v", err)
	}
	st2, err := w.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(reports) == 0 {
		t.Fatal("AddPack reported no progress")
	}
	for i := 1; i < len(reports); i++ {
		if reports[i] < reports[i-1] {
			t.Fatalf("progress went backwards: %v", reports)
		}
	}
	if last := reports[len(reports)-1]; last > p.Size() {
		t.Fatalf("progress %d exceeds the pack size %d", last, p.Size())
	}
	if got := readFileContent(t, s, big); !bytes.Equal(got, fx.big) {
		t.Error("big file read back differs")
	}
	if got := readFileContent(t, s, small); !bytes.Equal(got, fx.small) {
		t.Error("small file read back differs")
	}
	if k, err := s.LookupKey(root, "big"); err != nil || k != big {
		t.Errorf("Lookup big = %s, %v; want %s", k, err, big)
	}
	if st2.LogicalBytes != st.LogicalBytes {
		t.Errorf("AddPack LogicalBytes = %d, want the pack writer's %d", st2.LogicalBytes, st.LogicalBytes)
	}
	if st2.ObjectsNew == 0 || st2.DiskBytes == 0 || st2.NewLogicalBytes != st2.LogicalBytes {
		t.Errorf("AddPack stats = %+v, want everything new", st2)
	}
	if err := p.Close(); err != nil {
		t.Errorf("Pack.Close: %v", err)
	}
}

func TestAddPackStatsMatchLiveWriter(t *testing.T) {
	// The same objects written live into a second store must cost the
	// same: the records are byte-identical, so keys, disk bytes and object
	// counts agree.
	fx := newPackFixture(t)

	live := openWriterStore(t)
	lw := live.NewWriter(context.Background())
	lbig, lsmall, lroot := stageFixture(t, lw, fx)
	lst, err := lw.Close()
	if err != nil {
		t.Fatal(err)
	}

	staged := openWriterStore(t)
	pw, err := staged.NewPackWriter(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	pbig, psmall, proot := stageFixture(t, pw, fx)
	if _, err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	w := staged.NewWriter(context.Background())
	if err := w.AddPack(pw.Pack(), nil); err != nil {
		t.Fatal(err)
	}
	pst, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	if pbig != lbig || psmall != lsmall || proot != lroot {
		t.Fatalf("keys differ between the pack and the live writer")
	}
	if pst != lst {
		t.Fatalf("AddPack stats %+v differ from the live writer's %+v", pst, lst)
	}
}

func TestAddPackDedupsAgainstPresentObjects(t *testing.T) {
	s := openWriterStore(t)
	fx := newPackFixture(t)
	lw := s.NewWriter(context.Background())
	stageFixture(t, lw, fx)
	first, err := lw.Close()
	if err != nil {
		t.Fatal(err)
	}

	pw, err := s.NewPackWriter(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stageFixture(t, pw, fx)
	if _, err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	w := s.NewWriter(context.Background())
	if err := w.AddPack(pw.Pack(), nil); err != nil {
		t.Fatal(err)
	}
	st, err := w.Close()
	if err != nil {
		t.Fatal(err)
	}
	if st.ObjectsNew != 0 || st.DiskBytes != 0 || st.NewLogicalBytes != 0 {
		t.Errorf("stats = %+v, want nothing new", st)
	}
	if st.ObjectsDeduped != first.ObjectsNew || st.DedupedBytes != st.LogicalBytes {
		t.Errorf("stats = %+v, want every object of the first write deduped (%d)", st, first.ObjectsNew)
	}
}

func TestPackWriterAbortAndCancelReleaseTheFile(t *testing.T) {
	s := openWriterStore(t)
	dir := t.TempDir()

	pw, err := s.NewPackWriter(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pw.PutBytes([]byte("staged then aborted")); err != nil {
		t.Fatal(err)
	}
	pw.Abort()
	pw.Abort() // idempotent
	if pw.Pack() != nil {
		t.Error("Pack() after Abort is not nil")
	}
	if _, err := pw.PutBytes([]byte("after abort")); err == nil {
		t.Error("PutBytes after Abort succeeded")
	}
	if _, err := pw.Close(); !errors.Is(err, errAborted) {
		t.Errorf("Close after Abort: err = %v, want errAborted", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	pw2, err := s.NewPackWriter(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pw2.PutBytes([]byte("before cancel")); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := pw2.Close(); !errors.Is(err, context.Canceled) {
		t.Errorf("Close after cancel: err = %v, want context.Canceled", err)
	}
	if pw2.Pack() != nil {
		t.Error("Pack() after a cancelled Close is not nil")
	}
	assertDirEmpty(t, dir)
}

func TestAddPackRejectsTamperedPack(t *testing.T) {
	s := openWriterStore(t)
	pw, err := s.NewPackWriter(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	k, err := pw.PutBytes(pseudoRandomBytes(t, 4000, 9))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	p := pw.Pack()
	// The last byte is the end marker; the one before it is the last
	// payload byte of the last record, so its CRC no longer matches.
	if _, err := p.f.WriteAt([]byte{0xff}, p.Size()-2); err != nil {
		t.Fatal(err)
	}
	w := s.NewWriter(context.Background())
	err = w.AddPack(p, nil)
	if !errors.Is(err, amberpack.ErrMalformed) {
		t.Fatalf("AddPack: err = %v, want amberpack.ErrMalformed", err)
	}
	if _, err := w.Close(); err == nil {
		t.Fatal("Close after a failed AddPack succeeded")
	}
	if has, _ := s.Has(k); has {
		t.Fatal("object from the tampered pack was stored")
	}
}

func TestAddPackMisuse(t *testing.T) {
	s := openWriterStore(t)
	pw, err := s.NewPackWriter(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pw.PutBytes([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	p := pw.Pack()

	other, err := s.NewPackWriter(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Abort()
	if err := other.AddPack(p, nil); err == nil {
		t.Error("AddPack on a pack writer succeeded")
	}

	w := s.NewWriter(context.Background())
	if err := w.AddPack(p, nil); err != nil {
		t.Fatalf("first AddPack: %v", err)
	}
	if err := w.AddPack(p, nil); err == nil {
		t.Error("second AddPack of the same pack succeeded")
	}
	if err := w.AddPack(nil, nil); err == nil {
		t.Error("AddPack(nil) succeeded")
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPackWriterEmitAfterCloseFails(t *testing.T) {
	s := openWriterStore(t)
	pw, err := s.NewPackWriter(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pw.Close(); err != nil {
		t.Fatal(err)
	}
	obj, err := fstree.EncodeBlob([]byte("late"))
	if err != nil {
		t.Fatal(err)
	}
	if err := pw.Emit(obj); !errors.Is(err, errWriterClosed) {
		t.Errorf("Emit after Close: err = %v, want errWriterClosed", err)
	}
	// An empty pack is a valid pack.
	w := s.NewWriter(context.Background())
	if err := w.AddPack(pw.Pack(), nil); err != nil {
		t.Fatalf("AddPack of an empty pack: %v", err)
	}
	if st, err := w.Close(); err != nil || st != (Stats{}) {
		t.Fatalf("Close = %+v, %v; want zero stats", st, err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd /Users/dragan/draganm/oci-amber && nix develop --command go test ./store -run 'Pack|AddPack' -v`
Expected: compile error, `s.NewPackWriter undefined`.

- [ ] **Step 3: Refactor `store/write.go` over an item channel with a settle step**

Apply these changes to `store/write.go`:

1. Add the queued item type after `emitBuffer`:

```go
// item is one object queued for the backend: a built object (Data) or a
// record staged in a pack (Record), with the payload length the
// accounting charges for it.
type item struct {
	obj     packstore.Object
	logical int64
}
```

2. Replace the `Writer` struct and `NewWriter` with:

```go
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
	logical int64
	seen    map[key.Key]bool // live backend: every key offered -> Has reported it absent
	wstats  packstore.WriteStats
	werr    error

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
```

3. In `objects()`, change the receive to `case it, ok := <-w.ch:`, the account call to `w.account(it)` and the yield to `yield(it.obj, nil)`. Change `account` to:

```go
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
```

4. Replace `Emit` with a public wrapper over an internal `emit`:

```go
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
```

5. Replace `Close`, `finish` and `Abort` with:

```go
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
		ObjectsDeduped:  w.wstats.Deduped,
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
```

Leave `run`, `stoppedErr`, `closeStream`, `byteOpts`, `PutStream`, `addChunk`, `PutBytes`, `PutXattrs` and `writers` as they are (`run` keeps feeding `WriteParallel` from `objects()`).

- [ ] **Step 4: Add the pack backend in `store/pack.go`**

```go
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/amber-store/core/amberpack"
	"github.com/amber-store/core/packstore"
)

// Pack is a staged pack file: the objects a pack Writer received, encoded
// as records in amberpack's wire format, in a temp file that was unlinked
// as soon as it was created (the descriptor keeps it alive; a crash leaves
// nothing behind). A live Writer's AddPack inserts it into the store, at
// most once; Close releases it.
type Pack struct {
	f    *os.File
	size int64
	used bool
}

// Size is the pack file's length in bytes.
func (p *Pack) Size() int64 { return p.size }

// Close releases the file.
func (p *Pack) Close() error { return p.f.Close() }

// NewPackWriter starts a Writer that stages its objects in a pack file
// under dir instead of the store (spec "Store package"): a caller that
// does not yet know whether it will keep them, such as blob's speculative
// decompose, builds exactly what a live Writer would build, and a live
// Writer's AddPack later inserts the pack with the usual dedup,
// verification and accounting, or Close drops it. Objects are encoded on
// writers() goroutines and appended in whatever order they finish; the
// order of records in a pack carries no meaning. Close writes the pack's
// end marker and returns Stats holding LogicalBytes only, since dedup is
// unknown until AddPack; the pack is then available from Pack. Abort, a
// cancelled context or a failed Close release the file.
func (s *Store) NewPackWriter(ctx context.Context, dir string) (*Writer, error) {
	f, err := os.CreateTemp(dir, "pack-*")
	if err != nil {
		return nil, fmt.Errorf("store: creating pack file: %w", err)
	}
	if err := os.Remove(f.Name()); err != nil {
		f.Close()
		return nil, fmt.Errorf("store: unlinking pack file: %w", err)
	}
	w := s.newWriter(ctx)
	w.pack = &Pack{f: f}
	go w.runPack()
	return w, nil
}

// Pack returns the staged pack after a successful Close, and nil before
// Close, after Abort, or when Close reported an error (the file is
// released then).
func (w *Writer) Pack() *Pack {
	if !w.sealed {
		return nil
	}
	return w.pack
}

// runPack is the pack backend's goroutine: it records stage's outcome and
// signals done, exactly as run does for WriteParallel.
func (w *Writer) runPack() {
	defer close(w.done)
	w.werr = w.stage()
}

// stage drains the item channel into the pack file until the channel is
// closed or the context ends. writers() goroutines encode; a mutex
// serializes their appends and the accounting. The first failure cancels
// the others and is returned; the file is left as it is for settle to
// release.
func (w *Writer) stage() error {
	pw := amberpack.NewWriter(w.pack.f)
	ctx, stop := context.WithCancelCause(w.ctx)
	defer stop(nil)
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	for range writers() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				var it item
				select {
				case <-ctx.Done():
					return
				case i, ok := <-w.ch:
					if !ok {
						return
					}
					it = i
				}
				rec, err := amberpack.EncodeRecord(it.obj.Key, it.obj.Data)
				if err != nil {
					stop(err)
					return
				}
				mu.Lock()
				err = pw.AddRecord(rec)
				if err == nil {
					w.logical += it.logical
				}
				mu.Unlock()
				if err != nil {
					stop(fmt.Errorf("store: writing pack: %w", err))
					return
				}
			}
		}()
	}
	wg.Wait()
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if err := pw.Close(); err != nil {
		return fmt.Errorf("store: finishing pack: %w", err)
	}
	fi, err := w.pack.f.Stat()
	if err != nil {
		return fmt.Errorf("store: pack size: %w", err)
	}
	w.pack.size = fi.Size()
	return nil
}

// AddPack offers every record of p to the store through the live Writer's
// pipeline, so dedup, verification and accounting are exactly what a
// freshly built object gets and Stats keep their meaning, with a record's
// uncompressed length counted as its logical bytes. progress, when not
// nil, is called after each record with the pack bytes read so far. A
// malformed pack fails with an error wrapping amberpack.ErrMalformed and
// poisons the Writer, so Close reports it too. Only a live Writer accepts
// a pack, and a pack only once.
func (w *Writer) AddPack(p *Pack, progress func(read int64)) error {
	if w.pack != nil {
		return errors.New("store: AddPack on a pack writer")
	}
	if p == nil {
		return errors.New("store: AddPack: nil pack")
	}
	if p.used {
		return errors.New("store: pack already added")
	}
	p.used = true
	if _, err := p.f.Seek(0, io.SeekStart); err != nil {
		return w.fail(fmt.Errorf("store: rewinding pack: %w", err))
	}
	cr := &countingReader{r: p.f}
	for raw, err := range amberpack.NewReader(cr).Records() {
		if err != nil {
			return w.fail(fmt.Errorf("store: reading pack: %w", err))
		}
		it := item{obj: packstore.Object{Key: raw.Key, Record: raw.Bytes}, logical: int64(raw.Ulen)}
		if err := w.emit(it); err != nil {
			return err
		}
		if progress != nil {
			progress(cr.n)
		}
	}
	return nil
}

// fail poisons the Writer with err and returns it: later Emit calls and
// Close report it.
func (w *Writer) fail(err error) error {
	w.cancel(err)
	return err
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
```

- [ ] **Step 5: Run the store tests**

Run: `cd /Users/dragan/draganm/oci-amber && nix develop --command go test ./store -race`
Expected: every test passes, the pre-existing writer tests (`TestAbortAfterPartialWrites`, `TestCloseWithCancelledContextReturnsContextError`, `TestCloseIsIdempotentAndEmitAfterCloseFails`, `TestVerifyRejectsMismatchedObject`, `TestAbortUnblocksInFlightPutStream`) included.

- [ ] **Step 6: Run the packages that use the writer**

Run: `cd /Users/dragan/draganm/oci-amber && nix develop --command go vet ./... && nix develop --command go test ./blob ./image ./rootfs ./registry`
Expected: all pass; nothing outside `store` changed behaviour.

- [ ] **Step 7: Commit**

```bash
cd /Users/dragan/draganm/oci-amber
nix develop --command gofmt -l ./store   # expect no output
git add store/write.go store/pack.go store/pack_test.go
git commit -m "store: pack-backed Writer and AddPack

NewPackWriter stages objects as amberpack records in an unlinked temp
file; a live Writer's AddPack inserts them through the same accounting
iterator, as pre-encoded records, so dedup, verification and Stats are
unchanged. Close and Abort settle once, releasing a failed pack's file.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01M2DSRzbKx4Cc2Fbwap4VPP"
```

---

### Task 5: Rename the decompose stage to commit and add `observeProgress`

**Files:**
- Modify: `/Users/dragan/draganm/oci-amber/blob/observer.go`
- Modify: `/Users/dragan/draganm/oci-amber/blob/prism.go:355` (the `observeStage(d, StageDecompose)` call)
- Modify: `/Users/dragan/draganm/oci-amber/importer/tracker.go:271`
- Modify: `/Users/dragan/draganm/oci-amber/importer/tracker_test.go:66,67,81,87,89,102,105,109`
- Modify: `/Users/dragan/draganm/oci-amber/tui/view_test.go:23,44`
- Modify: `/Users/dragan/draganm/oci-amber/blob/observer_test.go:75,83,109`

**Interfaces:**
- Produces: `blob.StageCommit Stage = "commit"` (replaces `StageDecompose`); `func (b *Store) observeProgress(d oci.Digest, n int64)`. Task 6 reports commit progress through it.

- [ ] **Step 1: Update the tests first**

In `blob/observer_test.go` replace every `StageDecompose` with `StageCommit`. In `importer/tracker_test.go` replace every `blob.StageDecompose` with `blob.StageCommit` and the words "decompose" in the three `approx` labels and the comment with "commit". In `tui/view_test.go` line 23 replace `blob.StageDecompose` with `blob.StageCommit` and on line 44 the expected string `"decompose"` with `"commit"`.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd /Users/dragan/draganm/oci-amber && nix develop --command go test ./blob ./importer ./tui 2>&1 | head`
Expected: compile errors, `undefined: StageCommit` / `blob.StageCommit`.

- [ ] **Step 3: Rename the stage and add the helper**

In `blob/observer.go` replace the `Stage` doc comment and constants with:

```go
// Stage is one phase of a blob's finalization, in the order Put runs them.
// Analyze always comes first and, for a tar candidate, covers the
// speculative decompose that stages the layer's parts while the engine
// search runs; a prism continues with commit and, when VerifyRoundTrip is
// set, verify; a raw decision or a downgrade ends with raw.
type Stage string

const (
	StageAnalyze Stage = "analyze" // zrecipe pass one, the engine search and the staging
	StageCommit  Stage = "commit"  // inserting the staged pack into the store
	StageVerify  Stage = "verify"  // round-trip check
	StageRaw     Stage = "raw"     // storing the bytes verbatim
)
```

In the `Observer` interface comment replace "in decompose it is the pass-two read position" with "in commit it is the staged pack's bytes read so far, scaled to the size". Add after `observeStage`:

```go
// observeProgress reports n bytes of d handled in the current stage when
// an observer is configured.
func (b *Store) observeProgress(d oci.Digest, n int64) {
	if b.opts.Observer != nil {
		b.opts.Observer.BlobProgress(d, n)
	}
}
```

In `blob/prism.go` change `b.observeStage(d, StageDecompose)` to `b.observeStage(d, StageCommit)` (Task 6 moves the call; for now the name is what matters). In `importer/tracker.go` change `case blob.StageDecompose:` to `case blob.StageCommit:` and the comment above the `default:` branch stays as is.

- [ ] **Step 4: Run the tests**

Run: `cd /Users/dragan/draganm/oci-amber && nix develop --command go test ./blob ./importer ./tui ./cmd/...`
Expected: all pass.

- [ ] **Step 5: Commit**

```bash
cd /Users/dragan/draganm/oci-amber
git add blob/observer.go blob/observer_test.go blob/prism.go importer/tracker.go importer/tracker_test.go tui/view_test.go
git commit -m "blob: rename the decompose stage to commit

The stage after analyze will insert a staged pack rather than take the
layer apart; the tracker's weights and the TUI's stage column follow.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01M2DSRzbKx4Cc2Fbwap4VPP"
```

---

### Task 6: Stage during analyze, commit the pack (`blob`)

**Files:**
- Modify: `/Users/dragan/draganm/oci-amber/blob/analyze.go` (`decision`, `analyze`; new `staged`, `stage`)
- Modify: `/Users/dragan/draganm/oci-amber/blob/prism.go` (remove `ingestPrism`; `finalizePrism`, new `commit`; `spoolReader` renamed `streamReader`)
- Modify: `/Users/dragan/draganm/oci-amber/blob/store.go:330-365` (`Put`)
- Modify: `/Users/dragan/draganm/oci-amber/blob/observer_test.go` (the downgrade test's stages)
- Modify: `/Users/dragan/draganm/oci-amber/blob/prism_test.go:567-626` (two comments naming `ingestPrism`)
- Test: `/Users/dragan/draganm/oci-amber/blob/prism_fallback_test.go` (new tests appended)

**Interfaces:**
- Consumes: `store.NewPackWriter`, `Writer.Pack`, `Writer.AddPack`, `store.Pack` (Task 4); `StageCommit`, `observeProgress` (Task 5); `zrecipe.Options.Uncompressed`; the existing `pipe`, `amberSink`, `byteCounter`, `classifyDecomposeError`, error types.
- Produces: `analyze` returns a `decision` whose `staged` field carries the pack for prisms; `finalizePrism(ctx, dec decision, d oci.Digest, size int64)`; `ingestPrism` and its second inflate are gone.

- [ ] **Step 1: Update the observer test and write the new tests**

In `blob/observer_test.go`, `TestObserverDecomposeDowngrade`: the staging now fails inside analyze, so replace the assertion with

```go
	rec.assertStages(t, int64(len(data)), []Stage{StageAnalyze, StageRaw}, StageAnalyze, StageRaw)
```

and rename the test `TestObserverStagingDowngrade`. In `TestObserverPrismStages` and `TestObserverPrismWithoutVerify` keep `StageCommit` in both the stage list and the reach-size list: the commit stage ends exactly at the size.

Append to `blob/prism_fallback_test.go`:

```go
// TestPutNonReproducibleLeavesNoStagedObjects: a gzip zrecipe cannot
// reproduce is staged speculatively while the search runs, then dropped.
// The raw blob's objects are the only ones that land, the file content
// the tar carried never reaches the store, and no pack file survives.
func TestPutNonReproducibleLeavesNoStagedObjects(t *testing.T) {
	b, st, _ := newTestStore(t, Options{VerifyRoundTrip: true})
	content := textBytes(4096, 21)
	tarData := tarBytes(t, "etc/motd", content)
	data := twoLevelGzip(t, tarData[:len(tarData)/2], tarData[len(tarData)/2:])

	meta := putPrism(t, b, data)
	if meta.Kind != KindRaw || meta.RawReason != ReasonNotReproducible {
		t.Fatalf("kind/reason = %s/%s, want raw/not-reproducible", meta.Kind, meta.RawReason)
	}
	// The 4 KiB file is one chunk, so its content key is EncodeBlob's.
	obj, err := fstree.EncodeBlob(content)
	if err != nil {
		t.Fatal(err)
	}
	if has, _ := st.Has(obj.Key); has {
		t.Fatal("the staged file content reached the store although the blob is raw")
	}
	assertSpoolDirEmpty(t, b)
	got, _ := pullPrism(t, b, meta.Digest)
	if !bytes.Equal(got, data) {
		t.Fatal("pulled bytes differ")
	}
}

// cancelOnProgress is an Observer that cancels a context on the first
// progress report of the analyze stage, standing in for a client that
// goes away while pass one reads the spool.
type cancelOnProgress struct {
	cancel context.CancelFunc
	once   sync.Once
}

func (c *cancelOnProgress) BlobStage(oci.Digest, Stage) {}
func (c *cancelOnProgress) BlobProgress(oci.Digest, int64) {
	c.once.Do(c.cancel)
}

func TestPutCancelledDuringAnalyzeLeavesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	work := filepath.Join(t.TempDir(), "work")
	b, _, _ := newTestStore(t, Options{WorkDir: work, VerifyRoundTrip: true, Observer: &cancelOnProgress{cancel: cancel}})
	data := gzipBytes(t, prismTar(t, prismSmallFiles(t)), gzip.DefaultCompression)
	sp := spoolOf(data)

	_, err := b.Put(ctx, sp)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Put = %v, want context.Canceled", err)
	}
	if ok, _ := b.Exists(oci.DigestOfBytes(data)); ok {
		t.Fatal("blob published despite the cancelled context")
	}
	if _, err := sp.Open(); err != nil {
		t.Fatalf("spool removed after a failed Put: %v", err)
	}
	if n := countFiles(t, work); n != 0 {
		t.Fatalf("%d files left under the work directory", n)
	}
}

func TestPutUnwritableSpoolDirFailsUpload(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	b, _, _ := newTestStore(t, Options{VerifyRoundTrip: true})
	dir := spoolDirOf(b)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	data := gzipBytes(t, prismTar(t, prismSmallFiles(t)), gzip.DefaultCompression)
	sp := spoolOf(data)

	_, err := b.Put(context.Background(), sp)
	if err == nil {
		t.Fatal("Put succeeded although the pack file could not be created")
	}
	if ok, _ := b.Exists(oci.DigestOfBytes(data)); ok {
		t.Fatal("blob published although the upload failed")
	}
	if _, err := sp.Open(); err != nil {
		t.Fatalf("spool removed after a failed Put: %v", err)
	}
}

func TestPutUncompressedNonTarNeverStages(t *testing.T) {
	rec := &recorder{}
	b, _, _ := newTestStore(t, Options{VerifyRoundTrip: true, Observer: rec})
	data := []byte(`{"architecture":"arm64","os":"linux"}`) // a config blob
	meta := putPrism(t, b, data)
	if meta.Kind != KindRaw || meta.RawReason != ReasonNotTar || meta.Format != "none" {
		t.Fatalf("meta = %+v, want raw/not-tar/none", *meta)
	}
	rec.assertStages(t, int64(len(data)), []Stage{StageAnalyze, StageRaw}, StageRaw)
	assertSpoolDirEmpty(t, b)
}
```

Add the imports the file needs (`os`, `sync`, `github.com/amber-store/core/fstree`) next to the existing ones.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd /Users/dragan/draganm/oci-amber && nix develop --command go test ./blob -run 'TestObserver|TestPutNonReproducibleLeavesNoStagedObjects|TestPutCancelledDuringAnalyze|TestPutUnwritableSpoolDir|TestPutUncompressedNonTar' -v`
Expected: `TestObserverPrismStages` and `TestObserverStagingDowngrade` fail on the stage sequence (`[analyze decompose ...]` vs the expected), `TestPutNonReproducibleLeavesNoStagedObjects` passes for the wrong reason (nothing is staged yet; that is fine), `TestPutCancelledDuringAnalyzeLeavesNothing` may fail on the work directory check, `TestPutUnwritableSpoolDirFailsUpload` fails because today's Put succeeds (zrecipe keeps this small blob in memory).

- [ ] **Step 3: Stage in `analyze`**

In `blob/analyze.go`:

1. Add `staged` to `decision` and define the staging types after it:

```go
// decision is the outcome of pass one: how the blob will be stored.
type decision struct {
	kind   Kind
	reason RawReason       // set for KindRaw
	params *zrecipe.Params // set for KindPrism
	format string          // "gzip" | "zstd" | "none", always set
	staged *staged         // set for KindPrism: what the speculative decompose left
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
```

2. Replace the part of `analyze` from `actx, cancel := context.WithTimeout(...)` to the end of the function with:

```go
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

	// Every remaining stream is a tar candidate. Its decompressed form is
	// taken apart and staged in a pack while Analyze inflates and searches
	// it (spec "Speculative decompose"): the pack is inserted into the
	// store only once params are known and dropped on every other outcome.
	pw, err := b.st.NewPackWriter(ctx, b.spoolDir())
	if err != nil {
		return decision{}, fmt.Errorf("blob: %w", err)
	}
	p := newPipe(stagePipeSlots)
	done := make(chan *staged, 1)
	go func() { done <- b.stage(ctx, p, pw) }()

	actx, cancel := context.WithTimeout(ctx, b.opts.AnalyzeTimeout)
	defer cancel()
	params, err := zrecipe.Analyze(actx, r, &zrecipe.Options{
		TempDir:      b.spoolDir(),
		MaxInMemory:  b.opts.MaxInMemory,
		Parallelism:  b.opts.AnalyzeParallelism,
		Uncompressed: p,
	})
	// Analyze has written everything it will: end the stream for the
	// stager (its error, so a failed Analyze makes the stager's next read
	// fail instead of reporting a clean end) and collect the stager.
	p.CloseWrite(err)
	s := <-done
	if err != nil {
		s.drop()
		if s.err != nil {
			b.log.Debug("staging discarded", "digest", sp.Digest(), "error", s.err)
		}
		switch {
		case ctx.Err() != nil:
			return decision{}, fmt.Errorf("blob: analyze: %w", ctx.Err())
		case errors.Is(err, context.DeadlineExceeded):
			// Only the child deadline can have expired here.
			return decision{kind: KindRaw, reason: ReasonAnalyzeTimeout, format: format}, nil
		case errors.Is(err, zrecipe.ErrNotReproducible):
			return decision{kind: KindRaw, reason: ReasonNotReproducible, format: format}, nil
		case errors.Is(err, zrecipe.ErrUnsupported):
			return decision{kind: KindRaw, reason: ReasonUnsupported, format: format}, nil
		case errors.Is(err, zrecipe.ErrCorrupt):
			return decision{kind: KindRaw, reason: ReasonCorrupt, format: format}, nil
		default:
			return decision{}, fmt.Errorf("blob: analyze: %w", err)
		}
	}
	// Analyze does not observe ctx on uncompressed input.
	if err := ctx.Err(); err != nil {
		s.drop()
		return decision{}, fmt.Errorf("blob: analyze: %w", err)
	}
	return decision{kind: KindPrism, params: params, format: string(params.Format), staged: s}, nil
}

// stage runs the speculative decompose on its own goroutine: it reads the
// decompressed stream from p, hashes it with BLAKE3 and sha256, takes the
// tar apart with the amber sink over the pack writer w and returns what
// it left behind. It always reads p to its end, so Analyze, which writes
// p, is never blocked or failed by the stager; after a failure the rest
// of the stream is discarded and the pack writer aborted.
func (b *Store) stage(ctx context.Context, p *pipe, w *store.Writer) *staged {
	s := &staged{}
	b3 := blake3.New(32, nil)
	s256 := sha256.New()
	counter := &byteCounter{r: io.TeeReader(&streamReader{r: p}, io.MultiWriter(b3, s256))}
	sink := newAmberSink(w)
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
```

Delete the old post-Analyze `if params.Format == zrecipe.FormatNone { ... startsWithTarHeader ... }` block (it moved before Analyze). Add the imports `crypto/sha256`, `encoding/hex`, `github.com/amber-store/core/key`, `lukechampine.com/blake3`, `tarprism "github.com/draganm/tar-prism"`, `github.com/amber-store/oci-amber/oci`, `github.com/amber-store/oci-amber/store`.

Update the `analyze` doc comment to: "analyze runs zrecipe's first pass under the analyze deadline while the speculative decompose stages the stream (spec "Speculative decompose"), and classifies the result. It returns an error only for failures that must fail the upload: the request context ended, an I/O error, an unexpected zrecipe error, a pack file that could not be created. Every fallback case is a raw decision carrying its reason; a prism decision carries the staged pack, which the caller must drop or commit."

- [ ] **Step 4: Commit the pack in `prism.go`**

In `blob/prism.go`:

1. Rename `spoolReader` to `streamReader` (type, constructor uses, comment) with the comment: "streamReader tags errors coming from the stream Analyze feeds the stager, so they can be told apart from tar-prism and sink errors after DecomposeTo returns. tar-prism's bufio and io.TeeReader pass the underlying reader's error through unchanged."

2. Delete `ingestPrism` entirely. Keep `newDecompressor` (the compressed tar-header probe uses it), `byteCounter`, `readError`, `sinkError`, `decomposeError`, `rawFallback`, `classifyDecomposeError`, the sink types and `roundTripCheck`.

3. Replace `finalizePrism` with:

```go
// finalizePrism commits a staged prism (spec "Blob orchestration"): the
// staging outcome is judged first, then the pack and comp.json go into the
// store through one accounting writer, then, when VerifyRoundTrip is set,
// the pull pipeline runs over the fresh objects. It returns a *rawFallback
// when the spec stores the blob raw instead (decompose-failed,
// roundtrip-failed; both logged at error level), the context's error when
// the request went away, and any other error when the upload must fail.
// Objects written before a failure are left to GC; the pack is released
// on every path.
func (b *Store) finalizePrism(ctx context.Context, dec decision, d oci.Digest, size int64) (prismResult, store.Stats, error) {
	s, params := dec.staged, dec.params
	if s == nil || params == nil {
		return prismResult{}, store.Stats{}, errors.New("blob: prism decision without params or a staged pack")
	}
	defer s.drop()
	if err := s.check(params); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return prismResult{}, store.Stats{}, cerr
		}
		var de *decomposeError
		if errors.As(err, &de) {
			b.log.Error("decompose failed, storing raw", "digest", d, "format", params.Format, "engine", params.Engine, "error", err)
			return prismResult{}, store.Stats{}, &rawFallback{reason: ReasonDecomposeFailed, err: err}
		}
		return prismResult{}, store.Stats{}, err
	}
	b.observeStage(d, StageCommit)
	w := b.st.NewWriter(ctx)
	res, err := b.commit(w, s, params, d, size)
	if err != nil {
		w.Abort()
		if cerr := ctx.Err(); cerr != nil {
			return prismResult{}, store.Stats{}, cerr
		}
		return prismResult{}, store.Stats{}, err
	}
	stats, err := w.Close()
	if err != nil {
		return prismResult{}, store.Stats{}, err
	}
	if b.opts.VerifyRoundTrip {
		b.observeStage(d, StageVerify)
		// Read comp.json back from the store rather than reusing the
		// in-memory params pass one produced: the check must exercise
		// exactly the bytes a real pull will read (spec I5).
		storedParams, err := b.readCompParams(res.comp)
		if err != nil {
			return prismResult{}, store.Stats{}, fmt.Errorf("blob: reading back %s for round-trip check: %w", CompFile, err)
		}
		src := &Prism{st: b.st, recipe: res.recipe, index: res.index, blobs: res.blobs}
		if err := roundTripCheck(ctx, b, src, storedParams, d); err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return prismResult{}, store.Stats{}, cerr
			}
			b.log.Error("round-trip verification failed, storing raw", "digest", d, "format", params.Format, "engine", params.Engine, "engine_version", params.EngineVersion, "error", err)
			return prismResult{}, store.Stats{}, &rawFallback{reason: ReasonRoundTripFailed, err: err}
		}
	}
	return res, stats, nil
}

// commit inserts the staged pack and comp.json through w. Progress is the
// pack bytes read scaled to the blob's compressed size, so the commit
// stage ends exactly at size whatever the pack's length.
func (b *Store) commit(w *store.Writer, s *staged, params *zrecipe.Params, d oci.Digest, size int64) (prismResult, error) {
	var progress func(int64)
	if b.opts.Observer != nil && s.pack.Size() > 0 {
		packSize := float64(s.pack.Size())
		progress = func(read int64) {
			b.observeProgress(d, min(size, int64(float64(read)/packSize*float64(size))))
		}
	}
	if err := w.AddPack(s.pack, progress); err != nil {
		return prismResult{}, fmt.Errorf("blob: inserting staged pack: %w", err)
	}
	var comp bytes.Buffer
	if err := params.Write(&comp); err != nil {
		return prismResult{}, fmt.Errorf("blob: encoding %s: %w", CompFile, err)
	}
	compKey, err := w.PutBytes(comp.Bytes())
	if err != nil {
		return prismResult{}, fmt.Errorf("blob: storing %s: %w", CompFile, err)
	}
	b.observeProgress(d, size)
	return prismResult{
		recipe:           s.recipe,
		index:            s.index,
		blobs:            s.blobs,
		comp:             compKey,
		entries:          s.entries,
		diffID:           s.diffID,
		uncompressedSize: s.size,
	}, nil
}
```

Update the `prismResult` comment's first line to "prismResult is what the commit leaves in the store before the blob root is built". Update `decomposeError`'s comment to "decomposeError reports that the decompressed stream could not be taken apart: tar-prism rejected it, or the stream the stager hashed differs from what pass one recorded." Update `amberSink`'s `closeRecipe` comment: replace "ingestPrism can defer this right after newAmberSink" with "stage calls this right after DecomposeTo returns". Remove the imports `prism.go` no longer needs (`encoding/hex`, `lukechampine.com/blake3`, `github.com/amber-store/oci-amber/upload`); `crypto/sha256` stays for `roundTripCheck`, `bytes` for `commit`.

- [ ] **Step 5: Wire `Put`**

In `blob/store.go`, `Put`: after `dec, err := b.analyze(ctx, sp)` / its error check, add `defer dec.staged.drop()`. Replace the `case KindPrism:` block's head

```go
		if dec.params == nil {
			return nil, errors.New("blob: prism decision without params")
		}
		// Steps 6 and 8: pass two and the round-trip check.
		res, stats, perr := b.finalizePrism(ctx, sp, dec.params, d)
```

with

```go
		// Commit the staged pack, then the round-trip check.
		res, stats, perr := b.finalizePrism(ctx, dec, d, size)
```

Update `Put`'s doc comment: replace "analyze and classify, prism or raw ingest through the accounting writer" with "analyze, stage and classify, then the staged pack's commit or the raw ingest through the accounting writer".

- [ ] **Step 6: Fix the two test comments**

In `blob/prism_test.go` replace, in the comment above `TestRecipeWriterCloseIsIdempotent`, "ingestPrism now defers sink.closeRecipe() right after newAmberSink" with "stage calls sink.closeRecipe() right after DecomposeTo returns", and in the comment inside `TestAmberSinkCloseRecipeUnblocksPutStreamGoroutine` replace "stands in for ingestPrism's deferred call" with "stands in for stage's call".

- [ ] **Step 7: Run the blob tests**

Run: `cd /Users/dragan/draganm/oci-amber && nix develop --command go vet ./blob && nix develop --command go test ./blob -race -count=1`
Expected: every test passes, including the whole pre-existing prism, fallback, put, pull and observer suites and the four new tests. If `TestPutPgzipLayer` or `TestPutSystemGzipTarIsPrism` skip for a missing tool, that is unchanged from before.

- [ ] **Step 8: Run everything**

Run: `cd /Users/dragan/draganm/oci-amber && nix develop --command go test ./... -count=1`
Expected: all packages pass, including the registry end-to-end tests and the crane smoke test in `cmd/oci-amber`.

- [ ] **Step 9: Commit**

```bash
cd /Users/dragan/draganm/oci-amber
nix develop --command gofmt -l ./blob   # expect no output
git add blob/analyze.go blob/prism.go blob/store.go blob/observer_test.go blob/prism_test.go blob/prism_fallback_test.go
git commit -m "blob: decompose speculatively during pass one

zrecipe's single inflate is teed into tar-prism and a pack-backed store
writer while the engine search runs; the pack is inserted once params
are known and dropped on every other outcome, so a raw blob never leaves
objects behind. The second inflate and ingestPrism are gone; the
uncompressed tar probe runs before Analyze so a config blob never stages.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01M2DSRzbKx4Cc2Fbwap4VPP"
```

---

### Task 7: Docs, the dependency pin and the full verification

**Files:**
- Modify: `/Users/dragan/draganm/oci-amber/docs/superpowers/specs/2026-09-03-oci-amber-design.md:51,70,286-320,618`
- Modify: `/Users/dragan/draganm/oci-amber/docs/followups.md` (the "ingestion performance" section)
- Modify: `/Users/dragan/draganm/oci-amber/README.md` (the paragraph around line 410 if it describes two passes; otherwise unchanged)
- Modify: `/Users/dragan/draganm/oci-amber/go.mod`, `go.sum`

- [ ] **Step 1: Update the original design spec**

In `docs/superpowers/specs/2026-09-03-oci-amber-design.md`:

- Line 51 (the blake3 row of the dependency table): change the purpose to "hashing the staged stream against zrecipe's recorded digest".
- Line 70: change "push (analyze, decompose, ingest)" to "push (analyze with speculative decompose, commit)".
- Replace steps 4 to 6 of "Blob finalization" with:

```markdown
4. **Pass one: analyze and stage.** After the pre-checks (the zstd window
   bound, the compressed tar-header probe, and the uncompressed
   tar-header probe for streams `Detect` reports as `none`; each decides
   raw `not-tar` or `unsupported` without staging anything),
   `zrecipe.Analyze(ctx, spool, &Options{TempDir: <work-dir>/spool,
   MaxInMemory: --max-in-memory, Parallelism: --analyze-parallelism
   (default 2), Uncompressed: pipe})` runs under a child context with
   `--analyze-timeout` (default 15 min). The pipe carries the decompressed
   stream to a goroutine that hashes it (BLAKE3, sha256, length) and runs
   `tarprism.DecomposeTo` with the amber sink over a pack-backed
   `store.Writer`: the recipe, the index and every file content are built
   exactly as for the store and encoded as records into an unlinked temp
   pack under `<work-dir>/spool`. The stager always reads the pipe to its
   end, so it never blocks or fails `Analyze`. See
   `2026-09-05-speculative-decompose-design.md`.
5. **Classify.**
   - `Params.Format` gzip, zstd or none with staging succeeded: prism
     candidate. The staged stream's BLAKE3 and length must equal
     `Params.Uncompressed`.
   - `Params` found but tar-prism rejected the stream, or the digests
     differ: raw with reason `decompose-failed` (error-level log). A pack
     write failure fails the upload with `500`, as an amber write failure
     does.
   - `ErrNotReproducible`, `ErrUnsupported`, `ErrCorrupt`: raw with the
     matching reason; the pack is dropped.
   - Child deadline exceeded while the request context is still live: raw
     with reason `analyze-timeout`; the pack is dropped.
   - Any other error, including the request context being cancelled: the
     upload fails with `500`, the session is put back so the client may
     retry the PUT, and nothing is stored.
6. **Commit** (prism candidates). One accounting writer inserts the pack's
   records into the store (`AddPack`: dedup against the store and within
   the pack, verification, the GC barrier, exactly as freshly built
   objects get) and stores comp.json; its stats become the blob's. The
   sha256 of the staged stream becomes `diffId`. A store failure abandons
   the objects written so far to GC and fails the upload.
```

- In the error table row at line 618 replace "second-pass digest mismatch, tar-prism decompose error" with "staged-stream digest mismatch, tar-prism decompose error".

- [ ] **Step 2: Update the follow-ups**

In `docs/followups.md`, "ingestion performance" section: replace the bullet starting "Speculative pass two during pass one" with:

```markdown
- Speculative decompose during pass one: done (spec
  `2026-09-05-speculative-decompose-design.md`); the second inflate is
  gone and chunking overlaps the search. Left open from it: skipping
  chunks already in the store at staging time (needs the collector's
  write barrier around the staging window), and the pack format making a
  staged blob transferable to a remote store.
```

Leave the measurement bullet for Task 8.

- [ ] **Step 3: Check the README**

Run `grep -n "two passes\|pass one\|pass two\|second pass" README.md`. If nothing matches, the README needs no change (it describes the outcome, not the passes). If a line matches, rewrite it to say the layer is inflated once and taken apart while the compression search runs.

- [ ] **Step 4: Pin the upstream branch commit instead of the replace**

```bash
cd /Users/dragan/jobs-build/amber-store-core && git rev-parse HEAD   # the record-write-path tip, pushed in Task 3
cd /Users/dragan/draganm/oci-amber
go mod edit -dropreplace github.com/amber-store/core
nix develop --command go get github.com/amber-store/core@<that sha>
nix develop --command go mod tidy
git diff go.mod   # expect only the amber-store-core line changed, to a v0.0.3-0.<date>-<sha> pseudo-version, and no replace
```

If `go get` cannot see the commit (the module proxy has not indexed the branch yet), run it with `GOPROXY=direct GONOSUMDB=github.com/amber-store/core` or `GOFLAGS=-mod=mod GOPRIVATE=github.com/jobs-build`.

- [ ] **Step 5: Full verification**

```bash
cd /Users/dragan/draganm/oci-amber
nix develop --command go vet ./...
nix develop --command go test ./... -race -count=1
git status --short   # only the intended files, no binaries
```

Expected: no vet findings, everything passes.

- [ ] **Step 6: Commit**

```bash
cd /Users/dragan/draganm/oci-amber
git add docs/superpowers/specs/2026-09-03-oci-amber-design.md docs/followups.md README.md go.mod go.sum
git commit -m "docs, deps: one-pass finalization; amber-store-core with the record write path

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01M2DSRzbKx4Cc2Fbwap4VPP"
```

---

### Task 8: Measure on real layers and open the PR

**Files:**
- Modify: `/Users/dragan/draganm/oci-amber/docs/followups.md` (measurement bullet)

- [ ] **Step 1: Build both binaries**

```bash
cd /Users/dragan/draganm/oci-amber
git stash list >/dev/null
git worktree add /tmp/oci-amber-main main
(cd /tmp/oci-amber-main && nix develop --command go build -o /tmp/oci-amber-before ./cmd/oci-amber)
nix develop --command go build -o /tmp/oci-amber-after ./cmd/oci-amber
```

- [ ] **Step 2: Get an image whose layers are prisms**

Docker Desktop's containerd store keeps Docker Hub's original compressed blobs, so `docker save` yields them unchanged. Pick an image built with Go's compressor or pigz (the import report's "N stored (M prism, ...)" line says how many layers became prisms; the official `busybox` image is known not to reproduce, so avoid it). Try `golang:1.22-bookworm` first, then `python:3.12-slim`:

```bash
docker pull --platform linux/arm64 golang:1.22-bookworm
docker save golang:1.22-bookworm -o /tmp/golang.tar
```

- [ ] **Step 3: Import with each binary, fresh stores**

```bash
rm -rf /tmp/bench-before /tmp/bench-after
/tmp/oci-amber-before import --store /tmp/bench-before --progress plain --log-file /tmp/before.log /tmp/golang.tar
/tmp/oci-amber-after  import --store /tmp/bench-after  --progress plain --log-file /tmp/after.log  /tmp/golang.tar
grep -o 'digest=sha256:[0-9a-f]\{12\}\|kind=[a-z]*\|size=[0-9]*\|duration=[0-9.]*[a-zµ]*' /tmp/before.log | paste - - - - | column -t
grep -o 'digest=sha256:[0-9a-f]\{12\}\|kind=[a-z]*\|size=[0-9]*\|duration=[0-9.]*[a-zµ]*' /tmp/after.log  | paste - - - - | column -t
```

Expected: at least one layer stored as `kind=prism` (otherwise pick another image); the `after` durations of prism layers lower than `before` by roughly pass two's share (about 15 % on the 61 MiB go-flate reference layer, more on layers where inflate is slower than the search).

- [ ] **Step 4: Record the numbers**

In `docs/followups.md`, "ingestion performance", add a bullet:

```markdown
- Measured 2026-09-05 after the speculative decompose, `oci-amber import`
  of `<image>` (arm64), fresh store each time: <layer digest prefix>
  (<size>, <format>, <engine>) <before> s before, <after> s after; total
  import <before> s to <after> s. Remaining per-layer cost is the search
  and the round-trip recompression.
```

with the actual values. Then clean up:

```bash
rm -f /tmp/oci-amber-before /tmp/oci-amber-after /tmp/golang.tar /tmp/before.log /tmp/after.log
rm -rf /tmp/bench-before /tmp/bench-after
git worktree remove /tmp/oci-amber-main
```

- [ ] **Step 5: Commit and open the PR**

```bash
cd /Users/dragan/draganm/oci-amber
git add docs/followups.md
git commit -m "docs: speculative decompose measurements

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01M2DSRzbKx4Cc2Fbwap4VPP"
git push -u origin speculative-decompose
gh pr create --title "Speculative decompose during pass one" --body "$(cat <<'EOF'
Blob finalization inflates a compressed layer once: zrecipe's first pass is teed into tar-prism and a pack-backed store writer, so chunking and record encoding overlap the engine search. The staged pack is inserted into the store as pre-encoded records once params are known (dedup, verification, GC barrier and stats unchanged) and dropped on every other outcome, so a raw blob never leaves objects behind.

Spec: `docs/superpowers/specs/2026-09-05-speculative-decompose-design.md`. Plan: `docs/superpowers/plans/2026-09-05-speculative-decompose.md`.

- `store`: `NewPackWriter`, `Pack`, `Writer.AddPack`; the writer settles once on Close or Abort.
- `blob`: staging inside `analyze`, `finalizePrism` commits; `ingestPrism` and the second inflate are gone; the uncompressed tar probe runs before Analyze; stage `decompose` renamed `commit`.
- Depends on jobs-build/amber-store-core `record-write-path` (<upstream PR URL>): `packstore.Object.Record` and `amberpack.Reader.Records`. go.mod pins that branch's commit; re-pin to the merge commit once it lands.

Measurements are in `docs/followups.md` ("ingestion performance").

🤖 Generated with [Claude Code](https://claude.com/claude-code)

https://claude.ai/code/session_01M2DSRzbKx4Cc2Fbwap4VPP
EOF
)"
```

Replace `<upstream PR URL>` with the URL from Task 3.
