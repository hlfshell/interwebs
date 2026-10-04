# interweb core

One independent Node per Content identity. Nodes provide operations; applications
decide favorites, retention, refresh schedules, and startup behavior.

```go
signing, err := identity.Load(ctx, "./keys/blog.key")
// For a new identity, call identity.Create once instead. Never replace a lost key.
if err != nil {
    return err
}
blog, err := site.New(ctx, "./blog")
if err != nil {
    return err
}
node, err := interwebs.New(ctx,
    interwebs.WithContent(blog),
    interwebs.WithSigner(signing),
    interwebs.WithDataDir("./content/sites/blog"),
    interwebs.WithKeyFile("./keys/storage.key"),
)
if err != nil {
    return err
}
defer node.Close()

published, err := node.Publish(ctx)
if err != nil {
    return err
}
url, err := node.View(ctx)
if err != nil {
    return err
}
fmt.Println(published.Magnet, url)
```

Root import: `github.com/hlfshell/interweb/interwebs`; companion imports end in
`/site`, `/identity`, `/author`, and `/network`.
See [executable public examples](examples_test.go) and [package guide](../PROPOSAL.md).

## Folder file types

Folder import includes all regular file types by default, including extensionless
files. Use `site.New(ctx, folder, site.WithFileTypeFiltering(true))` to limit
publication to recognized web resource types. Hidden paths and symlinks stay excluded;
path confinement, required index.html, and size limits remain enforced.

The option controls publication only. Node.Read and Download can retrieve the
extra files; the browser server still permits only recognized resource types.
Reapply the option when constructing the local Site after restart.

## Operations, not preferences

- `View(ctx)`: prepare a version-specific loopback URL; no browser launch.
- `Download(ctx)`: fetch all files. Browsing normally fetches requested files only.
- `Read(ctx, path, network.ReadOptions{})`: read one file; close the returned reader.
- `DownloadFile(ctx, path, network.ReadOptions{Mode: network.Sequential})`: explicitly
  fetch one file with forward priority. Normal mode uses normal torrent scheduling.
- `Seed(ctx)` / `Stop(ctx)`: enable/disable uploads only. New Nodes do not upload.
  Publish enables seeding after committing a publication.
- `Refresh(ctx)`: check the signed address now. `WithRefreshInterval` opts into a
  caller-selected schedule; there is no default polling or live-folder watcher.
- `Publish(ctx)`: snapshot the local source using its signing key. Edit the folder
  externally and publish again to update the same signed address.
- `Versions(ctx)`: list known published hashes with valid local metadata, including
  partial downloads. Returns copied VersionInfo values, not storage handles.
- `StorageUsage(ctx)`: inspect all physical encrypted stores, including temporary
  staging and failed-publication leftovers. Bytes includes MetadataBytes.
- `ReleaseVersion`, `RemoveVersion`: explicitly reclaim selected versions. Readers,
  served versions, and active transfers are protected from removal.
- `Close()`: release resources, not data. Canceling the constructor context also
  closes the Node; canceling a download does not.

`Status()` separates availability, progress, authority, seeding, and download,
refresh, persistence, announcement, and NAT mapping errors. A partial site is not
an error. Mapping failures do not prevent outbound connections; NAT cleanup
failures are also returned by Close.
Publish returns after local durability, not after public-network discovery.
`Status().AnnouncedSequence` is zero until an announcement succeeds; compare it
with the current signed sequence rather than treating absence of an error as success.

## Storage and identity

Different Nodes require different directories. A pre-provisioned storage-key file
may be shared; runtime objects and stores are not. Without WithKeyFile, the default
backend generates or reuses storage.key in the Node data directory. Key files must
contain exactly 32 bytes; oversized files are rejected after a bounded read. A missing key
with existing encrypted stores is an error, never silently replaced. Custom
backends manage their own keys and do not create this file. Explicit signing keys are
independent of encryption keys and are never copied into Node state by construction.

The default content maximum is 512 MiB per version (`WithMaxSiteBytes`).
`WithStorageLimit` caps physical encrypted bytes including retained versions and staging,
defaulting to 2 GiB. Neither limit implies automatic eviction, and neither is saved
as an application preference. Publication can need space for old, staged,
and new data simultaneously. Metadata accounting includes both sandboxed's
checkpoint manifest and encrypted WAL. Quota reservations retain checkpoint
headroom for shutdown, so usable payload space can be lower than the disk budget.

Downloads use a bounded asynchronous receive queue: up to 4 MiB of plaintext
payload per Node, plus a 256 KiB merge buffer. Writes are batched around 256 KiB;
partial batches become eligible to flush after 25 ms (disk contention can delay
completion). Full queues apply backpressure rather than growing memory use.
Reads and piece verification wait for prior writes to reach encrypted storage;
normal shutdown drains accepted writes. A crash can lose uncommitted blocks,
which are downloaded again. An asynchronous storage failure prevents successful
verification and is returned by subsequent I/O and shutdown; fix the storage
problem and reopen the Node to retry. Publishing still writes directly to storage.

Publication staging is private: callers use Publish, not a workspace API. Staging
is encrypted and cleaned up on normal completion or failure; crash/cleanup-failure
leftovers remain accounted for. Automatic orphan cleanup is not implemented.

Publishing reads the local source, never changes its files, and writes directly to
encrypted storage. Remote references can be viewed or seeded but cannot publish
without a local source. Existing browser URLs stay on their immutable versions.

An optional author directory signs an endorsement of independent site keys. It
does not grant the author key authority to change a site's content or replace its
key. Author profile content is independently stored and explicitly published.

## Development

From this module:

```sh
GOWORK=off go test -buildvcs=false -count=1 -race -timeout 180s ./...
GOWORK=off go vet -buildvcs=false ./...
gofmt -l .
cd browser
npm ci
npx playwright install chromium firefox
npm test
```

The core browser suite needs Go, Node.js, ffmpeg, and compatible Chromium/Firefox
executables. `CHROMIUM_PATH` and `FIREFOX_PATH` override executable paths;
`BROWSERS=chromium` selects one engine. On this NixOS machine Chromium uses the
installed executable, and bundled Firefox runs with `steam-run npm test`.

Applications still need adaptation to this API. No compatibility layer is retained.
Node state format is version 4; earlier development directories are rejected.
Cloud resources are not used by these tests.

Receive benchmarks include draining buffered writes to encrypted storage:

```sh
GOWORK=off go test ./storage -run '^$' -bench 'BenchmarkTorrentWrites/16KiB|BenchmarkReceiveLargeChunks/shuffled=true/64KiB|BenchmarkReceiveVerified' -benchtime=1x -count=3
```

`BenchmarkReceiveVerified` also reads back, hashes, and completes each piece with
one or four concurrent simulated peers. These isolate storage performance, not
public-network throughput; run them without concurrent test/build jobs.
