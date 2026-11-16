package gcp

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

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// The few Google APIs tow calls itself, over HTTPS: what OpenTofu doesn't do (checks
// before anything is created, status, leftovers after a delete). Google's Go SDKs would do the
// same at many times the build's size.
const (
	computeAPI = "https://compute.googleapis.com/compute/v1"
	iamAPI     = "https://iam.googleapis.com/v1"
	tokenInfo  = "https://oauth2.googleapis.com/tokeninfo"
	scope      = "https://www.googleapis.com/auth/cloud-platform"
)

const credentialHelp = `Any of these works:
  - on your machine: 'gcloud auth application-default login' (tow and OpenTofu read what it saves)
  - as a service account or in CI: GOOGLE_APPLICATION_CREDENTIALS=<a key file, or a workload identity federation configuration>
  - on a Compute Engine VM: its service account`

// api calls Google's APIs for one project with Application Default Credentials, as OpenTofu's
// google provider finds them.
type api struct {
	project string
	client  *http.Client
	creds   *google.Credentials
	// The APIs' addresses: Google's, or a test server's.
	compute, iam, tokenInfo string
}

func newAPI(ctx context.Context, project string) (*api, error) {
	creds, err := google.FindDefaultCredentials(ctx, scope)
	if err != nil {
		return nil, fmt.Errorf("could not find Google credentials: %w\n%s", err, credentialHelp)
	}
	return &api{project: project, creds: creds, client: oauth2.NewClient(ctx, creds.TokenSource),
		compute: computeAPI, iam: iamAPI, tokenInfo: tokenInfo}, nil
}

// apiError is Google's error answer: {"error": {"code": 404, "message": "..."}}.
type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *apiError) Error() string { return fmt.Sprintf("%s (HTTP %d)", e.Message, e.Code) }

func isNotFound(err error) bool {
	var e *apiError
	return errors.As(err, &e) && e.Code == http.StatusNotFound
}

// do sends one request (a JSON body when body isn't nil) and decodes the answer into out.
func (a *api) do(ctx context.Context, method, u string, body, out any) error {
	var r io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error *apiError `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != nil {
			if e.Error.Code == 0 {
				e.Error.Code = resp.StatusCode
			}
			return e.Error
		}
		return &apiError{Code: resp.StatusCode, Message: strings.TrimSpace(string(data))}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (a *api) get(ctx context.Context, u string, out any) error {
	return a.do(ctx, http.MethodGet, u, nil, out)
}

// pages calls each with every page of a list (filtered when filter isn't empty), following
// nextPageToken.
func (a *api) pages(ctx context.Context, u, filter string, each func(page []byte) error) error {
	token := ""
	for {
		q := url.Values{}
		if filter != "" {
			q.Set("filter", filter)
		}
		if token != "" {
			q.Set("pageToken", token)
		}
		full := u
		if len(q) > 0 {
			full += "?" + q.Encode()
		}
		var raw json.RawMessage
		if err := a.get(ctx, full, &raw); err != nil {
			return err
		}
		if err := each(raw); err != nil {
			return err
		}
		var next struct {
			NextPageToken string `json:"nextPageToken"`
		}
		if err := json.Unmarshal(raw, &next); err != nil {
			return err
		}
		if token = next.NextPageToken; token == "" {
			return nil
		}
	}
}

// list reads a list's items: {"items": [...]}.
func list[T any](ctx context.Context, a *api, u, filter string) ([]T, error) {
	var out []T
	err := a.pages(ctx, u, filter, func(page []byte) error {
		var p struct {
			Items []T `json:"items"`
		}
		if err := json.Unmarshal(page, &p); err != nil {
			return err
		}
		out = append(out, p.Items...)
		return nil
	})
	return out, err
}

// aggregated reads an aggregated list, every zone's (or region's) items under key:
// {"items": {"zones/europe-west1-b": {"instances": [...]}}}.
func aggregated[T any](ctx context.Context, a *api, u, key, filter string) ([]T, error) {
	var out []T
	err := a.pages(ctx, u, filter, func(page []byte) error {
		var p struct {
			Items map[string]map[string]json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(page, &p); err != nil {
			return err
		}
		for _, scoped := range p.Items {
			raw, ok := scoped[key]
			if !ok {
				continue
			}
			var items []T
			if err := json.Unmarshal(raw, &items); err != nil {
				return err
			}
			out = append(out, items...)
		}
		return nil
	})
	return out, err
}

// account is who the credentials act as: a service account's email from its key file, else
// what Google says the token belongs to; "" when neither says.
func (a *api) account(ctx context.Context) string {
	var key struct {
		ClientEmail string `json:"client_email"`
	}
	if json.Unmarshal(a.creds.JSON, &key) == nil && key.ClientEmail != "" {
		return key.ClientEmail
	}
	tok, err := a.creds.TokenSource.Token()
	if err != nil {
		return ""
	}
	// The token goes in the body, never the address (which logs may keep).
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.tokenInfo, strings.NewReader(url.Values{"access_token": {tok.AccessToken}}.Encode()))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var info struct {
		Email string `json:"email"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info) != nil {
		return ""
	}
	return info.Email
}
