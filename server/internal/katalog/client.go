// Package katalog is katalog-manager's GraphQL API, as much of it as the
// setup checklist reads and writes: whether a TMDB key is in effect, how many
// titles the catalog holds, the latest scan — and setting the key, starting a
// scan.
//
// It has no identity of its own. Every call carries the bearer of the admin
// whose request it serves, so katalog-manager decides what that admin may do,
// exactly as it does for the catalog console; portal-api never reaches it as
// itself.
package katalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// TMDBSetting is the catalog setting katalog-manager reads its TMDB key from.
// A value there overrides the server's own TMDB_API_KEY.
const TMDBSetting = "tmdb.api_key"

// CountLimit is the most items of a kind katalog-manager lists in one page;
// a count that reaches it is a floor.
const CountLimit = 200

// callTimeout bounds one call: the checklist is read on the launchpad, and a
// catalog that does not answer must cost it seconds, not the page.
const callTimeout = 6 * time.Second

// ErrUnreachable: katalog-manager did not answer.
var ErrUnreachable = errors.New("katalog-manager did not answer")

// Refused is katalog-manager declining the call: the bearer is not one of its
// admins (FORBIDDEN), or not a bearer it takes (401/403).
type Refused struct{ Message string }

func (e *Refused) Error() string { return "katalog-manager refused: " + e.Message }

// Failed is any other error katalog-manager answered with.
type Failed struct{ Message string }

func (e *Failed) Error() string { return "katalog-manager: " + e.Message }

// Client calls katalog-manager's GraphQL endpoint.
type Client struct {
	endpoint string
	http     *http.Client
}

