package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdaterReplacesExecutableWithVerifiedRelease(t *testing.T) {
	asset := "sk-linux-amd64"
	binary := []byte("new sk binary")
	checksum := sha256.Sum256(binary)
	mux := http.NewServeMux()
	mux.HandleFunc("/releases", func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("User-Agent") != "skot/v0.1.0" {
			t.Errorf("User-Agent = %q", request.Header.Get("User-Agent"))
		}
		_, _ = io.WriteString(writer, `[{"tag_name":"other-v9.0.0"},{"tag_name":"skot-v0.2.0"}]`)
	})
	mux.HandleFunc("/download/skot-v0.2.0/checksums.txt", func(writer http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(writer, "%s  %s\n", hex.EncodeToString(checksum[:]), asset)
	})
	mux.HandleFunc("/download/skot-v0.2.0/"+asset, func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(binary)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	directory := t.TempDir()
	executable := filepath.Join(directory, "sk")
	if err := os.WriteFile(executable, []byte("old sk binary"), 0o751); err != nil {
		t.Fatal(err)
	}
	result, err := updater{
		client:         server.Client(),
		releasesURL:    server.URL + "/releases",
		releaseBaseURL: server.URL + "/download",
		executablePath: executable,
		currentVersion: "v0.1.0",
		goos:           "linux",
		goarch:         "amd64",
	}.update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.changed || result.from != "v0.1.0" || result.to != "v0.2.0" {
		t.Fatalf("result = %#v", result)
	}
	installed, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(installed, binary) {
		t.Fatalf("installed binary = %q", installed)
	}
	info, err := os.Stat(executable)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o751 {
		t.Fatalf("installed mode = %o", info.Mode().Perm())
	}
	assertNoStagedUpdates(t, directory)
}

func TestUpdaterLeavesExecutableUntouchedOnChecksumFailure(t *testing.T) {
	oldBinary := []byte("old sk binary")
	mux := http.NewServeMux()
	mux.HandleFunc("/releases", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, `[{"tag_name":"skot-v0.2.0"}]`)
	})
	mux.HandleFunc("/download/skot-v0.2.0/checksums.txt", func(writer http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(writer, "%064x  sk-linux-amd64\n", 1)
	})
	mux.HandleFunc("/download/skot-v0.2.0/sk-linux-amd64", func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("corrupt download"))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	directory := t.TempDir()
	executable := filepath.Join(directory, "sk")
	if err := os.WriteFile(executable, oldBinary, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := updater{
		client:         server.Client(),
		releasesURL:    server.URL + "/releases",
		releaseBaseURL: server.URL + "/download",
		executablePath: executable,
		currentVersion: "v0.1.0",
		goos:           "linux",
		goarch:         "amd64",
	}.update(context.Background())
	if err == nil || !strings.Contains(err.Error(), "checksum verification failed") {
		t.Fatalf("error = %v", err)
	}
	installed, readErr := os.ReadFile(executable)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(installed, oldBinary) {
		t.Fatalf("installed binary changed to %q", installed)
	}
	assertNoStagedUpdates(t, directory)
}

func TestUpdaterSkipsDownloadWhenCurrentReleaseIsLatest(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Path != "/releases" {
			t.Errorf("unexpected request path %q", request.URL.Path)
		}
		_, _ = io.WriteString(writer, `[{"tag_name":"skot-v0.2.0"}]`)
	}))
	defer server.Close()

	result, err := updater{
		client:         server.Client(),
		releasesURL:    server.URL + "/releases",
		releaseBaseURL: server.URL + "/download",
		executablePath: filepath.Join(t.TempDir(), "does-not-need-to-exist"),
		currentVersion: "v0.2.0",
		goos:           "linux",
		goarch:         "amd64",
	}.update(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.changed || result.to != "v0.2.0" || requests != 1 {
		t.Fatalf("result/requests = %#v/%d", result, requests)
	}
}

func TestUpdaterReportsMissingSkotRelease(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, `[{"tag_name":"other-v1.0.0"}]`)
	}))
	defer server.Close()

	_, err := updater{
		client:         server.Client(),
		releasesURL:    server.URL,
		releaseBaseURL: server.URL,
		executablePath: filepath.Join(t.TempDir(), "sk"),
		currentVersion: "v0.1.0",
		goos:           "linux",
		goarch:         "amd64",
	}.update(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no skot-v* GitHub release") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunUpdateRejectsDevelopmentBuildBeforeApplicationSetup(t *testing.T) {
	if version != "dev" {
		t.Skip("test requires the normal development build")
	}
	t.Setenv("SK_RETRY_BUDGET", "invalid")
	var output bytes.Buffer
	err := run(context.Background(), []string{"update"}, bytes.NewReader(nil), &output, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "cannot update a development build") {
		t.Fatalf("error = %v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("output = %q", output.String())
	}
}

func TestUpdateAssetName(t *testing.T) {
	for _, test := range []struct {
		goos, goarch, want string
	}{
		{goos: "linux", goarch: "amd64", want: "sk-linux-amd64"},
		{goos: "linux", goarch: "arm64", want: "sk-linux-arm64"},
		{goos: "darwin", goarch: "amd64", want: "sk-darwin-amd64"},
		{goos: "darwin", goarch: "arm64", want: "sk-darwin-arm64"},
	} {
		got, err := updateAssetName(test.goos, test.goarch)
		if err != nil || got != test.want {
			t.Errorf("updateAssetName(%q, %q) = %q, %v", test.goos, test.goarch, got, err)
		}
	}
	if _, err := updateAssetName("windows", "amd64"); err == nil {
		t.Fatal("Windows self-update unexpectedly supported")
	}
}

func assertNoStagedUpdates(t *testing.T, directory string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(directory, ".sk.update-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("staged update files remain: %v", matches)
	}
}
