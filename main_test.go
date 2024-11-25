package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/johejo/prometheus-jq-exporter/internal/diff"
)

func Test(t *testing.T) {
	*enableFileTransport = true
	*enableUnixSocketTransport = true
	t.Cleanup(func() {
		*enableFileTransport = false
		*enableUnixSocketTransport = false
	})

	cfg, err := loadConfig("./testdata/config.yaml", false)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("file", func(t *testing.T) {
		result := testReq(http.MethodGet, "/probe?module=tailscale&target=file://testdata/tailscale-status.json", nil, handleProbe(cfg))
		assert(t, 200, result.StatusCode)

		b := string(must[[]byte](t)(io.ReadAll(result.Body)))
		want := trim(`
tailscale_status_peer_rx_bytes{machine_name="testhostname"} 168365416
tailscale_status_peer_rx_bytes{machine_name="testhostname2"} 0
tailscale_status_peer_tx_bytes{machine_name="testhostname"} 363769796
tailscale_status_peer_tx_bytes{machine_name="testhostname2"} 0
tailscale_status_peer{created="2122-01-14T13:30:18.170320276Z",dns_name="testhostname.tailc2865.ts.net.",exit_node="false",exit_node_option="false",ipv4="100.12.34.56",ipv6="fd7a:115c:a1e0::ac99:b03d",key_expiry="2125-01-08T02:03:11Z",machine_name="testhostname",os="macOS",relay="tok"} 1
tailscale_status_peer{created="2124-06-14T14:17:04.079089567Z",dns_name="testhostname2.tailc2865.ts.net.",exit_node="false",exit_node_option="false",ipv4="100.123.4.56",ipv6="fd7a:115c:a1e0::ac01:b66c",key_expiry="2124-12-11T14:17:04Z",machine_name="testhostname2",os="android",relay="tok"} 1
`)
		assert(t, want, b)
	})

	t.Run("http", func(t *testing.T) {
		ts := httptest.NewServer(http.FileServer(http.Dir("./testdata")))
		t.Cleanup(ts.Close)

		target := fmt.Sprintf("/probe?module=tailscale&target=%s/tailscale-status.json", ts.URL)
		result := testReq(http.MethodGet, target, nil, handleProbe(cfg))
		assert(t, 200, result.StatusCode)

		b := string(must[[]byte](t)(io.ReadAll(result.Body)))
		want := trim(`
tailscale_status_peer_rx_bytes{machine_name="testhostname"} 168365416
tailscale_status_peer_rx_bytes{machine_name="testhostname2"} 0
tailscale_status_peer_tx_bytes{machine_name="testhostname"} 363769796
tailscale_status_peer_tx_bytes{machine_name="testhostname2"} 0
tailscale_status_peer{created="2122-01-14T13:30:18.170320276Z",dns_name="testhostname.tailc2865.ts.net.",exit_node="false",exit_node_option="false",ipv4="100.12.34.56",ipv6="fd7a:115c:a1e0::ac99:b03d",key_expiry="2125-01-08T02:03:11Z",machine_name="testhostname",os="macOS",relay="tok"} 1
tailscale_status_peer{created="2124-06-14T14:17:04.079089567Z",dns_name="testhostname2.tailc2865.ts.net.",exit_node="false",exit_node_option="false",ipv4="100.123.4.56",ipv6="fd7a:115c:a1e0::ac01:b66c",key_expiry="2124-12-11T14:17:04Z",machine_name="testhostname2",os="android",relay="tok"} 1
`)
		assert(t, want, b)
	})

	t.Run("unix", func(t *testing.T) {
		testSock := filepath.Join(t.TempDir(), "test.sock")
		ts := httptest.NewUnstartedServer(http.FileServer(http.Dir("./testdata")))
		ts.Listener = must[net.Listener](t)(net.Listen("unix", testSock))
		ts.Start()
		t.Cleanup(ts.Close)

		target := fmt.Sprintf("/probe?module=tailscale&target=http://%s/tailscale-status.json", testSock)
		result := testReq(http.MethodGet, target, nil, handleProbe(cfg))
		assert(t, 200, result.StatusCode)

		b := string(must[[]byte](t)(io.ReadAll(result.Body)))
		want := trim(`
tailscale_status_peer_rx_bytes{machine_name="testhostname"} 168365416
tailscale_status_peer_rx_bytes{machine_name="testhostname2"} 0
tailscale_status_peer_tx_bytes{machine_name="testhostname"} 363769796
tailscale_status_peer_tx_bytes{machine_name="testhostname2"} 0
tailscale_status_peer{created="2122-01-14T13:30:18.170320276Z",dns_name="testhostname.tailc2865.ts.net.",exit_node="false",exit_node_option="false",ipv4="100.12.34.56",ipv6="fd7a:115c:a1e0::ac99:b03d",key_expiry="2125-01-08T02:03:11Z",machine_name="testhostname",os="macOS",relay="tok"} 1
tailscale_status_peer{created="2124-06-14T14:17:04.079089567Z",dns_name="testhostname2.tailc2865.ts.net.",exit_node="false",exit_node_option="false",ipv4="100.123.4.56",ipv6="fd7a:115c:a1e0::ac01:b66c",key_expiry="2124-12-11T14:17:04Z",machine_name="testhostname2",os="android",relay="tok"} 1
`)
		assert(t, want, b)
	})
}

func trim(s string) string {
	return strings.TrimPrefix(s, "\n")
}

func testReq(method string, target string, body io.Reader, handler http.Handler) *http.Response {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, body)
	handler.ServeHTTP(rec, req)
	return rec.Result()
}

func assert[T any](t *testing.T, want T, got T) {
	t.Helper()
	if !reflect.DeepEqual(want, got) {
		t.Error(string(diff.Diff("want", []byte(fmt.Sprint(want)), "got", []byte(fmt.Sprint(got)))))
	}
}

func must[T any](t *testing.T) func(T, error) T {
	t.Helper()
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}
