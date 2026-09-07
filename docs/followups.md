# Follow-ups deferred from the implementation reviews

Collected from the per-task and whole-branch reviews of the initial implementation (2026-09-03/04). None of these blocks the current behaviour; the whole-branch review triaged them as "can wait". Items are grouped by the area they were found in.

## Upstream (outside this repository)

- tar-prism: `sink-source-api` is pinned as a pseudo-version of an unmerged branch; merge and tag it, then re-pin. Minor notes from its review: `DecomposeTo` closes the recipe writer through explicit calls rather than a defer; a sink or source returning `(nil, nil)` panics instead of erroring; a final `(n>0, non-EOF err)` read is treated as an error even when all bytes were consumed; the compose loop's error label says "copying recipe" for writer failures.
- zrecipe (formerly comp-prysm): the zlib level-0 feeding bug that made `Analyze` accept streams `Recompress` could not rebuild (`docs/zrecipe-zlib-level0-roundtrip.md`) is fixed in v0.2.0, which oci-amber now pins; the crane smoke test asserts the affected layer is a prism. Still open upstream: no option to bound the pass-one zstd decoder window (oci-amber pre-checks the frame header instead).

## By area

### oci (Task 2)

- untyped manifest with both config/layers and manifests classifies as index; no test pins it (oci/manifest.go:103)
- BlobDescriptors shallow-copies Annotations maps (oci/manifest.go:157)
- "not valid JSON" message also fires on type errors (oci/manifest.go:55)
- Algorithm()/Hex() on an unparsed Digest degrade silently (oci/digest.go:69)
- TestDigestOfBytes/TestIsDigest loop without t.Run

### store (Task 3)

- shutdown order expressed only via errors.Join argument order (store/store.go:213)
- config decode accepts trailing JSON after the first value (store/store.go:158)
- writeConfig does not fsync the parent dir; fixed .tmp name written before the packstore flock (store/store.go:181-201)
- Has returns unwrapped packstore errors while Get wraps (store/store.go:236)
- GC-interval test never exercises a real cycle; several tests open with nil Logger and print to stderr (store/store_test.go:125,141,162,230)
- ListRefs fails wholesale on one undecodable record; untested (store/refs.go:103)

### store writer (Task 4)

- a dropped Writer leaks its run goroutine until Abort/Close — later tasks must `defer w.Abort()` (store/write.go:99)
- emitBuffer memory comment undercounts packstore's own in-flight buffers (store/write.go:64)
- Dir not poisoned after a failed AddEntry; type check precedes done check (store/dir.go:36-71)
- generic identifier writers() (store/write.go:115)
- coverage gaps: Close with zero objects, PutStream at exactly MaxSize/MaxSize+1, prefix names in Dir, Abort concurrent with Close; 50ms sleep in TestAbortUnblocksInFlightPutStream

### store reader (Task 5)

- Skip on a directory key with large n succeeds silently; type check should precede the length comparison (store/read.go:187)
- Get-count test helper tallies Blob fetches only, not index nodes (store/read_test.go:99)
- inconsistent error wrapping (bare fstree errors from descend/Lookup/ListDir/WriteContent)
- LookupKey "has no content key" message also covers a malformed key (store/read.go:227)
- ListDir limit<=0 accumulates all entries; comment overstates the memory bound (store/read.go:232)
- untested: zero-length Read buffer, empty directory, ReadFile length-mismatch guard
- Reader guards use a different wording than ReadFile's length-mismatch error (store/read.go:40 vs 115,185)

### upload (Task 6)

