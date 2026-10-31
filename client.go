package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/net/proxy"
)

// ErrAPIRejected is returned when the API persistently refuses our requests
// (HTTP 400 with empty body, 401/403, or unreachable). The crawler stops and
// warns loudly so a human can investigate.
var ErrAPIRejected = errors.New("API rejected our requests")

// ErrRateLimited is returned on HTTP 429 so the crawler can slow down.
var ErrRateLimited = errors.New("rate limited (429)")

type Client struct {
	hc      *http.Client
	direct  *http.Client
	url     string
	headers map[string]string
	log     *Logger
	// retryBase/rateLimitBase control the retry backoff (overridable in tests)
	retryBase      time.Duration
	rateLimitBase  time.Duration

	proxyMu          sync.Mutex
	proxyBypassUntil time.Time
}

// buildTransport creates the HTTP transport, optionally routing through the
// configured proxy (socks5/socks5h/http/https). Proxies keep the crawler
// working when Cloudflare blocks the machine's direct IP.
func buildTransport(cfg Config) (*http.Transport, error) {
	tr := &http.Transport{
		MaxIdleConns:        4,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     60 * time.Second,
	}
	if cfg.ProxyURL == "" {
		return tr, nil
	}
	u, err := url.Parse(cfg.ProxyURL)
	if err != nil {
		return nil, fmt.Errorf("parsing proxy_url: %w", err)
	}
	if cfg.ProxyUser != "" {
		u.User = url.UserPassword(cfg.ProxyUser, cfg.ProxyPass)
	}
	dialer, err := proxy.FromURL(u, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("proxy dialer: %w", err)
	}
	tr.DialContext = dialer.(proxy.ContextDialer).DialContext
	return tr, nil
}

func NewClient(cfg Config, log *Logger) *Client {
	headers := map[string]string{
		"Content-Type":     "application/json; charset=UTF-8",
		"Accept":           "*/*",
		"App-Version":      cfg.AppVersion,
		"Req-App":          "iphone",
		"Type":             "iphone",
		"Accept-Language":  cfg.Language,
		"Req-Device-Token": cfg.DeviceToken,
		"User-Agent":       "AQAR-Saudi/" + cfg.AppVersion + " (iPhone; iOS 17.1.2; Scale/3.00)",
	}
	// Req-Time/Req-Hash are optional: the API does not validate them (verified
	// empirically â€” plain requests work fine). They are sent only if a pair is
	// configured, e.g. if the API ever starts enforcing them.
	if cfg.ReqTime != "" && cfg.ReqHash != "" {
		headers["Req-Time"] = cfg.ReqTime
		headers["Req-Hash"] = cfg.ReqHash
	}
	tr, err := buildTransport(cfg)
	if err != nil && log != nil {
		log.Warnf("%v (continuing without proxy)", err)
	}
	if tr == nil {
		tr = &http.Transport{}
	}
	var directHC *http.Client
	if cfg.ProxyURL != "" && cfg.ProxyFallbackDirect {
		directHC = &http.Client{
			Timeout: 90 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        4,
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     60 * time.Second,
			},
		}
	}
	return &Client{
		hc: &http.Client{
			Timeout:   90 * time.Second,
			Transport: tr,
		},
		direct:        directHC,
		url:           cfg.APIURL,
		headers:       headers,
		log:           log,
		retryBase:     15 * time.Second,
		rateLimitBase: 60 * time.Second,
	}
}

