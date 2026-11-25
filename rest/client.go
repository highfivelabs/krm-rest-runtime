// SPDX-License-Identifier: Apache-2.0

// Package rest is the HTTP side: a client for one vendor connection that
// sends JSON, authenticates the vendor's way, and reports 404s distinctly.
package rest

import (
	"bytes"
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
)

// Vendor is what differs between APIs, apart from the descriptors.
type Vendor struct {
	// Name, e.g. "github".
	Name string
	// DefaultBaseURL is used when a Connection names none.
	DefaultBaseURL string
	// CredentialKeys are the keys the Connection's Secret must hold.
	CredentialKeys []string
	// AltCredentialKeys are other sets of keys a Connection's Secret may
	// hold instead, for an API of the vendor that signs in otherwise
	// (Atlas's App Services Admin API takes a programmatic API key,
	// publicKey and privateKey, where the Atlas Admin API takes a service
	// account's clientId and clientSecret). The vendor's Authorize tells
	// them apart by the keys the credentials hold.
	AltCredentialKeys [][]string
	// Authorize adds the vendor's authentication to a request.
	Authorize func(req *http.Request, creds map[string]string)
	// AuthorizeE is Authorize for schemes that can fail, such as an OAuth
	// token exchange; set one or the other.
	AuthorizeE func(req *http.Request, creds map[string]string) error
	// Headers are sent on every request (API version pins and the like).
	Headers map[string]string
	// BaseURL may derive the base URL from the credentials (Datadog's site).
	BaseURL func(creds map[string]string) string
	// NotFound says whether an error response other than a 404 or 410
	// means the object does not exist (Vault answers a read of a deleted
	// PKI issuer or key with a 500 naming it). Such a response is
	// ErrNotFound.
	NotFound func(status int, body []byte) bool
	// NotFoundReq is NotFound with the request method and path, for
	// endpoints whose gone response cannot be distinguished across the
	// whole vendor (Confluent answers GET /iam/v2/api-keys/{id} on a
	// deleted key with 403 forbidden_access).
	NotFoundReq func(method, path string, status int, body []byte) bool
	// RESTPath is the end of a Connection's base URL that is the REST
	// API's own path, which a GraphQL endpoint's path takes the place of
	// (Client.GraphQL): GitLab's /api/v4 (https://gitlab.example.com/api/v4
	// and /api/graphql make https://gitlab.example.com/api/graphql),
	// GitHub Enterprise Server's /v3 (https://host/api/v3 and /graphql make
	// https://host/api/graphql). A base URL that does not end so keeps its
	// path: https://api.github.com and /graphql.
	RESTPath string
}

// MissingKeys names what a Connection's Secret (data, by key) lacks: ""
// when it holds every CredentialKeys key, or every key of one of
// AltCredentialKeys; else the first CredentialKeys key it lacks, quoted.
func (v *Vendor) MissingKeys(data map[string][]byte) string {
	has := func(keys []string) string {
		for _, k := range keys {
			if _, ok := data[k]; !ok {
				return fmt.Sprintf("%q", k)
			}
		}
		return ""
	}
	missing := has(v.CredentialKeys)
	if missing == "" {
		return ""
	}
	for _, alt := range v.AltCredentialKeys {
		if len(alt) > 0 && has(alt) == "" {
			return ""
		}
	}
	if len(v.AltCredentialKeys) > 0 {
		var sets []string
		for _, alt := range v.AltCredentialKeys {
			sets = append(sets, fmt.Sprintf("%q", alt))
		}
		missing += " (nor every key of " + strings.Join(sets, " or ") + ")"
	}
	return missing
}

// ErrNotFound is returned for a 404 (and a 410, and what Vendor.NotFound
// recognizes).
var ErrNotFound = errors.New("not found")

// APIError is any other non-2xx response.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	b := e.Body
	if len(b) > 500 {
		b = b[:500] + "…"
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, b)
}

// Client talks to one vendor account.
type Client struct {
	Vendor  *Vendor
	BaseURL string
	Creds   map[string]string
	HTTP    *http.Client
}

