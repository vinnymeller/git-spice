package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// restAPIVersion pins the REST API behavior this package decodes.
// GitHub changes REST response shapes only across versions.
const restAPIVersion = "2022-11-28"

// restRequest performs one authenticated request against GitHub's REST API.
//
// method is an HTTP method, and path is joined onto the REST API base URL.
// path must not carry a query string: [url.JoinPath] escapes it into the path
// and GitHub answers the result with 404.
// Pass query parameters as query instead.
// A non-nil body is encoded as the JSON request body.
// A non-nil result receives the decoded JSON response;
// it is left untouched for responses that carry no content.
//
// restRequest retrieves credentials for every call so dynamic token sources
// retain their request lifetime.
// It owns response-body closure, and reports any non-2xx response as an
// [*HTTPError] so callers can classify the failure without inspecting
// GitHub's message text.
//
// It reports the response status so callers can distinguish successful
// responses that GitHub differentiates only by status,
// such as a stack that was dissolved rather than reduced.
// The status is zero when the request did not reach a response.
func (c *Gateway) restRequest(
	ctx context.Context,
	method, path string,
	query url.Values,
	body, result any,
) (int, error) {
	// A query string inside path would be escaped into the path by
	// [url.JoinPath], and GitHub answers the result with 404 — which reads as
	// a missing resource rather than a malformed request. Fail loudly instead.
	if strings.Contains(path, "?") {
		return 0, fmt.Errorf("REST path %q must not contain a query string", path)
	}

	var payload io.Reader
	if body != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return 0, fmt.Errorf("encode REST request: %w", err)
		}
		payload = &buf
	}

	endpoint, err := url.JoinPath(c.apiURL, path)
	if err != nil {
		return 0, fmt.Errorf("build REST API URL for %q: %w", path, err)
	}
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	token, err := c.tokens.Token(ctx)
	if err != nil {
		return 0, fmt.Errorf("get GitHub token: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, payload)
	if err != nil {
		return 0, fmt.Errorf("build REST request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", restAPIVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("send REST request: %w", err)
	}

	if res.StatusCode < http.StatusOK || res.StatusCode >= http.StatusMultipleChoices {
		return res.StatusCode, newHTTPError(res)
	}

	// A successful response carries no body when GitHub reports 204,
	// which is how dissolving a stack succeeds,
	// and the caller may not want the body at all.
	// Drain within the diagnostic bound so the connection stays reusable
	// without letting an intermediary dictate the allocation.
	if res.StatusCode == http.StatusNoContent || result == nil {
		_, err := io.Copy(io.Discard, io.LimitReader(res.Body, maxErrorBody))
		return res.StatusCode, errors.Join(err, res.Body.Close())
	}

	responseBody, err := io.ReadAll(res.Body)
	err = errors.Join(err, res.Body.Close())
	if err != nil {
		return res.StatusCode, fmt.Errorf("read REST response: %w", err)
	}

	if err := json.Unmarshal(responseBody, result); err != nil {
		return res.StatusCode, fmt.Errorf("decode REST response: %w", err)
	}
	return res.StatusCode, nil
}

// HTTPError is a non-2xx response from GitHub's REST API.
//
// Classify it with [errors.Is] against this package's sentinel errors
// rather than by matching Message,
// which GitHub phrases inconsistently across releases.
type HTTPError struct {
	// StatusCode is the HTTP status GitHub reported.
	StatusCode int

	// Message is GitHub's human-readable description of the failure.
	// It is empty when the response carried no decodable message,
	// and is suitable for logging but not for control flow.
	Message string
}

// newHTTPError builds an [HTTPError] from a failed response,
// consuming and closing its body.
//
// Diagnostic content is bounded before closing the body so an intermediary
// cannot make error reporting consume unbounded memory.
func newHTTPError(res *http.Response) *HTTPError {
	httpErr := &HTTPError{StatusCode: res.StatusCode}

	body, err := io.ReadAll(io.LimitReader(res.Body, maxErrorBody))
	if err = errors.Join(err, res.Body.Close()); err != nil {
		return httpErr
	}

	// GitHub reports REST failures as {"message": ...}, but error responses
	// from a proxy or gateway may not be JSON at all.
	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.Message != "" {
		httpErr.Message = payload.Message
	} else {
		httpErr.Message = strings.TrimSpace(string(body))
	}
	return httpErr
}

// Is reports whether GitHub's status matches a package sentinel.
func (e *HTTPError) Is(target error) bool {
	switch target {
	case ErrNotFound:
		return e.StatusCode == http.StatusNotFound
	case ErrForbidden:
		return e.StatusCode == http.StatusForbidden
	case ErrUnprocessable:
		return e.StatusCode == http.StatusUnprocessableEntity
	default:
		return false
	}
}

// Error reports GitHub's status and message.
func (e *HTTPError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("GitHub REST HTTP status %d", e.StatusCode)
	}
	return fmt.Sprintf("GitHub REST HTTP status %d: %s", e.StatusCode, e.Message)
}
