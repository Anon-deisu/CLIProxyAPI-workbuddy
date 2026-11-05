package codex

import "github.com/router-for-me/CLIProxyAPI/v8/internal/codexdata"

// PKCECodes holds the verification codes for the OAuth2 PKCE (Proof Key for Code Exchange) flow.
// PKCE is an extension to the Authorization Code flow to prevent CSRF and authorization code injection attacks.
type PKCECodes struct {
	// CodeVerifier is the cryptographically random string used to correlate
	// the authorization request to the token request
	CodeVerifier string `json:"code_verifier"`
	// CodeChallenge is the SHA256 hash of the code verifier, base64url-encoded
	CodeChallenge string `json:"code_challenge"`
}

type CodexTokenData = codexdata.CodexTokenData

type CodexSIWCTokenData = codexdata.CodexSIWCTokenData

type CodexAuthBundle = codexdata.CodexAuthBundle
