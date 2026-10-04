set working-directory := '..'

# Start the desktop development app (requires Wails and native dependencies).
dev:
    wails dev -tags webkit2_41

# Build a native desktop release.
build:
    wails build -tags webkit2_41

# Build the headless integration driver.
node:
    go build -buildvcs=false -o build/bin/interweb-node ./cmd/interweb-node
