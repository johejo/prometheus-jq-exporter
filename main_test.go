package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VictoriaMetrics/metrics"
	"github.com/itchyny/gojq"
)

func TestHTTPResourceLimits(t *testing.T) {
	assert(t, defaultMaxResponseBodySize, *maxResponseBodySize)
	assert(t, defaultTargetTimeout, *targetTimeout)
	assert(t, defaultReadHeaderTimeout, *readHeaderTimeout)

	clientTimeout := 250 * time.Millisecond
	client := newHTTPClient(false, false, clientTimeout)
	assert(t, clientTimeout, client.Timeout)

	serverTimeout := 750 * time.Millisecond
	handler := http.NotFoundHandler()
	server := newHTTPServer("127.0.0.1:0", handler, serverTimeout)
	assert(t, "127.0.0.1:0", server.Addr)
	if server.Handler == nil {
		t.Fatal("HTTP server handler is nil")
	}
	assert(t, serverTimeout, server.ReadHeaderTimeout)
}

func TestValidateHTTPLimits(t *testing.T) {
	if err := validateHTTPLimits(defaultMaxResponseBodySize, defaultTargetTimeout, defaultReadHeaderTimeout); err != nil {
		t.Fatalf("valid defaults were rejected: %v", err)
	}
	tests := map[string]struct {
		maxResponseBodySize int64
		targetTimeout       time.Duration
		readHeaderTimeout   time.Duration
		want                string
	}{
		"zero response body size":      {0, defaultTargetTimeout, defaultReadHeaderTimeout, "max-response-body-size"},
		"negative response body size":  {-1, defaultTargetTimeout, defaultReadHeaderTimeout, "max-response-body-size"},
		"zero target timeout":          {defaultMaxResponseBodySize, 0, defaultReadHeaderTimeout, "target-timeout"},
		"negative target timeout":      {defaultMaxResponseBodySize, -1, defaultReadHeaderTimeout, "target-timeout"},
		"zero read header timeout":     {defaultMaxResponseBodySize, defaultTargetTimeout, 0, "read-header-timeout"},
		"negative read header timeout": {defaultMaxResponseBodySize, defaultTargetTimeout, -1, "read-header-timeout"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			err := validateHTTPLimits(test.maxResponseBodySize, test.targetTimeout, test.readHeaderTimeout)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %s error, got %v", test.want, err)
			}
		})
	}
}

func TestReadResponseBodyLimit(t *testing.T) {
	for name, test := range map[string]struct {
		body    string
		maxSize int64
		want    string
		wantErr string
	}{
		"below limit":   {body: "ab", maxSize: 3, want: "ab"},
		"at limit":      {body: "abc", maxSize: 3, want: "abc"},
		"over limit":    {body: "abcd", maxSize: 3, wantErr: "exceeds 3 bytes"},
		"invalid limit": {body: "", maxSize: 0, wantErr: "must be positive"},
	} {
		t.Run(name, func(t *testing.T) {
			body, err := readResponseBody(strings.NewReader(test.body), test.maxSize)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("expected %q error, got %v", test.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			assert(t, test.want, string(body))
		})
	}
}

func TestProbeResponseBodyLimit(t *testing.T) {
	const response = `{"value":1}`
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(target.Close)

	cfg := mustCompileConfig(t, &Config{Modules: map[string]Module{"test": {}}})
	query := "/probe?module=test&debug=true&target=" + url.QueryEscape(target.URL)
	result := testReq(http.MethodGet, query, nil, handleProbe(cfg, http.DefaultClient, int64(len(response)-1)))
	assert(t, http.StatusOK, result.StatusCode)
	body := string(must[[]byte](t)(io.ReadAll(result.Body)))
	if !strings.Contains(body, "target response body exceeds 10 bytes") {
		t.Fatalf("debug response does not contain response size error: %q", body)
	}
	if !strings.HasSuffix(body, probeStatus(0, 1, 0, 0, 0, 0)) {
		t.Fatalf("response size error was not recorded as a fetch error: %q", body)
	}
}

func TestProbeTargetTimeout(t *testing.T) {
	client := newHTTPClient(false, false, 10*time.Millisecond)
	client.Transport = RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		<-r.Context().Done()
		return nil, r.Context().Err()
	})

	cfg := mustCompileConfig(t, &Config{Modules: map[string]Module{"test": {}}})
	result := testReq(http.MethodGet, "/probe?module=test&target=http://example.test", nil, handleProbe(cfg, client, defaultMaxResponseBodySize))
	assert(t, http.StatusOK, result.StatusCode)
	body := string(must[[]byte](t)(io.ReadAll(result.Body)))
	assert(t, probeStatus(0, 1, 0, 0, 0, 0), body)

	req := must[*http.Request](t)(http.NewRequest(http.MethodGet, "http://example.test", nil))
	_, err := client.Do(req)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
}

