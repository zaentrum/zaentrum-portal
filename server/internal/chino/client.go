// Package chino is the calls portal-api makes to chino-api when an admin
// deletes a person on the People page: delete the data chino keeps of them —
// their watch history, progress, lists and likes — before their account
// goes, and, where their account then stays, have that deletion forgotten,
// so that they write again at once.
//
// Each call carries the admin's own bearer, which chino-api checks for the
// admin role, and the account deletion token (DeletionHeader), which says it
// comes from portal-api's delete and from no client: a person's data goes
// only with their account.
package chino

import (
	"context"
	"encoding/json"
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

// callTimeout bounds a call: deleting a person's rows takes moments.
const callTimeout = 15 * time.Second

// maxAnswer bounds what is read of an answer.
const maxAnswer = 1 << 16

// ErrUnreachable: chino-api did not answer.
var ErrUnreachable = errors.New("chino-api did not answer")

// ErrNotKept: chino-api keeps no id of the deletion named (409
// deletion_not_kept): it was forgotten already or is past its hour, or the id
// kept is another deletion's, which stays. There is nothing of its own to
// undo.
var ErrNotKept = errors.New("chino-api keeps no id of this deletion")

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
// authorization is, and answers the deletion's value, which ForgetDeletion
// takes. An account chino holds nothing of is no error. The value is "" where
// the answer names none — no body, no JSON, or no deletion in it, as a
// chino-api from before it named them answers: then there is nothing to
// forget.
func (c *Client) DeleteData(ctx context.Context, authorization, id string) (string, error) {
	status, raw, err := c.delete(ctx, authorization, "/api/v1/admin/accounts/"+url.PathEscape(id)+"/data")
	if err != nil {
		return "", err
	}
	if status/100 != 2 {
		return "", &Refused{Status: status}
	}
	var answer struct {
		Deletion string `json:"deletion"`
	}
	_ = json.Unmarshal(raw, &answer)
	return answer.Deletion, nil
}

// ForgetDeletion has chino-api forget the deletion named — DELETE
// /api/v1/admin/accounts/{sub}/deleted?deletion= — for an account whose data
// it deleted and which stays: the person writes again at once, and the data
// deleted stays deleted. ErrNotKept where chino-api keeps no id of that
// deletion.
func (c *Client) ForgetDeletion(ctx context.Context, authorization, id, deletion string) error {
	path := "/api/v1/admin/accounts/" + url.PathEscape(id) + "/deleted?" + url.Values{"deletion": {deletion}}.Encode()
	status, raw, err := c.delete(ctx, authorization, path)
	if err != nil {
		return err
	}
	var answer struct {
		Error string `json:"error"`
	}
	switch {
	case status == http.StatusConflict && json.Unmarshal(raw, &answer) == nil && answer.Error == "deletion_not_kept":
		return ErrNotKept
	case status/100 != 2:
		return &Refused{Status: status}
	}
	return nil
}

// delete sends DELETE path with the admin's authorization and the deletion
// token, and answers the status and the start of the body.
func (c *Client) delete(ctx context.Context, authorization, path string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.base+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set(DeletionHeader, c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	return resp.StatusCode, raw, nil
}
