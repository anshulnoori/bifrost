#!/usr/bin/env bash
# Read only. Never retrieves a client secret, code, ID token, or login URL.
set -euo pipefail
issuer=${1:?Pass the tsidp HTTPS origin}
[[ $issuer =~ ^https://[a-zA-Z0-9.-]+\.ts\.net$ ]] || exit 2
curl --fail --silent --show-error --max-time 15 "$issuer/.well-known/openid-configuration" |
  jq -e --arg issuer "$issuer" '
    select(.issuer == $issuer) |
    {issuer, authorization_endpoint, token_endpoint, jwks_uri,
     scopes_supported, claims_supported, code_challenge_methods_supported,
     token_endpoint_auth_methods_supported, id_token_signing_alg_values_supported} |
    select(.authorization_endpoint | startswith($issuer + "/")) |
    select(.token_endpoint | startswith($issuer + "/")) |
    select(.jwks_uri | startswith($issuer + "/"))'
