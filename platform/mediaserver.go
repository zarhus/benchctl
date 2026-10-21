// SPDX-FileCopyrightText: 2026 3mdeb <contact@3mdeb.com>
//
// SPDX-License-Identifier: Apache-2.0

package platform

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"sync"
	"time"
)

// imageServer serves one file over HTTPS for a BMC to fetch as virtual media.
// It answers HEAD and byte ranges through http.ServeContent, 404 on every other
// path, and logs the first request from each address and a line every
// summaryEvery while requests arrive.
type imageServer struct {
	file     string
	urlPath  string // the path the file is served at
	listener net.Listener
	server   *http.Server
	progress io.Writer
	done     chan struct{}

	mu       sync.Mutex
	seen     map[string]bool // remote hosts that have sent a request
	requests int
	bytes    int64
	reported int // requests at the last summary
}

// imageSummaryEvery is how often the server logs its request count.
const imageSummaryEvery = 30 * time.Second

// startImageServer serves file at urlPath on listener over TLS, with a
// self-signed certificate for host.
func startImageServer(file, urlPath string, listener net.Listener, host string, progress io.Writer) (*imageServer, error) {
	info, err := os.Stat(file)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", file)
	}
	cert, err := selfSignedCert(host)
	if err != nil {
		return nil, err
	}
	srv := &imageServer{
		file:     file,
		urlPath:  urlPath,
		listener: listener,
		progress: progress,
		done:     make(chan struct{}),
		seen:     map[string]bool{},
	}
	srv.server = &http.Server{
		Handler:           srv,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}},
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() { _ = srv.server.ServeTLS(listener, "", "") }()
	go srv.summarize(imageSummaryEvery)
	fmt.Fprintf(progress, "Serving %s on %s.\n", file, listener.Addr())
	return srv, nil
}

// ServeHTTP serves the file at urlPath and nothing else. It cleans the request
// path first, since AMI asks for "//<file name>" for an image URL at the root.
func (srv *imageServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	srv.logRequest(r)
	if path.Clean(r.URL.Path) != srv.urlPath || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(srv.file)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.ServeContent(&countingWriter{ResponseWriter: w, srv: srv}, r, "", info.ModTime(), f)
}

// logRequest counts r and logs the first request from its host.
func (srv *imageServer) logRequest(r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	srv.requests++
	if srv.seen[host] {
		return
	}
	srv.seen[host] = true
	rangeNote := ""
	if r.Header.Get("Range") != "" {
		rangeNote = " " + r.Header.Get("Range")
	}
	fmt.Fprintf(srv.progress, "Image server: first request from %s: %s %s%s (%s).\n", host, r.Method, r.URL.Path, rangeNote, r.UserAgent())
}

// summarize logs the request count and bytes served every interval in which
// requests arrived, until Close.
func (srv *imageServer) summarize(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-srv.done:
			return
		case <-ticker.C:
			srv.mu.Lock()
			if srv.requests != srv.reported {
				fmt.Fprintf(srv.progress, "Image server: %d requests, %d bytes served.\n", srv.requests, srv.bytes)
				srv.reported = srv.requests
			}
			srv.mu.Unlock()
		}
	}
}

// requested reports whether any request has reached the server.
func (srv *imageServer) requested() bool {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	return srv.requests > 0
}

// port is the port the server listens on.
func (srv *imageServer) port() int {
	return srv.listener.Addr().(*net.TCPAddr).Port
}

// Close stops the server and drops open connections.
func (srv *imageServer) Close() error {
	close(srv.done)
	return srv.server.Close()
}

// countingWriter adds the body bytes written to the server's count.
type countingWriter struct {
	http.ResponseWriter
	srv *imageServer
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.srv.mu.Lock()
	w.srv.bytes += int64(n)
	w.srv.mu.Unlock()
	return n, err
}

// selfSignedCert makes an ECDSA certificate for host, an IP address or a DNS
// name.
func selfSignedCert(host string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	template := &x509.Certificate{
		Subject:     pkix.Name{CommonName: host},
		NotBefore:   now.Add(-time.Hour),
		NotAfter:    now.AddDate(1, 0, 0),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else if host != "" {
		template.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}