func Test(t *testing.T) {
	httpClient := newHTTPClient(true, true, defaultTargetTimeout)

	cfg, err := loadConfig("./testdata/config.yaml", false)
	if err != nil {
		t.Fatal(err)
	}
	want := trim(`
probe_body_errors 0
probe_fetch_errors 0
probe_metrics_failed 0
probe_metrics_successful 6
probe_success 1
probe_timestamp_errors 0
tailscale_status_peer{created="2122-01-14T13:30:18.170320276Z",dns_name="testhostname.tailc2865.ts.net.",exit_node="false",exit_node_option="false",ipv4="100.12.34.56",ipv6="fd7a:115c:a1e0::ac99:b03d",key_expiry="2125-01-08T02:03:11Z",machine_name="testhostname",os="macOS",relay="tok"} 1
tailscale_status_peer{created="2124-06-14T14:17:04.079089567Z",dns_name="testhostname2.tailc2865.ts.net.",exit_node="false",exit_node_option="false",ipv4="100.123.4.56",ipv6="fd7a:115c:a1e0::ac01:b66c",key_expiry="2124-12-11T14:17:04Z",machine_name="testhostname2",os="android",relay="tok"} 1
tailscale_status_peer_rx_bytes{machine_name="testhostname"} 168365416
tailscale_status_peer_rx_bytes{machine_name="testhostname2"} 0
tailscale_status_peer_tx_bytes{machine_name="testhostname"} 363769796
tailscale_status_peer_tx_bytes{machine_name="testhostname2"} 0
`)
	t.Run("file", func(t *testing.T) {
		result := testReq(http.MethodGet, "/probe?module=tailscale&target=file://testdata/tailscale-status.json", nil, handleProbe(cfg, httpClient, defaultMaxResponseBodySize))
		assert(t, 200, result.StatusCode)

		b := string(must[[]byte](t)(io.ReadAll(result.Body)))
		assert(t, want, b)
	})

	t.Run("http", func(t *testing.T) {
		ts := httptest.NewServer(http.FileServer(http.Dir("./testdata")))
		t.Cleanup(ts.Close)

		target := fmt.Sprintf("/probe?module=tailscale&target=%s/tailscale-status.json", ts.URL)
		result := testReq(http.MethodGet, target, nil, handleProbe(cfg, httpClient, defaultMaxResponseBodySize))
		assert(t, 200, result.StatusCode)

		b := string(must[[]byte](t)(io.ReadAll(result.Body)))
		assert(t, want, b)
	})

	t.Run("unix", func(t *testing.T) {
		testSock := filepath.Join(t.TempDir(), "test.sock")
		ts := httptest.NewUnstartedServer(http.FileServer(http.Dir("./testdata")))
		ts.Listener = must[net.Listener](t)(net.Listen("unix", testSock))
		ts.Start()
		t.Cleanup(ts.Close)

		target := fmt.Sprintf("/probe?module=tailscale&target=unix://%s/tailscale-status.json", testSock)
		result := testReq(http.MethodGet, target, nil, handleProbe(cfg, httpClient, defaultMaxResponseBodySize))
		assert(t, 200, result.StatusCode)

		b := string(must[[]byte](t)(io.ReadAll(result.Body)))
		assert(t, want, b)
	})
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

	probe := handleProbe(mustCompileConfig(t, cfg), http.DefaultClient, defaultMaxResponseBodySize)
	type requestResult struct {
		statusCode int
		body       string
		err        error
	}
	request := func(path string) requestResult {
		query := "/probe?module=test&target=" + url.QueryEscape(target.URL+path)
		result := testReq(http.MethodGet, query, nil, probe)
		body, err := io.ReadAll(result.Body)
		return requestResult{statusCode: result.StatusCode, body: string(body), err: err}
	}
	checkRequest := func(path string, result requestResult) string {
		t.Helper()
		if result.err != nil {
			t.Fatalf("read response from %s: %v", path, result.err)
		}
		assert(t, http.StatusOK, result.statusCode)
		return result.body
	}

	metricNamesBefore := strings.Join(metrics.ListMetricNames(), "\n")
	first := checkRequest("/first", request("/first"))
	assert(t, trim(`
probe_body_errors 0
probe_fetch_errors 0
probe_metrics_failed 0
probe_metrics_successful 2
probe_success 1
probe_timestamp_errors 0
probe_value{id="a"} 1
probe_value{id="b"} 2
`), first)

	second := checkRequest("/second", request("/second"))
	assert(t, trim(`
probe_body_errors 0
probe_fetch_errors 0
probe_metrics_failed 0
probe_metrics_successful 1
probe_success 1
probe_timestamp_errors 0
probe_value{id="a"} 10
`), second)
	assert(t, metricNamesBefore, strings.Join(metrics.ListMetricNames(), "\n"))

	t.Run("concurrent targets", func(t *testing.T) {
		var wg sync.WaitGroup
		start := make(chan struct{})
		results := make([]requestResult, 2)
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
probe_body_errors 0
probe_fetch_errors 0
probe_metrics_failed 0
probe_metrics_successful 1
probe_success 1
probe_timestamp_errors 0
probe_value{id="a"} 10
`), checkRequest(paths[0], results[0]))
		assert(t, trim(`
probe_body_errors 0
probe_fetch_errors 0
probe_metrics_failed 0
probe_metrics_successful 1
probe_success 1
probe_timestamp_errors 0
probe_value{id="c"} 30
`), checkRequest(paths[1], results[1]))
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
	result := testReq(http.MethodGet, query, nil, handleProbe(mustCompileConfig(t, cfg), http.DefaultClient, defaultMaxResponseBodySize))
	assert(t, http.StatusOK, result.StatusCode)
	body := string(must[[]byte](t)(io.ReadAll(result.Body)))
	assert(t, trim(`
probe_body_errors 0
probe_fetch_errors 0
probe_metrics_failed 0
probe_metrics_successful 1
probe_success 1
probe_timestamp_errors 0
probe_value{label="quote\"line\nbreak\\"} 1
`), body)
}

func TestProbeErrorStatus(t *testing.T) {
	invalidJSONTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "not JSON")
	}))
	t.Cleanup(invalidJSONTarget.Close)

	tests := map[string]struct {
		cfg      *Config
		target   string
		want     int
		wantBody string
	}{
		"missing module": {
			cfg:    &Config{},
			target: "/probe?target=" + url.QueryEscape(invalidJSONTarget.URL),
			want:   http.StatusBadRequest,
		},
		"unknown module": {
			cfg:    &Config{},
			target: "/probe?module=unknown&target=" + url.QueryEscape(invalidJSONTarget.URL),
			want:   http.StatusBadRequest,
		},
		"missing target": {
			cfg:    &Config{Modules: map[string]Module{"test": {}}},
			target: "/probe?module=test",
			want:   http.StatusBadRequest,
		},
		"invalid target": {
			cfg:      &Config{Modules: map[string]Module{"test": {}}},
			target:   "/probe?module=test&target=%3A%2F%2F",
			want:     http.StatusOK,
			wantBody: probeStatus(0, 1, 0, 0, 0, 0),
		},
		"invalid JSON response": {
			cfg:      &Config{Modules: map[string]Module{"test": {}}},
			target:   "/probe?module=test&target=" + url.QueryEscape(invalidJSONTarget.URL),
			want:     http.StatusOK,
			wantBody: probeStatus(0, 1, 0, 0, 0, 0),
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			result := testReq(http.MethodGet, test.target, nil, handleProbe(mustCompileConfig(t, test.cfg), http.DefaultClient, defaultMaxResponseBodySize))
			assert(t, test.want, result.StatusCode)
			if test.wantBody != "" {
				body := string(must[[]byte](t)(io.ReadAll(result.Body)))
				assert(t, test.wantBody, body)
			}
		})
	}
}

func TestProbeBody(t *testing.T) {
	tests := map[string]struct {
		body            Body
		headers         map[string]string
		wantBody        string
		wantContentType string
	}{
		"none": {},
		"json": {
			body: Body{JSON: queryPointer(`{
  name: .name[0],
  tags: .tag,
  values: [1, true, null]
}`)},
			wantBody:        `{"name":"quote\"line\nbreak","tags":["a","b"],"values":[1,true,null]}`,
			wantContentType: "application/json",
		},
		"json excludes exporter parameters": {
			body:            Body{JSON: queryPointer(`.`)},
			wantBody:        `{"name":["quote\"line\nbreak"],"tag":["a","b"]}`,
			wantContentType: "application/json",
		},
		"text": {
			body:            Body{Text: queryPointer(`"name=\(.name[0]);tags=\(.tag | join(","))"`)},
			wantBody:        "name=quote\"line\nbreak;tags=a,b",
			wantContentType: "text/plain; charset=utf-8",
		},
		"text excludes exporter parameters": {
			body:            Body{Text: queryPointer(`"keys=\(keys | join(","))"`)},
			wantBody:        "keys=name,tag",
			wantContentType: "text/plain; charset=utf-8",
		},
		"empty text": {
			body:            Body{Text: queryPointer(`""`)},
			wantContentType: "text/plain; charset=utf-8",
		},
		"content type override": {
			body:            Body{Text: queryPointer(`"name=\(.name[0])"`)},
			headers:         map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
			wantBody:        "name=quote\"line\nbreak",
			wantContentType: "application/x-www-form-urlencoded",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			var gotBody, gotContentType string
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBody = string(must[[]byte](t)(io.ReadAll(r.Body)))
				gotContentType = r.Header.Get("Content-Type")
				_, _ = io.WriteString(w, `{}`)
			}))
			t.Cleanup(target.Close)

			cfg := mustCompileConfig(t, &Config{Modules: map[string]Module{
				"test": {Body: test.body, Headers: test.headers},
			}})
			params := url.Values{
				"module": {"test"},
				"target": {target.URL},
				"method": {http.MethodPost},
				"debug":  {"true"},
				"name":   {"quote\"line\nbreak"},
				"tag":    {"a", "b"},
			}
			result := testReq(http.MethodGet, "/probe?"+params.Encode(), nil, handleProbe(cfg, http.DefaultClient, defaultMaxResponseBodySize))
			assert(t, http.StatusOK, result.StatusCode)
			assert(t, test.wantBody, gotBody)
			assert(t, test.wantContentType, gotContentType)
		})
	}
}

func TestProbeRejectsInvalidBodyResults(t *testing.T) {
	tests := map[string]Body{
		"evaluation error": {JSON: queryPointer(`error("failed")`)},
		"no values":        {JSON: queryPointer(`empty`)},
		"multiple values":  {JSON: queryPointer(`1, 2`)},
		"non-string text":  {Text: queryPointer(`1`)},
	}

	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			targetCalled := false
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				targetCalled = true
				_, _ = io.WriteString(w, `{}`)
			}))
			t.Cleanup(target.Close)

			cfg := mustCompileConfig(t, &Config{Modules: map[string]Module{"test": {Body: body}}})
			query := "/probe?module=test&target=" + url.QueryEscape(target.URL)
			result := testReq(http.MethodGet, query, nil, handleProbe(cfg, http.DefaultClient, defaultMaxResponseBodySize))
			assert(t, http.StatusOK, result.StatusCode)
			probeBody := string(must[[]byte](t)(io.ReadAll(result.Body)))
			assert(t, probeStatus(1, 0, 0, 0, 0, 0), probeBody)
			if targetCalled {
				t.Fatal("target was called after body evaluation failed")
			}
		})
	}
}

func TestProbeMetricGenerationPartialFailure(t *testing.T) {
	tests := map[string]struct {
		body       string
		metrics    []Metric
		wantStatus int
		wantBody   string
	}{
		"metric query failure": {
			body: `{}`,
			metrics: []Metric{
				{Name: "failed_metric", Query: `error("failed")`, ValueType: valueTypeGauge, Value: "1"},
				{Name: "successful_metric", ValueType: valueTypeGauge, Value: "2"},
			},
			wantStatus: http.StatusOK,
			wantBody:   probeStatus(0, 0, 1, 1, 0, 0) + "successful_metric{} 2\n",
		},
		"value failure": {
			body: `{"items":[{"id":"a","value":1},{"id":"b","value":"invalid"},{"id":"c","value":3}]}`,
			metrics: []Metric{{
				Name:      "item_value",
				Query:     ".items",
				Labels:    map[string]Query{"id": ".id"},
				ValueType: valueTypeGauge,
				Value:     ".value",
			}},
			wantStatus: http.StatusOK,
			wantBody: trim(`
item_value{id="a"} 1
item_value{id="c"} 3
probe_body_errors 0
probe_fetch_errors 0
probe_metrics_failed 1
probe_metrics_successful 2
probe_success 0
probe_timestamp_errors 0
`),
		},
		"all values fail": {
			body: `{"items":[{"value":"invalid"}]}`,
			metrics: []Metric{{
				Name:      "item_value",
				Query:     ".items",
				ValueType: valueTypeGauge,
				Value:     ".value",
			}},
			wantStatus: http.StatusOK,
			wantBody:   probeStatus(0, 0, 1, 0, 0, 0),
		},
		"negative counter": {
			body: `{"value":-1}`,
			metrics: []Metric{{
				Name:      "item_count",
				ValueType: valueTypeCounter,
				Value:     ".value",
			}},
			wantStatus: http.StatusOK,
			wantBody:   probeStatus(0, 0, 1, 0, 0, 0),
		},
		"large counter": {
			body: `{"value":10000000}`,
			metrics: []Metric{{
				Name:      "item_count",
				ValueType: valueTypeCounter,
				Value:     ".value",
			}},
			wantStatus: http.StatusOK,
			wantBody:   "item_count{} 10000000\n" + probeStatus(0, 0, 0, 1, 1, 0),
		},
		"null metric query": {
			body: `{}`,
			metrics: []Metric{{
				Name:      "null_input",
				Query:     ".missing",
				ValueType: valueTypeGauge,
				Value:     "1",
			}},
			wantStatus: http.StatusOK,
			wantBody:   "null_input{} 1\n" + probeStatus(0, 0, 0, 1, 1, 0),
		},
		"empty values": {
			body: `{"items":[]}`,
			metrics: []Metric{{
				Name:      "item_value",
				Query:     ".items",
				ValueType: valueTypeGauge,
				Value:     ".value",
			}},
			wantStatus: http.StatusOK,
			wantBody:   probeStatus(0, 0, 0, 0, 1, 0),
		},
		"iterator query": {
			body: `{"items":[{"id":"a","value":1},{"id":"b","value":2}]}`,
			metrics: []Metric{{
				Name:      "item_value",
				Query:     ".items[]",
				Labels:    map[string]Query{"id": ".id"},
				ValueType: valueTypeGauge,
				Value:     ".value",
			}},
			wantStatus: http.StatusOK,
			wantBody:   "item_value{id=\"a\"} 1\nitem_value{id=\"b\"} 2\n" + probeStatus(0, 0, 0, 2, 1, 0),
		},
		"multiple array outputs": {
			body: `{"a":[{"id":"a","value":1}],"b":[{"id":"b","value":2}]}`,
			metrics: []Metric{{
				Name:      "item_value",
				Query:     ".a, .b",
				Labels:    map[string]Query{"id": ".id"},
				ValueType: valueTypeGauge,
				Value:     ".value",
			}},
			wantStatus: http.StatusOK,
			wantBody:   "item_value{id=\"a\"} 1\nitem_value{id=\"b\"} 2\n" + probeStatus(0, 0, 0, 2, 1, 0),
		},
		"empty query output": {
			body: `{}`,
			metrics: []Metric{{
				Name:      "item_value",
				Query:     "empty",
				ValueType: valueTypeGauge,
				Value:     ".value",
			}},
			wantStatus: http.StatusOK,
			wantBody:   probeStatus(0, 0, 0, 0, 1, 0),
		},
		"value multiple outputs": {
			body: `{}`,
			metrics: []Metric{{
				Name:      "item_value",
				ValueType: valueTypeGauge,
				Value:     "1, 2",
			}},
			wantStatus: http.StatusOK,
			wantBody:   probeStatus(0, 0, 1, 0, 0, 0),
		},
		"value no output": {
			body: `{}`,
			metrics: []Metric{{
				Name:      "item_value",
				ValueType: valueTypeGauge,
				Value:     "empty",
			}},
			wantStatus: http.StatusOK,
			wantBody:   probeStatus(0, 0, 1, 0, 0, 0),
		},
		"label multiple outputs": {
			body: `{}`,
			metrics: []Metric{{
				Name:      "item_value",
				Labels:    map[string]Query{"id": "1, 2"},
				ValueType: valueTypeGauge,
				Value:     "1",
			}},
			wantStatus: http.StatusOK,
			wantBody:   probeStatus(0, 0, 1, 0, 0, 0),
		},
		"timestamp multiple outputs": {
			body: `{"value":1,"ts":1712345678901}`,
			metrics: []Metric{{
				Name:           "ts_value",
				ValueType:      valueTypeGauge,
				Value:          ".value",
				EpochTimestamp: ".ts, .ts",
			}},
			wantStatus: http.StatusOK,
			wantBody:   probeStatus(0, 0, 0, 1, 0, 1) + "ts_value{} 1\n",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, test.body)
			}))
			t.Cleanup(target.Close)

			cfg := &Config{Modules: map[string]Module{
				"test": {Metrics: test.metrics},
			}}
			query := "/probe?module=test&target=" + url.QueryEscape(target.URL)
			result := testReq(http.MethodGet, query, nil, handleProbe(mustCompileConfig(t, cfg), http.DefaultClient, defaultMaxResponseBodySize))
			assert(t, test.wantStatus, result.StatusCode)
			body := string(must[[]byte](t)(io.ReadAll(result.Body)))
			assert(t, test.wantBody, body)
		})
	}
}

func TestProbeRejectsDynamicReservedMetricName(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"name":"probe_success","value":1}`)
	}))
	t.Cleanup(target.Close)

	cfg := &Config{Modules: map[string]Module{
		"test": {Metrics: []Metric{{Name: ".name", ValueType: valueTypeGauge, Value: ".value"}}},
	}}
	query := "/probe?module=test&target=" + url.QueryEscape(target.URL)
	result := testReq(http.MethodGet, query, nil, handleProbe(mustCompileConfig(t, cfg), http.DefaultClient, defaultMaxResponseBodySize))
	assert(t, http.StatusOK, result.StatusCode)
	body := string(must[[]byte](t)(io.ReadAll(result.Body)))
	assert(t, probeStatus(0, 0, 1, 0, 0, 0), body)
}

