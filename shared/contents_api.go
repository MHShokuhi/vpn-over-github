package shared

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// GitHubContentsClient implements Transport via the REST Contents API.
//
// Each Write is one PUT, each Read is one GET (with ETag → 304 when nothing
// changed, free of REST quota). No pack files, no commit-graph traversal,
// no fetch-before-push. Typical round-trip 200–300 ms vs ~800–1500 ms for
// git Smart HTTP push, which is what makes interactive protocols (Telegram,
// SSH) feel ~5× more responsive.
//
// REST quota: 5000 calls/hr/token (primary). At a 1.5 s push cadence that's
// ~2400 calls/hr, well under. Does NOT trigger gist's 80/min or 500/hr
// secondary caps — those are gist-specific.
//
// Updates require the previous file's blob `sha`; we cache it from the last
// PUT/GET response. On `422 Unprocessable Entity` (sha mismatch) we
// transparently refresh and retry once.
type GitHubContentsClient struct {
	token  string
	owner  string
	repo   string
	client HTTPClient

	rateLimit RateLimitInfo

	mu        sync.Mutex
	shaCache  map[string]string             // path → blob sha
	readCache map[string]*contentsReadCache // path → cached batch + ETag
	listETag  string
	listCache []*ChannelInfo
}

type contentsReadCache struct {
	etag  string
	batch *Batch
	sha   string
}

// NewGitHubContentsClient validates the repo string and constructs a client.
// httpClient may be nil; a default client with 15 s timeout is used.
func NewGitHubContentsClient(token, repo string, httpClient HTTPClient) (*GitHubContentsClient, error) {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("contents transport: repo must be 'owner/repo' format, got %q", repo)
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &GitHubContentsClient{
		token:     token,
		owner:     parts[0],
		repo:      parts[1],
		client:    httpClient,
		shaCache:  make(map[string]string),
		readCache: make(map[string]*contentsReadCache),
		rateLimit: RateLimitInfo{Remaining: 5000},
	}, nil
}

// ── JSON shapes ─────────────────────────────────────────────────────────────

