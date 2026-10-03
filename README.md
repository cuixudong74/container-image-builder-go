# container-image-builder

A local, daemon-less container image builder. It takes a build description
file (Dockerfile-compatible) and a build context directory, and produces an
OCI image archive that can be loaded directly by a container runtime — no
external daemon, no network, fully reproducible.

## Features

- **Multi-stage builds** with `COPY --from=<stage>` dependencies.
- **Content-addressed layer cache** reused across builds; upstream changes
  invalidate all downstream layers automatically.
- **Reproducible output**: fixed epoch timestamps, sorted file ordering,
  canonical permissions (dirs `0755`, files `0644`, symlinks `0777`).
  Rebuilding the same input always yields byte-identical configs, manifests
  and archives.
- **Multi-architecture** builds in one invocation, synthesized into an OCI
  image index (manifest list) with per-platform selection.
- **Parallel stage execution** with a shared cache; failures cancel the whole
  build immediately while completed cache entries are preserved.
- **Concurrency-safe cache** with file locking and atomic writes (temp +
  fsync + rename); cancellation never leaves half-written entries.
- **Locatable errors**: every failure reports the stage, line and instruction
  that caused it.
- **No daemon, no network**: `FROM scratch` and local OCI/docker archive
  tarballs for base images; `RUN` executes via chroot (requires root).

## Build

```sh
go build -o image-builder .
```

## Usage

```sh
image-builder build [options]
```

### Options

| Flag | Default | Description |
|------|---------|-------------|
| `-f` | `Dockerfile` | Build description file |
| `-c` | `.` | Build context directory |
| `-o` | `image.tar` | Output OCI archive path |
| `-cache` | `.build-cache` | Layer cache directory |
| `-platform` | host platform | Comma-separated target platforms (`linux/amd64,linux/arm64`) |
| `-t` | | Image tag (comma-separated) |
| `-base` | | Base image mapping `name=path.tar` (comma-separated) |
| `-epoch` | `0` | Fixed build epoch (unix seconds) for reproducibility |
| `-max-layer-size` | `0` | Maximum uncompressed layer size in bytes (0 = unlimited) |
| `-max-context-file` | `0` | Maximum context file size in bytes (0 = unlimited) |
| `-workers` | `4` | Parallel stage workers |

### Example

```sh
image-builder build -f Dockerfile -c . -o image.tar \
  --platform linux/amd64,linux/arm64 \
  -t myapp:latest
```

### Loading the artifact

The output is an OCI image layout archive. Load it with:

```sh
# containerd
ctr images import image.tar

# skopeo (to docker daemon)
skopeo copy oci:image.tar docker-daemon:myapp:latest
```

## Build description format

The description file is Dockerfile-compatible. Supported instructions:

- `FROM [--platform=os/arch] <image>[:tag] [AS <name>]`
- `RUN <command>` (shell form) or `RUN ["exec", "args"]` (exec form)
- `CMD` / `ENTRYPOINT` (shell or exec form)
- `COPY [--from=<stage>] [--chmod=octal] <src>... <dest>`
- `ADD [--from=<stage>] <src>... <dest>` (local tar archives are extracted)
- `ENV <key>=<value>...`
- `LABEL <key>=<value>...`
- `ARG <name>[=<default>]` (before or within stages)
- `WORKDIR <path>`
- `USER <user>`
- `EXPOSE <port>[/proto]...`
- `VOLUME <path>...`
- `MAINTAINER <text>` (deprecated, sets author)

Variable expansion: `$VAR`, `${VAR}`, `${VAR:-default}`, `${VAR:+word}`,
`$$` (literal `$`). Line continuations with `\` and `#` comments are
supported. The `# escape=` parser directive changes the escape character.

### Base images

`FROM scratch` starts from an empty rootfs. To build on a base image,
provide a local OCI or docker-save archive tarball:

```sh
image-builder build --base ubuntu=ubuntu-24.04.tar ...
```

Then `FROM ubuntu` in the description uses the archive. No network access is
performed.

### RUN execution

`RUN` executes the command inside the stage rootfs via `chroot` (requires
root). Shell form uses `/bin/sh -c`; exec form runs the binary directly.
The binary must exist in the rootfs (e.g. copied with `COPY --chmod=0755`).

## Build artifact structure

The output tar is an OCI image layout archive:

```
oci-layout              # {"imageLayoutVersion":"1.0.0"}
index.json              # OCI image index (manifest list)
blobs/sha256/<digest>   # config blobs, manifest blobs, layer tars
```

For a single-platform build, `index.json` contains one manifest descriptor.
For multi-platform builds, it contains one descriptor per platform, sorted
by platform string for determinism. Each manifest references its config blob
and layer blobs.

### Layer tar

Each layer is an uncompressed OCI layer tar (`application/vnd.oci.image.layer.v1.tar`):

- Entries are sorted by path.
- All timestamps are the fixed build epoch.
- UID/GID are `0`, user/group names empty.
- Permissions are canonicalized: directories `0755`, regular files `0644`,
  symlinks `0777`. `COPY --chmod` overrides the file mode.
- Deleted files are recorded as OCI whiteout entries (`.wh.<name>`).
- Parent directories are included implicitly.

## Digest calculation rules

All digests are SHA-256, serialized as `sha256:<hex>`.

- **Context tree digest**: over the sorted list of `(type, path, content-hash)`
  for every non-ignored entry. File content is hashed directly; symlinks are
  hashed by target. Permission bits and timestamps are excluded.
- **Layer digest**: over the uncompressed tar stream bytes.
- **Cache key**: over canonical JSON of `{version, parent-chain-key, platform,
  instruction-fields, context-tree-digest (for COPY/ADD), source-stage-chain
  (for COPY --from)}`. The parent is the previous instruction's chain key (or
  the base image manifest digest / a constant for scratch).
- **Config digest**: over the deterministic JSON serialization of the image
  config (created timestamp, architecture, runtime config, rootfs diff IDs,
  history).
- **Manifest digest**: over the deterministic JSON serialization of the
  manifest (config descriptor + layer descriptors).
- **Index digest**: over the deterministic JSON serialization of the index.

Because every input to these digests is content-addressed and canonical, the
same build input produces byte-identical artifacts regardless of machine,
time, or file ordering.

## Cache behavior

- The cache directory (`.build-cache` by default) stores layer blobs and
  entry metadata keyed by cache key.
- Cache hits reuse the layer blob across builds and across stages.
- A change to any instruction or context file changes the chain key,
  invalidating that layer and all downstream layers.
- Concurrent builds (same machine or different machines sharing a cache
  directory) are safe: per-key file locks plus in-process mutexes prevent
  corruption; all writes go through temp file + fsync + rename.
- Completed cache entries are immutable and preserved on failure or
  cancellation.

## Error reporting

Errors identify the stage, line and instruction:

```
stage "builder", line 12 (COPY): context file "src.txt" not found
stage #0, line 3 (RUN): command "/bin/sh" in chroot ... failed: ...
```

Malformed description files, missing context files, unknown stage references,
and oversized layers all produce stable, locatable error messages.

## Testing

```sh
go test ./...
```

Tests cover: parser errors and multi-stage parsing, context change detection
and ignore rules, cache hit/miss and concurrent writes, multi-stage
dependencies, multi-arch synthesis, concurrent builds, interrupt recovery,
reproducibility, and error paths.
