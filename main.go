package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"embed"
	"encoding/base64"
	"encoding/pem"
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

var pages = template.Must(template.New("ui.html").Funcs(controlFuncs).Funcs(template.FuncMap{
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
	tlsCA := flag.String("tls-ca", "", "public PEM CA certificate to offer without sign-in at /ca.crt")
	home, _ := os.UserHomeDir() // An empty default resolves to the working directory.
	projectRoot := flag.String("project-root", home, "directory to browse for projects (defaults to your home directory)")
	flag.Parse()
	password := os.Getenv("TAU_UI_PASSWORD")
	if (*tlsCert == "") != (*tlsKey == "") {
		log.Fatal("Pass both -tls-cert and -tls-key, or neither.")
	}
	if password == "" {
		log.Fatal("Set TAU_UI_PASSWORD to a strong password (the username is tau).")
	}
	root, err := filepath.Abs(*projectRoot)
	if err != nil {
		log.Fatalf("resolve project root: %v", err)
	}
	var caCert []byte
	if *tlsCA != "" {
		caCert, err = readCACertificate(*tlsCA)
		if err != nil {
			log.Fatal(err)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, *listen, *tlsCert, *tlsKey, *socket, password, root, caCert); err != nil {
		log.Fatal(err)
	}
}

func defaultSocket() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "tau", "tau.sock")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("tau-%d", os.Getuid()), "tau.sock")
}

func readCACertificate(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read CA certificate: %w", err)
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("%q must contain a single PEM CA certificate", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate %q: %w", path, err)
	}
	if !cert.IsCA {
		return nil, fmt.Errorf("%q is not a CA certificate", path)
	}
	// Serve only the parsed certificate, never any ancillary data from the file.
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), nil
}

func serve(ctx context.Context, addr, tlsCert, tlsKey, socket, password, projectRoot string, caCert []byte) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	srv := &http.Server{
		Handler: newApp(ctx, socket, password, projectRoot, caCert), ReadHeaderTimeout: 5 * time.Second,
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

func newApp(ctx context.Context, socket, password, projectRoot string, caCert []byte) http.Handler {
	a := app{socket: socket, inbox: new(inboxOrder), restarts: newRestartWatch(), projectRoot: projectRoot}
	if path, err := dismissalsPath(); err != nil {
		log.Printf("find dismissed usage limits: %v", err)
	} else {
		a.inbox.loadDismissed(path)
	}
	go a.inbox.follow(ctx, socket)
	go a.restarts.follow(ctx, socket)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.page)
	mux.HandleFunc("GET /instances", a.instances)
	mux.HandleFunc("GET /worker/{id}", a.page)
	mux.HandleFunc("GET /worker/{id}/feed", a.feed)
	mux.HandleFunc("GET /worker/{id}/history", a.history)
	mux.HandleFunc("GET /worker/{id}/content", a.entryContent)
	mux.HandleFunc("GET /code.css", codeCSS)
	mux.HandleFunc("POST /worker/{id}/lifecycle", a.lifecycle)
	mux.HandleFunc("POST /worker/{id}/dismiss-limit", a.dismissLimit)
	mux.HandleFunc("POST /worker/{id}/prompt", a.prompt)
	mux.HandleFunc("POST /worker/{id}/queue/edit", a.editQueue)
	mux.HandleFunc("POST /worker/{id}/queue/pause", a.pauseQueue)
	mux.HandleFunc("POST /worker/{id}/queue/resume", a.resumeQueue)
	mux.HandleFunc("POST /worker/{id}/interrupt", a.interrupt)
	mux.HandleFunc("POST /worker/{id}/answer", a.answer)
	mux.HandleFunc("GET /worker/{id}/controls", a.controls)
	mux.HandleFunc("GET /worker/{id}/controls/pick", a.controlsPick)
	mux.HandleFunc("POST /worker/{id}/controls", a.controlAction)
	mux.HandleFunc("GET /worker/{id}/export", a.exportHTML)
	mux.HandleFunc("GET /worker/{id}/last-reply", a.lastReply)
	mux.HandleFunc("GET /worker/{id}/commands", a.commands)
	mux.HandleFunc("POST /worker/{id}/complete", a.completePath)
	mux.HandleFunc("GET /project-dirs", a.projectDirs)
	mux.HandleFunc("POST /start", a.start)
	mux.HandleFunc("GET /app.webmanifest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/manifest+json")
		http.ServeFileFS(w, r, web, "web/app.webmanifest")
	})
	for _, asset := range []string{"htmx.min.js", "style.css", "ui.js", "icon-192.png", "icon-512.png"} {
		mux.HandleFunc("GET /"+asset, func(w http.ResponseWriter, r *http.Request) {
			http.ServeFileFS(w, r, web, "web/"+asset)
		})
	}
	want := sha256.Sum256([]byte(password))
	protected := new(http.CrossOriginProtection).Handler(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; manifest-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if len(caCert) > 0 && r.URL.Path == "/ca.crt" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			w.Header().Set("Content-Type", "application/x-x509-ca-cert")
			w.Header().Set("Content-Disposition", `attachment; filename="tau-ui-ca.crt"`)
			http.ServeContent(w, r, "tau-ui-ca.crt", time.Time{}, bytes.NewReader(caCert))
			return
		}
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
