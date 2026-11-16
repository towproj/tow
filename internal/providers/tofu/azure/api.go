package azure

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

// The few Azure Resource Manager calls tow makes itself, over HTTPS: what OpenTofu doesn't
// do (checks before anything is created, status, leftovers after a delete). Azure's Go SDKs
// for each service would do the same at many times the build's size; only the credential
// chain comes from Microsoft's own library, as OpenTofu's azurerm provider reads the same.
const (
	armAPI   = "https://management.azure.com"
	armScope = "https://management.azure.com/.default"
)

const credentialHelp = `Any of these works:
  - on your machine: 'az login'
  - as a service principal or in CI: ARM_CLIENT_ID, ARM_TENANT_ID and ARM_CLIENT_SECRET (as OpenTofu's azurerm provider reads them), or AZURE_CLIENT_ID, AZURE_TENANT_ID and a secret, certificate or workload identity token (AZURE_FEDERATED_TOKEN_FILE)
  - on an Azure VM: its managed identity`

// api calls Azure Resource Manager for one subscription.
type api struct {
	subscription string
	cred         azcore.TokenCredential
	client       *http.Client
	arm          string // Azure's address, or a test server's
}

// credential is the chain tow and OpenTofu both accept: OpenTofu's ARM_ service principal
// when its variables are set, then Azure's default chain (AZURE_ variables, workload identity,
// managed identity, the Azure CLI's login).
func credential() (azcore.TokenCredential, error) {
	var chain []azcore.TokenCredential
	if id, tenant, secret := os.Getenv("ARM_CLIENT_ID"), os.Getenv("ARM_TENANT_ID"), os.Getenv("ARM_CLIENT_SECRET"); id != "" && tenant != "" && secret != "" {
		sp, err := azidentity.NewClientSecretCredential(tenant, id, secret, nil)
		if err != nil {
			return nil, err
		}
		chain = append(chain, sp)
	}
	def, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, err
	}
	chain = append(chain, def)
	return azidentity.NewChainedTokenCredential(chain, nil)
}

func newAPI(subscription string) (*api, error) {
	cred, err := credential()
	if err != nil {
		return nil, fmt.Errorf("could not set up Azure credentials: %w\n%s", err, credentialHelp)
	}
	return &api{subscription: subscription, cred: cred, client: http.DefaultClient, arm: armAPI}, nil
}

// token is a bearer token for Resource Manager.
func (a *api) token(ctx context.Context) (string, error) {
	tok, err := a.cred.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{armScope}})
	if err != nil {
		return "", fmt.Errorf("could not get an Azure token: %w\n%s", err, credentialHelp)
	}
	return tok.Token, nil
}

// apiError is Resource Manager's error answer: {"error": {"code": "...", "message": "..."}}.
type apiError struct {
	Status  int
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s: %s (HTTP %d)", e.Code, e.Message, e.Status)
}

func isNotFound(err error) bool {
	var e *apiError
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

// do sends one request to a path under Resource Manager (or a full nextLink), with the API
// version, and decodes the answer into out.
func (a *api) do(ctx context.Context, method, path, apiVersion string, out any) error {
	u := path
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		u = a.arm + path + sep + "api-version=" + apiVersion
	}
	tok, err := a.token(ctx)
	if err != nil {
		return err
	}
	var body io.Reader
	if method == http.MethodPost {
		body = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error *apiError `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != nil {
			e.Error.Status = resp.StatusCode
			return e.Error
		}
		return &apiError{Status: resp.StatusCode, Code: http.StatusText(resp.StatusCode), Message: strings.TrimSpace(string(data))}
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (a *api) get(ctx context.Context, path, apiVersion string, out any) error {
	return a.do(ctx, http.MethodGet, path, apiVersion, out)
}

// list reads every page of a list: {"value": [...], "nextLink": "..."}.
func list[T any](ctx context.Context, a *api, path, apiVersion string) ([]T, error) {
	var out []T
	next := path
	for next != "" {
		var page struct {
			Value    []T    `json:"value"`
			NextLink string `json:"nextLink"`
		}
		if err := a.get(ctx, next, apiVersion, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Value...)
		next = page.NextLink
	}
	return out, nil
}

// account is who the token says tow acts as: a user's name, else an application's ID; ""
// when it doesn't say. The token's claims are only read, for this message.
func (a *api) account(ctx context.Context) string {
	tok, err := a.token(ctx)
	if err != nil {
		return ""
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		UPN        string `json:"upn"`
		UniqueName string `json:"unique_name"`
		AppID      string `json:"appid"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	switch {
	case claims.UPN != "":
		return claims.UPN
	case claims.UniqueName != "":
		return claims.UniqueName
	case claims.AppID != "":
		return "application " + claims.AppID
	}
	return ""
}
