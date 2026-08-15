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
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// releaseVersion is a stable vX.Y.Z release tag. Tags are also the version
// strings that release builds report.
type releaseVersion struct {
	tag   string
	parts [3]int
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
	latest, err := u.latestStableRelease(ctx)
	if err != nil {
		return updateResult{}, err
	}
	result := updateResult{from: u.currentVersion, to: latest.tag}
	// A build whose version is not a release tag parses as v0.0.0 and is
	// therefore replaced by any published release.
	current, _ := parseReleaseVersion(u.currentVersion)
	if !latest.newerThan(current) {
		return result, nil
	}

	tagPath := url.PathEscape(latest.tag)
	checksumsURL := strings.TrimRight(u.releaseBaseURL, "/") + "/" + tagPath + "/checksums.txt"
	checksums, err := u.readURL(ctx, checksumsURL, maxChecksumsBytes)
	if err != nil {
		return updateResult{}, fmt.Errorf("download checksums for %s: %w", latest.tag, err)
	}
	expectedChecksum, err := checksumForAsset(checksums, asset)
	if err != nil {
		return updateResult{}, fmt.Errorf("read checksums for %s: %w", latest.tag, err)
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

// latestStableRelease returns the highest published vX.Y.Z release. Drafts,
// prereleases, and tags that are not plain release versions are ignored, and
// the highest version wins rather than the first listed one, so neither a
// preview nor a late republished older tag can be selected.
func (u updater) latestStableRelease(ctx context.Context) (releaseVersion, error) {
	data, err := u.readURL(ctx, u.releasesURL, maxReleaseListBytes)
	if err != nil {
		return releaseVersion{}, fmt.Errorf("list GitHub releases: %w", err)
	}
	var releases []githubRelease
	if err := json.Unmarshal(data, &releases); err != nil {
		return releaseVersion{}, fmt.Errorf("decode GitHub releases: %w", err)
	}
	var latest releaseVersion
	for _, release := range releases {
		if release.Draft || release.Prerelease {
			continue
		}
		candidate, ok := parseReleaseVersion(release.TagName)
		if ok && (latest.tag == "" || candidate.newerThan(latest)) {
			latest = candidate
		}
	}
	if latest.tag == "" {
		return releaseVersion{}, errors.New("no stable vX.Y.Z GitHub release was found")
	}
	return latest, nil
}

// parseReleaseVersion accepts vX.Y.Z and rejects everything else, including
// prerelease and build suffixes such as v1.2.0-rc1.
func parseReleaseVersion(tag string) (releaseVersion, bool) {
	rest, found := strings.CutPrefix(tag, "v")
	if !found {
		return releaseVersion{}, false
	}
	fields := strings.Split(rest, ".")
	if len(fields) != 3 {
		return releaseVersion{}, false
	}
	version := releaseVersion{tag: tag}
	for index, field := range fields {
		if field == "" || len(field) > 9 {
			return releaseVersion{}, false
		}
		value := 0
		for _, digit := range []byte(field) {
			if digit < '0' || digit > '9' {
				return releaseVersion{}, false
			}
			value = value*10 + int(digit-'0')
		}
		version.parts[index] = value
	}
	return version, true
}

func (v releaseVersion) newerThan(other releaseVersion) bool {
	for index := range v.parts {
		if v.parts[index] != other.parts[index] {
			return v.parts[index] > other.parts[index]
		}
	}
	return false
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