func TestProbeDebugIncludesError(t *testing.T) {
	cfg := &Config{Modules: map[string]Module{"test": {}}}
	result := testReq(http.MethodGet, "/probe?module=test&debug=true&target=%3A%2F%2F", nil, handleProbe(mustCompileConfig(t, cfg), http.DefaultClient, defaultMaxResponseBodySize))
	assert(t, http.StatusOK, result.StatusCode)
	body := string(must[[]byte](t)(io.ReadAll(result.Body)))
	if !strings.Contains(body, `# probe_error "parse \"://\": missing protocol scheme"`+"\n") {
		t.Fatalf("debug response does not contain fetch error: %q", body)
	}
	if !strings.HasSuffix(body, probeStatus(0, 1, 0, 0, 0, 0)) {
		t.Fatalf("debug response does not contain probe metrics: %q", body)
	}
}

func TestAsCounterValueRejectsNegativeInt(t *testing.T) {
	if _, err := asCounterValue(-1); err == nil {
		t.Fatal("expected negative counter value error")
	}
}

func TestAsCounterValueFloat64(t *testing.T) {
	value, err := asCounterValue(float64(10000000))
	if err != nil {
		t.Fatal(err)
	}
	assert(t, uint64(10000000), value)

	for name, value := range map[string]float64{
		"negative":          -1,
		"fractional":        1.5,
		"not a number":      math.NaN(),
		"positive infinity": math.Inf(1),
		"out of range":      float64(math.MaxUint64),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := asCounterValue(value); err == nil {
				t.Fatalf("expected error for %v", value)
			}
		})
	}
}

