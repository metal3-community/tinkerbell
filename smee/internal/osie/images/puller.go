package images

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/pkg/credentialprovider"
	"k8s.io/kubernetes/pkg/util/parsers"
)

// Puller manages downloading OCI images and extracting them to a local directory.
// It uses wait.UntilWithContext to periodically attempt pulls when the destination
// directory is empty, following the Kubernetes image puller pattern.
type Puller struct {
	registry    string
	repository  string
	reference   string
	username    string
	password    string
	destDir     string
	pullTimeout time.Duration
	log         logr.Logger
	mu          sync.RWMutex
	ready       bool
}

// Option configures a Puller.
type Option func(*Puller)

// WithRegistry sets the OCI registry URL (e.g. "ghcr.io").
func WithRegistry(registry string) Option {
	return func(p *Puller) { p.registry = registry }
}

// WithRepository sets the OCI repository path (e.g. "tinkerbell/captain/artifacts").
func WithRepository(repository string) Option {
	return func(p *Puller) { p.repository = repository }
}

// WithReference sets the OCI image tag or digest (e.g. "latest", "v1.2.3").
func WithReference(reference string) Option {
	return func(p *Puller) { p.reference = reference }
}

// WithUsername sets the optional username for OCI registry authentication.
func WithUsername(username string) Option {
	return func(p *Puller) { p.username = username }
}

// WithPassword sets the optional password for OCI registry authentication.
func WithPassword(password string) Option {
	return func(p *Puller) { p.password = password }
}

// WithDestDir sets the local directory where images are extracted.
func WithDestDir(dir string) Option {
	return func(p *Puller) { p.destDir = dir }
}

// WithPullTimeout sets the timeout for individual OCI pull operations.
func WithPullTimeout(timeout time.Duration) Option {
	return func(p *Puller) { p.pullTimeout = timeout }
}

// NewPuller creates a new image puller and starts a background worker that
// periodically checks whether the destination directory contains files. If
// no files are found, it pulls the configured OCI image. The worker follows
// the Kubernetes serialImagePuller pattern, using wait.UntilWithContext to
// repeatedly call processImagePullRequests at a one-second interval until
// ctx is cancelled.
func NewPuller(ctx context.Context, log logr.Logger, opts ...Option) (*Puller, error) {
	p := &Puller{
		log: log,
	}
	for _, o := range opts {
		o(p)
	}

	if err := os.MkdirAll(p.destDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating image directory %q: %w", p.destDir, err)
	}

	if p.hasFiles() {
		log.Info("image directory already populated, skipping pull", "dir", p.destDir)
		p.ready = true
		return p, nil
	}

	log.Info("image directory is empty, starting background puller",
		"dir", p.destDir,
		"registry", p.registry,
		"repository", p.repository,
		"reference", p.reference)
	go wait.UntilWithContext(ctx, p.processImagePullRequests, time.Second)

	return p, nil
}

// Ready reports whether the image directory has been populated.
func (p *Puller) Ready() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.ready
}

// processImagePullRequests is the worker function called by wait.UntilWithContext.
// On each invocation it checks readiness, and if not ready, attempts an OCI pull.
// If the pull fails the function returns and wait.Until calls it again after the
// configured period. Once the pull succeeds the puller is marked ready and all
// subsequent invocations are no-ops.
func (p *Puller) processImagePullRequests(ctx context.Context) {
	if p.Ready() {
		return
	}

	p.log.Info("pulling OCI image",
		"registry", p.registry,
		"repository", p.repository,
		"reference", p.reference)

	if err := p.pull(ctx); err != nil {
		p.log.Error(err, "failed to pull OCI image, will retry")
		return
	}

	p.mu.Lock()
	p.ready = true
	p.mu.Unlock()
	p.log.Info("OCI image pulled and ready", "dir", p.destDir)
}

