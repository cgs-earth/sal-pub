#!/usr/bin/env bash
# Build the sal-pub, Keycloak and Caddy images for linux/amd64 (GKE nodes) and
# push them to Artifact Registry.
#
#   ./build-images.sh <registry> <tag>
#   ./build-images.sh "$(tofu output -raw registry)" "$(git rev-parse --short HEAD)"
set -euo pipefail

registry=${1:?usage: $0 <registry> <tag>}
tag=${2:?usage: $0 <registry> <tag>}
root=$(cd "$(dirname "$0")/../.." && pwd)

gcloud auth configure-docker "${registry%%/*}" --quiet

build() { # name, context
	docker buildx build --platform linux/amd64 --push -t "$registry/$1:$tag" "$2"
}
build salpub "$root"
build keycloak "$root/docker/keycloak"
build caddy "$root/docker/caddy"

echo "Pushed $registry/{salpub,keycloak,caddy}:$tag"
