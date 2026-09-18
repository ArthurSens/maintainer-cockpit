// Package auth implements GitHub identity verification and authorization
// evidence collection without retaining GitHub user access tokens.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const apiVersion = "2026-03-10"

// Identity is the immutable GitHub identity used as the product data key.
type Identity struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url"`
}

// Evidence is the set of active memberships verified during login.
type Evidence struct {
	Organizations []string
	Teams         []string
}

// Requirements bounds membership checks to authorization rules in use.
type Requirements struct {
	Organizations []string
	Teams         []string
}

// Provider drives one OAuth authorization and identity verification flow.
type Provider interface {
	AuthorizationURL(state, challenge, redirectURI string) string
	Authenticate(
		ctx context.Context, code, verifier, redirectURI string, requirements Requirements,
	) (Identity, Evidence, error)
}

// GitHubOptions configures the GitHub App user authorization client.
type GitHubOptions struct {
	ClientID, ClientSecret string
	OAuthBaseURL           string
	APIBaseURL             string
	HTTPClient             *http.Client
}

// GitHubProvider performs the GitHub App web application flow.
type GitHubProvider struct {
	clientID, clientSecret string
	oauthBase, apiBase     *url.URL
	client                 *http.Client
}

// NewGitHubProvider creates a GitHub user authorization provider.
func NewGitHubProvider(options GitHubOptions) (*GitHubProvider, error) {
	if strings.TrimSpace(options.ClientID) == "" || options.ClientSecret == "" {
		return nil, errors.New("GitHub OAuth client ID and secret are required")
	}
	oauthBase := options.OAuthBaseURL
	if oauthBase == "" {
		oauthBase = "https://github.com"
	}
	apiBase := options.APIBaseURL
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	parsedOAuth, err := url.Parse(oauthBase)
	if err != nil || parsedOAuth.Scheme == "" || parsedOAuth.Host == "" {
		return nil, errors.New("invalid GitHub OAuth base URL")
	}
	parsedAPI, err := url.Parse(apiBase)
	if err != nil || parsedAPI.Scheme == "" || parsedAPI.Host == "" {
		return nil, errors.New("invalid GitHub API base URL")
	}
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &GitHubProvider{
		clientID: options.ClientID, clientSecret: options.ClientSecret,
		oauthBase: parsedOAuth, apiBase: parsedAPI, client: client,
	}, nil
}

// AuthorizationURL builds a state- and PKCE-protected GitHub authorization URL.
func (p *GitHubProvider) AuthorizationURL(state, challenge, redirectURI string) string {
	target := p.oauthBase.ResolveReference(&url.URL{Path: "/login/oauth/authorize"})
	query := target.Query()
	query.Set("client_id", p.clientID)
	query.Set("redirect_uri", redirectURI)
	query.Set("state", state)
	query.Set("code_challenge", challenge)
	query.Set("code_challenge_method", "S256")
	target.RawQuery = query.Encode()
	return target.String()
}

// Authenticate exchanges the temporary code, verifies the numeric identity,
// collects only required active memberships, and then drops the user token.
func (p *GitHubProvider) Authenticate(
	ctx context.Context,
	code, verifier, redirectURI string,
	requirements Requirements,
) (Identity, Evidence, error) {
	token, err := p.exchange(ctx, code, verifier, redirectURI)
	if err != nil {
		return Identity{}, Evidence{}, err
	}
	var identity Identity
	if err := p.get(ctx, token, "/user", &identity); err != nil {
		return Identity{}, Evidence{}, fmt.Errorf("load GitHub identity: %w", err)
	}
	if identity.ID <= 0 || identity.Login == "" {
		return Identity{}, Evidence{}, errors.New("GitHub returned an invalid identity")
	}
	identity.AvatarURL = validatedAvatarURL(identity.AvatarURL)
	evidence := Evidence{}
	for _, organization := range deduplicate(requirements.Organizations) {
		var membership struct {
			State string `json:"state"`
		}
		found, err := p.getOptional(ctx, token,
			"/user/memberships/orgs/"+url.PathEscape(organization), &membership)
		if err != nil {
			return Identity{}, Evidence{}, fmt.Errorf("verify organization %q: %w", organization, err)
		}
		if found && membership.State == "active" {
			evidence.Organizations = append(evidence.Organizations, strings.ToLower(organization))
		}
	}
	for _, team := range deduplicate(requirements.Teams) {
		organization, slug, _ := strings.Cut(team, "/")
		var membership struct {
			State string `json:"state"`
		}
		requestPath := "/orgs/" + url.PathEscape(organization) + "/teams/" +
			url.PathEscape(slug) + "/memberships/" + url.PathEscape(identity.Login)
		found, err := p.getOptional(ctx, token, requestPath, &membership)
		if err != nil {
			return Identity{}, Evidence{}, fmt.Errorf("verify team %q: %w", team, err)
		}
		// GitHub's team membership endpoint includes inherited membership from
		// child teams, so an active result authorizes a configured parent team.
		if found && membership.State == "active" {
			evidence.Teams = append(evidence.Teams, strings.ToLower(team))
		}
	}
	return identity, evidence, nil
}

func (p *GitHubProvider) exchange(
	ctx context.Context, code, verifier, redirectURI string,
) (string, error) {
	if code == "" || verifier == "" {
		return "", errors.New("OAuth code and PKCE verifier are required")
	}
	values := url.Values{
		"client_id": {p.clientID}, "client_secret": {p.clientSecret},
		"code": {code}, "redirect_uri": {redirectURI}, "code_verifier": {verifier},
	}
	target := p.oauthBase.ResolveReference(&url.URL{Path: "/login/oauth/access_token"})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(),
		strings.NewReader(values.Encode()))
	if err != nil {
		return "", fmt.Errorf("create GitHub token request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := p.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("exchange GitHub authorization code: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return "", fmt.Errorf("GitHub token endpoint returned %s", response.Status)
	}
	var result struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil {
		return "", fmt.Errorf("decode GitHub token response: %w", err)
	}
	if result.Error != "" || result.AccessToken == "" {
		return "", errors.New("GitHub rejected the authorization code")
	}
	return result.AccessToken, nil
}

func (p *GitHubProvider) get(ctx context.Context, token, requestPath string, target any) error {
	found, err := p.getOptional(ctx, token, requestPath, target)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("GitHub API resource not found")
	}
	return nil
}

func (p *GitHubProvider) getOptional(
	ctx context.Context, token, requestPath string, target any,
) (bool, error) {
	endpoint := p.apiBase.ResolveReference(&url.URL{Path: requestPath})
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), http.NoBody)
	if err != nil {
		return false, fmt.Errorf("create GitHub API request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-GitHub-Api-Version", apiVersion)
	response, err := p.client.Do(request)
	if err != nil {
		return false, fmt.Errorf("call GitHub API: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return false, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return false, fmt.Errorf("GitHub API returned %s", response.Status)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(target); err != nil {
		return false, fmt.Errorf("decode GitHub API response: %w", err)
	}
	return true, nil
}

// PKCEChallenge returns the S256 challenge for an OAuth verifier.
func PKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func deduplicate(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
	}
	return result
}

func validatedAvatarURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" ||
		!strings.EqualFold(parsed.Hostname(), "avatars.githubusercontent.com") ||
		parsed.User != nil {
		return ""
	}
	return parsed.String()
}