// pull performs a single OCI image pull with timeout.
func (p *Puller) pull(ctx context.Context) error {
	pullCtx, cancel := context.WithTimeout(ctx, p.pullTimeout)
	defer cancel()

	// Build and parse the full image reference.
	imageName := fmt.Sprintf("%s/%s:%s", p.registry, p.repository, p.reference)
	if strings.HasPrefix(p.reference, "sha256:") {
		imageName = fmt.Sprintf("%s/%s@%s", p.registry, p.repository, p.reference)
	}

	repoToPull, tag, digest, err := parsers.ParseImageName(imageName)
	if err != nil {
		return fmt.Errorf("parsing image name %q: %w", imageName, err)
	}

	ref := tag
	if digest != "" {
		ref = digest
	}

	// Split "registry/path" into registry host and repository path.
	registry, repoPath, ok := strings.Cut(repoToPull, "/")
	if !ok {
		return fmt.Errorf("invalid repository %q: missing registry host", repoToPull)
	}

	// Resolve credentials: prefer static config, fall back to the default Docker keyring
	// (reads ~/.docker/config.json and other configured credential helpers).
	username, password := p.username, p.password
	if username == "" && password == "" {
		kr := credentialprovider.NewDefaultDockerKeyring()
		if creds, found := kr.Lookup(repoToPull); found && len(creds) > 0 {
			username = creds[0].Username
			password = creds[0].Password
		}
	}

	httpClient := &http.Client{Timeout: p.pullTimeout}

	// Fetch the OCI manifest, resolving a multi-arch index to the current platform's manifest.
	manifest, err := fetchOCIManifest(pullCtx, httpClient, registry, repoPath, ref, username, password)
	if err != nil {
		return fmt.Errorf("fetching OCI manifest: %w", err)
	}

	p.log.Info("fetched OCI manifest", "layers", len(manifest.Layers), "mediaType", manifest.MediaType)

	// Download and extract each layer into destDir.
	for i, layer := range manifest.Layers {
		p.log.Info("extracting layer", "index", i, "digest", layer.Digest, "size", layer.Size)
		if err := fetchAndExtractOCILayer(pullCtx, httpClient, registry, repoPath, layer.Digest, p.destDir, username, password); err != nil {
			return fmt.Errorf("extracting layer %s: %w", layer.Digest, err)
		}
	}

	p.log.Info("OCI image pulled successfully",
		"registry", registry,
		"repo", repoPath,
		"ref", ref,
		"layers", len(manifest.Layers))
	return nil
}

// hasFiles reports whether destDir contains at least one regular file.
func (p *Puller) hasFiles() bool {
	entries, err := os.ReadDir(p.destDir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			return true
		}
	}
	return false
}

// OCI Distribution Spec types and helpers.

// ociDescriptor represents an OCI content descriptor used in manifests and indexes.
type ociDescriptor struct {
	MediaType string       `json:"mediaType"`
	Digest    string       `json:"digest"`
	Size      int64        `json:"size"`
	Platform  *ociPlatform `json:"platform,omitempty"`
}

// ociPlatform identifies the operating system and CPU architecture a manifest targets.
type ociPlatform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
}

// ociManifest represents an OCI image manifest (schema version 2).
type ociManifest struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Config        ociDescriptor   `json:"config"`
	Layers        []ociDescriptor `json:"layers"`
}

// ociIndex represents an OCI image index (multi-arch manifest list).
type ociIndex struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     string          `json:"mediaType"`
	Manifests     []ociDescriptor `json:"manifests"`
}

