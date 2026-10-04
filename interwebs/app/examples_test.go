package app_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/hlfshell/interweb/interwebs/app"
)

func Example() {
	if err := exampleApplication(); err != nil {
		panic(err)
	}
	// Output:
	// published version: 1
	// browser response: Hello interweb
	// author version: 1
}

func exampleApplication() error {
	ctx := context.Background()
	root, err := os.MkdirTemp("", "interweb-app-example-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	source := filepath.Join(root, "blog")
	if err := os.Mkdir(source, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(source, "index.html"), []byte("Hello interweb"), 0600); err != nil {
		return err
	}

	application, err := app.Open(ctx, filepath.Join(root, "data"), app.WithOffline(true))
	if err != nil {
		return err
	}
	defer application.Close()
	profile, err := application.CreateProfile(ctx, "Personal", app.WithSourceRoots(source))
	if err != nil {
		return err
	}
	blog, err := profile.AddFolder(ctx, source)
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
	fmt.Println("published version:", result.Publication.Record.Sequence)

	operation, err = blog.View(ctx)
	if err != nil {
		return err
	}
	result, err = operation.Wait(ctx)
	if err != nil {
		return err
	}
	defer result.View.Close()
	response, err := http.Get(result.View.URL())
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1024))
	if err != nil {
		return err
	}
	fmt.Println("browser response:", string(body))

	writer, err := profile.CreateAuthor(ctx, "My projects")
	if err != nil {
		return err
	}
	if err := writer.SetSites(ctx, blog.ID()); err != nil {
		return err
	}
	operation, err = writer.Publish(ctx)
	if err != nil {
		return err
	}
	result, err = operation.Wait(ctx)
	if err != nil {
		return err
	}
	fmt.Println("author version:", result.Publication.Record.Sequence)
	return nil
}