type contentsResp struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	SHA      string `json:"sha"`
	Type     string `json:"type"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

type channelsDirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	SHA  string `json:"sha"`
	Type string `json:"type"`
}

type contentsCommitResp struct {
	Content contentsResp `json:"content"`
}

// ── Transport interface ─────────────────────────────────────────────────────

func (c *GitHubContentsClient) EnsureChannel(ctx context.Context, existingID string) (string, error) {
	if existingID != "" {
		if _, err := c.getFile(ctx, channelFilePath(existingID, ClientBatchFile)); err == nil {
			return existingID, nil
		} else if !errors.Is(err, ErrNotFound) {
			return "", fmt.Errorf("EnsureChannel probe %s: %w", existingID, err)
		}
	}

	id, err := GenerateID()
	if err != nil {
		return "", fmt.Errorf("EnsureChannel generate ID: %w", err)
	}
	if existingID != "" {
		id = existingID
	}

	placeholder, err := EncodeBatchBytes(&Batch{Seq: 0, Ts: 0, Frames: []Frame{}})
	if err != nil {
		return "", fmt.Errorf("EnsureChannel encode placeholder: %w", err)
	}
	for _, fname := range []string{ClientBatchFile, ServerBatchFile} {
		if err := c.putFile(ctx, channelFilePath(id, fname), placeholder, "", "init "+id); err != nil {
			return "", fmt.Errorf("EnsureChannel PUT %s: %w", fname, err)
		}
	}
	return id, nil
}

func (c *GitHubContentsClient) DeleteChannel(ctx context.Context, channelID string) error {
	for _, fname := range []string{ClientBatchFile, ServerBatchFile} {
		path := channelFilePath(channelID, fname)
		sha := c.getCachedSha(path)
		if sha == "" {
			f, err := c.getFile(ctx, path)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					continue
				}
				return fmt.Errorf("DeleteChannel get-sha %s: %w", path, err)
			}
			sha = f.sha
		}
		if err := c.deleteFile(ctx, path, sha, "cleanup "+channelID); err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return fmt.Errorf("DeleteChannel %s: %w", path, err)
		}
	}
	c.invalidateChannelCache(channelID)
	return nil
}

// ListChannels returns the immediate subdirectories of channels/. Uses ETag
// so unchanged listings cost 0 quota.
func (c *GitHubContentsClient) ListChannels(ctx context.Context) ([]*ChannelInfo, error) {
	apiPath := "/repos/" + c.owner + "/" + c.repo + "/contents/" + url.PathEscape(channelsDir)
	req, err := c.newRequest(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return nil, fmt.Errorf("ListChannels build: %w", err)
	}
	c.mu.Lock()
	if c.listETag != "" {
		req.Header.Set("If-None-Match", c.listETag)
	}
	c.mu.Unlock()

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ListChannels request: %w", err)
	}
	defer resp.Body.Close()
	c.updateRateLimitFromHeaders(resp)

	if resp.StatusCode == http.StatusNotModified {
		c.mu.Lock()
		cached := c.listCache
		c.mu.Unlock()
		return cached, nil
	}
	if resp.StatusCode == http.StatusNotFound {
		// channels/ doesn't exist yet — no channels.
		return nil, nil
	}
	if err := c.checkStatus(resp, http.StatusOK); err != nil {
		return nil, fmt.Errorf("ListChannels: %w", err)
	}

	var entries []channelsDirEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("ListChannels decode: %w", err)
	}
	now := time.Now()
	out := make([]*ChannelInfo, 0, len(entries))
	for _, e := range entries {
		if e.Type != "dir" {
			continue
		}
		out = append(out, &ChannelInfo{
			ID:          e.Name,
			Description: ChannelDescPrefix,
			UpdatedAt:   now, // Contents API doesn't report directory mtime
		})
	}
	c.mu.Lock()
	if etag := resp.Header.Get("ETag"); etag != "" {
		c.listETag = etag
	}
	c.listCache = out
	c.mu.Unlock()
	return out, nil
}

func (c *GitHubContentsClient) Write(ctx context.Context, channelID, filename string, batch *Batch) error {
	data, err := EncodeBatchBytes(batch)
	if err != nil {
		return fmt.Errorf("contents Write encode: %w", err)
	}
	path := channelFilePath(channelID, filename)
	return c.putFile(ctx, path, data, c.getCachedSha(path), "tunnel data")
}

func (c *GitHubContentsClient) Read(ctx context.Context, channelID, filename string) (*Batch, error) {
	path := channelFilePath(channelID, filename)
	f, err := c.getFile(ctx, path)
	if err != nil {
		return nil, err
	}
	if f == nil {
		// 304 Not Modified — return whatever we cached last time.
		c.mu.Lock()
		cached := c.readCache[path]
		c.mu.Unlock()
		if cached != nil {
			return cached.batch, nil
		}
		return nil, nil
	}
	if len(f.bytes) == 0 {
		return nil, nil
	}
	batch, err := DecodeBatchBytes(f.bytes)
	if err != nil {
		return nil, fmt.Errorf("contents Read parse %s: %w", path, err)
	}
	if batch.Seq == 0 && len(batch.Frames) == 0 {
		return nil, nil
	}
	return batch, nil
}

func (c *GitHubContentsClient) GetRateLimitInfo() RateLimitInfo {
	return c.rateLimit
}

// ── HTTP helpers ────────────────────────────────────────────────────────────

type contentsFile struct {
	sha   string
	bytes []byte
	etag  string
}

func channelFilePath(channelID, filename string) string {
	return channelsDir + "/" + channelID + "/" + filename
}

func (c *GitHubContentsClient) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, githubAPIBase+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// getFile returns nil, nil on 304 (use the cached batch); ErrNotFound on 404.
func (c *GitHubContentsClient) getFile(ctx context.Context, path string) (*contentsFile, error) {
	apiPath := "/repos/" + c.owner + "/" + c.repo + "/contents/" + escapePath(path)
	req, err := c.newRequest(ctx, http.MethodGet, apiPath, nil)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	cached := c.readCache[path]
	c.mu.Unlock()
	if cached != nil && cached.etag != "" {
		req.Header.Set("If-None-Match", cached.etag)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("contents GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	c.updateRateLimitFromHeaders(resp)

	if resp.StatusCode == http.StatusNotModified {
		return nil, nil
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if err := c.checkStatus(resp, http.StatusOK); err != nil {
		return nil, err
	}

	var cr contentsResp
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return nil, fmt.Errorf("contents GET decode %s: %w", path, err)
	}
	if cr.Type != "file" {
		return nil, fmt.Errorf("contents GET %s: expected file, got %q", path, cr.Type)
	}

	var raw []byte
	if cr.Encoding == "base64" {
		clean := strings.NewReplacer("\n", "", "\r", "").Replace(cr.Content)
		raw, err = base64.StdEncoding.DecodeString(clean)
		if err != nil {
			return nil, fmt.Errorf("contents GET base64 %s: %w", path, err)
		}
	} else {
		raw = []byte(cr.Content)
	}

	etag := resp.Header.Get("ETag")
	c.mu.Lock()
	c.shaCache[path] = cr.SHA
	if len(raw) > 0 {
		entry := &contentsReadCache{etag: etag, sha: cr.SHA}
		if batch, err := DecodeBatchBytes(raw); err == nil {
			entry.batch = batch
		}
		c.readCache[path] = entry
	}
	c.mu.Unlock()
	return &contentsFile{sha: cr.SHA, bytes: raw, etag: etag}, nil
}

// putFile creates or updates a file. prevSha must be "" for create or the
// last-seen blob sha for update. On 422 (sha conflict) we re-fetch and
// retry once.
func (c *GitHubContentsClient) putFile(ctx context.Context, path string, content []byte, prevSha, message string) error {
	apiPath := "/repos/" + c.owner + "/" + c.repo + "/contents/" + escapePath(path)
	body := map[string]interface{}{
		"message": message,
		"content": base64.StdEncoding.EncodeToString(content),
	}
	if prevSha != "" {
		body["sha"] = prevSha
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("contents PUT marshal %s: %w", path, err)
	}
	req, err := c.newRequest(ctx, http.MethodPut, apiPath, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("contents PUT %s: %w", path, err)
	}
	defer resp.Body.Close()
	c.updateRateLimitFromHeaders(resp)

	if resp.StatusCode == http.StatusUnprocessableEntity {
		// Either a stale prevSha or no prevSha for an already-existing file
		// (the server discovers channels via ListChannels and never reads
		// server.json before its first write, so its shaCache is empty even
		// though the file was created by the client's EnsureChannel). Refresh
		// the sha and retry once.
		if _, rerr := c.getFile(ctx, path); rerr != nil && !errors.Is(rerr, ErrNotFound) {
			return fmt.Errorf("contents PUT %s: refresh sha: %w", path, rerr)
		}
		fresh := c.getCachedSha(path)
		if fresh == "" || fresh == prevSha {
			return fmt.Errorf("contents PUT %s: 422 (prev_sha=%q fresh=%q)", path, prevSha, fresh)
		}
		return c.putFile(ctx, path, content, fresh, message)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		// Use 200 as the "expected" sentinel; checkStatus handles all error codes.
		if err := c.checkStatus(resp, http.StatusOK); err != nil {
			return fmt.Errorf("contents PUT %s: %w", path, err)
		}
	}

	var cr contentsCommitResp
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return fmt.Errorf("contents PUT decode %s: %w", path, err)
	}
	c.mu.Lock()
	if cr.Content.SHA != "" {
		c.shaCache[path] = cr.Content.SHA
	}
	delete(c.readCache, path) // next read re-fetches fresh
	c.mu.Unlock()
	return nil
}

func (c *GitHubContentsClient) deleteFile(ctx context.Context, path, sha, message string) error {
	apiPath := "/repos/" + c.owner + "/" + c.repo + "/contents/" + escapePath(path)
	body := map[string]interface{}{
		"message": message,
		"sha":     sha,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("contents DELETE marshal %s: %w", path, err)
	}
	req, err := c.newRequest(ctx, http.MethodDelete, apiPath, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("contents DELETE %s: %w", path, err)
	}
	defer resp.Body.Close()
	c.updateRateLimitFromHeaders(resp)
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return c.checkStatus(resp, http.StatusOK)
}

func (c *GitHubContentsClient) getCachedSha(path string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.shaCache[path]
}

func (c *GitHubContentsClient) invalidateChannelCache(channelID string) {
	prefix := channelsDir + "/" + channelID + "/"
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.shaCache {
		if strings.HasPrefix(k, prefix) {
			delete(c.shaCache, k)
		}
	}
	for k := range c.readCache {
		if strings.HasPrefix(k, prefix) {
			delete(c.readCache, k)
		}
	}
}

func (c *GitHubContentsClient) updateRateLimitFromHeaders(resp *http.Response) {
	info := parseRateLimitHeaders(resp)
	if info.Limit <= 0 {
		return
	}
	if info.Resource != "" && info.Resource != "core" {
		return
	}
	c.rateLimit.Limit = info.Limit
	c.rateLimit.Remaining = info.Remaining
	if !info.ResetAt.IsZero() {
		c.rateLimit.ResetAt = info.ResetAt
	}
	if !info.RetryAfter.IsZero() {
		c.rateLimit.RetryAfter = info.RetryAfter
	}
	c.rateLimit.LastUpdated = info.LastUpdated
}

func (c *GitHubContentsClient) checkStatus(resp *http.Response, expected int) error {
	if resp.StatusCode == expected {
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var errResp githubErrorResp
	_ = json.Unmarshal(raw, &errResp)

	switch resp.StatusCode {
	case http.StatusForbidden:
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return ErrRateLimit
		}
		if isSecondaryRateLimitMsg(errResp.Message) {
			return ErrSecondaryRateLimit
		}
		return fmt.Errorf("%w: %s", ErrForbidden, errResp.Message)
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusUnprocessableEntity:
		return fmt.Errorf("github validation failed (422): %s", errResp.Message)
	case http.StatusTooManyRequests:
		if resp.Header.Get("Retry-After") != "" || isSecondaryRateLimitMsg(errResp.Message) {
			return ErrSecondaryRateLimit
		}
		return ErrRateLimit
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable:
		return fmt.Errorf("github server error (%d): %s", resp.StatusCode, errResp.Message)
	default:
		return fmt.Errorf("unexpected HTTP %d: %s", resp.StatusCode, errResp.Message)
	}
}

// escapePath URL-escapes each path segment but keeps slashes literal so the
// path is `channels/{id}/{file}`, not `channels%2F...`.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

var _ Transport = (*GitHubContentsClient)(nil)