func TestProductionFlagsExpandEnvAndDefaultMetadata(t *testing.T) {
	if !*exposeMetadata {
		t.Fatal("production expose-metadata default must be true")
	}
	t.Setenv("PROMETHEUS_JQ_EXPORTER_METRIC_NAME", "expanded_counter")
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	configYAML := `modules:
  test:
    metrics:
      - name: ${PROMETHEUS_JQ_EXPORTER_METRIC_NAME}
        valueType: counter
        value: .value
`
	if err := os.WriteFile(configPath, []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	originalConfig, originalExpandEnv := *config, *expandEnv
	t.Cleanup(func() {
		_ = flag.Set("config", originalConfig)
		_ = flag.Set("expand-env", strconv.FormatBool(originalExpandEnv))
	})
	if err := flag.Set("config", configPath); err != nil {
		t.Fatal(err)
	}
	if err := flag.Set("expand-env", "true"); err != nil {
		t.Fatal(err)
	}

	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"value":3}`)
	}))
	t.Cleanup(target.Close)
	t.Cleanup(func() { metrics.ExposeMetadata(false) })

	handler := must[http.Handler](t)(newHandler(*config, *expandEnv, *exposeMetadata, http.DefaultClient, defaultMaxResponseBodySize))
	query := "/probe?module=test&target=" + url.QueryEscape(target.URL)
	result := testReq(http.MethodGet, query, nil, handler)
	assert(t, http.StatusOK, result.StatusCode)
	body := string(must[[]byte](t)(io.ReadAll(result.Body)))
	assert(t, trim(`
# HELP expanded_counter
# TYPE expanded_counter counter
expanded_counter{} 3
# HELP probe_body_errors
# TYPE probe_body_errors gauge
probe_body_errors 0
# HELP probe_fetch_errors
# TYPE probe_fetch_errors gauge
probe_fetch_errors 0
# HELP probe_metrics_failed
# TYPE probe_metrics_failed gauge
probe_metrics_failed 0
# HELP probe_metrics_successful
# TYPE probe_metrics_successful gauge
probe_metrics_successful 1
# HELP probe_success
# TYPE probe_success gauge
probe_success 1
# HELP probe_timestamp_errors
# TYPE probe_timestamp_errors gauge
probe_timestamp_errors 0
`), body)
}

func TestProbeValidStatusCodes(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := must[int](t)(strconv.Atoi(r.URL.Query().Get("status")))
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(target.Close)

	tests := map[string]struct {
		status           int
		validStatusCodes []int
		want             int
		wantSuccess      int
	}{
		"default accepts 2xx": {
			status:      299,
			want:        http.StatusOK,
			wantSuccess: 1,
		},
		"empty list accepts 2xx": {
			status:           http.StatusOK,
			validStatusCodes: []int{},
			want:             http.StatusOK,
			wantSuccess:      1,
		},
		"default rejects non-2xx": {
			status: http.StatusNotFound,
			want:   http.StatusOK,
		},
		"configured accepts non-2xx": {
			status:           http.StatusNotFound,
			validStatusCodes: []int{http.StatusNotFound},
			want:             http.StatusOK,
			wantSuccess:      1,
		},
		"configured rejects unlisted 2xx": {
			status:           http.StatusOK,
			validStatusCodes: []int{http.StatusCreated},
			want:             http.StatusOK,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := &Config{Modules: map[string]Module{
				"test": {ValidStatusCodes: test.validStatusCodes},
			}}
			probeTarget := fmt.Sprintf("%s?status=%d", target.URL, test.status)
			query := "/probe?module=test&target=" + url.QueryEscape(probeTarget)
			result := testReq(http.MethodGet, query, nil, handleProbe(mustCompileConfig(t, cfg), http.DefaultClient, defaultMaxResponseBodySize))
			assert(t, test.want, result.StatusCode)
			body := string(must[[]byte](t)(io.ReadAll(result.Body)))
			fetchErrors := 0
			if test.wantSuccess == 0 {
				fetchErrors = 1
			}
			assert(t, probeStatus(0, fetchErrors, 0, 0, test.wantSuccess, 0), body)
		})
	}
}

func TestLoadConfigValidStatusCodes(t *testing.T) {
	tests := map[string]string{
		"yaml": "modules:\n  test:\n    valid_status_codes: [200, 404]\n",
		"json": `{"modules":{"test":{"valid_status_codes":[200,404]}}}`,
	}

	for extension, content := range tests {
		t.Run(extension, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config."+extension)
			if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := must[*Config](t)(loadConfig(configPath, false))
			assert(t, []int{http.StatusOK, http.StatusNotFound}, cfg.Modules["test"].ValidStatusCodes)
		})
	}
}

func TestLoadConfigDefaultsValueTypeToUntyped(t *testing.T) {
	tests := map[string]string{
		"yaml": "modules:\n  test:\n    metrics:\n      - name: test_metric\n        value: 1\n",
		"json": `{"modules":{"test":{"metrics":[{"name":"test_metric","value":"1"}]}}}`,
	}

	for extension, content := range tests {
		t.Run(extension, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config."+extension)
			if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := must[*Config](t)(loadConfig(configPath, false))
			assert(t, valueTypeUntyped, cfg.Modules["test"].Metrics[0].ValueType)
		})
	}
}

func TestLoadConfigEpochTimestamp(t *testing.T) {
	tests := map[string]string{
		"yaml": "modules:\n  test:\n    metrics:\n      - name: test_metric\n        value: 1\n        epochTimestamp: .timestamp\n",
		"json": `{"modules":{"test":{"metrics":[{"name":"test_metric","value":"1","epochTimestamp":".timestamp"}]}}}`,
	}

	for extension, content := range tests {
		t.Run(extension, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config."+extension)
			if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg := must[*Config](t)(loadConfig(configPath, false))
			assert(t, Query(".timestamp"), cfg.Modules["test"].Metrics[0].EpochTimestamp)
		})
	}
}

func TestLoadConfigBody(t *testing.T) {
	tests := map[string]string{
		"yaml": "modules:\n  test:\n    body:\n      json: '{value: .value[0]}'\n",
		"json": `{"modules":{"test":{"body":{"text":"\"value=\\(.value[0])\""}}}}`,
	}

	for extension, content := range tests {
		t.Run(extension, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config."+extension)
			if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			body := must[*Config](t)(loadConfig(configPath, false)).Modules["test"].Body
			if body.query == nil {
				t.Fatal("body query was not compiled")
			}
			wantFormat := bodyFormatJSON
			if extension == "json" {
				wantFormat = bodyFormatText
			}
			assert(t, wantFormat, body.format)
		})
	}
}

func TestCompileConfigCompilesAndReusesQueries(t *testing.T) {
	cfg := &Config{Modules: map[string]Module{
		"test": {
			Body: Body{JSON: queryPointer(".value")},
			Metrics: []Metric{{
				Query:          ".value",
				Name:           ".value",
				Labels:         map[string]Query{"value": ".value"},
				ValueType:      valueTypeGauge,
				Value:          ".value",
				EpochTimestamp: ".value",
			}},
		},
	}}

	compiled := mustCompileConfig(t, cfg).Modules["test"].Metrics[0]
	wantCode := compiled.query.code
	for field, gotCode := range map[string]*gojq.Code{
		"body":           cfg.Modules["test"].Body.query.code,
		"name":           compiled.name.code,
		"label":          compiled.labels["value"].code,
		"value":          compiled.value.code,
		"epochTimestamp": compiled.epochTimestamp.code,
	} {
		if gotCode != wantCode {
			t.Errorf("%s did not reuse the compiled query", field)
		}
	}
}

func TestCompileConfigRejectsInvalidBodies(t *testing.T) {
	tests := map[string]struct {
		body Body
		part string
	}{
		"json and text": {
			body: Body{JSON: queryPointer("."), Text: queryPointer(".")},
			part: "mutually exclusive",
		},
		"empty json": {
			body: Body{JSON: queryPointer("")},
			part: "json query is empty",
		},
		"empty text": {
			body: Body{Text: queryPointer("")},
			part: "text query is empty",
		},
		"json parse error": {
			body: Body{JSON: queryPointer("(")},
			part: "json",
		},
		"text compile error": {
			body: Body{Text: queryPointer("unknown_function")},
			part: "text",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			err := compileConfig(&Config{Modules: map[string]Module{"test": {Body: test.body}}})
			if err == nil {
				t.Fatal("expected body compilation error")
			}
			for _, part := range []string{`module "test"`, "body", test.part} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error %q does not contain %q", err, part)
				}
			}
		})
	}
}

func TestCompileConfigRejectsInvalidQueries(t *testing.T) {
	tests := map[string]struct {
		metric Metric
		field  string
	}{
		"query parse error": {
			metric: Metric{Query: "(", Name: "metric", Value: "1"},
			field:  "query",
		},
		"query compile error": {
			metric: Metric{Query: "unknown_function", Name: "metric", Value: "1"},
			field:  "query",
		},
		"name parse error": {
			metric: Metric{Name: "(", Value: "1"},
			field:  "name",
		},
		"label parse error": {
			metric: Metric{Name: "metric", Labels: map[string]Query{"label": "("}, Value: "1"},
			field:  `label "label"`,
		},
		"value compile error": {
			metric: Metric{Name: "metric", Value: "unknown_function"},
			field:  "value",
		},
		"epoch timestamp parse error": {
			metric: Metric{Name: "metric", Value: "1", EpochTimestamp: "("},
			field:  "epochTimestamp",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			err := compileConfig(&Config{Modules: map[string]Module{
				"test": {Metrics: []Metric{test.metric}},
			}})
			if err == nil {
				t.Fatal("expected query compilation error")
			}
			for _, part := range []string{`module "test"`, "metric 0", test.field} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error %q does not contain %q", err, part)
				}
			}
		})
	}
}

func TestCompileConfigRejectsReservedMetricName(t *testing.T) {
	for metricName := range reservedProbeMetrics {
		t.Run(metricName, func(t *testing.T) {
			err := compileConfig(&Config{Modules: map[string]Module{
				"test": {Metrics: []Metric{{Name: metricName, Value: "1"}}},
			}})
			if err == nil {
				t.Fatal("expected reserved metric name error")
			}
			for _, part := range []string{`module "test"`, "metric 0", fmt.Sprintf(`metric family %q is reserved`, metricName)} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("error %q does not contain %q", err, part)
				}
			}
		})
	}
}

func TestCompileConfigPreservesLiteralFallback(t *testing.T) {
	metric := mustCompileConfig(t, &Config{Modules: map[string]Module{
		"test": {Metrics: []Metric{{
			Name:      "probe_value",
			Labels:    map[string]Query{"kind": "static"},
			ValueType: valueTypeGauge,
			Value:     "1",
		}}},
	}}).Modules["test"].Metrics[0]

	if metric.name.code != nil || metric.labels["kind"].code != nil {
		t.Fatal("literal name and label should not have executable jq code")
	}
	metricSet := newProbeMetricSet()
	if err := makeMetrics(t.Context(), metricSet, nil, metric); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	metricSet.WritePrometheus(&output)
	assert(t, "probe_value{kind=\"static\"} 1\n", output.String())
}

func TestProbeMetricFloatValueFormatting(t *testing.T) {
	tests := map[string]struct {
		value     float64
		valueType string
		want      string
	}{
		"gauge at two to the power of 63": {
			value:     math.Exp2(63),
			valueType: valueTypeGauge,
			want:      "probe_value{} 9.223372036854776e+18\n",
		},
		"untyped at two to the power of 63": {
			value:     math.Exp2(63),
			valueType: valueTypeUntyped,
			want:      "probe_value{} 9.223372036854776e+18\n",
		},
		"largest float below two to the power of 63": {
			value:     math.Nextafter(math.Exp2(63), 0),
			valueType: valueTypeGauge,
			want:      "probe_value{} 9223372036854774784\n",
		},
		"minimum int64": {
			value:     math.MinInt64,
			valueType: valueTypeGauge,
			want:      "probe_value{} -9223372036854775808\n",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			metricSet := newProbeMetricSet()
			metricSet.metrics["probe_value{}"] = probeMetric{
				family:     "probe_value",
				name:       "probe_value{}",
				valueType:  test.valueType,
				floatValue: test.value,
			}

			metrics.ExposeMetadata(false)
			var output strings.Builder
			metricSet.WritePrometheus(&output)
			assert(t, test.want, output.String())
		})
	}
}

func TestCompiledNameAndLabelDoNotFallbackAfterEvaluationErrors(t *testing.T) {
	tests := map[string]Metric{
		"name": {
			Name:      `error("failed name")`,
			ValueType: valueTypeGauge,
			Value:     "1",
		},
		"label": {
			Name:      "probe_value",
			Labels:    map[string]Query{"kind": `error("failed label")`},
			ValueType: valueTypeGauge,
			Value:     "1",
		},
	}

	for name, metric := range tests {
		t.Run(name, func(t *testing.T) {
			err := makeMetrics(t.Context(), newProbeMetricSet(), nil, mustCompileMetric(t, metric))
			if err == nil {
				t.Fatal("expected jq evaluation error")
			}
			if !strings.Contains(err.Error(), "failed "+name) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestProbeEpochTimestampPerValue(t *testing.T) {
	cfg := &Config{Modules: map[string]Module{
		"test": {
			Metrics: []Metric{
				{
					Name:           "probe_value",
					Query:          ".items",
					Labels:         map[string]Query{"id": ".id"},
					ValueType:      valueTypeGauge,
					Value:          ".value",
					EpochTimestamp: ".timestamp",
				},
			},
		},
	}}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"items":[{"id":"a","value":1,"timestamp":1712345678901},{"id":"b","value":2,"timestamp":"1712345678902"},{"id":"c","value":3}]}`)
	}))
	t.Cleanup(target.Close)

	query := "/probe?module=test&target=" + url.QueryEscape(target.URL)
	result := testReq(http.MethodGet, query, nil, handleProbe(mustCompileConfig(t, cfg), http.DefaultClient, defaultMaxResponseBodySize))
	assert(t, http.StatusOK, result.StatusCode)
	body := string(must[[]byte](t)(io.ReadAll(result.Body)))
	assert(t, trim(`
probe_body_errors 0
probe_fetch_errors 0
probe_metrics_failed 0
probe_metrics_successful 3
probe_success 1
probe_timestamp_errors 0
probe_value{id="a"} 1 1712345678901
probe_value{id="b"} 2 1712345678902
probe_value{id="c"} 3
`), body)
}

