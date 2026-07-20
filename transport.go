package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"path"
	"strings"
	"time"
)

func newHTTPClient(enableFileTransport, enableUnixSocketTransport bool, timeout time.Duration) *http.Client {
	defaultTransport := http.DefaultTransport.(*http.Transport).Clone()

	if enableFileTransport {
		fileTransport := http.NewFileTransport(http.Dir("."))
		defaultTransport.RegisterProtocol("file", RoundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host != "." {
				r.URL.Path = path.Join(r.URL.Host, r.URL.Path)
				r.URL.Host = "."
			}
			return fileTransport.RoundTrip(r)
		}))
	}

	var transport http.RoundTripper
	if enableUnixSocketTransport {
		transport = transportWithUnixSupport(defaultTransport)
	} else {
		transport = defaultTransport
	}

	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}
}

func transportWithUnixSupport(transport *http.Transport) http.RoundTripper {
	unixTransport := transport.Clone()
	unixTransport.Proxy = nil
	dialer := &net.Dialer{}
	unixTransport.DialContext = func(ctx context.Context, _, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		encodedPath, ok := strings.CutSuffix(host, ".unix")
		if !ok {
			return nil, fmt.Errorf("invalid unix socket address: %s", address)
		}
		socketPath, err := base64.RawURLEncoding.DecodeString(encodedPath)
		if err != nil {
			return nil, fmt.Errorf("invalid unix socket address %s: %w", address, err)
		}
		return dialer.DialContext(ctx, "unix", string(socketPath))
	}

	return &unixSupportTransport{
		transport:     transport,
		unixTransport: unixTransport,
	}
}

type unixSupportTransport struct {
	transport     *http.Transport
	unixTransport *http.Transport
}

func (t *unixSupportTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "unix" {
		return t.transport.RoundTrip(r)
	}
	if r.URL.Host != "" || !strings.HasPrefix(r.URL.Path, "/") {
		return nil, fmt.Errorf("invalid unix socket URL: expected unix:///absolute/path.sock[/resource]")
	}

	parts := strings.Split(r.URL.Path, "/")
	for i, part := range parts {
		if !strings.HasSuffix(part, ".sock") {
			continue
		}

		originalRequest := r
		r = r.Clone(r.Context())
		socketPath := strings.Join(parts[:i+1], "/")
		resourcePath := "/"
		if i+1 < len(parts) {
			resourcePath += strings.Join(parts[i+1:], "/")
		}
		if r.Host == "" {
			r.Host = r.Header.Get("Host")
			if r.Host == "" {
				r.Host = "localhost"
			}
		}
		r.URL.Scheme = "http"
		r.URL.Host = base64.RawURLEncoding.EncodeToString([]byte(socketPath)) + ".unix"
		r.URL.Path = resourcePath
		r.URL.RawPath = ""
		resp, err := t.unixTransport.RoundTrip(r)
		if resp != nil {
			resp.Request = originalRequest
		}
		return resp, err
	}
	return nil, fmt.Errorf("invalid unix socket URL: path must contain a segment ending in .sock")
}

func (t *unixSupportTransport) CloseIdleConnections() {
	t.transport.CloseIdleConnections()
	t.unixTransport.CloseIdleConnections()
}

type RoundTripFunc func(*http.Request) (*http.Response, error)

func (fn RoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r)
}
