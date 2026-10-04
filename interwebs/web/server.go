// Package web serves isolated content on loopback, never an application control API.
package web

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hlfshell/interweb/interwebs/content"
	"github.com/hlfshell/interweb/interwebs/network"
	"github.com/hlfshell/interweb/interwebs/site"
)

type Source interface {
	Manifest() content.Manifest
	Open(context.Context, string) (content.Reader, error)
}

type Server struct {
	server   *http.Server
	url      string
	once     sync.Once
	stop     func() bool
	done     chan struct{}
	closeErr error
}

// New starts a unique loopback origin and borrows source until Close returns.
func New(ctx context.Context, source Source) (*Server, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if source == nil || source.Manifest().Hash() == "" {
		return nil, content.ErrUnresolved
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	host := l.Addr().String()
	s := &Server{url: "http://" + host + "/", done: make(chan struct{})}
	s.server = &http.Server{ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, WriteTimeout: 65 * time.Second, Handler: handler(host, source)}
	s.stop = context.AfterFunc(ctx, func() { s.server.Close() })
	go func() {
		defer close(s.done)
		err := s.server.Serve(l)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.closeErr = err
		}
	}()
	return s, nil
}
func (s *Server) URL() string { return s.url }
func (s *Server) Close() error {
	s.once.Do(func() { s.stop(); err := s.server.Close(); <-s.done; s.closeErr = errors.Join(s.closeErr, err) })
	return s.closeErr
}

func handler(host string, source Source) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != host {
			http.Error(w, "Invalid host", http.StatusForbidden)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" {
			w.Header().Set("Allow", "GET, HEAD, OPTIONS")
			http.Error(w, "Method not allowed", 405)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" || strings.HasSuffix(name, "/") {
			name += "index.html"
		}
		if _, err := source.Manifest().File(name); err != nil {
			http.NotFound(w, r)
			return
		}
		mime := site.ContentType(name)
		if mime == "" {
			http.NotFound(w, r)
			return
		}
		h := w.Header()
		origin := "http://" + host
		h.Set("Content-Type", mime)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy", fmt.Sprintf("sandbox allow-scripts; default-src 'none'; script-src %s 'unsafe-inline'; style-src %s 'unsafe-inline'; img-src %s data:; media-src %s; font-src %s; connect-src %s; worker-src 'none'; frame-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'", origin, origin, origin, origin, origin, origin))
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), usb=(), serial=(), payment=()")
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Range")
		h.Set("Access-Control-Expose-Headers", "Content-Range, Accept-Ranges, Content-Length")
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		var reader content.Reader
		var err error
		if streaming, ok := source.(interface {
			Read(context.Context, string, network.ReadOptions) (content.Reader, error)
		}); ok && (strings.HasPrefix(mime, "video/") || strings.HasPrefix(mime, "audio/")) {
			reader, err = streaming.Read(ctx, name, network.ReadOptions{Mode: network.Sequential})
		} else {
			reader, err = source.Open(ctx, name)
		}
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				http.NotFound(w, r)
			} else {
				http.Error(w, "Content unavailable", 503)
			}
			return
		}
		defer reader.Close()
		http.ServeContent(w, r, name, time.Time{}, reader)
	})
}
