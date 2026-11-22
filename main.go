package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

//go:embed web/*
var web embed.FS

var pages = template.Must(template.New("ui.html").Funcs(template.FuncMap{
	"project": projectName,
	"requestID": func(session, id string) string {
		return "request-" + base64.RawURLEncoding.EncodeToString([]byte(session+"\x00"+id))
	},
	"markdown": renderMarkdown,
	"historyLink": func(id, session, before string) historyView {
		return historyView{ID: id, Session: session, Before: before}
	},
}).ParseFS(web, "web/ui.html"))

func projectName(path string) string {
	if path == "" {
		return "Tau session"
	}
	return filepath.Base(path)
}

func render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func main() {
	socket := flag.String("socket", defaultSocket(), "Tau coordinator Unix socket")
	listen := flag.String("listen", "127.0.0.1:8787", "address to listen on; use :8787 to accept connections from other hosts")
	tlsCert := flag.String("tls-cert", "", "TLS certificate file; serves HTTPS together with -tls-key")
	tlsKey := flag.String("tls-key", "", "TLS private key file")
	flag.Parse()
	password := os.Getenv("TAU_UI_PASSWORD")
	if (*tlsCert == "") != (*tlsKey == "") {
		log.Fatal("Pass both -tls-cert and -tls-key, or neither.")
	}
	if password == "" {
		log.Fatal("Set TAU_UI_PASSWORD to a strong password (the username is tau).")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, *listen, *tlsCert, *tlsKey, *socket, password); err != nil {
		log.Fatal(err)
	}
}

func defaultSocket() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "tau", "tau.sock")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("tau-%d", os.Getuid()), "tau.sock")
}

func serve(ctx context.Context, addr, tlsCert, tlsKey, socket, password string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	srv := &http.Server{
		Handler: newApp(socket, password), ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout: time.Minute,
	}
	scheme := "http"
	if tlsCert != "" {
		scheme = "https"
	} else if ip, ok := listener.Addr().(*net.TCPAddr); ok && !ip.IP.IsLoopback() {
		log.Print("Warning: serving plain HTTP beyond loopback sends the password unencrypted. Use -tls-cert and -tls-key, or a trusted network.")
	}
	if tcp, ok := listener.Addr().(*net.TCPAddr); ok && tcp.IP.IsUnspecified() {
		log.Printf("Tau UI: %s://<this host's address>:%d (listening on all interfaces, IPv4 and IPv6)", scheme, tcp.Port)
	} else {
		log.Printf("Tau UI: %s://%s", scheme, listener.Addr())
	}
	done := make(chan error, 1)
	go func() {
		if tlsCert != "" {
			done <- srv.ServeTLS(listener, tlsCert, tlsKey)
		} else {
			done <- srv.Serve(listener)
		}
	}()
	select {
	case err := <-done:
		return fmt.Errorf("serve Tau UI: %w", err)
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownErr := srv.Shutdown(shutdown)
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			shutdownErr = errors.Join(shutdownErr, err)
		}
		return shutdownErr
	}
}

func newApp(socket, password string) http.Handler {
	a := app{socket: socket}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.page)
	mux.HandleFunc("GET /instances", a.instances)
	mux.HandleFunc("GET /worker/{id}", a.page)
	mux.HandleFunc("GET /worker/{id}/feed", a.feed)
	mux.HandleFunc("GET /worker/{id}/history", a.history)
	mux.HandleFunc("GET /worker/{id}/content", a.entryContent)
	mux.HandleFunc("GET /code.css", codeCSS)
	mux.HandleFunc("POST /worker/{id}/lifecycle", a.lifecycle)
	mux.HandleFunc("POST /worker/{id}/prompt", a.prompt)
	mux.HandleFunc("POST /worker/{id}/queue/edit", a.editQueue)
	mux.HandleFunc("POST /worker/{id}/interrupt", a.interrupt)
	mux.HandleFunc("POST /worker/{id}/answer", a.answer)
	mux.HandleFunc("POST /start", a.start)
	for _, asset := range []string{"htmx.min.js", "style.css", "ui.js"} {
		mux.HandleFunc("GET /"+asset, func(w http.ResponseWriter, r *http.Request) {
			http.ServeFileFS(w, r, web, "web/"+asset)
		})
	}
	want := sha256.Sum256([]byte(password))
	protected := new(http.CrossOriginProtection).Handler(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		user, secret, ok := r.BasicAuth()
		got := sha256.Sum256([]byte(secret))
		if !ok || user != "tau" || subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="Tau UI", charset="UTF-8"`)
			http.Error(w, "Authentication required", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost && r.Header.Get("HX-Request") != "true" {
			http.Error(w, "Submit from the Tau UI", http.StatusForbidden)
			return
		}
		protected.ServeHTTP(w, r)
	})
}
