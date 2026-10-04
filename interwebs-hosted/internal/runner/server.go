package runner

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/hlfshell/interweb/interwebs/app"
)

// endpoint streams a single site's verified loopback response without buffering
// the payload. A separate listener/origin per site preserves browser isolation.
type endpoint struct {
	mu        sync.RWMutex
	view      *app.View
	target    *url.URL
	hash      string
	origin    string
	address   string
	server    *http.Server
	transport *http.Transport
	done      chan struct{}
	err       error
}

func serve(c ServeConfig, view *app.View, hash string) (*endpoint, error) {
	var target *url.URL
	if view != nil {
		var err error
		target, err = url.Parse(view.URL())
		if err != nil {
			return nil, err
		}
	}
	l, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", c.Listen, err)
	}
	origin := c.PublicURL
	if origin == "" {
		origin = "http://" + l.Addr().String()
	}
	public, _ := url.Parse(origin)
	e := &endpoint{view: view, target: target, hash: hash, origin: origin, address: l.Addr().String(), done: make(chan struct{})}
	e.transport = &http.Transport{DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		MaxIdleConns: 16, MaxIdleConnsPerHost: 16, IdleConnTimeout: 30 * time.Second,
		ResponseHeaderTimeout: 65 * time.Second, DisableCompression: true}
	e.server = &http.Server{ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, WriteTimeout: 70 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Host != public.Host {
				http.Error(w, "Invalid host", http.StatusForbidden)
				return
			}
			if r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" {
				w.Header().Set("Allow", "GET, HEAD, OPTIONS")
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}
			e.mu.RLock()
			if e.target == nil {
				e.mu.RUnlock()
				w.Header().Set("Retry-After", "5")
				w.Header().Set("Cache-Control", "no-store")
				http.Error(w, "Site is preparing; retry shortly.", http.StatusServiceUnavailable)
				return
			}
			target := *e.target
			e.mu.RUnlock()
			// Fixed targets come only from our own backend, never a client URL/header.
			proxy := &httputil.ReverseProxy{Transport: e.transport, Rewrite: func(p *httputil.ProxyRequest) {
				p.SetURL(&target)
				p.Out.Host = target.Host
				p.Out.Header.Del("Cookie")
				p.Out.Header.Del("Authorization")
			}, ModifyResponse: func(response *http.Response) error {
				csp := response.Header.Get("Content-Security-Policy")
				response.Header.Set("Content-Security-Policy", strings.ReplaceAll(csp, "http://"+target.Host, origin))
				response.Header.Del("Set-Cookie")
				return nil
			}, ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
				http.Error(w, "Content unavailable", http.StatusBadGateway)
			}}
			proxy.ServeHTTP(w, r)
		})}
	go func() {
		defer close(e.done)
		if err := e.server.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			e.err = err
		}
	}()
	return e, nil
}

func (e *endpoint) replace(view *app.View, hash string) error {
	target, err := url.Parse(view.URL())
	if err != nil {
		return errors.Join(err, view.Close())
	}
	e.mu.Lock()
	old := e.view
	e.view, e.target, e.hash = view, target, hash
	e.mu.Unlock()
	if old != nil {
		return old.Close()
	}
	return nil
}

func (e *endpoint) close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := e.server.Shutdown(ctx)
	if err != nil {
		err = errors.Join(err, e.server.Close())
	}
	<-e.done
	e.transport.CloseIdleConnections()
	if e.view != nil {
		err = errors.Join(err, e.view.Close())
	}
	return errors.Join(err, e.err)
}
