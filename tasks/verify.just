set working-directory := '..'

# Deterministic backend tests, including real local peer transfers.
test:
    go test -race -timeout 180s ./internal/... ./cmd/...
    node --test scripts/remote-lifecycle.test.mjs

# Validate and build frontend assets.
frontend:
    npm --prefix frontend ci
    npm --prefix frontend run build

# Real browser-engine sandbox and desktop UI tests (requires Playwright browsers and ffmpeg).
browser:
    go build -buildvcs=false -o build/bin/interweb-node ./cmd/interweb-node
    npm --prefix frontend test

# Temporary public-DHT test with automatic DigitalOcean cleanup.
remote:
    node scripts/remote-test.mjs

# Reconcile and remove resources from expired interweb test runs.
cleanup:
    node scripts/remote-test.mjs --cleanup