func TestMakeMetricsEpochTimestampValueTypes(t *testing.T) {
	metricSet := newProbeMetricSet()
	for _, metric := range []Metric{
		{Name: "probe_counter", ValueType: valueTypeCounter, Value: "3", EpochTimestamp: ".timestamp"},
		{Name: "probe_gauge", ValueType: valueTypeGauge, Value: "1.5", EpochTimestamp: ".timestamp"},
		{Name: "probe_untyped", ValueType: valueTypeUntyped, Value: "-2", EpochTimestamp: ".timestamp"},
	} {
		if err := makeMetrics(t.Context(), metricSet, map[string]any{"timestamp": float64(1712345678901)}, mustCompileMetric(t, metric)); err != nil {
			t.Fatal(err)
		}
	}

	metrics.ExposeMetadata(false)
	var output strings.Builder
	metricSet.WritePrometheus(&output)
	assert(t, trim(`
probe_counter{} 3 1712345678901
probe_gauge{} 1.5 1712345678901
probe_untyped{} -2 1712345678901
`), output.String())
}

func TestMakeMetricsEpochTimestampFallback(t *testing.T) {
	tests := map[string]struct {
		query Query
		value any
	}{
		"query evaluation error": {query: `error("failed")`, value: map[string]any{"timestamp": 1}},
		"fractional value": {
			query: ".timestamp",
			value: map[string]any{"timestamp": 1.5},
		},
		"out of range": {
			query: ".timestamp",
			value: map[string]any{"timestamp": "9223372036854775808"},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			metricSet := newProbeMetricSet()
			err := makeMetrics(t.Context(), metricSet, test.value, mustCompileMetric(t, Metric{
				Name:           "probe_value",
				ValueType:      valueTypeGauge,
				Value:          "1",
				EpochTimestamp: test.query,
			}))
			if err != nil {
				t.Fatal(err)
			}

			metrics.ExposeMetadata(false)
			var output strings.Builder
			metricSet.WriteProbeResult(&output, false)
			assert(t, probeStatus(0, 0, 0, 1, 0, 1)+"probe_value{} 1\n", output.String())
		})
	}
}