// New is a client for katalog-manager at base (its in-cluster address, e.g.
// http://katalog-manager-api); nil when base is empty, which is how a
// deployment without a catalog manager says so.
func New(base string) *Client {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return nil
	}
	return &Client{
		endpoint: base + "/query",
		http: &http.Client{
			Timeout: callTimeout,
			// An answer that redirects is no answer of katalog-manager's.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Setting is a catalog setting, as much of it as the checklist reads. A
// secret one never comes with its value; IsSet says whether it has one.
type Setting struct {
	Key       string     `json:"key"`
	IsSecret  bool       `json:"isSecret"`
	IsSet     bool       `json:"isSet"`
	UpdatedAt *time.Time `json:"updatedAt"`
}

// ScanJob is a scan of the library, as katalog-manager records it: status is
// running, done or failed, and the counts are written when it ends.
type ScanJob struct {
	ID            string     `json:"id"`
	Status        string     `json:"status"`
	StartedAt     *time.Time `json:"startedAt"`
	FinishedAt    *time.Time `json:"finishedAt"`
	Error         string     `json:"error,omitempty"`
	FilesSeen     int        `json:"filesSeen"`
	ItemsInserted int        `json:"itemsInserted"`
	ItemsUpdated  int        `json:"itemsUpdated"`
}

// scanJob is a ScanJob as the GraphQL answer spells it.
type scanJob struct {
	ID            string     `json:"id"`
	Status        string     `json:"status"`
	StartedAt     *time.Time `json:"startedAt"`
	FinishedAt    *time.Time `json:"finishedAt"`
	ErrorMessage  *string    `json:"errorMessage"`
	FilesSeen     *int       `json:"filesSeen"`
	ItemsInserted *int       `json:"itemsInserted"`
	ItemsUpdated  *int       `json:"itemsUpdated"`
}

func (j scanJob) job() ScanJob {
	out := ScanJob{ID: j.ID, Status: j.Status, StartedAt: j.StartedAt, FinishedAt: j.FinishedAt}
	if j.ErrorMessage != nil {
		out.Error = *j.ErrorMessage
	}
	deref := func(p *int) int {
		if p == nil {
			return 0
		}
		return *p
	}
	out.FilesSeen, out.ItemsInserted, out.ItemsUpdated = deref(j.FilesSeen), deref(j.ItemsInserted), deref(j.ItemsUpdated)
	return out
}

const scanJobFields = `id status startedAt finishedAt errorMessage filesSeen itemsInserted itemsUpdated`

// Overview is what the checklist reads from the catalog in one call.
type Overview struct {
	// TMDBKey is the tmdb.api_key setting; nil when the catalog holds none.
	TMDBKey *Setting
	// EnvironmentKey: katalog-manager runs with a TMDB_API_KEY of its own.
	EnvironmentKey bool
	// Movies and Series are the titles the catalog holds, each counted to
	// CountLimit; More says a count reached it.
	Movies, Series int
	More           bool
	// LastScan is the most recent scan; nil when none ran.
	LastScan *ScanJob
}

// Titles is how many titles the catalog holds — movies and series, not
// their episodes — or at least that many when More is set.
func (o Overview) Titles() int { return o.Movies + o.Series }

var overviewQuery = fmt.Sprintf(`{
  settings { key isSecret isSet updatedAt }
  enrichStatus { tmdbEnabled }
  movies(limit: %[1]d) { id }
  series(limit: %[1]d) { id }
  scanJobs(limit: 1) { %[2]s }
}`, CountLimit, scanJobFields)

// Overview reads the TMDB key's state, the titles held and the latest scan.
func (c *Client) Overview(ctx context.Context, bearer string) (Overview, error) {
	var data struct {
		Settings     []Setting `json:"settings"`
		EnrichStatus struct {
			TMDBEnabled bool `json:"tmdbEnabled"`
		} `json:"enrichStatus"`
		Movies   []struct{} `json:"movies"`
		Series   []struct{} `json:"series"`
		ScanJobs []scanJob  `json:"scanJobs"`
	}
	if err := c.do(ctx, bearer, overviewQuery, nil, &data); err != nil {
		return Overview{}, err
	}
	out := Overview{
		EnvironmentKey: data.EnrichStatus.TMDBEnabled,
		Movies:         len(data.Movies), Series: len(data.Series),
		More: len(data.Movies) >= CountLimit || len(data.Series) >= CountLimit,
	}
	for i := range data.Settings {
		if strings.TrimSpace(data.Settings[i].Key) == TMDBSetting {
			s := data.Settings[i]
			out.TMDBKey = &s
			break
		}
	}
	if len(data.ScanJobs) > 0 {
		j := data.ScanJobs[0].job()
		out.LastScan = &j
	}
	return out, nil
}

// SetSecretSetting sets a secret setting. Write-only: the answer says it is
// set, never what to. value never appears in an error this returns.
func (c *Client) SetSecretSetting(ctx context.Context, bearer, key, value string) (Setting, error) {
	var data struct {
		SetSecretSetting Setting `json:"setSecretSetting"`
	}
	err := c.do(ctx, bearer, `mutation($k: String!, $v: String!) { setSecretSetting(key: $k, value: $v) { key isSecret isSet updatedAt } }`,
		map[string]any{"k": key, "v": value}, &data)
	if err != nil {
		return Setting{}, withoutValue(err, value)
	}
	return data.SetSecretSetting, nil
}

// TriggerScan starts a scan of the library and answers its job, running.
func (c *Client) TriggerScan(ctx context.Context, bearer string) (ScanJob, error) {
	var data struct {
		TriggerScan scanJob `json:"triggerScan"`
	}
	if err := c.do(ctx, bearer, `mutation { triggerScan { `+scanJobFields+` } }`, nil, &data); err != nil {
		return ScanJob{}, err
	}
	return data.TriggerScan.job(), nil
}

// gqlError is one entry of a GraphQL answer's errors.
type gqlError struct {
	Message    string `json:"message"`
	Extensions struct {
		Code string `json:"code"`
	} `json:"extensions"`
}

// do sends one GraphQL request with the caller's bearer and decodes its data.
func (c *Client) do(ctx context.Context, bearer, query string, vars map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", bearer)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return &Refused{Message: fmt.Sprintf("%d %s", resp.StatusCode, firstLine(raw))}
	case resp.StatusCode != http.StatusOK:
		return &Failed{Message: fmt.Sprintf("%d %s", resp.StatusCode, firstLine(raw))}
	}
	var answer struct {
		Data   json.RawMessage `json:"data"`
		Errors []gqlError      `json:"errors"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil {
		return &Failed{Message: "an answer that is no GraphQL: " + err.Error()}
	}
	if len(answer.Errors) > 0 {
		msgs := make([]string, 0, len(answer.Errors))
		refused := false
		for _, e := range answer.Errors {
			msgs = append(msgs, e.Message)
			refused = refused || e.Extensions.Code == "FORBIDDEN" || strings.HasPrefix(e.Message, "forbidden")
		}
		msg := strings.Join(msgs, "; ")
		if refused {
			return &Refused{Message: msg}
		}
		return &Failed{Message: msg}
	}
	if err := json.Unmarshal(answer.Data, out); err != nil {
		return &Failed{Message: "an answer without the data asked for: " + err.Error()}
	}
	return nil
}

// firstLine is the start of a body, for an error message.
func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// withoutValue keeps a secret out of an error: katalog-manager's words, with
// the value replaced wherever they repeat it.
func withoutValue(err error, value string) error {
	if value == "" || !strings.Contains(err.Error(), value) {
		return err
	}
	scrub := func(s string) string { return strings.ReplaceAll(s, value, "***") }
	var refused *Refused
	var failed *Failed
	switch {
	case errors.As(err, &refused):
		return &Refused{Message: scrub(refused.Message)}
	case errors.As(err, &failed):
		return &Failed{Message: scrub(failed.Message)}
	}
	return errors.New(scrub(err.Error()))
}