- Spool.Remove races Open (no sync; doc says Open is concurrent) (upload/spool.go:57-78)
- errSpoolRemoved/errClosed unexported; Open on a removed session file returns a raw PathError (upload/spool.go:26, manager.go:15)
- Manager.Remove takes s.mu via Offset() just for a log field (upload/manager.go:157)
- sweeper passes the ticker's stale timestamp to Sweep (upload/manager.go:108)
- untested: zero-byte Append, exactly-one-over threshold, writer error mid-copy, Sweep racing Append; brittle "count=3" log assertion
- active() rebuilds time.Time from UnixNano, losing the monotonic reading; idle timeout is wall-clock sensitive (upload/session.go:171)
- upload/TestBackgroundSweeper flaked once under whole-repo -race, passes on rerun — timing-sensitive test

### blob raw path (Task 7)

- finalizeRaw/writeRoot do not call Abort when Close fails (harmless: Close releases everything) (blob/store.go:383,411)
- no test for failures after the accounting writer started (abort branches, Publish failure) (blob/store.go:380,408)
- readMeta error paths untested (missing meta.json, non-directory root)
- Delete blocks on the per-digest lock without ctx during a finalize; range responses are served without the sha256 tee (blob/store.go:160, pull.go:58)

### blob prism path (Task 8)

- "amber write errors never fall back to raw" has no fault-injection test (blob/prism.go:317-324, 169-172)
- no test for a tar with zero regular files (empty blobs/)
- compose failure coinciding with client disconnect is reported as cancellation (blob/prism.go:475)
- round-trip failure leaves the decomposed objects committed and unreferenced for GC (blob/prism.go:332)
- roundTripCheck package var swapped by tests without sync; empty `case KindRaw:` (blob/store.go:377)

### image (Task 9)

- Put doc claims nothing published on error, false after a tag/referrer publish failure (image/store.go:118)
- TakeRecent consumed before publish; an internal failure afterwards loses per-blob stats for the retry (image/stats.go:46)
- compression_ratio +Inf renders as an error string under slog JSON handler (text handler is the default) (image/stats.go:102)
- resolveChildren and computeStats resolve children twice; resolveBlobs/resolveChildren near-duplicates (plan-mandated signature)
- ParseTagRef cuts at the first ':' (equivalent given charsets) (image/refs.go:44)
- repo lock held across the whole Put; re-push mints a new root (meta has CreatedAt); no Content-Type vs body-kind cross-check; readMeta ignores Version

### image listings (Task 10)

- "oci/tag/" and "oci/manifest/" literals in list.go duplicate TagPrefix/ManifestPrefix (image/list.go:115,124)
- Referrers aborts the whole listing on one readMeta failure; a lock-free listing racing Delete+GC could 500 (image/list.go:87)
- unparseable refs skipped silently without logging; Repositories scans all refs twice (image/list.go:29,84,115,124)

### registry blobs/uploads (Task 11)

- serveBlob abort path untested (registry/blobs.go:97-110)
- PATCH parses then discards Content-Range end; PUT ignores Content-Range; POST with both mount and digest silently prefers mount (registry/uploads.go:96-110,151,172)
- blob 416 carries no error envelope; DELETE of a spilled session untested; undrained body on 416/400 costs a reconnect for multi-MB PATCHes
- isClientGone treats any net.Error as client-gone; safe today (no network-backed stores), revisit if one is added (registry/errors.go:98)

### registry manifests/lists (Task 12)

- isClientGone precedes the started check, so a local short read after the first byte logs at debug (registry/manifests.go:72, blobs.go:93)
- unreachable nil guard on referrers list; writeJSONAs duplicates writeJSON; paginate linear scan; no empty-body PUT test; HEAD on tags/list and _catalog answers 405

### cmd (Task 13)

- second SIGINT during the 30 s drain does nothing (defer stop()) (cmd/oci-amber/main.go:66-73)
- accept-failure branch closes stores without Shutdown/Close of the server (main.go:266-269); Close after a capped Shutdown does not stop handler goroutines (main.go:273-278); <-serveErr value discarded (main.go:280)
- unconditional os.RemoveAll of <work-dir>/spool on an operator-supplied path (main.go:202)
- README mentions the crane smoke test before Task 14 adds it (README.md:180); README suggests /usr/local/bin build path; no run-level test with a non-default work dir