func TestMakeMetricsEpochTimestampOptional(t *testing.T) {
	tests := map[string]struct {
		query Query
		value any
	}{
		"null":      {query: ".timestamp", value: map[string]any{"timestamp": nil}},
		"missing":   {query: ".timestamp", value: map[string]any{}},
		"no result": {query: ".timestamp // empty", value: map[string]any{}},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			metricSet := newProbeMetricSet()
			err := makeMetrics(t.Context(), metricSet, test.value, mustCompileMetric(t, Metric{
				Name:           "probe_value",
				ValueType:      valueTypeGauge,
				Value:          "1",
				EpochTimestamp: test.query,
			}))
			if err != nil {
				t.Fatal(err)
			}

			metrics.ExposeMetadata(false)
			var output strings.Builder
			metricSet.WriteProbeResult(&output, false)
			assert(t, probeStatus(0, 0, 0, 1, 1, 0)+"probe_value{} 1\n", output.String())
		})
	}
}

func TestMakeMetricsUntyped(t *testing.T) {
	metricSet := newProbeMetricSet()
	metric := Metric{
		Name:      "probe_value",
		Labels:    map[string]Query{"id": ".id"},
		ValueType: valueTypeUntyped,
		Value:     ".value",
	}
	for _, value := range []any{
		map[string]any{"id": "b", "value": -2},
		map[string]any{"id": "a", "value": 1.5},
	} {
		if err := makeMetrics(t.Context(), metricSet, value, mustCompileMetric(t, metric)); err != nil {
			t.Fatal(err)
		}
	}

	metrics.ExposeMetadata(false)
	var withoutMetadata strings.Builder
	metricSet.WritePrometheus(&withoutMetadata)
	assert(t, trim(`
probe_value{id="a"} 1.5
probe_value{id="b"} -2
`), withoutMetadata.String())

	metrics.ExposeMetadata(true)
	t.Cleanup(func() { metrics.ExposeMetadata(false) })
	var withMetadata strings.Builder
	metricSet.WritePrometheus(&withMetadata)
	assert(t, trim(`
# HELP probe_value
# TYPE probe_value untyped
probe_value{id="a"} 1.5
probe_value{id="b"} -2
`), withMetadata.String())
}

