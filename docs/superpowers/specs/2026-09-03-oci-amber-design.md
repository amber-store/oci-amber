# oci-amber design

oci-amber is an OCI distribution registry whose storage is an embedded
[amber-store-core](https://github.com/amber-store/core) store. It
accepts image pushes over HTTP like any registry, but instead of keeping each
layer as an opaque compressed blob it takes the layer apart with
[zrecipe](https://github.com/draganm/zrecipe) (compressed stream ->
uncompressed tar + compression recipe) and
[tar-prism](https://github.com/draganm/tar-prism) (tar -> per-file contents +
tar recipe), so that file contents deduplicate across layers and images in
amber's content-defined-chunked store. Pulls rebuild the original bytes on the
fly by streaming the pieces back through tar-prism and zrecipe, and every
served blob is byte-identical to what was pushed.

## Goals

- Push and pull with docker, containerd/nerdctl, podman/skopeo, crane/ggcr,
  oras and buildkit without client-side configuration beyond the registry URL.
- Deduplicate at the file-content level across layers and images.
- Keep as much of an upload in memory as possible; spill large blobs to a
  work directory.
- Pull without writing anything to disk: tar reassembly and recompression are
  streamed.
- After every image push, log the total image size, the amount that was
  already present (deduplicated), and the effective compression ratio.
- Amber references name image roots, so an image is a single reachable tree
  in the store that amber's own tools can inspect, export and garbage collect.

## Non-goals (first version)

- Authentication and authorization. The registry is single tenant and blobs
  are global, not repository scoped.
- Multi-process access to the store. An amber store directory is single
  owner; the registry embeds amber in process and nothing else opens the
  store while it runs.
- Resumable uploads across server restarts. In-flight sessions are lost on
  restart; clients handle `BLOB_UPLOAD_UNKNOWN` by restarting the blob.
- Range requests on layers stored as prisms. They are answered with the full
  body (spec-legal).
- Storing layers that zrecipe cannot reproduce as anything other than the
  verbatim bytes (no tar-level dedup for them).

## Dependencies

| Module | Used for |
|---|---|
| `github.com/amber-store/core` | packstore, refstore, fstree builders/readers, chunkers, gc |
| `github.com/draganm/zrecipe` | `Analyze`, `Recompress`, `Params` |
| `github.com/draganm/tar-prism` | `DecomposeTo`, `ComposeFrom`, `Index`, `Entry` (new sink/source API, see below) |
| `github.com/klauspost/compress` | the bounded compressed tar-header probe |
| `lukechampine.com/blake3` | hashing the staged stream against zrecipe's recorded digest |
| `github.com/urfave/cli/v2` | the `oci-amber` command line |
| `log/slog` | logging |

All three owner libraries are AGPL-3.0-or-later; oci-amber uses the same
license.

The Nix dev shell provides Go, `pkg-config`, `zlib`, `zstd` (headers and
libraries, for zrecipe's cgo engines), `gzip`, `pigz` (fixtures) and
`crane` (client smoke test).

## Package layout

Top-level packages, no `internal/`:

```
cmd/oci-amber/   the binary: `oci-amber serve`
oci/             OCI grammar and wire types: digests, names, tags, media types, error envelope
store/           the amber embedding: open/close, streaming put, streaming read, refs
blob/            one OCI blob in amber: push (analyze with speculative decompose, commit) and pull (compose, recompress)
image/           manifests and indexes: validation, image roots, tag/digest/referrer refs, catalog
upload/          upload sessions: memory buffer that spills to the work directory
registry/        net/http handlers and router
```

Import direction: `cmd` -> `registry` -> `image`, `blob`, `upload`, `oci`;
`image` -> `blob` -> `store` -> `oci`. Nothing imports `registry`.

## Store parameters

These are fixed for the lifetime of a store and recorded in
`<store>/oci-amber.json` when the store is created:

```json
{"version":1,"chunking":{"minSize":32768,"normalSize":524288,"maxSize":1048576},"segmentSize":2147483648}
```

- Content-defined chunking (`chunkers.ByteOpts`): min 32 KiB, normal 512 KiB,
  max 1 MiB.
- Item chunking for tree nodes: amber's default `ItemBits` of 7.
- Pack segments (`packstore.WithSegmentSize`): 2 GiB.

Opening a store whose recorded parameters differ from the binary's compiled
defaults is refused with a clear error, because changing chunk boundaries
silently defeats deduplication against existing content.

The store directory layout is `<store>/packstore`, `<store>/refs`,
`<store>/gc` and `<store>/oci-amber.json`. In-flight state lives under
`<work-dir>/oci-amber/` (default work directory `<store>/work`), a
subdirectory the registry owns: `uploads/` (spilled upload sessions) and
`spool/` (zrecipe's temp dir). The contents of those two are removed at
startup; neither they nor anything else under the operator-supplied
`--work-dir` is ever deleted.

## Amber data model

Every directory object is built with `fstree.NewDirBuilder` and every file
with `chunkers.SplitBytes` + `fstree.NewFileIndexBuilder`, exactly like
amber's own `ingest` driver. Entry `UID`, `GID` and `Mtime` are always zero and
`Mode` is `0o100644` for files and `0o040755` for directories, so identical
content always yields identical keys.

### Blob root

One per OCI blob digest. It is literally a tar-prism prism directory with two
extra files, so `amber-store restore` + `tar-prism compose` + `zrecipe
recompress` reproduce the blob with the existing CLIs.

```
meta.json     oci-amber metadata (below)
comp.json     zrecipe Params (Params.Write output)          prism only
recipe.bin    tar-prism recipe                                  prism only
recipe.json   tar-prism Index                                   prism only
blobs/        00000001, 00000002, ... regular-file contents     prism only
raw           the verbatim uploaded bytes                       raw only
```

`meta.json`:

```json
{
  "version": 1,
  "digest": "sha256:…",
  "size": 12345678,
  "kind": "prism",
  "format": "gzip",
  "diffId": "sha256:…",
  "uncompressedSize": 45678901,
  "entries": 1234,
  "engine": "gnu-gzip",
  "engineVersion": "1.14",
  "uploadedAt": "2026-09-03T18:00:00Z",
  "stats": {
    "logicalBytes": 45900000,
    "newLogicalBytes": 4100000,
    "dedupedBytes": 41800000,
    "diskBytes": 1900000,
    "objectsNew": 40,
    "objectsDeduped": 380
  }
}
```

- `size` is the OCI blob size (compressed bytes as received).
- `kind` is `prism` or `raw`.
- `format` is zrecipe's detected format: `gzip`, `zstd` or `none`.
- `rawReason` is omitted for prisms; for raw blobs one of `not-reproducible`,
  `unsupported`, `corrupt`, `not-tar`, `analyze-timeout`, `roundtrip-failed`,
  `decompose-failed`.
- `diffId`, `uncompressedSize`, `entries`, `engine`, `engineVersion` are
  present for prisms only. `diffId` is the sha256 of the uncompressed tar,
  which is what the image config's `rootfs.diff_ids` carries.
- `stats` are defined under Accounting.

### Image root

One per manifest or index, what tag and digest refs point at.

```
manifest      the exact bytes the client PUT
meta.json     oci-amber metadata (below)
blobs/        <algo>:<hex> -> that blob's blob root (config and layers)
manifests/    <algo>:<hex> -> child image root                   index only
```

`blobs/` and `manifests/` entries are directory entries whose `ContentKey` is
the referenced root, so the image root reaches everything the image needs and
structural sharing makes the link free.

`meta.json`:

```json
{
  "version": 1,
  "kind": "manifest",
  "mediaType": "application/vnd.oci.image.manifest.v1+json",
  "digest": "sha256:…",
  "size": 1234,
  "artifactType": "application/vnd.example+type",
  "subject": {"mediaType": "…", "digest": "sha256:…", "size": 999},
  "annotations": {"org.opencontainers.image.created": "…"},
  "createdAt": "2026-09-03T18:00:00Z",
  "stats": {"totalBytes": 0, "logicalBytes": 0, "dedupedBytes": 0, "diskBytes": 0}
}
```

`kind` is `manifest` or `index`. `mediaType` is the request Content-Type
without parameters, or, when the client sent none, the manifest's own
`mediaType` field, or `application/vnd.oci.image.manifest.v1+json` as the
last resort. `artifactType` is the manifest's `artifactType` field, falling
back to `config.mediaType` for image manifests; it and `subject` and
`annotations` are omitted when absent. `stats` are the per-image numbers from
the log line at push time.

### References

Amber `reference.ValidateName` allows `/` and `:` and forbids `@`, so:

```
oci/blob/<algo>:<hex>                        -> blob root
oci/manifest/<repo>/<algo>:<hex>             -> image root
oci/tag/<repo>:<tag>                         -> image root
oci/referrer/<repo>/<subject digest>/<referrer digest>  -> referrer's image root
```

Repository names cannot contain `:` and tags cannot contain `/` or `:`, so a
repository's tags are the refs with prefix `oci/tag/<repo>:`, and a manifest
ref is parsed by splitting at the last `/`. The reference record's `User` is
`oci-amber`.

Blob refs are the digest -> key index for HEAD, GET and mount, and the GC root
that keeps a blob alive between its upload and the manifest that references
it. They are never dropped automatically; `DELETE /v2/<name>/blobs/<digest>`
removes one.

Ref publication is `gc.Collector.PrepareRef(root)` -> `refstore.Put` ->
`commit()`, serialized per ref name with a mutex because `Put` is an
unconditional overwrite. Tag moves are last-writer-wins.

## Push

### Upload sessions (`upload`)

A session is created by `POST /v2/<name>/blobs/uploads/` and identified by a
random 128-bit hex id. It holds:

- a `bytes.Buffer` while total bytes are at or below `--max-in-memory`
  (default 64 MiB); the first write that would exceed it creates
  `<work-dir>/oci-amber/uploads/<id>`, writes the buffer out, drops the
  buffer and appends to the file from then on;
- the byte count, a running sha256 over every byte received, and the last
  activity time.

Sessions live in a mutex-protected map. A sweeper removes sessions idle for
longer than `--upload-timeout` (default 1 h) together with their files.
Finalization takes a snapshot of the session's data as an `io.ReadSeeker`
that also implements `io.ReaderAt` (`*bytes.Reader`, or a section reader over
the spilled file), so zrecipe can search candidates in parallel. The
session stays registered until the blob is published, so a `500` during
finalization leaves it in place for the client to retry the `PUT`; success
removes the session and its file.

### Blob finalization (`blob.Store.Put`)

Runs on `PUT /v2/<name>/blobs/uploads/<id>?digest=` after the optional final
body has been appended, and on a monolithic `POST ?digest=`.

Both requests name the digest before their body, so the registry looks it
up first (`blob.Store.Reuse`): when `oci/blob/<digest>` is published the
answer is `201` with `Location` and `Docker-Content-Digest` right away, the
body is left unread (net/http drains a small one and closes the connection
behind the response for a large one), a `PUT`'s session is dropped with
whatever was `PATCH`ed into it, and the blob is recorded in the
recent-uploads table as fully deduplicated, with the same `blob already
present` line step 2 logs. The client's bytes are never compared with the
digest in that case. The lookup does not serialize on the digest: a
finalization of the same digest still in flight is not published yet, so
the upload proceeds and step 2 catches it.

1. **Digest check.** The session's sha256 must equal the requested digest,
   else `400 DIGEST_INVALID`. Only `sha256` is accepted.
2. **Whole-blob dedup.** If `oci/blob/<digest>` exists, the spool is discarded
   and the answer is `201`. The blob is recorded in the recent-uploads table
   as fully deduplicated.
3. **Finalize slot.** Acquire one of `--max-concurrent-finalize` slots
   (default `max(1, NumCPU/2)`). This bounds engine-search CPU and spool
   memory.
4. **Analyze.** After the pre-checks (the zstd window bound, the
   compressed tar-header probe, and the uncompressed tar-header probe for
   streams `Detect` reports as `none`; each decides raw `not-tar` or
   `unsupported` without touching anything else),
   `zrecipe.Start(ctx, spool, &Options{TempDir: <work-dir>/oci-amber/spool,
   MaxInMemory: --max-in-memory, Parallelism: --analyze-parallelism
   (default 2)})` runs under a child context with `--analyze-timeout`
   (default 15 min): it decompresses the stream once into zrecipe's spool
   and eliminates the deflate/zstd candidates down to the one that
   reproduces the input. Nothing is staged here, so a layer that turns out
   not reproducible never touches a pack. See
   `2026-09-06-single-pass-confirm-design.md`.
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
6. **Confirm and commit** (prism candidates). `zrecipe.Analysis.Confirm`
   reads the decompressed content once and rebuilds the input from it
   through the same code the pull path runs, comparing byte for byte, so
   the layer is recompressed once and the parameters are verified by
   construction; a mismatch is `not-reproducible`. As Confirm streams the
   content it tees it to a goroutine that hashes it (BLAKE3, sha256,
   length) and runs `tarprism.DecomposeTo` with the amber sink over a
   pack-backed `store.Writer`, encoding the records the store lacks into an
   unlinked temp pack (present chunks are skipped and accounted at commit).
   The staged stream's BLAKE3 and length must equal `params.Uncompressed`,
   else raw `decompose-failed`. One accounting writer then inserts the
   pack's records (`AddPack`: dedup, verification, the GC barrier, exactly
   as freshly built objects get) and stores comp.json; its stats become the
   blob's. The sha256 of the staged stream becomes `diffId`. A store
   failure abandons the objects written so far to GC and fails the upload.
7. **Raw path.** `store.PutStream(spool)` through the accounting iterator.
   The spool is the verbatim bytes, whatever their format.
8. **Round-trip verification** (`--verify-roundtrip`, default off, prisms
   only). Run the full pull pipeline from the freshly built objects into a
   sha256 and compare with the OCI digest. Failure logs at error level and
   downgrades the blob to raw with reason `roundtrip-failed` (or refuses it,
   see step 5). The confirming pass in step 6 already verifies the
   recompression by construction, so this only adds the store read-back and
   the tar-prism compose; it is an opt-in diagnostic.
9. **Root and publish.** Build `meta.json` and the blob root through a second,
   small accounting writer whose stats are not counted, publish
   `oci/blob/<digest>`, record the stats in the recent-uploads table,
   log the blob line, delete the spool file, answer `201` with `Location:
   /v2/<name>/blobs/<digest>` and `Docker-Content-Digest`.

Concurrent finalizations of the same digest are serialized on the digest;
the second one hits the whole-blob dedup check.

### Manifest push (`image.Store.Put`)

`PUT /v2/<name>/manifests/<reference>`:

1. Read the body with a 4 MiB cap (`413`). Compute the sha256. When the
   reference is a digest it must match (`400 DIGEST_INVALID`). The tag, when
   the reference is a tag, must match the tag grammar (`400 MANIFEST_INVALID`
   with the message prefix `invalid tag`; the standard code list has no
   tag-specific code).
2. Parse the body only for validation and to collect descriptors:
   `schemaVersion`, `mediaType`, `config`, `layers`, `manifests`, `subject`,
   `artifactType`, `annotations`. Invalid JSON or a schema version other than
   2 is `400 MANIFEST_INVALID`. The stored bytes are the raw body.
3. Resolve every `config` and `layers` digest through `oci/blob/<digest>` and
   every `manifests` child through `oci/manifest/<repo>/<digest>`. A missing
   one is `404 MANIFEST_BLOB_UNKNOWN` naming the digest. `subject` does not
   have to exist.
4. Store the manifest bytes with `PutStream`, write `meta.json`, build
   `blobs/` (and `manifests/` for an index) with the resolved roots as
   directory entries, build the image root.
5. Publish `oci/manifest/<repo>/<digest>`, then `oci/tag/<repo>:<tag>` when
   pushed by tag, then `oci/referrer/<repo>/<subject>/<digest>` when a
   subject is present. Re-pushing an identical manifest is idempotent.
6. Emit the image log line. Answer `201` with `Location`,
   `Docker-Content-Digest`, and `OCI-Subject: <subject digest>` when a subject
   is present (this tells clients the referrers API is native, so they do not
   maintain tag-schema indexes).

## Pull

### Blob (`blob.Store.Open`)

`HEAD`/`GET /v2/<name>/blobs/<digest>`:

1. Resolve `oci/blob/<digest>` (`404 BLOB_UNKNOWN`). Look up `meta.json` and
   read it (a few hundred bytes).
2. Headers: `Content-Length: <size>`, `Docker-Content-Digest`,
   `Content-Type: application/octet-stream`; `Accept-Ranges: bytes` for raw
   blobs only. `HEAD` stops here.
3. Raw: stream `raw` with `fstree.WriteContent` teed through sha256. A single
   `Range: bytes=a-b` request is honoured with `206` and `Content-Range` by
   walking the file index and skipping whole chunks that end before `a`;
   unsatisfiable ranges are `416` with `Content-Range: bytes */<size>`;
   multi-range requests get the full `200`.
4. Prism: read `comp.json` into `Params`. A goroutine runs
   `tarprism.ComposeFrom(source, pipeWriter)` where `source` serves
   `recipe.bin`, `recipe.json` and `blobs/%08d` as amber streaming readers.
   The handler runs `zrecipe.Recompress(ctx, params, pipeReader,
   io.MultiWriter(w, sha256), &RecompressOptions{AllowVersionMismatch:
   true})`. Range requests are answered with the full body.
5. After the body: if `Recompress` or `ComposeFrom` returned an error, or the
   sha256 differs from the digest, the handler panics with
   `http.ErrAbortHandler` so the connection is cut and the client sees a
   truncated body rather than a clean, wrong one. The failure is logged at
   error level with the digest and error, because it indicates compressor
   drift and needs operator attention.

Nothing on the pull path writes to disk. Memory per pull is one engine working
set, the chunks currently being read (three readers: recipe, current blob,
raw or manifest), one index node per tree level, and the parsed
`recipe.json`.

### Manifest (`image.Store.Open`)

`HEAD`/`GET /v2/<name>/manifests/<reference>`: resolve
`oci/tag/<repo>:<tag>` or `oci/manifest/<repo>/<digest>`
(`404 MANIFEST_UNKNOWN`), read `meta.json`, set `Content-Type` to the stored
media type, `Content-Length` from the manifest file's key length, and
`Docker-Content-Digest`. `GET` streams the `manifest` file teed through sha256
with the same abort-on-mismatch discipline. The `Accept` header is ignored;
real clients accept the concrete type returned.

## HTTP surface (`registry`)

Routing is a small hand-written matcher because repository names contain
slashes: the path is matched against
`^/v2/(?P<name>.+)/(blobs|manifests|tags|referrers)/(?P<rest>.*)$` with a
greedy name, so the last such segment wins, then `name` is validated against
the OCI distribution grammar
`[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:\.|_|__|-+)[a-z0-9]+)*)*`
(max 255 bytes; a single dot, a single or double underscore, or a run of
hyphens separates alphanumeric runs), tags against
`[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}`, digests
against `sha256:[a-f0-9]{64}`. Invalid names are `400 NAME_INVALID`.

| Method and path | Behaviour |
|---|---|
| `GET /v2/` | `200`, body `{}`, `Docker-Distribution-API-Version: registry/2.0` |
| `HEAD`/`GET /v2/<name>/blobs/<digest>` | see Pull |
| `DELETE /v2/<name>/blobs/<digest>` | delete `oci/blob/<digest>`; `202`, or `404 BLOB_UNKNOWN` |
| `POST /v2/<name>/blobs/uploads/` | new session: `202`, `Location: /v2/<name>/blobs/uploads/<id>`, `Range: 0-0`, `Docker-Upload-UUID`. With `?mount=<digest>[&from=…]`: `201` + `Location: /v2/<name>/blobs/<digest>` + `Docker-Content-Digest` if the blob ref exists, otherwise the normal `202`. With `?digest=` and a body: monolithic upload, finalized immediately, `201`; a digest that is already stored is answered `201` before the body is read. |
| `PATCH /v2/<name>/blobs/uploads/<id>` | append body. `Content-Range` start, when present, must equal the current offset else `416 BLOB_UPLOAD_INVALID` with the current `Range`. `202` + `Range: 0-<last byte index>` (inclusive; `0-0` when empty) + `Docker-Upload-UUID`. |
| `PUT /v2/<name>/blobs/uploads/<id>?digest=` | append optional body, finalize. `201` + `Location` + `Docker-Content-Digest`. Missing digest is `400 DIGEST_INVALID`. A digest that is already stored is answered `201` before the body is read and the session is dropped. |
| `GET /v2/<name>/blobs/uploads/<id>` | `204` + `Range` + `Docker-Upload-UUID` |
| `DELETE /v2/<name>/blobs/uploads/<id>` | cancel; `204` |
| `HEAD`/`GET /v2/<name>/manifests/<reference>` | see Pull |
| `PUT /v2/<name>/manifests/<reference>` | see Push |
| `DELETE /v2/<name>/manifests/<reference>` | by tag: delete the tag ref. By digest: delete the manifest ref, every `oci/tag/<repo>:*` ref pointing at the same key, and the manifest's own referrer ref. `202`, or `404 MANIFEST_UNKNOWN`. |
| `GET /v2/<name>/tags/list` | `{"name":…,"tags":[…]}` sorted lexically; `n` and `last` pagination with `Link: </v2/<name>/tags/list?n=…&last=…>; rel="next"` when more remain; `n=0` returns an empty list without a `Link`. `404 NAME_UNKNOWN` when the repository has no tags and no manifests. |
| `GET /v2/<name>/referrers/<digest>` | always `200` with `Content-Type: application/vnd.oci.image.index.v1+json` and an index whose `manifests` are descriptors (`mediaType`, `digest`, `size`, `artifactType`, `annotations`) of every `oci/referrer/<repo>/<digest>/*` entry, empty when none. `?artifactType=` filters and sets `OCI-Filters-Applied: artifactType`. Bad digest syntax is `400 DIGEST_INVALID`. |
| `GET /v2/_catalog` | `{"repositories":[…]}` derived from the set of repositories in tag and manifest refs, sorted, with `n`/`last`/`Link` pagination. |

Unknown blob upload ids are `404 BLOB_UPLOAD_UNKNOWN`. Unsupported methods on
known paths are `405`. Everything else under `/v2/` is `404` with an empty
errors list.

Errors use the standard envelope
`{"errors":[{"code":"…","message":"…","detail":…}]}` with the standard codes:
`BLOB_UNKNOWN`, `BLOB_UPLOAD_INVALID`, `BLOB_UPLOAD_UNKNOWN`, `DIGEST_INVALID`,
`MANIFEST_BLOB_UNKNOWN`, `MANIFEST_INVALID`, `MANIFEST_UNKNOWN`,
`NAME_INVALID`, `NAME_UNKNOWN`, `SIZE_INVALID`, `UNAUTHORIZED`, `DENIED`,
`UNSUPPORTED`, `TOOMANYREQUESTS`. Handler panics are recovered into `500`
with `{"errors":[]}`.

The server uses `net/http` with a read-header timeout, no body size limit on
blob uploads, a 4 MiB limit on manifests, and graceful shutdown on `SIGINT`
and `SIGTERM`: stop accepting, wait up to 30 s for handlers, close the store.

## Accounting and logging

### Per blob

Measured by the accounting iterator that wraps the object stream handed to
`packstore.WriteParallel`:

- `logicalBytes`: sum of the encoded size of every object offered (content
  chunks, file index nodes, `blobs/` directory nodes, recipe and index chunks,
  `comp.json`). The blob root and `meta.json` are written afterwards and are
  excluded, since `meta.json` carries these numbers.
- `newLogicalBytes`: `WriteStats.BytesStored`.
- `dedupedBytes`: `logicalBytes - newLogicalBytes`.
- `diskBytes`: for every key that `Store.Has` reported absent when it was
  offered and that had not been offered earlier in the same stream, the sum
  of `Store.StoredSize(key) + amberpack.RecHeaderSize` after the write
  finishes. This is the number of bytes appended to pack segments.
- `objectsNew`, `objectsDeduped`: `WriteStats.Stored` and `Deduped`.

A blob that was skipped at step 2 of finalization counts `logicalBytes =
size`, `dedupedBytes = size`, `newLogicalBytes = diskBytes = 0`.

Blob log line (Info):

```
blob stored digest=sha256:… size=… kind=prism format=gzip engine=gnu-gzip entries=… logical_bytes=… deduped_bytes=… disk_bytes=… duration=…
```

### Per image

Computed by the manifest handler after publication. The recent-uploads table
maps digest -> per-blob stats for blobs finalized in this process within
`--upload-timeout`; the manifest handler consumes matching entries.

- `totalBytes`: the image size as described by the manifest: sum of `size`
  over config and layer descriptors, plus the manifest bytes. For an index:
  the index bytes plus the `totalBytes` of every child image.
- For each referenced blob: if the recent-uploads table has it, use its
  `logicalBytes`, `dedupedBytes`, `diskBytes`; otherwise it was already
  present before this push and counts `logicalBytes = dedupedBytes = size`,
  `diskBytes = 0`. Child images of an index contribute their stored
  `meta.json` stats.
- The manifest's own objects go through the same accounting iterator and are
  added.
- `compressionRatio = totalBytes / diskBytes`, `+Inf` when `diskBytes` is 0.
- `dedupedPercent = 100 * dedupedBytes / logicalBytes`.

Image log line (Info):

```
image pushed repo=library/app reference=v1 digest=sha256:… kind=manifest blobs=7 total_bytes=95631872 logical_bytes=327545651 deduped_bytes=293700000 deduped_percent=89.7 disk_bytes=10276044 compression_ratio=9.31
```

Byte counts are raw integers so the lines are machine friendly. The same
numbers are stored in the image's `meta.json`. Both lines may carry extra
keys (`raw_reason` on raw blob lines, `manifests` on index lines, `duration`
on both); the keys listed above are the guaranteed minimum.

`diskBytes` is approximate under concurrent uploads that share chunks (both
may see a key as absent); the stored bytes are correct because
`WriteParallel` deduplicates. The numbers are for logging, not billing.

## Concurrency and limits

- Upload sessions: mutex-protected map; requests on the same session are
  serialized on the session's own mutex, so a client that overlaps a PATCH
  and a PUT gets a consistent offset rather than corruption.
- Finalization: semaphore of `--max-concurrent-finalize`. Per-digest mutex so
  two uploads of the same blob do not both ingest it.
- Ref publication: per-name mutex around PrepareRef/Put/commit.
- zrecipe `Parallelism` is `--analyze-parallelism`, default 2; each worker
  holds one engine working set (libzstd at high levels uses hundreds of MB).
- `WriteParallel` writers: `GOMAXPROCS/2`, minimum 1, with `Verify: true`
  (recompute the key before appending; cheap insurance).
- Memory per finalize: the compressed spool (up to `--max-in-memory`),
  zrecipe's decompressed spool (up to `--max-in-memory`, then an unlinked
  temp file), engine working sets, and about 8 MiB of pipeline buffers.
  Everything else streams.

## Garbage collection

`--gc-interval` (default 0, off) starts amber's collector in the background
with amber's default grace (1 h) and garbage fraction. Deleting refs makes
objects unreachable; the collector reclaims them once their pack is old
enough. With GC off, the amber CLI's `gc run` can be used while the server is
stopped. Objects written by failed or downgraded uploads are unreachable by
design and reclaimed the same way.

Blob refs whose blob is no longer referenced by any image are not cleaned
automatically in this version; `DELETE` on the blob removes them.

## Configuration

`oci-amber serve` flags (all also settable via `OCI_AMBER_*` environment
variables through urfave/cli):

| Flag | Default | Meaning |
|---|---|---|
| `--store` | required | store directory |
| `--work-dir` | `<store>/work` | registry state lives under `<work-dir>/oci-amber/{uploads,spool}`; only those two directories are emptied at startup |
| `--listen` | `:5000` | listen address |
| `--max-in-memory` | `64MiB` | upload spool and zrecipe spool threshold |
| `--analyze-parallelism` | `2` | zrecipe candidate workers per blob |
| `--analyze-timeout` | `15m` | per-blob analyze deadline before raw fallback |
| `--max-concurrent-finalize` | `NumCPU/2` (min 1) | concurrent blob finalizations |
| `--verify-roundtrip` | `false` | opt-in diagnostic: pull-pipeline check before publishing a prism (the confirming pass already verifies the recompression) |
| `--allow-raw` | `false` | store a layer raw when it cannot be a prism instead of refusing the upload; non-tar blobs are always stored raw |
| `--upload-timeout` | `1h` | idle upload session expiry and recent-uploads table TTL |
| `--gc-interval` | `0` | background GC cycle interval, 0 disables |
| `--log-level` | `info` | slog level |

## tar-prism change

tar-prism gains a sink/source API; the directory functions become adapters
over it, so every existing fixture and round-trip test also exercises the new
path.

```go
// Sink receives the parts of a decomposed archive. Recipe is called once
// before anything else, Blob once per regular file in archive order, and
// Index once at the end.
type Sink interface {
    Recipe() (io.WriteCloser, error)
    // Blob must consume exactly entry.Size bytes from r. DecomposeTo reports
    // an error if it consumes fewer.
    Blob(index int, entry Entry, r io.Reader) error
    Index(idx *Index) error
}

// Source serves the parts of a prism to ComposeFrom.
type Source interface {
    Index() (*Index, error)
    Recipe() (io.ReadCloser, error)
    Blob(index int, entry Entry) (io.ReadCloser, error)
}

func DecomposeTo(r io.Reader, sink Sink) error
func ComposeFrom(src Source, w io.Writer) error
```

- `index` is 0-based and matches `Index.Entries[index]`; `entry.Blob` keeps
  the `blobs/%08d` naming so directory and non-directory sinks agree.
- `DecomposeTo` wraps `r` for `Blob` in a counting `io.LimitReader`; if the
  sink returns without consuming `entry.Size` bytes the call fails with a
  descriptive error. `Blob` is called after the entry's header and any PAX or
  GNU meta entries have been written to the recipe, and the padding is
  written after it returns, so `Entry.Offset` semantics are unchanged.
- `ComposeFrom` copies exactly `entry.Size` bytes from each blob reader and
  fails if the reader ends early or has more; the file-size `Stat` check of
  the directory version becomes this check. It verifies the BLAKE3 digest at
  the end exactly as `Compose` does.
- `Decompose(r, dir)` = `DecomposeTo(r, DirSink(dir))` and `Compose(dir, w)`
  = `ComposeFrom(DirSource(dir), w)`; both adapters are exported so callers
  can mix them.
- The CLI is unchanged.

The change lands in tar-prism as its own commit with tests for a memory sink
and source; oci-amber then requires that version.

## Error handling summary

| Situation | Outcome |
|---|---|
| zrecipe `ErrNotReproducible`, `ErrUnsupported`, `ErrCorrupt`, non-tar `none` or compressed non-tar (first decompressed block), zstd frame window above 128 MiB (`unsupported`), analyze deadline | stored raw, reason recorded, `201` |
| staged-stream digest mismatch, tar-prism decompose error, round-trip failure | stored raw, reason recorded, error-level log for the last two, `201`. A compressed blob that is not a tar never reaches this row: step 4's pre-checks classify it `not-tar` before the engine search, so the error level is reserved for archives that really did look decomposable |
| amber write error, I/O error on the spool, request context cancelled during finalize | `500`, session retained for retry, nothing published |
| ref publish error | `500`, objects left for GC |
| pull-side compose/recompress error or digest mismatch | connection aborted, error-level log |
| handler panic | recovered, `500` |

## Testing

- **Unit tests** per package, table driven: digest and name grammar, error
  envelope, upload session spill and resume and expiry, accounting arithmetic
  including the skipped-blob and already-present cases, ref name construction
  and parsing, the range parser and chunk skipping.
- **Blob round trips** against a temporary store (`t.TempDir()`): gzip layers
  produced by Go's `compress/gzip` at several levels and by GNU `gzip` when
  on PATH, a zstd layer, an uncompressed tar, a JSON config blob, an empty
  blob, a deliberately non-reproducible gzip stream (a hand-built deflate
  stream), a tar with PAX long names and a hard link, a layer larger than a
  small `--max-in-memory` so both spill paths run. Each asserts the pulled
  bytes equal the pushed bytes, the sha256 matches, `kind` and `rawReason`
  are as expected, and no file exists under the work directory afterwards.
- **Dedup**: push two layers sharing most files; assert the second reports
  `dedupedBytes` above 90 % of its logical bytes and `diskBytes` well below
  its size. Push the same layer twice; assert the second is skipped.
- **HTTP conformance** with `httptest.Server`: scripted push and pull the way
  containerd (POST, then one PUT carrying the whole body), ggcr and buildkit
  (POST, PATCH with `Content-Range`, PUT) and podman (POST, single PATCH,
  empty PUT) do it, HEAD before PUT, monolithic POST, mount hit and
  mount fallback, manifest by tag and by digest, an image index with two
  children, tag listing with pagination and `Link`, referrers with and
  without `artifactType` filter, catalog, deletes, and the error envelope
  and status for every failure path in the table above.
- **Real client smoke test**, skipped unless `crane` is on PATH: start a
  server on a random port, `crane push` a small image built from a tarball,
  `crane pull` it back, compare digests, `crane ls` the tags.
- **tar-prism**: memory sink/source round trips over every existing fixture,
  the short-consumption error, and the short/long blob errors in
  `ComposeFrom`.

## Implementation notes

The implementation deviates from the text above in five places. Each is
deliberate, consistent across the code base, and pinned by
`registry/e2e_test.go` (`TestE2EPushPull`).

1. **Invalid tag on manifest push.** "Manifest push" step 1 names
   `400 TAG_INVALID`; that code is not in the standard list under "HTTP
   surface", so the registry answers `400 MANIFEST_INVALID` with a message
   starting `invalid tag`. On `GET`, `HEAD` and `DELETE` a malformed tag is
   `404 MANIFEST_UNKNOWN`.
2. **Upload session ownership.** "Finalization removes the session from the
   map" happens only after `blob.Store.Put` succeeds. On an internal failure
   (`500`) the session and its spool stay registered with every byte
   received, as the error-handling table requires, so the client can retry
   the `PUT`; `GET` on the session keeps answering `204` with the full
   `Range`. A digest mismatch discards the session, since its bytes can never
   become valid. A monolithic `POST ?digest=` session is discarded on every
   failure because the client never learns its id.
3. **`rawReason` in blob `meta.json`.** The key is omitted for prisms
   (`omitempty`) rather than written as `""`; raw blobs always carry it.
   Likewise `diffId`, `uncompressedSize`, `entries`, `engine` and
   `engineVersion` are absent for raw blobs rather than zero-valued, and
   `engine`/`engineVersion` are absent for an uncompressed prism
   (`format: "none"`), which has no compressor to name.
4. **Blob root write.** The blob root and `meta.json` go through a second
   accounting writer (`packstore.WriteParallel` with `Verify`, one extra
   fsync) whose stats are discarded, not through a separate `WriteBatch`.
   The observable contract is unchanged: the stats in `meta.json` and in the
   blob log line cover exactly the ingest objects, never the root or
   `meta.json`. The image root and its `meta.json` are written the same way.
5. **Log line keys.** The blob line carries `raw_reason=` and `duration=`
   for raw blobs and `engine=`, `entries=` and `duration=` for prisms; the
   image line carries `manifests=` and `duration=` in addition to the keys
   listed under "Accounting and logging". An identical manifest re-push logs
   again with `disk_bytes=0` and `compression_ratio=+Inf`. Internal failures
   log `request failed` at Error level. The README shows the real shape.