### end-to-end tests (Task 14)

- error-log assertion in pushInterrupted matches any PUT "request failed", not this session's path (registry/e2e_test.go:915)
- fault injection by renaming the spool file depends on unexported layout (registry/e2e_test.go:897)
- io.ErrUnexpectedEOF also over-matches local truncated reads in isClientGone (registry/errors.go:113)
- crane test only logs the appended layer's kind; roundtrip-failed on crane's gzip is under investigation (cmd/oci-amber/crane_test.go:156)
- craneHeadBlob uses http.Head/DefaultTransport; e2e `check` closure has 8 positional params

### upload sweeper fix

- if file.Close fails but the unlink succeeds, the fd close is never retried (no OS leak; stale *os.File in a forgotten session) (upload/session.go:207-219)


### Observed during the final fix wave

- A gzipped empty tar (the 1024 zero-byte end-of-archive marker, e.g. an OCI empty layer) is now classified raw/not-tar because an all-zero first block fails the tar-header probe; harmless (round-trips, 32 bytes) and consistent with the format=none rule, but a behaviour change worth a test.
- Sweep counts a session whose close failed; blob.New no longer removes a stale non-directory at <WorkDir>/spool (unreachable under the ownership rule).

### rootfs view (2026-09-04)

Deferred from `docs/superpowers/specs/2026-09-04-rootfs-view-design.md`:

- Move blob resolution and the rootfs build before the repository lock so concurrent pushes to one repository do not serialize on it: **done** (2026-09-07). `image.Store.Put` resolves blobs and builds the tree first and takes the lock for child resolution and publication only; `oci-amber import` publishes an entry's image manifests through the blob worker pool (`--max-concurrent-finalize`) before its indexes, so a multi-arch image's platforms build at once.
- A view for raw tar layers, storing their contents a second time.
- Backfill command for existing stores (images already stored get a rootfs only when pushed again).
- Metadata for the rootfs root directory itself (amber roots carry none).
- Rootfs API (`/fs/`): extended attributes in listings, content-type sniffing for files, compressed tars, a `?platform=` default from the index's first child, nested indexes.
- PAX sparse 1.0 maps are served for real from the content region so the layer parses; the entry is still skipped. Representing sparse files would need the holes expanded into the CAS.
- `image.Meta.Rootfs.Entries` is always present (0 without a tree) rather than omitted, so an empty rootfs is distinguishable from an absent field in JSON without a pointer.
- Stores written before this change hold Docker's empty layer (a gzipped empty tar) as raw `not-tar`; an image referencing it reports `unavailable` until that blob is deleted and pushed again, after which the next manifest push builds the view (an unavailable rootfs is never reused).
- The merge tree and each layer's parsed entry list live in memory under the repository lock, proportional to the merged entry count; a hard cap (`unavailable: too many entries`) would bound a crafted push cheaply.
- Rootfs API: a listing without `n` materialises the whole directory (tens of MB for a 200k-entry directory); a server-side page cap with `Link` continuation would bound it. A dangling symlink answers `PATH_UNKNOWN` without naming its target; the parent listing carries it, but the message could too.

### import

- derive names from index.json's io.containerd.image.name /
  org.opencontainers.image.ref.name annotations, RepoTags as fallback; this
  also names archives saved by image ID from the containerd store.

### ingestion performance (2026-09-04)

Measured on an M4 Pro with real Docker Hub layers. For a 61 MiB go-flate layer (228 MiB of tar, 15k entries) the 11.2 s blob finalization was: pass one (stdlib inflate, spool, hashes) 0.8 s; search 3.6 s, all of it the one go-flate recompression that matches; pass two (klauspost inflate, hashes, chunking, store writes) 1.7 s; round-trip check 5.0 s, of which compose 1.5 s and recompress 3.5 s. Done since: the round-trip's compose and recompress overlap through a queued pipe (`blob/pipe.go`, 5.1 s to 3.8 s), and zrecipe's pigz engine compresses blocks on parallel workers (a pigz-compressed 28 MiB debian layer recompressed at 15 MB/s before). Deferred:

