package instagram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/arvlas/instalker/internal/domain"
)

// Posts come from the GraphQL query Instagram's own profile page runs. In
// September 2026 Instagram stopped serving /api/v1/feed/user/ to web sessions
// and started redirecting it to the homepage.
const (
	graphqlPath = "/graphql/query"
	postsQuery  = "PolarisProfilePostsTabContentQuery_connection"
	postsField  = "xdt_api__v1__feed__user_timeline_graphql_connection"
	// postsDocID names the persisted query. Instagram rotates these, so when it
	// stops working the current id is read out of the page's scripts.
	postsDocID = "29240983615539641"

	// tokensTTL bounds how long the page tokens are reused before refetching.
	tokensTTL = time.Hour
	// scriptPrefix marks the script bundles linked from a page.
	scriptPrefix = `href="https://static.cdninstagram.com/rsrc.php/`
)

// docIDPattern finds the persisted query id exported right after its name.
var docIDPattern = regexp.MustCompile(`exports\s*=\s*["']?(\d{10,20})`)

// pageTokens are the per-session values a GraphQL call must echo back, read
// from any logged-in page.
type pageTokens struct {
	lsd     string
	dtsg    string
	scripts []string
	fetched time.Time
	// docID replaces postsDocID once a stale id has been re-read from scripts.
	docID string
}

// Posts returns the most recent posts of the given user, newest first.
func (c *Client) Posts(ctx context.Context, owner domain.User) ([]domain.Media, error) {
	tokens, err := c.pageTokens(ctx, owner.Username)
	if err != nil {
		return nil, fmt.Errorf("posts %s: %w", owner.Username, err)
	}

	docID := tokens.docID
	if docID == "" {
		docID = postsDocID
	}

	items, err := c.queryPosts(ctx, owner.Username, tokens, docID)
	if errors.Is(err, domain.ErrBadResponse) {
		// A rotated doc_id or expired page tokens both look like a malformed
		// answer. Refresh both once before giving up.
		fresh, refreshErr := c.refreshTokens(ctx, owner.Username, true)
		if refreshErr != nil {
			return nil, fmt.Errorf("posts %s: %w", owner.Username, refreshErr)
		}
		items, err = c.queryPosts(ctx, owner.Username, fresh, firstNonEmpty(fresh.docID, postsDocID))
	}
	if err != nil {
		return nil, fmt.Errorf("posts %s: %w", owner.Username, err)
	}

	media := make([]domain.Media, 0, len(items))
	for _, it := range items {
		media = append(media, it.toMedia(domain.KindPost, owner))
	}

	return media, nil
}

