package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	updateReleasesURL    = "https://api.github.com/repos/levmv/skot/releases?per_page=100"
	updateReleaseBaseURL = "https://github.com/levmv/skot/releases/download"
	maxReleaseListBytes  = 4 << 20
	maxChecksumsBytes    = 1 << 20
	maxUpdateBinaryBytes = 256 << 20
)

type updater struct {
	client         *http.Client
	releasesURL    string
	releaseBaseURL string
	executablePath string
	currentVersion string
	goos           string
	goarch         string
}

type updateResult struct {
	from    string
	to      string
	changed bool
}

type githubRelease struct {
	TagName string `json:"tag_name"`
	Draft   bool   `json:"draft"`
}

func runUpdateCommand(ctx context.Context, output io.Writer) error {
	if version == "dev" {
		return errors.New("cannot update a development build; install a release build first")
	}
	executablePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate the running executable: %w", err)
	}
	result, err := updater{
		client:         &http.Client{Timeout: 5 * time.Minute},
		releasesURL:    updateReleasesURL,
		releaseBaseURL: updateReleaseBaseURL,
		executablePath: executablePath,
		currentVersion: version,
		goos:           runtime.GOOS,
		goarch:         runtime.GOARCH,
	}.update(ctx)
	if err != nil {
		return err
	}
	if !result.changed {
		_, err = fmt.Fprintf(output, "Skot is already up to date (%s).\n", result.to)
		return err
	}
	_, err = fmt.Fprintf(output, "Updated Skot from %s to %s. Restart running Skot sessions to use the new version.\n", result.from, result.to)
	return err
}

func (u updater) update(ctx context.Context) (updateResult, error) {
	if u.client == nil {
		return updateResult{}, errors.New("update HTTP client is not configured")
	}
	if strings.TrimSpace(u.executablePath) == "" {
		return updateResult{}, errors.New("update executable path is empty")
	}
	asset, err := updateAssetName(u.goos, u.goarch)
	if err != nil {
		return updateResult{}, err
	}
	tag, err := u.latestReleaseTag(ctx)
	if err != nil {
		return updateResult{}, err
	}
	latestVersion := strings.TrimPrefix(tag, "skot-")
	result := updateResult{from: u.currentVersion, to: latestVersion}
	if latestVersion == u.currentVersion {
		return result, nil
	}

	tagPath := url.PathEscape(tag)
	checksumsURL := strings.TrimRight(u.releaseBaseURL, "/") + "/" + tagPath + "/checksums.txt"
	checksums, err := u.readURL(ctx, checksumsURL, maxChecksumsBytes)
	if err != nil {
		return updateResult{}, fmt.Errorf("download checksums for %s: %w", tag, err)
	}
	expectedChecksum, err := checksumForAsset(checksums, asset)
	if err != nil {
		return updateResult{}, fmt.Errorf("read checksums for %s: %w", tag, err)
	}

	executablePath, err := filepath.Abs(u.executablePath)
	if err != nil {
		return updateResult{}, fmt.Errorf("resolve executable path: %w", err)
	}
	info, err := os.Stat(executablePath)
	if err != nil {
		return updateResult{}, fmt.Errorf("inspect executable %s: %w", executablePath, err)
	}
	if !info.Mode().IsRegular() {
		return updateResult{}, fmt.Errorf("executable %s is not a regular file", executablePath)
	}

	staged, err := os.CreateTemp(filepath.Dir(executablePath), ".sk.update-*")
	if err != nil {
		return updateResult{}, fmt.Errorf("stage update next to %s: %w", executablePath, err)
	}
	stagedPath := staged.Name()
	keepStaged := false
	defer func() {
		_ = staged.Close()
		if !keepStaged {
			_ = os.Remove(stagedPath)
		}
	}()

	assetURL := strings.TrimRight(u.releaseBaseURL, "/") + "/" + tagPath + "/" + url.PathEscape(asset)
	actualChecksum, err := u.downloadBinary(ctx, assetURL, staged)
	if err != nil {
		return updateResult{}, fmt.Errorf("download %s: %w", asset, err)
	}
	if actualChecksum != expectedChecksum {
		return updateResult{}, fmt.Errorf("checksum verification failed for %s", asset)
	}
	mode := info.Mode().Perm()
	if mode&0o111 == 0 {
		mode = 0o755
	}
	if err := staged.Chmod(mode); err != nil {
		return updateResult{}, fmt.Errorf("make staged update executable: %w", err)
	}
	if err := staged.Sync(); err != nil {
		return updateResult{}, fmt.Errorf("sync staged update: %w", err)
	}
	if err := staged.Close(); err != nil {
		return updateResult{}, fmt.Errorf("close staged update: %w", err)
	}
	if err := os.Rename(stagedPath, executablePath); err != nil {
		return updateResult{}, fmt.Errorf("replace executable %s: %w", executablePath, err)
	}
	keepStaged = true
	result.changed = true
	return result, nil
}

func (u updater) latestReleaseTag(ctx context.Context) (string, error) {
	data, err := u.readURL(ctx, u.releasesURL, maxReleaseListBytes)
	if err != nil {
		return "", fmt.Errorf("list GitHub releases: %w", err)
	}
	var releases []githubRelease
	if err := json.Unmarshal(data, &releases); err != nil {
		return "", fmt.Errorf("decode GitHub releases: %w", err)
	}
	for _, release := range releases {
		if !release.Draft && strings.HasPrefix(release.TagName, "skot-v") {
			return release.TagName, nil
		}
	}
	return "", errors.New("no skot-v* GitHub release was found")
}

func (u updater) readURL(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	response, err := u.get(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if err := requireSuccessfulResponse(response); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return data, nil
}

func (u updater) downloadBinary(ctx context.Context, rawURL string, destination io.Writer) (string, error) {
	response, err := u.get(ctx, rawURL)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if err := requireSuccessfulResponse(response); err != nil {
		return "", err
	}
	hash := sha256.New()
	limited := &io.LimitedReader{R: response.Body, N: maxUpdateBinaryBytes + 1}
	written, err := io.Copy(io.MultiWriter(destination, hash), limited)
	if err != nil {
		return "", err
	}
	if written > maxUpdateBinaryBytes {
		return "", fmt.Errorf("binary exceeds %d bytes", maxUpdateBinaryBytes)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (u updater) get(ctx context.Context, rawURL string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "skot/"+u.currentVersion)
	return u.client.Do(request)
}

func requireSuccessfulResponse(response *http.Response) error {
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	detail, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
	message := strings.TrimSpace(string(detail))
	if message == "" {
		return fmt.Errorf("HTTP %s", response.Status)
	}
	return fmt.Errorf("HTTP %s: %s", response.Status, message)
}

func checksumForAsset(data []byte, asset string) (string, error) {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.TrimPrefix(fields[1], "*") != asset {
			continue
		}
		checksum := strings.ToLower(fields[0])
		decoded, err := hex.DecodeString(checksum)
		if err != nil || len(decoded) != sha256.Size {
			return "", fmt.Errorf("invalid SHA-256 checksum for %s", asset)
		}
		return checksum, nil
	}
	return "", fmt.Errorf("%s is missing from checksums.txt", asset)
}

func updateAssetName(goos, goarch string) (string, error) {
	if goos != "linux" && goos != "darwin" {
		return "", fmt.Errorf("self-update is unsupported on %s", goos)
	}
	if goarch != "amd64" && goarch != "arm64" {
		return "", fmt.Errorf("self-update is unsupported on %s/%s", goos, goarch)
	}
	return "sk-" + goos + "-" + goarch, nil
}
