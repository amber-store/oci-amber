# oci-amber

oci-amber is an OCI distribution registry whose storage is an embedded
[amber-store-core](https://github.com/jobs-build/amber-store-core) store. It
speaks the standard `/v2/` API, so docker, containerd/nerdctl, podman/skopeo,
crane, oras and buildkit push and pull against it without any client-side
configuration beyond the registry URL, imports and writes `docker image
save` archives directly, lists what it holds, and has a terminal browser
over the store.

Instead of keeping each layer as an opaque compressed blob, oci-amber takes
layers apart on push: [zrecipe](https://github.com/draganm/zrecipe)
turns the compressed stream into the uncompressed tar plus a compression
recipe, [tar-prism](https://github.com/draganm/tar-prism) turns the tar into
per-file contents plus a tar recipe, and the file contents land in amber's
content-defined-chunked store where they deduplicate across layers and
images. Pulls rebuild the original bytes on the fly (tar-prism compose,
zrecipe recompress) and every served blob is byte-identical to what was
pushed; the sha256 is verified on the way out and a mismatch cuts the
connection rather than serving wrong bytes.

Layers whose compression cannot be reproduced exactly (a compressor
zrecipe does not know, a corrupt stream, a blob that is not a tar) are
stored verbatim and served with range support.

Every pushed container image also gets a root filesystem view: the layers
applied in order with OCI whiteout semantics, stored in the image root as
an amber directory tree whose regular files point at the content the layers
already hold. Building it replays tar headers from the stored recipes and
reads at most a block of file content. The `/fs/` API serves it: directory
listings as JSON, files with ranges, directories as tars.

## Requirements

Everything is provided by the Nix flake: Go 1.26, `pkg-config`, `zlib` and
`zstd` (zrecipe's cgo engines), `gzip` and `pigz` (fixtures), and `crane`
(client smoke test).

```sh
nix develop            # or `direnv allow` once
```

## Running

```sh
# straight from the dev shell
nix develop --command go run ./cmd/oci-amber serve --store /tmp/oci-amber --listen 127.0.0.1:5000

# or build the binary first
nix develop --command go build -o ./oci-amber ./cmd/oci-amber
./oci-amber serve --store /var/lib/oci-amber
```

Then push and pull as usual:

```sh
crane push image.tar 127.0.0.1:5000/library/app:v1
crane pull 127.0.0.1:5000/library/app:v1 pulled.tar
crane ls 127.0.0.1:5000/library/app

docker tag alpine:3.20 127.0.0.1:5000/alpine:3.20
docker push 127.0.0.1:5000/alpine:3.20
```

The registry is single tenant, unauthenticated and speaks plain HTTP; put it
behind a TLS terminator or use it on a trusted network.

`SIGINT`/`SIGTERM` stop the listener, wait up to 30 s for in-flight requests,
close the upload sessions and the store, and exit 0.

## Importing a docker save archive

`oci-amber import` stores an image saved with `docker image save` straight
into the store, without running the registry, through the same pipeline a
push goes through. On a terminal it shows per-layer progress and an ETA;
when it is done it prints a report of what was ingested.

```sh
docker image save busybox:1.37 -o busybox.tar
./oci-amber import --store /var/lib/oci-amber busybox.tar
docker image save app:v1 | ./oci-amber import --store /var/lib/oci-amber -
```

The image is published under the archive's `RepoTags` with a leading
registry host removed (`registry.example.ch/team/app:v1` becomes
`team/app:v1`); `--name repo:tag` overrides that for a single-image
archive. Only the platforms whose blobs the archive holds are published:
the index is pruned to them, as `docker push` does. Archives without an
OCI layout (`index.json`; Docker before 25, podman's docker-archive) are
rejected.

`import` shares `--store`, `--work-dir`, `--max-in-memory`,
`--analyze-parallelism`, `--analyze-timeout`, `--max-concurrent-finalize`,
`--verify-limit`, `--verify-roundtrip`, `--allow-raw` and `--log-level` with
`serve`, and adds
`--progress auto|tui|plain` (`auto` picks the TUI on a terminal),
`--log-file path` and `--name`. It cannot run while `serve` has the store
open. An interrupted import leaves the blobs it stored in place; running
it again skips them. The platform manifests of a multi-arch image are
published, and their root filesystem views built, `--max-concurrent-finalize`
at a time; the index follows once they are all stored.

The report:

```
Imported busybox.tar in 42s

  busybox:1.37   sha256:3f0a…9c2e   index, 1 platform + 1 attestation   rootfs ok, 1,204 entries

Blobs           8 processed: 6 stored (5 prism, 1 raw: not-tar), 2 already present
Compressed      1.8 MiB     1,900,727 bytes   blob and manifest bytes as they are in the archive
Uncompressed    4.2 MiB     4,388,352 bytes   after decompression; raw blobs counted as is
Added to CAS    1.1 MiB     1,153,024 bytes   appended to pack segments, manifests and rootfs tree included
Dedup ratio     1.65x       compressed bytes ÷ bytes added to CAS   (39.3% not written)
Chunks reused   38.2%       of offered bytes were already in the store
```

## Browsing a store

`oci-amber browse` opens a store in a terminal browser: the repositories
and their tags, how every image is stored, the root filesystem its layers
produce, and the content of any file it reaches.

```sh
./oci-amber browse --store /var/lib/oci-amber
./oci-amber browse --store /var/lib/oci-amber library/app:v1
```

It opens the store directly, so it cannot run while `serve` has it open
(`store … is in use by another process`), and it never writes: the work
directory is not touched and a missing store is an error, not created.
Stdout must be a terminal. `--log-file path` keeps the log; without it,
warnings are printed to stderr after the screen closes. The optional
argument starts inside a repository (`repo`) or an image (`repo:tag`,
`repo@sha256:…`).

The screen is one listing at a time under a breadcrumb. The repositories
come first; a repository lists its tags (kind, short digest, size, rootfs
status when not `ok`) and then the manifests no tag points at. An image
opens in the **storage** view: the amber tree under its root, annotated
with what oci-amber knows about it.

```
library/app › :v1 › storage
  blobs/          3 blobs
  manifest        application/vnd.oci.image.manifest.v1+json    1.2 KiB
  meta.json       kind, digest, stats and rootfs status          812 B
  rootfs/         ok · 1,204 entries
```

`blobs/` lists the config and layers in manifest order with how each is
stored (`layer 1/2 · prism gzip go-flate · 1,834 files · 402.1 MiB
uncompressed`, `config · raw not-tar`); a blob opens its root
(`meta.json`, `comp.json`, `recipe.json`, `recipe.bin`, `blobs/`, or
`meta.json` and `raw`), and a prism's `blobs/` shows every numbered file
next to the tar entry it holds. An index's `manifests/` lists its
children by platform. `rootfs/` is the stored tree as it is, in `ls -l`
columns, symlinks shown but not followed.

`f` switches to the **filesystem** view of the image the breadcrumb is
under: the same tree through the resolver the `/fs/` API uses, so
symlinks are followed (Enter on a symlink to a directory descends under
the link's own name). An index offers its platforms first; an image
without a view says why. `f` again returns to the storage view where it
was; both views keep their position.

Enter on a file opens the viewer. Text (valid UTF-8 without NUL bytes) is
shown with line numbers; JSON is pretty-printed and `p` shows the stored
bytes; anything else is a hex dump that reads only the window on screen,
so a 2 GiB blob opens at once, and `:` jumps to an offset. `h` switches
between text and hex; files over 8 MiB are hex only.

| Key | Action |
|---|---|
| `↑` `↓` `pgup` `pgdn` `g` `G` | move |
| `enter` `→` | open the row |
| `backspace` `←` `esc` | back |
| `f` | storage ↔ filesystem |
| `/` | filter the listing; in the viewer, search (`n`/`N` between hits) |
| `i` | full digest, key, mode, owner, mtime and more for the row |
| `h` | text ↔ hex in the viewer; back in a listing |
| `p` | JSON pretty ↔ stored bytes |
| `:` | go to an offset in hex |
| `q` `ctrl-c` | quit |

## Listing images

`oci-amber ls` prints the images in a store, one row per tag, sorted by
repository and tag.

```sh
./oci-amber ls --store /var/lib/oci-amber
./oci-amber ls --store /var/lib/oci-amber library/app
```

```
REPOSITORY    TAG    DIGEST         KIND                                  SIZE        ROOTFS   PUSHED
busybox       1.37   3f0a9c2e1b4d   manifest                              4.2 MiB     ok       2026-09-05 10:22
library/app   v1     a1b2c3d4e5f6   index (2 platforms + 1 attestation)   402.1 MiB   -        2026-09-04 18:03
```

DIGEST is the start of the manifest digest, SIZE the image as its manifest
describes it (config, layers and manifest bytes; for an index, every
child), ROOTFS whether the root filesystem view is there (`ok`, `partial`,
`unavailable`, `not-applicable`; `-` for an index) and PUSHED when the
image was stored, in local time. `-a` adds the manifests no tag points at
as `<none>`, except the children of an index, which its row accounts
for. An optional repository name restricts the listing to it. Like
`browse`, `ls` opens the store directly, so it cannot run while `serve`
has it open, and never writes.

## Saving an image

`oci-amber save` writes images to a `docker image save` archive, the
counterpart of `import`: an OCI layout inside a tar (`oci-layout`,
`index.json`, `blobs/sha256/*`) with Docker's `manifest.json` next to it,
the archive `docker save` writes since Docker 25 and `docker load` and
`crane push` read.

```sh
./oci-amber save --store /var/lib/oci-amber -o app.tar library/app:v1
./oci-amber save --store /var/lib/oci-amber busybox | docker load
./oci-amber save --store /var/lib/oci-amber -o all.tar library/app:v1 library/app@sha256:3f0a… busybox
```

A reference is `repo:tag`, `repo@sha256:…`, or a bare `repo`, which saves
every tag of the repository; several references go into one archive. The
archive goes to stdout unless `-o path` is given; a terminal is refused.
Layers stored as prisms are recomposed on the way out and their sha256
verified, and manifests are the stored bytes, so what `docker image
inspect` shows after a load is what the store holds. An index is saved
whole, every platform and attestation included; its `manifest.json`
entry, the one Docker's graph-driver loader reads, describes the child
for the host's architecture on linux (windows on a windows host), or the
first child that is an image with a config when none matches. An artifact
manifest, which has no config, gets no `manifest.json` entry.
`index.json` names the image the way docker does (`io.containerd.image.name`
is `docker.io/library/busybox:1.37` for `busybox:1.37`); an image saved by
digest has no name, and `import` needs `--name` for it. The archive is a
function of its content: saving the same image twice gives the same
bytes. Nothing is written before every reference has been resolved, and
`-o` removes a partial file after a failure. `save` opens the store
directly, so it cannot run while `serve` has it open.

Progress goes to stderr, so it shows whether the archive is a file or a
pipe: a screen with the blob being written, an overall bar, the elapsed
time and an ETA when stderr and stdin are terminals (`q` or ctrl-c
cancels and removes a partial `-o` file), a status line every few
seconds otherwise; `--progress tui|plain` (or `OCI_AMBER_PROGRESS`)
overrides the choice. Either way the save ends with one line on stderr,
`Saved library/app:v1 → app.tar: 1.2 GiB in 1m45s`. The ETA is the bytes
left over the rate so far, so it settles after a few seconds and is only
as good as the layers are alike: a prism layer is recomposed and
recompressed on the way out, a raw one copied.

## Configuration

`oci-amber serve` flags. Every flag can also be set through the environment
variable in the last column; a flag on the command line wins.

| Flag | Default | Meaning | Environment |
|---|---|---|---|
| `--store` | required | store directory (created on first start) | `OCI_AMBER_STORE` |
| `--work-dir` | `<store>/work` | parent of `<work-dir>/oci-amber/`, where spilled uploads, the zrecipe spool and the packs of layers being taken apart live; a staged pack is always on disk, whatever `--max-in-memory` says, and holds about the blob's compressed size for the length of one finalization; at startup the *contents* of `<work-dir>/oci-amber/uploads` and `<work-dir>/oci-amber/spool` are deleted (`import` also removes stale `<work-dir>/oci-amber/import-*.tar` stdin copies) and nothing else under `--work-dir` is touched | `OCI_AMBER_WORK_DIR` |
| `--listen` | `:5000` | listen address | `OCI_AMBER_LISTEN` |
| `--max-in-memory` | `64MiB` | upload spool and zrecipe spool threshold before spilling to `--work-dir`; units `B`, `KiB`, `MiB`, `GiB`, `KB`, `MB`, `GB` | `OCI_AMBER_MAX_IN_MEMORY` |
| `--analyze-parallelism` | `2` | zrecipe candidate workers per blob (each holds one engine working set) | `OCI_AMBER_ANALYZE_PARALLELISM` |
| `--analyze-timeout` | `15m` | per-blob analyze deadline; on expiry the blob cannot be a prism (see `--allow-raw`) | `OCI_AMBER_ANALYZE_TIMEOUT` |
| `--max-concurrent-finalize` | `NumCPU/2` (min 1) | concurrent blob finalizations | `OCI_AMBER_MAX_CONCURRENT_FINALIZE` |
| `--verify-limit` | `150MiB` | how much of a layer's compressed form the ingest reproduces before accepting the recompression: once zrecipe has matched this many bytes, in its engine search and in the confirming pass, the rest of the layer is taken apart without being recompressed; `0` verifies every byte; same units as `--max-in-memory` | `OCI_AMBER_VERIFY_LIMIT` |
| `--verify-roundtrip` | `false` | diagnostic: also run the full pull pipeline over every prism before publishing it; a mismatch is a round-trip failure (refused, or stored raw with `--allow-raw`) | `OCI_AMBER_VERIFY_ROUNDTRIP` |
| `--allow-raw` | `false` | store a layer raw when it cannot be stored as a prism (`not-reproducible`, `unsupported`, `corrupt`, `analyze-timeout`, `decompose-failed`, `roundtrip-failed`) instead of refusing the upload with `400 BLOB_UPLOAD_INVALID`; blobs that are not tars (configs, attestations, other artifacts) are always stored raw | `OCI_AMBER_ALLOW_RAW` |
| `--upload-timeout` | `1h` | idle upload session expiry and recent-uploads table TTL | `OCI_AMBER_UPLOAD_TIMEOUT` |
| `--gc-interval` | `0` | background GC cycle interval; `0` disables | `OCI_AMBER_GC_INTERVAL` |
| `--log-level` | `info` | `debug`, `info`, `warn` or `error` | `OCI_AMBER_LOG_LEVEL` |

## Storage layout

```
<store>/oci-amber.json          chunking and segment parameters, written on creation
<store>/packstore               amber pack segments
<store>/refs                    amber references
<store>/gc                      amber collector state
<store>/work/oci-amber/uploads  upload sessions that outgrew --max-in-memory
<store>/work/oci-amber/spool    zrecipe temporary files and staged packs
```

`oci-amber.json` pins the content-defined chunking (min 32 KiB, normal
512 KiB, max 1 MiB) and the 2 GiB pack segment size. A store created with
different parameters is refused, because changing chunk boundaries silently
defeats deduplication against existing content.

Every OCI object is an amber tree named by a reference, so amber's own tools
can inspect, export and garbage-collect them:

```
oci/blob/<algo>:<hex>                                   blob root
oci/manifest/<repo>/<algo>:<hex>                        image root
oci/tag/<repo>:<tag>                                    image root
oci/referrer/<repo>/<subject digest>/<referrer digest>  referrer's image root
```

A blob root is a tar-prism prism directory (`recipe.bin`, `recipe.json`,
`blobs/00000001…`) plus `comp.json` (zrecipe parameters) and `meta.json`;
raw blobs hold the verbatim bytes in `raw` instead. An image root holds the
exact manifest bytes in `manifest`, `meta.json`, and `blobs/` (and
`manifests/` for an index) whose entries point at the referenced roots, so an
image is one reachable tree.

The image root of a container image (an image manifest whose config is an
OCI or Docker image config) also holds `rootfs/`, the root filesystem the
layers produce: a plain amber directory tree with the tar's modes,
ownership, mtimes, symlinks, hard links, device nodes, FIFOs and extended
attributes, whose regular files point at the content the layers' prisms
already store, so it adds only directory objects and spilled xattr sets,
and amber's own tools can export or restore it. Its `meta.json` carries a
`rootfs` object with
`status` (`ok`, `partial`, `unavailable` or `not-applicable`) and `entries`;
`partial` lists the first 100 skipped entries with their layer, path and
reason plus `skippedCount`, `unavailable` gives the `reason`. A raw layer or
one whose headers `archive/tar` rejects makes the rootfs unavailable; a
sparse file, a path escaping the root, a hard link without a target or a
type tar cannot place is skipped. The push succeeds either way. Indexes and
artifacts have no rootfs; re-pushing a manifest reuses the tree already
stored under its digest (an unavailable one is tried again), and the same
image in two repositories shares every rootfs object. The tree is built
before the push takes the repository lock, so the platform manifests of
one image pushed at once are built in parallel.

## HTTP surface

The full distribution API: blob HEAD/GET/DELETE with single-range requests
on raw blobs, chunked (`POST`/`PATCH`/`PUT`) and monolithic uploads, cross
repository mounts, manifests and indexes by tag and digest, `tags/list` and
`_catalog` with `n`/`last` pagination and `Link` headers, the referrers API
(`OCI-Subject` is returned on push so clients do not fall back to tag
schemas), and the standard error envelope
`{"errors":[{"code":…,"message":…,"detail":…}]}`.

An upload whose digest is known before its bytes arrive, that is a
monolithic `POST ?digest=` or the `PUT ?digest=` that completes a session,
is checked against the stored blobs first. When the digest already exists
the answer is `201` without the request body being read: a small unread
body is drained, a large one makes the server close the connection behind
the response, and a session completed this way is dropped together with
whatever was `PATCH`ed into it. The bytes such a client sends are never
compared with the digest; the blob stored under it is the right one
regardless of what the client had.

Manifests are capped at 4 MiB (`413`). Blob uploads are not size limited.
Range requests on prism-stored layers are answered with the full body.

## Rootfs API

The root filesystem view of a container image is served under `/fs/`,
outside the distribution API:

```
GET /fs/<repo>:<tag>/<path>
GET /fs/<repo>@<digest>/<path>
```

The first path segment holding `@` or `:` ends the image reference (a
repository name can contain neither), everything after it is a path inside
the rootfs, cleaned with URL semantics so `..` never leaves the root.
Symlinks are followed in every component, the last one included, with
absolute targets rooted at the rootfs and a 40-hop bound, so `bin/ls` works
on a usrmerge image. `HEAD` returns the same headers as `GET` without a
body; other methods are `405`.

| Resolved entry | Query | Answer |
|---|---|---|
| directory | none | `200 application/json` listing, `n`/`last` pagination with a `Link` header like `tags/list` |
| directory | `format=tar` | `200 application/x-tar`, a streamed PAX tar of the subtree with names relative to the directory (like `tar -C dir .`); the root gives the whole rootfs |
| regular file | none | `200 application/octet-stream` with `Content-Length`, `Accept-Ranges: bytes`, a single `Range` honoured with `206`, `ETag` from the content key and `If-None-Match` answering `304` |
| regular file | `format=tar` | `400 PATH_INVALID` |
| device, fifo, socket | none | `200 application/json`, the entry object alone |

A listing:

```json
{"path": "etc",
 "entries": [
  {"name": "passwd", "type": "file", "mode": "0644", "uid": 0, "gid": 0, "mtime": "2026-09-03T18:00:00Z", "size": 1234},
  {"name": "rc.d", "type": "dir", "mode": "0755", "uid": 0, "gid": 0, "mtime": "2026-09-03T18:00:00Z"},
  {"name": "mtab", "type": "symlink", "mode": "0777", "uid": 0, "gid": 0, "mtime": "2026-09-03T18:00:00Z", "target": "/proc/mounts"},
  {"name": "null", "type": "char", "mode": "0666", "uid": 0, "gid": 0, "mtime": "2026-09-03T18:00:00Z", "major": 1, "minor": 3}]}
```

`type` is `file`, `dir`, `symlink`, `char`, `block`, `fifo` or `socket`;
`mode` is the permission, setuid, setgid and sticky bits in octal; `size`
appears on files, `target` on symlinks, `major` and `minor` on devices.
Entries are in name order.

A reference that resolves to an index needs `?platform=<os>/<arch>[/<variant>]`
to pick the child manifest; without it, or with no match, the answer is
`400 PLATFORM_UNKNOWN` whose `detail` lists the children's platforms. An
image without a view (a raw layer, an artifact, a root stored before views
existed) is `404 ROOTFS_UNAVAILABLE` with the status and reason in
`detail`. A missing path is `404 PATH_UNKNOWN`; a symlink loop, a bad
`format` or `format=tar` on a file is `400 PATH_INVALID`. These four codes
are oci-amber extensions rendered in the standard error envelope.

```sh
curl -s http://127.0.0.1:5000/fs/library/app:v1/etc | jq .
curl -s http://127.0.0.1:5000/fs/library/app:v1/etc/os-release
curl -s -r 0-1023 http://127.0.0.1:5000/fs/library/app:v1/usr/bin/app -o head.bin
curl -s 'http://127.0.0.1:5000/fs/library/app:v1/usr/share?format=tar' | tar -tv
curl -s 'http://127.0.0.1:5000/fs/library/app:latest/etc?platform=linux/arm64' | jq .
```

## Logging

Logs are `log/slog` text lines on stderr; `--log-level` selects the
threshold. Two Info lines carry the accounting.

After every blob finalization, one line per blob:

```
time=2026-09-03T18:00:01.412+02:00 level=INFO msg="blob stored" digest=sha256:4f7c… size=31457280 kind=prism format=gzip engine=go-flate entries=1834 logical_bytes=98304211 deduped_bytes=91250688 disk_bytes=2611200 duration=1.842s
time=2026-09-03T18:00:01.420+02:00 level=INFO msg="blob stored" digest=sha256:9b2e… size=1523 kind=raw format=none raw_reason=not-tar logical_bytes=1523 deduped_bytes=0 disk_bytes=1560 duration=3.1ms
```

`kind` is `prism` (file contents plus recipes; `engine` names the compressor
that reproduces the layer and `entries` counts its regular files) or `raw`
(verbatim bytes; `raw_reason` is one of `not-reproducible`, `unsupported`,
`corrupt`, `not-tar`, `analyze-timeout`, `roundtrip-failed`,
`decompose-failed`). An uncompressed tar is a prism with `format=none` and an
empty `engine`: there is no compressor whose output has to be reproduced. An
empty archive (nothing but zero blocks, the blob Docker uses as an empty
layer) is a prism with `entries=0`. A prism line never carries
`raw_reason`; a raw line never carries `engine` or `entries`. `logical_bytes` is the encoded size of every object offered to the
store, `deduped_bytes` the part that already existed, and `disk_bytes` what
was actually appended to pack segments; the blob root and its `meta.json` are
not counted. A blob that is uploaded again is not re-ingested and counts as
fully deduplicated: instead of a `blob stored` line, a whole-blob dedup hit
(the pushed digest already exists, whether that was found before the body
was read or at finalization) logs `msg="blob already present"` at Info with
`digest` and `size`, and nothing is written.

After every manifest or index push, one line per image (an identical
re-push logs again, with `disk_bytes=0` and `compression_ratio=+Inf`):

```
time=2026-09-03T18:00:02.007+02:00 level=INFO msg="image pushed" repo=library/app reference=v1 digest=sha256:c81d… kind=manifest blobs=3 manifests=0 rootfs=ok rootfs_entries=4213 total_bytes=95631872 logical_bytes=327545651 deduped_bytes=293700000 deduped_percent=89.7 disk_bytes=10276044 compression_ratio=9.31 duration=18.6ms
time=2026-09-03T18:00:02.311+02:00 level=INFO msg="image pushed" repo=library/app reference=latest digest=sha256:e07a… kind=index blobs=0 manifests=2 total_bytes=191267209 logical_bytes=655091302 deduped_bytes=620841219 deduped_percent=94.8 disk_bytes=10280120 compression_ratio=18.61 duration=4.2ms
```

`total_bytes` is the image size as the manifest describes it (config, layers
and the manifest bytes; for an index, the index bytes plus every child's
total). `blobs` and `manifests` count the descriptors that were resolved. The
byte counters add up the blobs uploaded for this image (blobs that were
already present count as fully deduplicated) and the manifest's own objects.
`compression_ratio` is `total_bytes / disk_bytes` (`+Inf` when nothing new
reached the disk) and `deduped_percent` is
`100 * deduped_bytes / logical_bytes`. The same numbers are stored in the
image's `meta.json`.

A manifest line also carries `rootfs=<status>` and, when a tree was stored,
`rootfs_entries=<n>`; an index line carries neither. A rootfs that is
missing or incomplete logs one more line at Warn level:

```
time=2026-09-03T18:00:02.008+02:00 level=WARN msg="rootfs unavailable" repo=library/app digest=sha256:c81d… reason="layer sha256:9b2e… is stored raw (not-reproducible)"
time=2026-09-03T18:00:02.008+02:00 level=WARN msg="rootfs partial" repo=library/app digest=sha256:c81d… skipped=1 path=usr/lib/big.img reason="sparse file"
```

Internal failures answer `500` with `{"errors":[]}` and log
`msg="request failed"` at Error level with `method`, `path` and `error`; a
blob upload that fails this way keeps its session so the client can retry
the `PUT`. A pull-side reproduction failure (compressor drift after an
upgrade) is logged at Error level with the digest, and the connection is cut
so the client sees a truncated body rather than wrong bytes.

## Garbage collection

Deleting a tag, manifest or blob removes references; the objects become
unreachable and are reclaimed by amber's collector once their pack is old
enough. `--gc-interval` runs the collector in-process; with it off, run the
amber CLI's `gc run` on the store while the server is stopped. Blob
references are never dropped automatically: `DELETE /v2/<name>/blobs/<digest>`
removes one.

## Limitations

- No authentication or authorization; blobs are global, not repository
  scoped.
- One process owns the store; nothing else may open it while the server
  runs.
- Upload sessions do not survive a restart; clients restart the blob on
  `BLOB_UPLOAD_UNKNOWN`.
- Only `sha256` digests.
- The rootfs view is built for image manifests only and skips what a tar
  cannot faithfully place: sparse files, paths escaping the root, hard links
  without a target, unknown entry types. A raw layer or a hard link carrying
  a payload (which `archive/tar` cannot parse) leaves the image without a
  view.
- The rootfs API is read-only, unauthenticated like the rest, lists no
  extended attributes (the tar carries them) and does not sniff content
  types.
- A layer zrecipe cannot rebuild is refused: the upload answers `400
  BLOB_UPLOAD_INVALID` with a message naming the reason and `--allow-raw`,
  and `import` fails with the same message. Every prism is verified on the
  way in: the confirming pass recompresses the layer once through the pull
  path and compares it byte for byte with the upload, so a layer that would
  not rebuild is caught as `not-reproducible` before it is published, with
  no second recompression. That comparison stops at `--verify-limit`
  (default 150 MiB of compressed bytes): a compressor that has reproduced
  that much of a layer and diverges later is rare, and recompressing the
  rest of a multi-GiB layer costs minutes of single-threaded deflate, so
  past the limit the layer is taken apart without being recompressed. A
  layer that does diverge past the limit is stored as a prism and fails
  the recompression's digest check on pull, as an error rather than wrong
  bytes; `--verify-limit 0` restores whole-layer verification.
  `--verify-roundtrip` (default off) additionally
  replays the whole pull pipeline from the stored objects as a diagnostic.
  With `--allow-raw` such a layer is stored raw with its reason instead
  (`raw_reason=roundtrip-failed` when `--verify-roundtrip` caught it before
  publishing); bytes are never lost
  either way. zrecipe v0.1.0 failed the round trip for layers gzipped at
  best-speed over content that barely compresses (the shape `crane append`
  produces); v0.2.0 fixed it and the crane smoke test now asserts such a
  layer becomes a prism. `docs/zrecipe-zlib-level0-roundtrip.md` records the
  investigation.
- Layers written by umoci and rockcraft (Canonical's rocks on Docker Hub,
  `ubuntu` included) are klauspost pgzip streams over an old klauspost
  encoder; zrecipe v0.2.0 stored them raw as `not-reproducible`, v0.3.0
  reproduces them with its `pgzip` engine
  ([zrecipe#2](https://github.com/draganm/zrecipe/issues/2)). A blob is
  classified once: a layer stored raw by an earlier binary stays raw, and
  gets no rootfs view, until `DELETE /v2/<name>/blobs/<digest>` removes it
  and the image is pushed again.

## Development

```sh
nix develop --command go test ./...              # unit, round-trip and HTTP conformance tests
nix develop --command go vet ./...
nix develop --command go build ./cmd/oci-amber   # the binary, with the cgo engines
```

`registry/e2e_test.go` pushes a two-image index, an artifact and a second
repository through the HTTP API the way docker, podman, containerd and crane
do it (including a `PUT` that fails with `500` and is retried), checks how
every blob was stored, pulls everything back byte for byte and checks the
log lines above. The crane smoke test in `cmd/oci-amber` runs only when
`crane` is on `PATH` (it is, inside the dev shell): it starts the real server
wiring, runs `crane push`, `pull`, `validate`, `append`, `ls`, `catalog` and
`delete` against it, restarts it on the same store and pulls again. Tests
never need the network.

The design document lives in `docs/superpowers/specs/2026-09-03-oci-amber-design.md`.

## License

AGPL-3.0-or-later, like amber-store-core, zrecipe and tar-prism.
