# interweb headless runner

A YAML-configured process for publishing, seeding, and serving websites through
the shared `interwebs/app` backend. No Wails dependency, dashboard, or management API.

## Try the included site

From the repository root:

```sh
cd interwebs-hosted
GOWORK=off go build -o ../build/bin/interweb ./cmd/interweb
../build/bin/interweb run --config examples/local.yaml
```

Open **http://127.0.0.1:8080/**. The runner publishes the example folder and serves
its encrypted stored version, not the source directory. Stop with Ctrl+C or SIGTERM.
Restart preserves identities and keys. JSON output includes profile/site IDs,
magnets, browser URLs, progress, and backend errors. It emits a ready snapshot and
status every 5 seconds. Ready means listeners are available, not that all remote
downloads or DHT announcements have finished.

## Simple CLI

With the built `interweb` executable on your PATH:

```sh
interweb init
interweb add --folder ./blog --listen 127.0.0.1:8080
interweb add --name mirror --magnet 'magnet:?xt=urn:btih:YOUR_HASH' --listen 127.0.0.1:8081
interweb list
interweb check
interweb run
# In another terminal:
interweb status
```

`init` creates `interweb.yaml`, never overwriting an existing file. `add` preserves
comments and replaces the config atomically; it does not start networking.
All commands accept `--config path/to/config.yaml`. Use `interweb COMMAND --help`
for flags. Folders supply a default name; magnets and IDs require `--name`.
Use `add --id SITE_ID --name existing` to select stored content without publishing
a source folder again. Use `--live` for automatic publication of source changes.
Omit `--listen` for torrent-only hosting/seeding.

Adding a folder explicitly grants access to **that folder only** in `source_roots`.
CLI folder paths resolve from your current directory and are stored canonically.
Manually written YAML paths resolve from the config directory. Concurrent config
editors must coordinate. `check` checks schema/values/path resolution, not network
availability, filesystem publication permissions, or whether a port is free.

## YAML configuration

```yaml
data_dir: ./data
source_roots: [./websites]
offline: false
max_site_mib: 512
node_storage_mib: 2048
sites:
  - name: blog
    folder: ./websites/blog
    live: false
    hosting: true
    favorite: false
    serve:
      listen: 127.0.0.1:8080
  - name: mirror
    magnet: "magnet:?xt=urn:btih:YOUR_HASH"
    hosting: true
```