- Recompress once, not twice: **done** (2026-09-06, spec `docs/superpowers/specs/2026-09-06-single-pass-confirm-design.md`, zrecipe v0.5.0). zrecipe now finds the engine by eliminating candidates over a growing prefix (`search.Eliminate`) and confirms the survivor in a single pass (`Analysis.Confirm`) that recompresses the content through the same `Recompress` code the pull path runs, streaming the content to tar-prism as it goes. The blob store runs that as `analyze` (Start) plus a `confirm` stage that recompresses once while the tar is taken apart. The old round-trip check became `--verify-roundtrip`, default **off**: the confirming pass already verifies the recompression by construction (it runs the pull-path rebuild and compares byte for byte), so the round-trip only adds the store read-back and the tar-prism compose, kept as an opt-in diagnostic. The zlib level-0 class of mismatch (`docs/zrecipe-zlib-level0-roundtrip.md`) is now impossible by construction rather than caught after the fact.
- Measured 2026-09-06 on `dmilhdef/lhh:82` (arm64, `oci-amber import --progress plain`, fresh store, best of two runs), before = main at v0.8.0 with its `--verify-roundtrip` default on, after = this change with its default (off):

  | layer | before (verify on) | after (default) | after `--verify-roundtrip` |
  |---|---|---|---|
  | go-flate 28.2 MiB | 3.61 s | 2.85 s | 4.31 s |
  | zlib 29.7 MiB | 14.55 s | 9.86 s | 16.82 s |
  | total import (wall) | 14.8 s | 10.1 s | 17.1 s |
  | 106 KiB layer | 62 ms | 596 ms | 598 ms |
  | 433 KiB layer | 123 ms | 883 ms | 904 ms |

  The default-to-default win (14.8 s to 10.1 s, 32% on the zlib layer) comes from dropping the second recompression, which the confirming pass makes safe to skip. The single-pass extract on its own (after `--verify-roundtrip` against before) is slightly slower on large layers, because the elimination now gives every candidate its first window, including those after the winner in tier order, and carries pigz/pgzip candidates to their block size; that overhead exceeds what overlapping the extract with the recompression saves, so keeping the round trip on is now a net loss and the diagnostic is off by default. Small layers regress about tenfold in relative terms (still sub-second) for the same reason: the winner's own recompression is tiny there, so the extra candidates dominate. Raising `--analyze-parallelism` to 8 cut the small layers to 225 ms and 348 ms with no change to the large ones; the default of 2, chosen when the old search stopped at the first tier match, is now a poor fit for the elimination.
- Speculative decompose during pass one: done (spec
  `2026-09-05-speculative-decompose-design.md`); superseded by the
  single-pass confirm (2026-09-06), which moved the extract out of the
  speculative position and into the confirming pass. Skipping chunks
  already in the store at staging time is now **done** too
  (`store.Pack` asks the store before encoding each record and accounts
  the skipped ones at commit); the pack format making a staged blob
  transferable to a remote store is still open.
- New follow-ups from the single-pass confirm (2026-09-06):
  - The elimination gives every candidate its first window, which regresses
    small layers (see the table above). The escape hatch recorded in the
    zrecipe spec is to stop at the first survivor in tier order and, should
    the confirming pass fail, resume the search past it and extract again;
    not built, since it trades the current simplicity for a rare path.
  - The elimination is CPU-bound on the candidates' first windows, so it
    parallelizes where the old search did not. `--analyze-parallelism`
    should default higher than 2 now, or the elimination should take its
    own worker count.
  - Overlap the elimination with pass one: it needs only the first blocks
    and could run while pass one is still inflating (a zrecipe follow-up).
  - pigz and pgzip candidates buffer a whole block before they emit, which
    is what carries the elimination to their block size; deriving their
    verdict from an already-tested zlib or klauspost-flate candidate would
    let them be dropped without compressing a block.