func TestMakeMetricsRejectsConflictingValueTypes(t *testing.T) {
	metricSet := newProbeMetricSet()
	for _, valueType := range []string{valueTypeGauge, valueTypeUntyped} {
		err := makeMetrics(t.Context(), metricSet, nil, mustCompileMetric(t, Metric{
			Name:      "probe_value",
			ValueType: valueType,
			Value:     "1",
		}))
		if valueType == valueTypeGauge && err != nil {
			t.Fatal(err)
		}
		if valueType == valueTypeUntyped {
			if err == nil {
				t.Fatal("expected conflicting valueType error")
			}
			if !strings.Contains(err.Error(), "conflicting valueTypes") {
				t.Fatalf("unexpected error: %v", err)
			}
		}
	}
}

func TestMakeMetricsRejectsUnknownValueType(t *testing.T) {
	err := makeMetrics(t.Context(), newProbeMetricSet(), nil, mustCompileMetric(t, Metric{
		Name:      "probe_value",
		ValueType: "unknown",
		Value:     "1",
	}))
	if err == nil {
		t.Fatal("expected unsupported valueType error")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("unexpected error: %v", err)
	}
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
			err := makeMetrics(t.Context(), newProbeMetricSet(), nil, mustCompileMetric(t, metric))
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

func probeStatus(bodyErrors, fetchErrors, metricsFailed, metricsSuccessful, success, timestampErrors int) string {
	return fmt.Sprintf(`probe_body_errors %d
probe_fetch_errors %d
probe_metrics_failed %d
probe_metrics_successful %d
probe_success %d
probe_timestamp_errors %d
`, bodyErrors, fetchErrors, metricsFailed, metricsSuccessful, success, timestampErrors)
}

func mustCompileConfig(t *testing.T, cfg *Config) *Config {
	t.Helper()
	if err := compileConfig(cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func queryPointer(query Query) *Query {
	return &query
}

func mustCompileMetric(t *testing.T, metric Metric) Metric {
	t.Helper()
	compiled, err := compileMetric(make(queryCompiler), metric)
	if err != nil {
		t.Fatal(err)
	}
	return compiled
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