Each entry requires a unique name and exactly one of `folder`, `magnet`, or `id`.
An empty list is valid for initial setup. Files are limited to 1 MiB and 256 sites.
Unknown fields, duplicate keys, multiple YAML documents, and invalid values fail
before application startup. Decoding uses [Go YAML v3](https://pkg.go.dev/go.yaml.in/yaml/v3).

Defaults: publish folders once at each startup; Live off; hosting on; favorite off;
512 MiB per version; 2 GiB ciphertext per Node. There is no startup/content-preparation deadline.
File-type filtering remains off. The backend's default retention policy applies.
Configuration changes require restart; Live watches source content, not YAML.

`hosting: false` removes hosting intent; favorites and active View leases still
follow the shared application's seeding policy. It is not a global uploads-off switch.

`serve: {}` allocates a loopback port and reports its URL. Omitting `serve` creates
no browser-facing listener. Serving holds a View lease for the process lifetime.
Each site has its own origin. The stable listener switches to newly published or
refreshed versions; open pages may need reloading. As with many static servers,
asset requests crossing an update can span versions.

## Serving to other machines

LAN/public exposure is explicit:

```yaml
serve:
  listen: 0.0.0.0:8080
  public_url: http://192.168.1.20:8080
```

Alternatively, bind loopback behind your TLS reverse proxy and set
`public_url: https://blog.example.com`. Preserve that Host header at the proxy.
The runner serves plain HTTP; it does not provision certificates. Configure
firewalls/TLS externally. Non-loopback listeners are public and unauthenticated.
Use a distinct origin per site, separate from administrative applications; do not
route multiple sites under paths on one origin.

Listeners expose content only: GET/HEAD/OPTIONS, range support, exact Host checks,
and the core sandbox/CSP translated to the configured public origin. Responses
stream from the backend's loopback server and verified encrypted stores. No whole
file buffering, arbitrary proxy targets, application state, keys, or source-directory
serving is introduced.

## Persistence and lifecycle

Use a dedicated data directory with one profile. The runner creates that profile
on first run and refuses ambiguous multi-profile directories. Nodes and stores
remain independent. Back up the data directory securely; never publish or commit it.

Profiles start paused while config is applied. YAML controls active hosting,
favorite, and Live settings. Removing entries disables their activity next run,
but does not delete registrations, source folders, signing keys, or owned stores.
Application is not an all-or-nothing transaction: a later startup failure may leave
earlier saved changes. Listeners and Nodes are cleaned up; fix the error and restart.

Shutdown closes listeners, releases View leases, and closes the backend. HTTP
requests get five seconds to drain before forced connection closure. Startup and
output/listener failures also clean up. No browser launch, management API, config
hot reload, or multiple-profile orchestration is implemented in this adapter.

## Tests

```sh
GOWORK=off go test -race -count=1 ./...
GOWORK=off go vet ./...
```

Local tests cover config edits/validation, CLI flows, real stored-site HTTP,
ranges, host checks, update switching, restart identity, failed/canceled startup,
context shutdown, and Unix SIGTERM. Cloud/public-network acceptance, browser/TLS
proxy acceptance, and native Windows/macOS runs remain follow-ups.

## Preparation, retrying, and public discovery

Public DHT discovery/announcements and torrent networking are enabled by default.
Use `offline: true` only for explicitly isolated tests. Announcing a signed address
is separate from preparing local content or uploading pieces to interested peers;
there is no meaningful "fully broadcast all content" milestone.

HTTP listeners start before content is ready. They return 503 with Retry-After
while preparing; retrying a page later serves verified content once available.
For remote sites, viewing needs the authenticated manifest and verified
`index.html`, not the entire download. Other assets are fetched on demand; a
missing asset can wait or fail independently without blocking the index. Initial
publication still has to hash the complete source to construct its manifest.
Sites prepare independently through backend operations. Slow downloads/publication
do not exit the runner, and runtime content failures retry instead of taking other
sites down. Existing served content remains available while a replacement prepares.
On restart, an owned site's stored index is opened before preparing its source
again. The stored version resumes seeding during publication; the HTTP endpoint
keeps serving it until a replacement view is ready, even if the update fails.
Status reports `updating` while a stored view is available during publication.
This still requires a valid configured source folder at registration, and local
storage must open and verify successfully before its content can be served.
There is no runner-wide deadline. Individual network requests and HTTP connections
still have safety timeouts; failed content attempts are retried.

Check from another terminal:

```sh
interweb status --config interweb.yaml
# Or refresh continuously:
interweb status --watch --config interweb.yaml
```

`run` and `status` show a human-readable dashboard by default. Interactive
terminals redraw in place; redirected output contains plain snapshots without
terminal escape codes. Use `--json` on either command for machine-readable output.
`status --watch` only reads the saved status; it can monitor an already running
instance without restarting it, and Ctrl+C stops the watcher, not that instance.
Snapshots older than 15 seconds are flagged as stale. The on-disk status remains
JSON regardless of display mode. Download progress is not source-import progress.

This reads `data_dir/runner-status.json`, atomically refreshed every five seconds,
without acquiring the application's lock or opening a control API. Check `updated`
and `event`: clean shutdown writes `stopped`; crashes can leave a stale snapshot.

Per-site `phase`, `error`, and `operation` show preparation/retries and queued or
running publication/view work. `state.Core` reports download Bytes/Total, Peers,
Seeding, signed Record.Sequence versus AnnouncedSequence, and distinct announcement,
refresh, mapping, and download errors. Publication currently reports operation
state and start time, not a byte-level snapshot-import percentage. Torrent byte
progress must not be mistaken for publication or upload progress.

`PeerAnnouncementError` reports failures starting torrent peer discovery separately
from `AnnouncementError` (the signed address). Seeding explicitly announces peers
on enable/new content and retries every minute. Neither a successful announcement
nor an empty error guarantees connectivity through a firewall or NAT. Testing two
processes behind one router can also depend on that router's NAT loopback support.