// fetchOCIManifest fetches the OCI manifest for the given image reference. If the registry
// returns a multi-arch index it selects the manifest matching the current OS/architecture.
func fetchOCIManifest(ctx context.Context, client *http.Client, registry, repo, ref, username, password string) (*ociManifest, error) {
	const (
		mediaTypeOCIIndex    = "application/vnd.oci.image.index.v1+json"
		mediaTypeOCIManifest = "application/vnd.oci.image.manifest.v1+json"
		mediaTypeDockerV2    = "application/vnd.docker.distribution.manifest.v2+json"
		mediaTypeDockerList  = "application/vnd.docker.distribution.manifest.list.v2+json"
	)

	manifestURL := fmt.Sprintf("https://%s/v2/%s/manifests/%s", registry, repo, ref)
	headers := map[string]string{
		"Accept": strings.Join([]string{mediaTypeOCIManifest, mediaTypeDockerV2, mediaTypeOCIIndex, mediaTypeDockerList}, ","),
	}

	resp, err := ociDo(ctx, client, http.MethodGet, manifestURL, headers, username, password)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("manifest fetch returned HTTP %d for %s", resp.StatusCode, manifestURL)
	}

	// Determine content type, stripping any parameters (e.g. charset).
	contentType := resp.Header.Get("Content-Type")
	if i := strings.Index(contentType, ";"); i >= 0 {
		contentType = strings.TrimSpace(contentType[:i])
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading manifest response: %w", err)
	}

	switch contentType {
	case mediaTypeOCIIndex, mediaTypeDockerList:
		// Multi-arch index: recurse with the digest of the matching platform manifest.
		var index ociIndex
		if err := json.Unmarshal(body, &index); err != nil {
			return nil, fmt.Errorf("parsing OCI index: %w", err)
		}
		for _, m := range index.Manifests {
			if m.Platform != nil && m.Platform.OS == "linux" && m.Platform.Architecture == runtime.GOARCH {
				return fetchOCIManifest(ctx, client, registry, repo, m.Digest, username, password)
			}
		}
		return nil, fmt.Errorf("no manifest for linux/%s found in OCI index", runtime.GOARCH)
	default:
		var manifest ociManifest
		if err := json.Unmarshal(body, &manifest); err != nil {
			return nil, fmt.Errorf("parsing OCI manifest: %w", err)
		}
		if manifest.MediaType == "" {
			manifest.MediaType = contentType
		}
		return &manifest, nil
	}
}

// fetchAndExtractOCILayer downloads a single OCI layer blob and extracts it into destDir.
func fetchAndExtractOCILayer(ctx context.Context, client *http.Client, registry, repo, digest, destDir, username, password string) error {
	blobURL := fmt.Sprintf("https://%s/v2/%s/blobs/%s", registry, repo, digest)
	resp, err := ociDo(ctx, client, http.MethodGet, blobURL, nil, username, password)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("blob fetch returned HTTP %d for %s", resp.StatusCode, blobURL)
	}

	return extractTar(resp.Body, destDir)
}

// extractTar extracts an optionally gzip-compressed tar stream into destDir.
// Entries that would escape destDir via ".." or absolute symlinks are silently skipped.
func extractTar(r io.Reader, destDir string) error {
	// Detect gzip by peeking at the two-byte magic number (0x1F 0x8B).
	peek := make([]byte, 2)
	n, _ := io.ReadFull(r, peek)
	combined := io.MultiReader(bytes.NewReader(peek[:n]), r)

	var tarReader *tar.Reader
	if n == 2 && peek[0] == 0x1f && peek[1] == 0x8b {
		gz, err := gzip.NewReader(combined)
		if err != nil {
			return fmt.Errorf("creating gzip reader: %w", err)
		}
		defer gz.Close()
		tarReader = tar.NewReader(gz)
	} else {
		tarReader = tar.NewReader(combined)
	}

	cleanDest := filepath.Clean(destDir) + string(os.PathSeparator)
	for {
		hdr, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar entry: %w", err)
		}

		// Reject path traversal.
		cleanName := filepath.Clean(filepath.FromSlash(hdr.Name))
		if strings.HasPrefix(cleanName, "..") {
			continue
		}
		target := filepath.Join(destDir, cleanName)
		if !strings.HasPrefix(target+string(os.PathSeparator), cleanDest) {
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("creating directory %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("creating parent directory for %s: %w", target, err)
			}
			// #nosec G304 -- target path is validated against destDir above
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, hdr.FileInfo().Mode())
			if err != nil {
				return fmt.Errorf("creating file %s: %w", target, err)
			}
			// #nosec G110 -- layers originate from a configured, trusted OCI registry
			_, copyErr := io.Copy(f, tarReader)
			f.Close()
			if copyErr != nil {
				return fmt.Errorf("writing file %s: %w", target, copyErr)
			}
		case tar.TypeSymlink:
			// Skip symlinks that escape destDir.
			if filepath.IsAbs(hdr.Linkname) || strings.HasPrefix(filepath.Clean(hdr.Linkname), "..") {
				continue
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil && !os.IsExist(err) {
				return fmt.Errorf("creating symlink %s: %w", target, err)
			}
		}
	}
	return nil
}