- Verify limit: **done** (2026-09-06, `--verify-limit`, default `150MiB`,
  zrecipe v0.6.0 `Options.VerifyLimit`). The confirming pass and the
  search's `Run` fallback (the pigz case, where agreeing survivors used to
  run to the end) accept a candidate once it has reproduced that many
  compressed bytes, so a 1.5 GiB go-flate layer no longer pays a minute of
  single-threaded deflate to confirm what its first 150 MiB already showed;
  the content still streams through the stager in full. The trade is
  documented in the README: a compressor that diverges past the limit is
  stored as a prism and fails the recompression's digest check on pull.
  Not capped: the elimination itself, which feeds the survivor as far as
  the window in which the last buffering pigz/pgzip candidate died (8 MiB
  or 32 MiB of tar, depending on when their asynchronous first block
  surfaces) plus a margin, small next to the limit.
- Measured 2026-09-05 after the speculative decompose, `oci-amber import`
  of `dmilhdef/lhh:82` (arm64), fresh store each time: sha256:54a144adda00
  (26.9 MiB, gzip, go-flate) 3.95 s before, 3.62 s after; sha256:39a945af8df2
  (28.4 MiB, gzip, zlib) 15.06 s before, 14.56 s after; total import 15.9 s
  to 15.5 s. Remaining per-layer cost is the search and the round-trip
  recompression.
- The commit re-decodes and rehashes every staged record under
  `WriteOpts.Verify`, which is the spec's deliberate choice: a staged
  record passes exactly the gate a freshly built object passes. Should the
  commit stage ever show up in a profile, the obvious lever is that these
  records were produced in-process moments earlier, so a trusted fast path
  that skips the rehash would be sound for them in a way it is not for a
  pack that arrived from somewhere else.
