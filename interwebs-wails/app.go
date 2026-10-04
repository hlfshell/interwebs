package main

import (
	"context"
	"fmt"
	stdRuntime "runtime"
	"sync/atomic"
	"time"

	"github.com/energye/systray"
	"github.com/godbus/dbus/v5"
	"github.com/wailsapp/wails/v2/pkg/runtime"
	site "github.com/hlfshell/interweb/interwebs"
)

type App struct {
	ctx          context.Context
	service      *site.Service
	startupError error
	trayReady    atomic.Bool
	quitting     atomic.Bool
	trayEnd      func()
}

func NewApp() *App { return &App{} }
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.service, a.startupError = site.New(site.Options{})
	if a.startupError != nil {
		return
	}
	if trayAvailable() {
		start, end := systray.RunWithExternalLoop(nil, func() { a.trayReady.Store(false) })
		a.trayEnd = end
		start()
		icon := trayIcon
		if stdRuntime.GOOS != "windows" {
			icon = trayPNG
		}
		systray.SetIcon(icon)
		systray.SetTitle("interweb")
		systray.SetTooltip("interweb — distributed websites")
		systray.SetOnClick(func(systray.IMenu) { runtime.WindowShow(ctx) })
		systray.SetOnRClick(func(menu systray.IMenu) { menu.ShowMenu() })
		systray.AddMenuItem("Show interweb", "").Click(func() { runtime.WindowShow(ctx) })
		systray.AddMenuItem("Pause / resume hosting", "").Click(func() { a.service.SetPaused(!a.service.Status().Paused) })
		systray.AddMenuItem("Quit", "").Click(func() { a.quitting.Store(true); runtime.Quit(ctx) })
		a.trayReady.Store(true)
	}
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				runtime.EventsEmit(ctx, "sites:changed", a.service.Status())
			}
		}
	}()
}
func trayAvailable() bool {
	if stdRuntime.GOOS != "linux" {
		return true
	}
	conn, e := dbus.ConnectSessionBus()
	if e != nil {
		return false
	}
	defer conn.Close()
	var present bool
	e = conn.BusObject().Call("org.freedesktop.DBus.NameHasOwner", 0, "org.kde.StatusNotifierWatcher").Store(&present)
	return e == nil && present
}
func (a *App) shutdown(context.Context) {
	if a.service != nil {
		a.service.Close()
	}
	if a.trayEnd != nil {
		a.trayEnd()
	}
}
func (a *App) beforeClose(context.Context) bool {
	if a.trayReady.Load() && trayAvailable() && !a.quitting.Load() {
		runtime.WindowHide(a.ctx)
		return true
	}
	return false
}
func (a *App) ready() error {
	if a.startupError != nil {
		return a.startupError
	}
	if a.service == nil {
		return fmt.Errorf("interweb is starting")
	}
	return nil
}
func (a *App) Status() (site.Status, error) {
	if e := a.ready(); e != nil {
		return site.Status{}, e
	}
	return a.service.Status(), nil
}
func (a *App) AddFolder() (string, error) {
	if e := a.ready(); e != nil {
		return "", e
	}
	p, e := runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{Title: "Select a static website folder"})
	if e != nil || p == "" {
		return "", e
	}
	return a.service.AddFolder(p)
}
func (a *App) Publish(id string) error {
	if e := a.ready(); e != nil {
		return e
	}
	return a.service.Publish(id)
}
func (a *App) Open(magnet string) error {
	if e := a.ready(); e != nil {
		return e
	}
	address, e := a.service.Open(magnet)
	if e != nil {
		return e
	}
	runtime.BrowserOpenURL(a.ctx, address)
	return nil
}
func (a *App) SetFavorite(id string, value bool) error {
	if e := a.ready(); e != nil {
		return e
	}
	return a.service.SetFavorite(id, value)
}
func (a *App) SetHosting(id string, value bool) error {
	if e := a.ready(); e != nil {
		return e
	}
	return a.service.SetHosting(id, value)
}
func (a *App) SetLive(id string, value bool) error {
	if e := a.ready(); e != nil {
		return e
	}
	return a.service.SetLive(id, value)
}
func (a *App) Stop(id string) error {
	if e := a.ready(); e != nil {
		return e
	}
	return a.service.Stop(id)
}
func (a *App) ClearCache(id string) error {
	if e := a.ready(); e != nil {
		return e
	}
	return a.service.ClearCache(id)
}
func (a *App) SetSettings(settings site.Settings) error {
	if e := a.ready(); e != nil {
		return e
	}
	return a.service.SetSettings(settings)
}
func (a *App) SetPaused(value bool) error {
	if e := a.ready(); e != nil {
		return e
	}
	a.service.SetPaused(value)
	return nil
}
func (a *App) Quit() { a.quitting.Store(true); runtime.Quit(a.ctx) }
