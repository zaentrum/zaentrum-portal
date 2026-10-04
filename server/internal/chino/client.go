// Package chino is the one call portal-api makes to chino-api: delete the
// data chino keeps of a person an admin deletes on the People page — their
// watch history, progress, lists and likes — before their account goes.
//
// The call carries the admin's own bearer, which chino-api checks for the
// admin role, and the account deletion token (DeletionHeader), which says it
// comes from portal-api's delete and from no client: a person's data goes
// only with their account.
package chino

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DeletionHeader carries the account deletion token, between chino-api and
// portal-api, both ways.
const DeletionHeader = "X-Account-Deletion-Token"

// callTimeout bounds the call: deleting a person's rows takes moments.
const callTimeout = 15 * time.Second

// ErrUnreachable: chino-api did not answer.
var ErrUnreachable = errors.New("chino-api did not answer")

// Refused is chino-api declining the call, with its status.
type Refused struct{ Status int }

func (e *Refused) Error() string { return fmt.Sprintf("chino-api answered %d", e.Status) }

// Client calls chino-api.
type Client struct {
	base, token string
	http        *http.Client
}

// New is a client for chino-api at base with the account deletion token;
// nil when either is missing — then nothing of chino's is deleted.
func New(base, token string) *Client {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" || token == "" {
		return nil
	}
	return &Client{base: base, token: token, http: &http.Client{
		Timeout:       callTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// DeleteData deletes what chino keeps of the account id — DELETE
// /api/v1/admin/accounts/{sub}/data — as the admin whose Authorization header
// authorization is. An account chino holds nothing of is no error.
func (c *Client) DeleteData(ctx context.Context, authorization, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.base+"/api/v1/admin/accounts/"+url.PathEscape(id)+"/data", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set(DeletionHeader, c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return &Refused{Status: resp.StatusCode}
	}
	return nil
}
