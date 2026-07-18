package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/VictoriaMetrics/metrics"
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
tailscale_status_peer{created="2122-01-14T13:30:18.170320276Z",dns_name="testhostname.tailc2865.ts.net.",exit_node="false",exit_node_option="false",ipv4="100.12.34.56",ipv6="fd7a:115c:a1e0::ac99:b03d",key_expiry="2125-01-08T02:03:11Z",machine_name="testhostname",os="macOS",relay="tok"} 1
tailscale_status_peer{created="2124-06-14T14:17:04.079089567Z",dns_name="testhostname2.tailc2865.ts.net.",exit_node="false",exit_node_option="false",ipv4="100.123.4.56",ipv6="fd7a:115c:a1e0::ac01:b66c",key_expiry="2124-12-11T14:17:04Z",machine_name="testhostname2",os="android",relay="tok"} 1
tailscale_status_peer_rx_bytes{machine_name="testhostname"} 168365416
tailscale_status_peer_rx_bytes{machine_name="testhostname2"} 0
tailscale_status_peer_tx_bytes{machine_name="testhostname"} 363769796
tailscale_status_peer_tx_bytes{machine_name="testhostname2"} 0
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
tailscale_status_peer{created="2122-01-14T13:30:18.170320276Z",dns_name="testhostname.tailc2865.ts.net.",exit_node="false",exit_node_option="false",ipv4="100.12.34.56",ipv6="fd7a:115c:a1e0::ac99:b03d",key_expiry="2125-01-08T02:03:11Z",machine_name="testhostname",os="macOS",relay="tok"} 1
tailscale_status_peer{created="2124-06-14T14:17:04.079089567Z",dns_name="testhostname2.tailc2865.ts.net.",exit_node="false",exit_node_option="false",ipv4="100.123.4.56",ipv6="fd7a:115c:a1e0::ac01:b66c",key_expiry="2124-12-11T14:17:04Z",machine_name="testhostname2",os="android",relay="tok"} 1
tailscale_status_peer_rx_bytes{machine_name="testhostname"} 168365416
tailscale_status_peer_rx_bytes{machine_name="testhostname2"} 0
tailscale_status_peer_tx_bytes{machine_name="testhostname"} 363769796
tailscale_status_peer_tx_bytes{machine_name="testhostname2"} 0
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
tailscale_status_peer{created="2122-01-14T13:30:18.170320276Z",dns_name="testhostname.tailc2865.ts.net.",exit_node="false",exit_node_option="false",ipv4="100.12.34.56",ipv6="fd7a:115c:a1e0::ac99:b03d",key_expiry="2125-01-08T02:03:11Z",machine_name="testhostname",os="macOS",relay="tok"} 1
tailscale_status_peer{created="2124-06-14T14:17:04.079089567Z",dns_name="testhostname2.tailc2865.ts.net.",exit_node="false",exit_node_option="false",ipv4="100.123.4.56",ipv6="fd7a:115c:a1e0::ac01:b66c",key_expiry="2124-12-11T14:17:04Z",machine_name="testhostname2",os="android",relay="tok"} 1
tailscale_status_peer_rx_bytes{machine_name="testhostname"} 168365416
tailscale_status_peer_rx_bytes{machine_name="testhostname2"} 0
tailscale_status_peer_tx_bytes{machine_name="testhostname"} 363769796
tailscale_status_peer_tx_bytes{machine_name="testhostname2"} 0
`)
		assert(t, want, b)
	})
}

func TestProbeMetricsAreRequestScoped(t *testing.T) {
	cfg := &Config{Modules: map[string]Module{
		"test": {
			Metrics: []Metric{
				{
					Name:      "probe_value",
					Query:     ".items",
					Labels:    map[string]Query{"id": ".id"},
					ValueType: "gauge",
					Value:     ".value",
				},
			},
		},
	}}

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/first":
			_, _ = io.WriteString(w, `{"items":[{"id":"a","value":1},{"id":"b","value":2}]}`)
		case "/second":
			_, _ = io.WriteString(w, `{"items":[{"id":"a","value":10}]}`)
		case "/third":
			_, _ = io.WriteString(w, `{"items":[{"id":"c","value":30}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(target.Close)

	probe := handleProbe(cfg)
	request := func(path string) string {
		t.Helper()
		query := "/probe?module=test&target=" + url.QueryEscape(target.URL+path)
		result := testReq(http.MethodGet, query, nil, probe)
		assert(t, http.StatusOK, result.StatusCode)
		return string(must[[]byte](t)(io.ReadAll(result.Body)))
	}

	metricNamesBefore := strings.Join(metrics.ListMetricNames(), "\n")
	first := request("/first")
	assert(t, trim(`
probe_value{id="a"} 1
probe_value{id="b"} 2
`), first)

	second := request("/second")
	assert(t, trim(`
probe_value{id="a"} 10
`), second)
	assert(t, metricNamesBefore, strings.Join(metrics.ListMetricNames(), "\n"))

	t.Run("concurrent targets", func(t *testing.T) {
		var wg sync.WaitGroup
		start := make(chan struct{})
		results := make([]string, 2)
		paths := []string{"/second", "/third"}
		for i := range paths {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results[i] = request(paths[i])
			}()
		}
		close(start)
		wg.Wait()

		assert(t, trim(`
probe_value{id="a"} 10
`), results[0])
		assert(t, trim(`
probe_value{id="c"} 30
`), results[1])
	})
}

