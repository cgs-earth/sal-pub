#!/bin/bash
set -euo pipefail

# Render the realm template by replacing ${VAR} placeholders with values from
# the environment. Pure bash because the Keycloak image ships without envsubst.
TEMPLATE=/opt/keycloak/sal.json
OUTPUT=/opt/keycloak/data/import/sal.json

content="$(<"$TEMPLATE")"

# Collect the placeholder names used in the template
names=()
rest="$content"
while [[ $rest =~ \$\{([A-Za-z_][A-Za-z0-9_]*)\} ]]; do
    names+=("${BASH_REMATCH[1]}")
    rest="${rest#*"${BASH_REMATCH[0]}"}"
done

for name in "${names[@]}"; do
    if [[ -z "${!name:-}" ]]; then
        echo "entrypoint: warning: $name is not set, substituting empty string" >&2
    fi
    value="${!name:-}"
    # Escape for use inside a JSON string
    value="${value//\\/\\\\}"
    value="${value//\"/\\\"}"
    content=${content//"\${$name}"/"$value"}
done

mkdir -p "$(dirname "$OUTPUT")"
printf '%s\n' "$content" > "$OUTPUT"

# Run the 'exec' command as the last step of the script.
# As it replaces the current shell process, no additional shell commands will run after the 'exec' command.
exec /opt/keycloak/bin/kc.sh "$@"