- Pass two overhead: a CPU profile of the former `ingestPrism` (now the pack commit) showed 1.9 s of wall time for under 0.5 s of hashing, chunking and zstd; the rest is one `WriteAt` syscall per object in amber-store-core's `appendLocked` and goroutine wakeups on the object channels and the append mutex among the GOMAXPROCS/2 writers. Batching appends belongs in amber-store-core; in this repo `emitBuffer` (8 objects, about 80 KiB with 10 KiB chunks) and `writers()` are worth tuning, and fewer writers may well be faster.
- Search parallelism buys little: with 838 gzip candidates a non-reproducible 228 MiB layer costs 2.6 s at `--analyze-parallelism 1` and 1.35 s at 8, because the comparison aborts at the first divergent byte. Leave the default at 2.
- Rootfs build: 1.2 s for 17.6k entries (0.4 s for 7.4k), mostly one `LookupKey` per regular file in `parseLayer`. Walking the prism's `blobs/` directory once in order, or parsing layers concurrently before applying them in order, would cut it.
- Large layers, measured 2026-09-06 (M4 Pro, `blob.Store.Put` on one real
  layer at a time, an Observer timing the stage transitions):

  | layer | engine | analyze | confirm | commit |
  |---|---|---|---|---|
  | rust:1.94-bookworm 203 MiB (570 MiB tar, 10.7k entries) | go-flate 6 | 3.4 s | 8.9 s | 0.35 s |
  | rust:1.94-bookworm 48 MiB (137 MiB tar) | pigz 9 | 9.4 s | 1.1 s | 0.12 s |
  | dmilhdef/lhh:82 29.7 MiB (80 MiB tar) | zlib 9 | 2.8 s | 7.0 s | 0.07 s |

  Chunk processing (ultracdc, blake3 keys, `Has`, zstd in `EncodeRecord`)
  is already spread over GOMAXPROCS/2 encoders per pack writer and costs
  well under a second of CPU per 200 MiB layer; the commit is what the
  table shows. The confirm is the engine's floor: a bare `compress/flate`
  level 6 pass over the same 570 MiB takes 8.3 s (68 MB/s), zlib level 9
  through cgo about 11 MB/s of tar on that data, both single-threaded by
  construction, so a lone large go-flate or zlib layer holds one core for
  size ÷ engine speed whatever the pool does. Done since: the importer
  feeds the largest absent blobs first (longest-processing-time-first),
  and zrecipe hands survivors that all agree past 4 MiB to `Run`
  (draganm/zrecipe#7, released as v0.5.2 and pinned here), which cut the pigz layer's analyze from 9.4 s to
  3.3 s (its parallel and single-thread candidates agree over the whole
  input, and the lockstep ran the single-thread one on one core). Whole
  import of rust:1.94-bookworm: 20.0 s to 17.3 s, bounded by the two
  203 and 178 MiB go-flate layers at 14 s each. Still open:
  - the pigz winner is compressed twice, once by `Run` to complete it
    and once by the confirming pass; settling on it unverified at the
    bound needs the resume-on-confirm-failure escape hatch first;
  - zrecipe's `search.Compare` reads the reference 240 bytes per `pread`
    (one per flate flush, 784k on the 203 MiB layer); a 128 KiB
    read-ahead saved 1.5 s of system time per such layer but moved wall
    time not at all, so it was not kept;
  - macOS pprof attributes far more CPU to `syscall.rawsyscalln` and
    `pthread_cond_signal` than `time -l` shows as sys; trust wall and sys
    time over the profile for syscalls on this machine.

### browse (2026-09-05)

Deferred from `docs/superpowers/specs/2026-09-05-browse-command-design.md`:

- An HTTP-backed browser (`browse --url`) that works against a running
  `serve`; needs read endpoints for the storage tree, since `/fs/` covers
  only the rootfs. The `browse` node interface is the seam to plug it into.
- A raw references view (every `oci/tag/`, `oci/manifest/`,
  `oci/referrer/`, `oci/blob/` ref by namespace) for objects no image
  reaches.
- Exporting a file or a directory (as a tar) out of the viewer.
- The viewer's status line is cut at the terminal width; at 100 columns
  the key hints after the file's labels are hidden. Dropping the `image`
  label (the breadcrumb already names the image) or wrapping the status
  onto two lines would fix it.
- Repository listings open every tag's meta.json; a store with thousands
  of tags in one repository will feel it. A paged or lazily annotated
  listing would bound it.
- Syntax highlighting and content sniffing beyond text/JSON/binary were
  ruled out of the first version.

### ls and save (2026-09-05)

- `save --platform` that prunes an index to one platform the way
  `docker save --platform` does; today the whole index is saved and the
  platform only picks the `manifest.json` entry (the host's, with no flag
  to override it).
- Progress on stderr: done (2026-09-07), `--progress auto|tui|plain`
  like `import`, with a summary line at the end. The ETA is a single
  byte rate over every blob; a raw layer streams far faster than a prism
  is recomposed, so an archive mixing the two gets a rough estimate until
  the slow layers dominate. A per-kind rate (the blob's meta.json says
  which) would tighten it.
- docker's containerd store also writes a
  `containerd.io/distribution.source.<registry>` annotation on the
  `index.json` entry; `save` does not, and nothing reads it on load.
- `ls` opens every tag's meta.json and, for an index, its manifest; a
  store with thousands of tags will feel it, as `browse` does.
- Machine-readable `ls` output (`--json`) and columns for the accounting
  numbers in meta.json (bytes added to the CAS, dedup ratio).
- `ls` and `save` against a running `serve` over HTTP, as noted for
  `browse`.
- Two indexes that share a platform child (say `multi:v1` and `multi:v2`
  after a platform was added) save as docker would, one `manifest.json`
  entry carrying both RepoTags, but `import` rejects that shape as
  ambiguous (`dockerarchive.assignNames`); the import follow-up above,
  naming from the `index.json` annotations `save` writes, would fix the
  round trip.

