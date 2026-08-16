package exporter

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProbeTargetTimeout(t *testing.T) {
	client := newHTTPClient(false, false, 10*time.Millisecond)
	client.Transport = RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})

	cfg := mustCompileConfig(t, &Config{Modules: map[string]Module{"test": {}}})
	result := testReq(http.MethodGet, "/probe?module=test&debug=true&target=http://example.test", nil, handleProbe(cfg, client, defaultMaxResponseBodySize))
	assert(t, http.StatusOK, result.StatusCode)
	body := string(must[[]byte](t)(io.ReadAll(result.Body)))
	if !strings.Contains(body, "context deadline exceeded") {
		t.Fatalf("debug response does not contain timeout error: %q", body)
	}
	if !strings.HasSuffix(body, probeStatus(0, 1, 0, 0, 0, 0)) {
		t.Fatalf("timeout was not recorded as a fetch error: %q", body)
	}
}

func TestUnixTransportReusesConnections(t *testing.T) {
	type unixServer struct {
		target      string
		connections *atomic.Int64
	}
	newUnixServer := func(name string) unixServer {
		t.Helper()
		tempDir := must[string](t)(os.MkdirTemp("/tmp", "prometheus-jq-exporter-"))
		t.Cleanup(func() { os.RemoveAll(tempDir) })
		socketPath := filepath.Join(tempDir, name+".sock")
		connections := &atomic.Int64{}
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/redirect" {
				w.Header().Set("Location", "status")
				w.WriteHeader(http.StatusFound)
				return
			}
			fmt.Fprintf(w, "%s %s", r.Host, r.URL.RequestURI())
		}))
		listener := must[net.Listener](t)(net.Listen("unix", socketPath))
		server.Listener = &countingListener{Listener: listener, connections: connections}
		server.Start()
		t.Cleanup(server.Close)
		return unixServer{
			target:      "unix://" + socketPath + "/status",
			connections: connections,
		}
	}

	first := newUnixServer("first")
	second := newUnixServer("second")
	client := newHTTPClient(false, true, defaultTargetTimeout)
	t.Cleanup(client.CloseIdleConnections)
	request := func(target string) string {
		t.Helper()
		req := must[*http.Request](t)(http.NewRequest(http.MethodGet, target, nil))
		req.Header.Set("Host", "unix.test")
		originalURL := req.URL.String()
		resp := must[*http.Response](t)(client.Do(req))
		defer resp.Body.Close()
		body := string(must[[]byte](t)(io.ReadAll(resp.Body)))
		assert(t, originalURL, req.URL.String())
		return body
	}

	assert(t, "unix.test /status", request(first.target))
	assert(t, "unix.test /status", request(first.target))
	assert(t, "unix.test /status", request(second.target))
	assert(t, "unix.test /status?format=json", request(first.target+"?format=json"))
	assert(t, "unix.test /status", request(strings.TrimSuffix(first.target, "/status")+"/redirect"))
	assert(t, int64(1), first.connections.Load())
	assert(t, int64(1), second.connections.Load())
}

func TestUnixTransportRootAndDefaultHost(t *testing.T) {
	tempDir := must[string](t)(os.MkdirTemp("/tmp", "prometheus-jq-exporter-"))
	t.Cleanup(func() { os.RemoveAll(tempDir) })
	testSock := filepath.Join(tempDir, "test.sock")
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s %s", r.Host, r.URL.RequestURI())
	}))
	server.Listener = must[net.Listener](t)(net.Listen("unix", testSock))
	server.Start()
	t.Cleanup(server.Close)

	client := newHTTPClient(false, true, defaultTargetTimeout)
	t.Cleanup(client.CloseIdleConnections)
	req := must[*http.Request](t)(http.NewRequest(http.MethodGet, "unix://"+testSock+"?format=json", nil))
	originalURL := req.URL.String()
	resp := must[*http.Response](t)(client.Do(req))
	defer resp.Body.Close()

	assert(t, "localhost /?format=json", string(must[[]byte](t)(io.ReadAll(resp.Body))))
	assert(t, originalURL, req.URL.String())
}

func TestUnixTransportDoesNotHijackHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, r.URL.RequestURI())
	}))
	t.Cleanup(server.Close)

	client := newHTTPClient(false, true, defaultTargetTimeout)
	t.Cleanup(client.CloseIdleConnections)
	resp := must[*http.Response](t)(client.Get(server.URL + "/assets/remote.sock/status?format=json"))
	defer resp.Body.Close()

	assert(t, "/assets/remote.sock/status?format=json", string(must[[]byte](t)(io.ReadAll(resp.Body))))
}

func TestUnixTransportRejectsInvalidURLs(t *testing.T) {
	client := newHTTPClient(false, true, defaultTargetTimeout)
	t.Cleanup(client.CloseIdleConnections)
	for _, target := range []string{
		"unix://host/path.sock/status",
		"unix:///path/without/socket",
	} {
		t.Run(target, func(t *testing.T) {
			_, err := client.Get(target)
			if err == nil || !strings.Contains(err.Error(), "invalid unix socket URL") {
				t.Fatalf("expected invalid unix socket URL error, got %v", err)
			}
		})
	}
}

func TestUnixTransportMustBeEnabled(t *testing.T) {
	client := newHTTPClient(false, false, defaultTargetTimeout)
	_, err := client.Get("unix:///path/to/target.sock")
	if err == nil || !strings.Contains(err.Error(), `unsupported protocol scheme "unix"`) {
		t.Fatalf("expected unsupported protocol scheme error, got %v", err)
	}
}

type countingListener struct {
	net.Listener
	connections *atomic.Int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.connections.Add(1)
	}
	return conn, err
}