// New returns a client; baseURL may be empty.
func New(v *Vendor, baseURL string, creds map[string]string) *Client {
	if baseURL == "" && v.BaseURL != nil {
		baseURL = v.BaseURL(creds)
	}
	if baseURL == "" {
		baseURL = v.DefaultBaseURL
	}
	return &Client{Vendor: v, BaseURL: strings.TrimRight(baseURL, "/"), Creds: creds, HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// Do sends body (nil for none) and decodes a JSON response (nil for an empty one).
func (c *Client) Do(ctx context.Context, method, path string, body any) (any, error) {
	return c.DoWith(ctx, method, path, body, nil)
}

// MultipartBody is a request body encoded as multipart/form-data.
type MultipartBody struct {
	Fields map[string]any
	Files  []string
}

// DoWith is Do with extra headers, set after the vendor's.
func (c *Client) DoWith(ctx context.Context, method, path string, body any, headers map[string]string) (any, error) {
	out, _, err := c.DoPaged(ctx, method, path, body, headers)
	return out, err
}

// DoPaged is DoWith that also returns where the next page of a list is: the
// Link header's rel="next" (GitHub, GitLab), as a path relative to the base
// URL; "" when there is none, or when it points anywhere else.
func (c *Client) DoPaged(ctx context.Context, method, path string, body any, headers map[string]string) (any, string, error) {
	out, h, err := c.do(ctx, method, path, body, headers)
	if err != nil || h == nil {
		return out, "", err
	}
	return out, c.nextPage(h.Get("Link")), nil
}

var linkRE = regexp.MustCompile(`<([^>]+)>\s*;[^,]*\brel="?next"?`)

func (c *Client) nextPage(link string) string {
	m := linkRE.FindStringSubmatch(link)
	if m == nil {
		return ""
	}
	if next, ok := strings.CutPrefix(m[1], c.BaseURL); ok && (next == "" || next[0] == '/' || next[0] == '?') {
		return next // never another host: the credentials go only to the Connection's
	}
	return ""
}

func (c *Client) do(ctx context.Context, method, path string, body any, headers map[string]string) (any, http.Header, error) {
	return c.doURL(ctx, method, c.BaseURL+path, path, body, headers)
}

// DoRaw is DoWith for an answer that is not JSON: its bytes as they are
// (Cloudflare's Workers KV answers a key's value so).
func (c *Client) DoRaw(ctx context.Context, method, path string, headers map[string]string) ([]byte, error) {
	h := map[string]string{"Accept": "*/*"}
	for k, v := range headers {
		h[k] = v
	}
	data, _, err := c.send(ctx, method, c.BaseURL+path, path, nil, h)
	return data, err
}

// doURL is do to url; path, relative to the base URL, is what a vendor's
// NotFoundReq is told.
func (c *Client) doURL(ctx context.Context, method, rawURL, path string, body any, headers map[string]string) (any, http.Header, error) {
	data, header, err := c.send(ctx, method, rawURL, path, body, headers)
	if err != nil {
		return nil, nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, header, nil
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		if ct := header.Get("Content-Type"); ct != "" && !strings.Contains(ct, "json") && !strings.HasPrefix(ct, "text/plain") {
			return nil, header, nil
		}
		return nil, nil, fmt.Errorf("decoding %s %s response: %w", method, path, err)
	}
	return out, header, nil
}

// send makes the request and answers the response's body and headers, or
// the error its status means.
func (c *Client) send(ctx context.Context, method, rawURL, path string, body any, headers map[string]string) ([]byte, http.Header, error) {
	var rd io.Reader
	var contentType string
	if mp, ok := body.(*MultipartBody); ok {
		ct, b, err := EncodeMultipart(mp.Fields, mp.Files)
		if err != nil {
			return nil, nil, err
		}
		rd = bytes.NewReader(b)
		contentType = ct
	} else if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
		rd = bytes.NewReader(b)
		contentType = "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range c.Vendor.Headers {
		req.Header.Set(k, v)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("User-Agent", "highfivelabs-"+c.Vendor.Name+"-extensions")
	if c.Vendor.Authorize != nil {
		c.Vendor.Authorize(req, c.Creds)
	}
	if c.Vendor.AuthorizeE != nil {
		if err := c.Vendor.AuthorizeE(req, c.Creds); err != nil {
			return nil, nil, fmt.Errorf("authorizing: %w", err)
		}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		return nil, nil, ErrNotFound
	}
	if resp.StatusCode > 299 && c.Vendor.NotFound != nil && c.Vendor.NotFound(resp.StatusCode, data) {
		return nil, nil, ErrNotFound
	}
	if resp.StatusCode > 299 && c.Vendor.NotFoundReq != nil && c.Vendor.NotFoundReq(method, path, resp.StatusCode, data) {
		return nil, nil, ErrNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, nil, &APIError{Status: resp.StatusCode, Body: string(data)}
	}
	return data, resp.Header, nil
}

// WithQuery appends an operation's query parameters to a path.
func WithQuery(path string, query map[string]string) string {
	if len(query) == 0 {
		return path
	}
	q := url.Values{}
	for k, v := range query {
		q.Set(k, v)
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + q.Encode()
}

// Expand fills a path's {param}s; values are path-escaped. A {+param}
// keeps the value's slashes, escaping each segment between them (GitHub's
// branch feature/x at .../git/refs/heads/{+branch}).
func Expand(path string, values map[string]string) (string, error) {
	var missing, bad []string
	out := placeholderRE.ReplaceAllStringFunc(path, func(m string) string {
		name := m[1 : len(m)-1]
		keep := strings.HasPrefix(name, "+")
		name = strings.TrimPrefix(name, "+")
		v, ok := values[name]
		if !ok || v == "" {
			missing = append(missing, name)
			return m
		}
		if keep {
			segs := strings.Split(v, "/")
			for i, s := range segs {
				if s == "" || s == "." || s == ".." {
					// Not a name: it would point somewhere else.
					bad = append(bad, name)
					return m
				}
				segs[i] = url.PathEscape(s)
			}
			return strings.Join(segs, "/")
		}
		return url.PathEscape(v)
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("%s: no value for %s", path, strings.Join(missing, ", "))
	}
	if len(bad) > 0 {
		return "", fmt.Errorf("%s: %s has an empty, . or .. segment", path, strings.Join(bad, ", "))
	}
	return out, nil
}