func (c *Client) queryPosts(ctx context.Context, username string, tokens pageTokens, docID string) ([]item, error) {
	variables, err := json.Marshal(map[string]any{
		"after":  nil,
		"before": nil,
		"data": map[string]any{
			"count":                             postsPageSize,
			"include_reel_media_seen_timestamp": true,
			"include_relationship_info":         true,
			"latest_besties_reel_media":         true,
			"latest_reel_media":                 true,
		},
		"first":                  postsPageSize,
		"include_multi_captions": true,
		"last":                   nil,
		"username":               username,
		"__relay_internal__pv__PolarisMultiCaptionCarouselEnabledrelayprovider":  true,
		"__relay_internal__pv__PolarisShortDramaEnabledrelayprovider":            false,
		"__relay_internal__pv__PolarisReelsRecoDebugOverlayEnabledrelayprovider": false,
	})
	if err != nil {
		return nil, fmt.Errorf("encode variables: %w", err)
	}

	form := url.Values{}
	form.Set("__d", "www")
	form.Set("__user", "0")
	form.Set("__a", "1")
	form.Set("__req", "1")
	form.Set("__comet_req", "7")
	form.Set("fb_dtsg", tokens.dtsg)
	form.Set("lsd", tokens.lsd)
	form.Set("jazoest", "26461")
	form.Set("fb_api_caller_class", "RelayModern")
	form.Set("fb_api_req_friendly_name", postsQuery)
	form.Set("server_timestamps", "true")
	form.Set("variables", string(variables))
	form.Set("doc_id", docID)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+graphqlPath, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	req.Header.Set("x-fb-friendly-name", postsQuery)
	req.Header.Set("x-root-field-name", postsField)
	req.Header.Set("x-fb-lsd", tokens.lsd)
	req.Header.Set("x-ig-max-touch-points", "0")
	req.Header.Set("origin", baseURL)
	req.Header.Set("referer", baseURL+"/"+url.PathEscape(username)+"/")

	body, err := c.send(req)
	if err != nil {
		return nil, err
	}

	var parsed struct {
		Data map[string]struct {
			Edges []struct {
				Node item `json:"node"`
			} `json:"edges"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	err = json.Unmarshal(body, &parsed)
	if err != nil {
		return nil, fmt.Errorf("%w: decode %s: %v", domain.ErrBadResponse, postsQuery, err)
	}

	timeline, ok := parsed.Data[postsField]
	if !ok {
		return nil, fmt.Errorf("%w: %s returned no %s: %s", domain.ErrBadResponse, postsQuery, postsField, truncate(string(body), 200))
	}

	items := make([]item, 0, len(timeline.Edges))
	for _, edge := range timeline.Edges {
		items = append(items, edge.Node)
	}

	return items, nil
}

// pageTokens returns cached page tokens, fetching them when they are missing or
// older than tokensTTL.
func (c *Client) pageTokens(ctx context.Context, username string) (pageTokens, error) {
	c.mu.RLock()
	tokens := c.tokens
	c.mu.RUnlock()

	if tokens.dtsg != "" && time.Since(tokens.fetched) < tokensTTL {
		return tokens, nil
	}

	return c.refreshTokens(ctx, username, false)
}

// refreshTokens reads fresh tokens from the user's profile page. With
// rereadDocID it also re-reads the posts doc_id from the page's scripts, which
// costs a download per bundle and so only happens after a query failed.
func (c *Client) refreshTokens(ctx context.Context, username string, rereadDocID bool) (pageTokens, error) {
	page, err := c.page(ctx, "/"+url.PathEscape(username)+"/")
	if err != nil {
		return pageTokens{}, err
	}

	tokens, err := parseTokens(page)
	if err != nil {
		return pageTokens{}, err
	}

	c.mu.RLock()
	tokens.docID = c.tokens.docID
	c.mu.RUnlock()

	if rereadDocID {
		tokens.docID = c.extractDocID(ctx, tokens.scripts)
	}

	c.mu.Lock()
	c.tokens = tokens
	c.mu.Unlock()

	return tokens, nil
}

// parseTokens pulls lsd and fb_dtsg out of a page. A page without fb_dtsg was
// served to a logged-out visitor.
func parseTokens(page string) (pageTokens, error) {
	tokens := pageTokens{fetched: time.Now()}

	var eqmc struct {
		L string `json:"l"`
		F string `json:"f"`
	}
	if start := strings.Index(page, ` id="__eqmc"`); start >= 0 {
		rest := page[start:]
		open := strings.IndexByte(rest, '>')
		end := strings.Index(rest, "</script>")
		if open >= 0 && end > open {
			_ = json.Unmarshal([]byte(rest[open+1:end]), &eqmc)
		}
	}

	tokens.lsd = firstNonEmpty(eqmc.L, between(page, `"lsd":"`, `"`), between(page, `"LSD",[],{"token":"`, `"`))
	tokens.dtsg = firstNonEmpty(eqmc.F, between(page, `"dtsg":{"token":"`, `"`))
	if tokens.dtsg == "" {
		return pageTokens{}, fmt.Errorf("%w: page carries no fb_dtsg, so it was served logged out", domain.ErrUnauthorized)
	}
	if tokens.lsd == "" {
		return pageTokens{}, fmt.Errorf("%w: page carries no lsd token", domain.ErrBadResponse)
	}

	seen := map[string]bool{}
	for rest := page; ; {
		start := strings.Index(rest, scriptPrefix)
		if start < 0 {
			break
		}
		rest = rest[start+len(scriptPrefix):]
		end := strings.IndexByte(rest, '"')
		if end < 0 {
			break
		}
		path := rest[:end]
		if strings.HasSuffix(path, ".js") && !seen[path] {
			seen[path] = true
			tokens.scripts = append(tokens.scripts, "https://static.cdninstagram.com/rsrc.php/"+path)
		}
	}

	return tokens, nil
}

// extractDocID finds the current posts doc_id in the page's scripts, returning
// postsDocID when none of them names it.
func (c *Client) extractDocID(ctx context.Context, scripts []string) string {
	needle := postsQuery + "_instagramRelayOperation"

	for _, script := range scripts {
		body, err := c.fetchScript(ctx, script)
		if err != nil {
			continue
		}

		pos := strings.Index(body, needle)
		if pos < 0 {
			continue
		}

		window := body[pos:min(len(body), pos+1000)]
		if match := docIDPattern.FindStringSubmatch(window); match != nil {
			return match[1]
		}
	}

	return postsDocID
}

// fetchScript downloads a static script bundle. These come from a CDN and need
// no session.
func (c *Client) fetchScript(ctx context.Context, address string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("user-agent", userAgent)

	httpClient, _, _ := c.snapshot()
	res, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, maxBodySize))
	if err != nil {
		return "", err
	}

	return string(body), nil
}

// page fetches an HTML page the way a browser navigation does.
func (c *Client) page(ctx context.Context, path string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("user-agent", userAgent)
	req.Header.Set("accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("accept-language", "en-US,en;q=0.9")
	req.Header.Set("sec-fetch-dest", "document")
	req.Header.Set("sec-fetch-mode", "navigate")
	req.Header.Set("sec-fetch-site", "none")
	req.Header.Set("sec-fetch-user", "?1")

	httpClient, _, _ := c.snapshot()
	hadSession := cookieFrom(httpClient, sessionCookie) != ""

	res, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("do request: %w", err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(io.LimitReader(res.Body, maxBodySize))
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}

	if hadSession && cookieFrom(httpClient, sessionCookie) == "" {
		return "", fmt.Errorf("%w: instagram expired the session cookie", domain.ErrUnauthorized)
	}

	switch res.StatusCode {
	case http.StatusOK:
		return string(body), nil
	case http.StatusNotFound:
		return "", domain.ErrNotFound
	case http.StatusTooManyRequests:
		return "", fmt.Errorf("%w: status 429 for %s", domain.ErrRateLimited, path)
	default:
		return "", fmt.Errorf("%w: status %d for %s", domain.ErrBadResponse, res.StatusCode, path)
	}
}

func between(s, open, close string) string {
	start := strings.Index(s, open)
	if start < 0 {
		return ""
	}
	rest := s[start+len(open):]
	end := strings.Index(rest, close)
	if end < 0 {
		return ""
	}

	return rest[:end]
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}

	return ""
}
