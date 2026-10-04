package controllers

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// InstallationTokenFunc mints a GitHub App installation token for a clone.
// privateKey is the App's PEM key from the connection's Secret.
type InstallationTokenFunc func(ctx context.Context, conn *kwerftv1.GitConnection, privateKey []byte) (string, error)

// permanentError is a minting failure retrying will not fix (bad key, app
// uninstalled); the build fails with it.
type permanentError struct{ error }

// githubAPIBase is the REST API of the connection's host: api.github.com for
// github.com, <url>/api/v3 for GitHub Enterprise Server.
func githubAPIBase(hostURL string) string {
	u, err := url.Parse(hostURL)
	if err != nil || u.Host == "" || strings.EqualFold(u.Hostname(), "github.com") {
		return "https://api.github.com"
	}
	return strings.TrimRight(hostURL, "/") + "/api/v3"
}

// GitHubInstallationToken mints an installation token through the GitHub
// API: an RS256 JWT for the App, exchanged for a token of the installation
// (valid one hour, enough for a clone).
func GitHubInstallationToken(client *http.Client) InstallationTokenFunc {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return func(ctx context.Context, conn *kwerftv1.GitConnection, privateKey []byte) (string, error) {
		app := conn.Spec.GitHubApp
		if app == nil {
			return "", permanentError{errors.New("the connection has no GitHub App settings")}
		}
		jwt, err := githubAppJWT(app.AppID, privateKey, time.Now())
		if err != nil {
			return "", permanentError{err}
		}
		endpoint := fmt.Sprintf("%s/app/installations/%d/access_tokens", githubAPIBase(conn.Spec.URL), app.InstallationID)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+jwt)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		resp, err := client.Do(req)
		if err != nil {
			return "", fmt.Errorf("GitHub API: %w", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		if resp.StatusCode != http.StatusCreated {
			var e struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(body, &e)
			err := fmt.Errorf("GitHub API: %s: %s", resp.Status, e.Message)
			if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
				return "", permanentError{err}
			}
			return "", err
		}
		var tok struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(body, &tok); err != nil || tok.Token == "" {
			return "", errors.New("GitHub API: no token in the response")
		}
		return tok.Token, nil
	}
}

// githubAppJWT signs the App's identity: issued a minute ago (clock skew),
// valid nine minutes (GitHub allows ten).
func githubAppJWT(appID int64, pemKey []byte, now time.Time) (string, error) {
	block, _ := pem.Decode(pemKey)
	if block == nil {
		return "", errors.New("the GitHub App private key is not PEM")
	}
	var key *rsa.PrivateKey
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = k
	} else if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return "", errors.New("the GitHub App private key is not an RSA key")
		}
		key = rk
	} else {
		return "", errors.New("cannot parse the GitHub App private key")
	}
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(appID, 10),
	})
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}
