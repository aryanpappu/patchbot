package github

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// CurrentUser obtains the authenticated bot login for fail-closed comment sync.
func (c *HTTPClient) CurrentUser(ctx context.Context) (string, error) {
	target := *c.baseURL
	target.Path = strings.TrimRight(target.Path, "/") + "/user"
	target.RawPath = ""
	var user struct {
		Login string `json:"login"`
	}
	_, err := c.request(ctx, http.MethodGet, &target, nil, &user, http.StatusOK)
	if err != nil {
		return "", operationError("CurrentUser", err)
	}
	if user.Login == "" {
		return "", errors.New("CurrentUser: missing login")
	}
	return user.Login, nil
}
