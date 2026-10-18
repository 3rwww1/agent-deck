package session

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// openCodeServiceRoutes are the 2.x service routes behind the `opencode api` operations agent-deck runs.
var openCodeServiceRoutes = map[string]struct{ method, path string }{
	"session.get":         {http.MethodGet, "/api/session/%s"},
	"session.remove":      {http.MethodDelete, "/api/session/%s"},
	"session.update":      {http.MethodPatch, "/api/session/%s"},
	"session.fork":        {http.MethodPost, "/api/session/%s/fork"},
	"session.switchAgent": {http.MethodPost, "/api/session/%s/agent"},
	"session.switchModel": {http.MethodPost, "/api/session/%s/model"},
}

// callOpenCodeService sends one request to the guard's healthy service; served is false when there is none.
func callOpenCodeService(ctx context.Context, binary, method, path, body string) (out []byte, served bool, err error) {
	svc, ok := sharedOpenCodeService.attach(binary)
	if !ok {
		return nil, false, nil
	}
	var reqBody io.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, svc.URL+path, reqBody)
	if err != nil {
		return nil, true, err
	}
	if svc.Password != "" {
		req.SetBasicAuth("opencode", svc.Password)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := openCodeSessionHTTPClient.Do(req)
	if err != nil {
		return nil, true, err
	}
	defer resp.Body.Close()
	out, err = io.ReadAll(io.LimitReader(resp.Body, openCodeSessionResponseLimit))
	if err != nil {
		return nil, true, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// The same text `opencode api` prints, so callers see one error whichever path ran.
		return nil, true, fmt.Errorf("HTTP %s", resp.Status)
	}
	return out, true, nil
}
