# fmc_api.sh — shared FMC REST helper for the integration test scripts.
# Source it; it defines one function and changes no shell options.
#
# FMC cancels a user's earlier access token as soon as the same user logs in
# again, and every terraform run logs in. A token kept across terraform calls
# is therefore usually dead by the time it is used (HTTP 401 "Access token
# invalid"). fmc_api logs in fresh for every request and checks the HTTP
# status, so a refused request can't go unnoticed.
#
# Requires FMC_URL, FMC_USERNAME and FMC_PASSWORD in the environment.
#
# Usage: fmc_api [--ok CODES] METHOD PATH [JSON_BODY]
#   PATH       relative to /api/fmc_config/v1/domain/<domain>, e.g. /object/hosts
#   --ok CODES comma-separated non-2xx statuses to accept, e.g. --ok 404
# Prints the response body on stdout. On any other status, prints the method,
# path, status and body on stderr and returns 1.
fmc_api() {
  local ok=""
  if [[ "$1" == "--ok" ]]; then
    ok=",$2,"
    shift 2
  fi
  local method="$1" path="$2" body="${3:-}"
  local hdr status token domain out

  hdr="$(mktemp)"
  status=$(curl -s -k -D "$hdr" -o /dev/null -w '%{http_code}' -X POST \
    "${FMC_URL}/api/fmc_platform/v1/auth/generatetoken" \
    --user "${FMC_USERNAME}:${FMC_PASSWORD}") || status="curl error"
  token="$(awk 'tolower($1)=="x-auth-access-token:"{print $2}' "$hdr" | tr -d '\r\n')"
  domain="$(awk 'tolower($1)=="domain_uuid:"{print $2}' "$hdr" | tr -d '\r\n')"
  rm -f "$hdr"
  if [[ -z "$token" || -z "$domain" ]]; then
    echo "fmc_api: login failed (HTTP $status) before $method $path" >&2
    return 1
  fi

  local args=(-s -k -X "$method" -H "X-auth-access-token: $token" -w $'\n%{http_code}')
  [[ -n "$body" ]] && args+=(-H "Content-Type: application/json" -d "$body")
  if ! out=$(curl "${args[@]}" "${FMC_URL}/api/fmc_config/v1/domain/${domain}${path}"); then
    echo "fmc_api: $method $path: curl failed" >&2
    return 1
  fi
  status="${out##*$'\n'}"
  out="${out%$'\n'*}"
  if [[ "$status" != 2?? && "$ok" != *",$status,"* ]]; then
    echo "fmc_api: $method $path -> HTTP $status: $out" >&2
    return 1
  fi
  printf '%s' "$out"
}
