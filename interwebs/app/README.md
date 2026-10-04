# interweb application backend

`app` manages profiles and site policy above independent core Nodes. Wails and
hosted adapters can share this package without owning torrent, key, or cache logic.
It does not implement a GUI, remote API, authentication, or browser launch.

```go
application, err := app.Open(ctx, "./interweb-data")
if err != nil {
    return err
}
defer application.Close()

profile, err := application.CreateProfile(ctx, "Personal",
    app.WithSourceRoots("./websites"))
if err != nil {
    return err
}
blog, err := profile.AddFolder(ctx, "./websites/blog")
if err != nil {
    return err
}
operation, err := blog.Publish(ctx)
if err != nil {
    return err
}
result, err := operation.Wait(ctx)
if err != nil {
    return err
}
fmt.Println(result.Publication.Magnet)
```

Create a profile once; reopen it with `application.Profile(id)` on later runs.
`AddFolder` saves an independent signing identity without publishing. File-type
filtering is off unless requested. Publication enables hosting; Live is opt-in.

See [REVIEW.md](REVIEW.md) for usage, defaults, tradeoffs, and remaining work.
The [core review](../REVIEW.md) covers the lower-level primitives underneath this layer.

From `interwebs/`:

```sh
gofmt -w app
GOWORK=off go test -buildvcs=false -race ./...
GOWORK=off go vet -buildvcs=false ./...
```
