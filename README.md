# interweb

*basically...*

internet made and hosted by peoples. Share a site from your own hardware. Visitors and fans help host it! Completely removed from the trillion dollar companies that watch your every move.

*technically...*

interweb uses distributed protocols (BitTorrent, DHTs, etc) to serve static web apps. Downloaded data is sandboxed an executed safely. Visiting sites or favoriting them turns you into a peer for the site. The more popular a site becomes, the more peers it has.

Host a static website from a folder, share a signed magnet link, and help keep other people's sites online by favoriting them. interweb handles BitTorrent and local HTTP serving; websites open directly in your normal browser.

See [encrypted storage and viewing sites](STORAGE.md) for usage, keys, limits, and verification.

## Development status

The independent-Node core is ready for API review; application adapters are still
being updated. Start with [the core README](interwebs/README.md),
[the review checkpoint](interwebs/REVIEW.md), and [the checklist](TODO.md).
The first shared application backend now implements profiles, favorites, hosting,
Live publishing, and retention above the core. Start its review with
[the application walkthrough](interwebs/app/REVIEW.md). Wails and hosted adapters
have not been connected to it yet.