// ociDo performs an authenticated HTTP request against an OCI registry endpoint. It handles
// the standard bearer-token challenge (OCI Distribution Spec §4.2) transparently.
func ociDo(ctx context.Context, client *http.Client, method, rawURL string, headers map[string]string, username, password string) (*http.Response, error) {
	newReq := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
		if err != nil {
			return nil, fmt.Errorf("building request for %s: %w", rawURL, err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		return req, nil
	}

	req, err := newReq()
	if err != nil {
		return nil, err
	}
	if username != "" || password != "" {
		req.SetBasicAuth(username, password)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusUnauthorized {
		return resp, nil
	}
	resp.Body.Close()

	// The registry requires a bearer token — parse the WWW-Authenticate challenge.
	wwwAuth := resp.Header.Get("Www-Authenticate")
	if !strings.HasPrefix(strings.ToLower(wwwAuth), "bearer ") {
		return nil, fmt.Errorf("registry returned 401 with unsupported auth scheme: %q", wwwAuth)
	}

	params := parseAuthChallenge(wwwAuth[len("Bearer "):])
	token, err := fetchOCIToken(ctx, client, params["realm"], params["service"], params["scope"], username, password)
	if err != nil {
		return nil, fmt.Errorf("fetching registry token: %w", err)
	}

	// Retry the original request with the bearer token.
	req2, err := newReq()
	if err != nil {
		return nil, err
	}
	req2.Header.Set("Authorization", "Bearer "+token)
	return client.Do(req2)
}

// authChallengeRegexp matches key="value" pairs in a WWW-Authenticate Bearer challenge.
var authChallengeRegexp = regexp.MustCompile(`(\w+)="([^"]*)"`)

// parseAuthChallenge parses a Bearer challenge string into a parameter map.
// Example input: `realm="https://ghcr.io/token",service="ghcr.io",scope="repository:foo/bar:pull"`
func parseAuthChallenge(challenge string) map[string]string {
	params := make(map[string]string)
	for _, m := range authChallengeRegexp.FindAllStringSubmatch(challenge, -1) {
		params[m[1]] = m[2]
	}
	return params
}

// fetchOCIToken obtains a bearer token from the registry's token endpoint.
func fetchOCIToken(ctx context.Context, client *http.Client, realm, service, scope, username, password string) (string, error) {
	tokenURL, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("parsing token realm %q: %w", realm, err)
	}
	q := tokenURL.Query()
	if service != "" {
		q.Set("service", service)
	}
	if scope != "" {
		q.Set("scope", scope)
	}
	tokenURL.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL.String(), nil)
	if err != nil {
		return "", fmt.Errorf("building token request: %w", err)
	}
	if username != "" || password != "" {
		req.SetBasicAuth(username, password)
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned HTTP %d", resp.StatusCode)
	}

	var tokenResp struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", fmt.Errorf("decoding token response: %w", err)
	}
	if tokenResp.Token != "" {
		return tokenResp.Token, nil
	}
	return tokenResp.AccessToken, nil
}
