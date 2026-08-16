package exporter

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

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