func TestEscapeLabelValue(t *testing.T) {
	tests := map[string]string{
		"plain":                "plain",
		`quote"`:               `quote\"`,
		"line\nbreak":          `line\nbreak`,
		`backslash\`:           `backslash\\`,
		"quote\"\nbackslash\\": `quote\"\nbackslash\\`,
	}
	for value, want := range tests {
		t.Run(fmt.Sprintf("%q", value), func(t *testing.T) {
			assert(t, want, escapeLabelValue(value))
		})
	}
}

func TestProbeEscapesLabelValues(t *testing.T) {
	cfg := &Config{Modules: map[string]Module{
		"test": {
			Metrics: []Metric{
				{
					Name:      "probe_value",
					Labels:    map[string]Query{"label": ".label"},
					ValueType: "gauge",
					Value:     ".value",
				},
			},
		},
	}}

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"label":"quote\"line\nbreak\\","value":1}`)
	}))
	t.Cleanup(target.Close)

	query := "/probe?module=test&target=" + url.QueryEscape(target.URL)
	result := testReq(http.MethodGet, query, nil, handleProbe(cfg))
	assert(t, http.StatusOK, result.StatusCode)
	body := string(must[[]byte](t)(io.ReadAll(result.Body)))
	assert(t, "probe_value{label=\"quote\\\"line\\nbreak\\\\\"} 1\n", body)
}

func TestMakeMetricsRejectsInvalidNames(t *testing.T) {
	tests := map[string]Metric{
		"metric name": {
			Name:      `"invalid-name"`,
			ValueType: "gauge",
			Value:     "1",
		},
		"label name": {
			Name:      `"valid_metric"`,
			Labels:    map[string]Query{"invalid-label": `"value"`},
			ValueType: "gauge",
			Value:     "1",
		},
	}
	for name, metric := range tests {
		t.Run(name, func(t *testing.T) {
			err := makeMetrics(t.Context(), metrics.NewSet(), nil, metric)
			if err == nil {
				t.Fatal("expected invalid metric error")
			}
			if !strings.Contains(err.Error(), "invalid metric") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
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
	if reflect.DeepEqual(want, got) {
		return
	}

	wantText := fmt.Sprint(want)
	gotText := fmt.Sprint(got)
	if wantText == gotText {
		t.Errorf("values differ but their textual representations are identical:\nwant (%T): %#v\ngot  (%T): %#v", want, want, got, got)
		return
	}

	var commands [][]string
	if gitPath, err := exec.LookPath("git"); err == nil {
		commands = append(commands, []string{gitPath, "diff", "--no-index", "--no-ext-diff", "--no-textconv", "--no-color", "--", "want", "got"})
	}
	if diffPath, err := exec.LookPath("diff"); err == nil {
		commands = append(commands, []string{diffPath, "-u", "--label", "want", "--label", "got", "want", "got"})
	}
	if len(commands) == 0 {
		t.Error("values differ; git and diff are unavailable")
		return
	}

	dir := t.TempDir()
	for name, text := range map[string]string{"want": wantText, "got": gotText} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatalf("write %s for diff: %v", name, err)
		}
	}

	for _, command := range commands {
		cmd := exec.Command(command[0], command[1:]...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			_, _ = t.Output().Write(out)
			t.Fail()
			return
		}
	}
	t.Error("values differ; failed to generate diff")
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