func (c *Client) doHTTP(ctx context.Context, body []byte) (*http.Response, error) {
	buildReq := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("building request: %w", err)
		}
		for k, v := range c.headers {
			req.Header.Set(k, v)
		}
		return req, nil
	}

	if c.direct != nil {
		c.proxyMu.Lock()
		bypass := time.Now().Before(c.proxyBypassUntil)
		c.proxyMu.Unlock()
		if bypass {
			req, err := buildReq()
			if err != nil {
				return nil, err
			}
			return c.direct.Do(req)
		}
	}

	req, err := buildReq()
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil && ctx.Err() == nil && c.direct != nil {
		c.proxyMu.Lock()
		c.proxyBypassUntil = time.Now().Add(15 * time.Minute)
		c.proxyMu.Unlock()
		if c.log != nil {
			c.log.Warnf("proxy request failed (%v); falling back to direct connection for 15m", err)
		}
		req2, rerr := buildReq()
		if rerr == nil {
			if resp2, derr := c.direct.Do(req2); derr == nil {
				return resp2, nil
			}
		}
	}
	return resp, err
}

// graphqlRequest posts a query and decodes the response envelope.
// On HTTP 400 with empty body it returns ErrAPIRejected; on 429 ErrRateLimited.
func (c *Client) graphqlRequest(ctx context.Context, query string) (map[string]any, error) {
	body, err := json.Marshal(map[string]string{"query": query})
	if err != nil {
		return nil, fmt.Errorf("encoding query: %w", err)
	}

	resp, err := c.doHTTP(ctx, body)
	if err != nil {
		return nil, fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// ok
	case http.StatusTooManyRequests:
		return nil, ErrRateLimited
	case http.StatusBadRequest:
		// Distinguish "bad query syntax" (has a body) from a hard refusal (empty body).
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		if len(bytes.TrimSpace(b)) == 0 {
			return nil, ErrAPIRejected
		}
		return nil, fmt.Errorf("api 400 with body: %s", truncate(string(b), 300))
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, ErrAPIRejected
	default:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, truncate(string(b), 300))
	}

	var env struct {
		Data   map[string]any `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&env); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	if len(env.Errors) > 0 {
		return nil, fmt.Errorf("graphql errors: %s", env.Errors[0].Message)
	}
	return env.Data, nil
}

// doWithRetry runs fn with retries and backoff. ErrAPIRejected and
// context-cancellation are returned immediately; 429 gets extra backoff.
func (c *Client) doWithRetry(ctx context.Context, fn func() (any, error)) (any, error) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		if attempt > 1 {
			delay := time.Duration(attempt) * c.retryBase
			if errors.Is(lastErr, ErrRateLimited) {
				delay = time.Duration(attempt) * c.rateLimitBase
			}
			c.log.Warnf("request attempt %d failed (%v); backing off %v", attempt, lastErr, delay)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		v, err := fn()
		if err == nil {
			return v, nil
		}
		lastErr = err
		if errors.Is(err, ErrAPIRejected) || errors.Is(ctx.Err(), context.Canceled) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("giving up after retries: %w", lastErr)
}

// CheckAPI runs a minimal count query to verify the API is reachable and
// answers. Returns ErrAPIRejected when it refuses our requests.
func (c *Client) CheckAPI(ctx context.Context) error {
	query := fmt.Sprintf(countQuery, 1)
	_, err := c.doWithRetry(ctx, func() (any, error) {
		return c.graphqlRequest(ctx, query)
	})
	if err != nil {
		if errors.Is(err, ErrAPIRejected) {
			return ErrAPIRejected
		}
		return fmt.Errorf("api check failed: %w", err)
	}
	return nil
}

// Count returns the number of published listings for one category nationwide.
func (c *Client) Count(ctx context.Context, catID int) (int, error) {
	query := fmt.Sprintf(countQuery, catID)
	v, err := c.doWithRetry(ctx, func() (any, error) {
		return c.graphqlRequest(ctx, query)
	})
	if err != nil {
		return 0, err
	}
	// graphqlRequest already unwraps the "data" envelope: v = {"Search": {...}}.
	main, ok := jsonAny(v, "Search", "main")
	if !ok {
		return 0, errors.New("count response missing Search.main")
	}
	return int(toFloat(main)), nil
}

type searchPage struct {
	Total    int              `json:"total"`
	Featured []map[string]any `json:"featured"`
	Listings []map[string]any `json:"listings"`
}

// FindPage fetches one page of listings for a category.
func (c *Client) FindPage(ctx context.Context, catID, from, size int) (searchPage, error) {
	var page searchPage
	query := fmt.Sprintf(findQuery, catID, from, size)
	v, err := c.doWithRetry(ctx, func() (any, error) {
		return c.graphqlRequest(ctx, query)
	})
	if err != nil {
		return page, err
	}
	// graphqlRequest already unwraps the "data" envelope: v = {"Search": {...}}.
	main, ok := jsonAny(v, "Search", "main")
	if !ok {
		return page, errors.New("find response missing Search.main")
	}
	m, ok := main.(map[string]any)
	if !ok {
		return page, errors.New("Search.main is not an object")
	}
	if n, ok := m["total"].(float64); ok {
		page.Total = int(n)
	}
	if f, ok := m["featured"].([]any); ok {
		page.Featured = toMapSlice(f)
	}
	if l, ok := m["listings"].([]any); ok {
		page.Listings = toMapSlice(l)
	}
	return page, nil
}

// WebClient talks to the aqar.fm WEBSITE GraphQL API (sa.aqar.fm), which
// returns canonical uri/path per listing - used to enrich the archive with
// public aqar.fm URLs. Verified working with app-version 1.0.0.
type WebClient struct {
	hc               *http.Client
	direct           *http.Client
	url              string
	headers          map[string]string
	log              *Logger
	proxyMu          sync.Mutex
	proxyBypassUntil time.Time
}

func NewWebClient(cfg Config, log *Logger) *WebClient {
	tr, err := buildTransport(cfg)
	if err != nil && log != nil {
		log.Warnf("%v (continuing without proxy)", err)
	}
	if tr == nil {
		tr = &http.Transport{}
	}
	var directHC *http.Client
	if cfg.ProxyURL != "" && cfg.ProxyFallbackDirect {
		directHC = &http.Client{
			Timeout: 60 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        4,
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     60 * time.Second,
			},
		}
	}
	return &WebClient{
		hc:     &http.Client{Timeout: 60 * time.Second, Transport: tr},
		direct: directHC,
		url:    "https://sa.aqar.fm/graphql",
		headers: map[string]string{
			"Content-Type": "application/json",
			"Accept":       "*/*",
			"app-version":  "1.0.0",
			"req-app":      "web",
			"User-Agent":   "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko)",
		},
		log: log,
	}
}

const webFindQuery = `query findListings($size: Int, $from: Int, $sort: SortInput, $where: WhereInput, $polygon: [LocationInput!], $daily_renting_filter: DailyRentingFilter) {
  Web {
    find(size: $size, from: $from, sort: $sort, where: $where, polygon: $polygon, daily_renting_filter: $daily_renting_filter) {
      listings { id uri path } total __typename
    }
    __typename
  }
}`

// FetchListingPaths returns id -> website path for published listings of a
// category. The web API caps at 70 listings per page regardless of size, so
// pagination follows the response's total. Enrichment is best-effort: at most
// maxWebPages requests per category to stay gentle on the website.
func (w *WebClient) FetchListingPaths(ctx context.Context, category int) (map[int64]string, error) {
	paths := make(map[int64]string)
	const maxWebPages = 4
	from := 0
	for page := 1; ; page++ {
		body, err := json.Marshal(map[string]any{
			"operationName": "findListings",
			"variables": map[string]any{
				"size": 500, "from": from,
				"sort": map[string]string{"create_time": "desc", "has_img": "desc"},
				"where": map[string]any{
					"category": map[string]any{"eq": category},
				},
			},
			"query": webFindQuery,
		})
		if err != nil {
			return nil, fmt.Errorf("encode web query: %w", err)
		}
		buildReq := func() (*http.Request, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
			if err != nil {
				return nil, fmt.Errorf("build web request: %w", err)
			}
			for k, v := range w.headers {
				req.Header.Set(k, v)
			}
			return req, nil
		}
		var resp *http.Response
		if w.direct != nil {
			w.proxyMu.Lock()
			bypass := time.Now().Before(w.proxyBypassUntil)
			w.proxyMu.Unlock()
			if bypass {
				req, err := buildReq()
				if err != nil {
					return nil, err
				}
				resp, err = w.direct.Do(req)
				if err != nil {
					return nil, fmt.Errorf("web request: %w", err)
				}
			}
		}
		if resp == nil {
			req, err := buildReq()
			if err != nil {
				return nil, err
			}
			resp, err = w.hc.Do(req)
			if err != nil && ctx.Err() == nil && w.direct != nil {
				w.proxyMu.Lock()
				w.proxyBypassUntil = time.Now().Add(15 * time.Minute)
				w.proxyMu.Unlock()
				if req2, rerr := buildReq(); rerr == nil {
					if resp2, derr := w.direct.Do(req2); derr == nil {
						resp = resp2
						err = nil
					}
				}
			}
			if err != nil {
				return nil, fmt.Errorf("web request: %w", err)
			}
		}
		var env struct {
			Data struct {
				Web struct {
					Find struct {
						Listings []struct {
							ID   float64 `json:"id"`
							Path string  `json:"path"`
						} `json:"listings"`
						Total int `json:"total"`
					} `json:"find"`
				} `json:"Web"`
			} `json:"data"`
			Errors []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		decErr := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&env)
		resp.Body.Close()
		if decErr != nil {
			return nil, fmt.Errorf("decode web response: %w", decErr)
		}
		if len(env.Errors) > 0 {
			return nil, fmt.Errorf("web api error: %s", env.Errors[0].Message)
		}
		for _, l := range env.Data.Web.Find.Listings {
			if l.Path != "" {
				paths[int64(l.ID)] = l.Path
			}
		}
		got := len(env.Data.Web.Find.Listings)
		if got == 0 || env.Data.Web.Find.Total == 0 || from+got >= env.Data.Web.Find.Total {
			break
		}
		if page >= maxWebPages {
			if w.log != nil {
				w.log.Warnf("web enrichment cap: fetched %d of %d paths for cat %d (max %d pages)",
					len(paths), env.Data.Web.Find.Total, category, maxWebPages)
			}
			break
		}
		from += got
	}
	return paths, nil
}

// randomBetween returns a uniform random duration in [minS, maxS] seconds.
func randomBetween(minS, maxS float64) time.Duration {
	if maxS <= minS {
		return time.Duration(minS * float64(time.Second))
	}
	r := minS + rand.Float64()*(maxS-minS)
	return time.Duration(r * float64(time.Second))
}

func toMapSlice(in []any) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, v := range in {
		if m, ok := v.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func jsonAny(root any, path ...string) (any, bool) {
	cur := root
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// toFloat converts JSON numbers/strings/bools to float64 (used for DB columns).
func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case json.Number:
		f, _ := n.Float64()
		return f
	case int:
		return float64(n)
	case int8:
		return float64(n)
	case int16:
		return float64(n)
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	case uint:
		return float64(n)
	case uint8:
		return float64(n)
	case uint16:
		return float64(n)
	case uint32:
		return float64(n)
	case uint64:
		return float64(n)
	case string:
		var f float64
		_, _ = fmt.Sscanf(n, "%f", &f)
		return f
	case bool:
		if n {
			return 1
		}
	}
	return 0
}

// truncate truncates s to n bytes WITHOUT splitting UTF-8 runes, so the
// result is always valid UTF-8.
func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	for i := n; i > 0; i-- {
		if utf8.RuneStart(s[i]) {
			return s[:i] + "..."
		}
	}
	return s[:n] + "..."
}
