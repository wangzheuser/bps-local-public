package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ModelInfo is one entry of the upstream model catalog.
type ModelInfo struct {
	ID            string     `json:"id"`
	Label         string     `json:"label"`
	DefaultEffort string     `json:"default_effort"`
	Efforts       []ModelEffort `json:"efforts"`
}

// ModelEffort is one reasoning level offered for a model.
type ModelEffort struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// ModelCatalog is the upstream answer for the account's usable models.
type ModelCatalog struct {
	Allowed          bool        `json:"allowed"`
	DenialReason     string      `json:"denial_reason"`
	PlanType         string      `json:"plan_type"`
	AuthMode         string      `json:"auth_mode"`
	Models           []ModelInfo `json:"models"`
	DefaultModel     string      `json:"default_model"`
	Source           string      `json:"source"`
	RestrictedModels []string    `json:"restricted_models"`
}

// accessURL derives the model/access endpoint from the Responses endpoint.
// The catalog lives directly under the responses path:
// .../basispoints/api/responses/access
func (s *Server) accessURL() string {
	return strings.TrimSuffix(s.cfg.Upstream, "/") + "/access?include_models=true"
}

// Models queries the upstream catalog for this account's credentials. It is a
// read-only metadata call: no generation happens and no usage is consumed.
func (s *Server) Models(ctx context.Context) (*ModelCatalog, error) {
	token, accountID, err := s.store.Credentials()
	if err != nil {
		return nil, err
	}
	if accountID == "" {
		accountID = accountIDFromJWT(token)
	}
	if accountID == "" {
		return nil, fmt.Errorf("account id is missing from the credential")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.accessURL(), nil)
	if err != nil {
		return nil, fmt.Errorf("build access request: %w", err)
	}
	applyUpstreamHeaders(req, token, accountID)
	req.Header.Del("Content-Type")
	req.Header.Set("Accept", "application/json")
	for name, value := range excelClientInfo() {
		req.Header.Set(name, value)
	}
	s.applyCustomHeaders(req)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach upstream access endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read upstream catalog: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// The body may echo identifiers; report only the status.
		return nil, fmt.Errorf("upstream access endpoint returned HTTP %d", resp.StatusCode)
	}
	var doc struct {
		Allowed      bool   `json:"allowed"`
		DenialReason string `json:"denial_reason"`
		PlanType     string `json:"plan_type"`
		AuthMode     string `json:"auth_mode"`
		ModelCatalog struct {
			Models           []ModelInfo `json:"models"`
			DefaultModel     string      `json:"default_model"`
			Source           string      `json:"source"`
			RestrictedModels []string    `json:"restricted_models"`
		} `json:"model_catalog"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("upstream catalog is not valid JSON")
	}
	cat := &ModelCatalog{
		Allowed:          doc.Allowed,
		DenialReason:     doc.DenialReason,
		PlanType:         doc.PlanType,
		AuthMode:         doc.AuthMode,
		Models:           doc.ModelCatalog.Models,
		DefaultModel:     doc.ModelCatalog.DefaultModel,
		Source:           doc.ModelCatalog.Source,
		RestrictedModels: doc.ModelCatalog.RestrictedModels,
	}
	if cat.Models == nil {
		cat.Models = []ModelInfo{}
	}
	return cat, nil
}

// accountIDFromJWT recovers the ChatGPT account id when the store did not carry
// an explicit field. Kept local to avoid widening the auth API.
func accountIDFromJWT(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := decodeRawURL(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
	if auth == nil {
		return ""
	}
	id, _ := auth["chatgpt_account_id"].(string)
	return strings.TrimSpace(id)
}
