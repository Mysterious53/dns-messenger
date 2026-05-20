package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	updatepkg "github.com/mahdi/dnsmessenger/internal/update"
)

const (
	githubReleasesLatestURL = "https://api.github.com/repos/sartoopjj/thefeed/releases/latest"
	gitlabReleasesURL       = "https://gitlab.com/api/v4/projects/sartoopjj%2Fthefeed/releases?per_page=20"
)

type releaseInfo struct {
	TagName string `json:"tag_name"`
}

// startLatestVersionTracker periodically fetches the latest release version
// from GitHub and GitLab mirrors and stores the higher one in the dedicated
// version channel.
func startLatestVersionTracker(ctx context.Context, feed *Feed) {
	check := func() {
		v, err := fetchLatestReleaseVersion(ctx)
		if err != nil {
			log.Printf("[version] check latest release failed: %v", err)
			return
		}
		feed.SetLatestVersion(v)
	}

	check()
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}

// fetchLatestReleaseVersion queries GitHub and GitLab in parallel and returns
// the newer of the two. Errors from one side (e.g. 404 when the GitHub
// account is suspended) are logged and ignored when the other side succeeds.
func fetchLatestReleaseVersion(parent context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	var ghVer, glVer string
	var ghErr, glErr error

	wg.Add(2)
	go func() {
		defer wg.Done()
		ghVer, ghErr = fetchGitHubLatest(ctx)
	}()
	go func() {
		defer wg.Done()
		glVer, glErr = fetchGitLabLatest(ctx)
	}()
	wg.Wait()

	if ghErr != nil {
		log.Printf("[version] github: %v", ghErr)
	}
	if glErr != nil {
		log.Printf("[version] gitlab: %v", glErr)
	}

	best, source := ghVer, "github"
	if best == "" || (glVer != "" && updatepkg.IsNewer(glVer, best)) {
		best, source = glVer, "gitlab"
	}
	if best == "" {
		return "", fmt.Errorf("no mirror returned a release version")
	}
	log.Printf("[version] latest=%s source=%s (github=%q gitlab=%q)", best, source, ghVer, glVer)
	return best, nil
}

// fetchGitHubLatest returns the latest stable release tag (no "v" prefix).
// GitHub's /releases/latest endpoint already excludes prereleases.
func fetchGitHubLatest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubReleasesLatestURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "thefeed-server")
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status: %s", resp.Status)
	}

	var rel releaseInfo
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", err
	}
	return cleanReleaseTag(rel.TagName)
}

// fetchGitLabLatest returns the most recent stable release from GitLab.
// Pre-releases are detected by a hyphen in the tag (v1.0.0-rc1) — same
// convention as install.sh and the release-cli pipeline job.
func fetchGitLabLatest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gitlabReleasesURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "thefeed-server")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status: %s", resp.Status)
	}

	var rels []releaseInfo
	if err := json.NewDecoder(resp.Body).Decode(&rels); err != nil {
		return "", err
	}
	for _, r := range rels {
		t := strings.TrimSpace(r.TagName)
		if t == "" || strings.Contains(t, "-") {
			continue
		}
		return cleanReleaseTag(t)
	}
	return "", fmt.Errorf("no stable release found")
}

func cleanReleaseTag(tag string) (string, error) {
	v := strings.TrimSpace(tag)
	v = strings.TrimPrefix(v, "v")
	if v == "" {
		return "", fmt.Errorf("empty tag")
	}
	return v, nil
}
