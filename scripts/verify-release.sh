#!/usr/bin/env bash
# verify-release.sh — consume a published cyoda release the way a user does,
# and fail on the first route that does not work.
#
# Usage: scripts/verify-release.sh vX.Y.Z [--latest] [--chart C.D.E | --chart-if-published]
#
#   (always)  SHA256SUMS is signed by the release workflow for exactly this
#             tag; the help asset carries the version; install.sh from the
#             release installs it with CYODA_COSIGN_VERIFY=required; the .deb
#             and the .rpm install in clean containers; the image verifies
#             with cosign and runs by its digest.
#   --latest  the tag is GitHub's Latest release, and the unpinned routes
#             (/releases/latest/download/SHA256SUMS and install.sh, install.sh
#             without CYODA_VERSION, the image tag `latest`) give this release.
#             Use it only for the newest stable release.
#   --chart C.D.E         the Helm repository serves chart C.D.E, its
#                         appVersion is this release's version, and it
#                         downloads and renders.
#   --chart-if-published  download and render every chart whose appVersion is this
#                         release's version; none is not a failure (a release
#                         may ship without a chart).
#
# Needs curl, jq, cosign, docker (or $DOCKER), sha256sum or shasum, and helm
# for the chart checks. Every file it writes goes to a temporary directory:
# HOME, the XDG directories, cosign's cache, Docker's client config and
# Helm's directories all point there (the images it pulls go to the Docker
# daemon's storage). GH_TOKEN, when set, is sent to api.github.com only and
# is not passed to anything the script runs.
#
# INSTALL_SH, when set, is a local install.sh to run instead of the
# release's own: for testing an install.sh change against a published
# release.
set -euo pipefail

REPO="cyoda/cyoda-go"
IMAGE="ghcr.io/cyoda/cyoda"
DEB_IMAGE="${DEB_IMAGE:-ubuntu:24.04}"
RPM_IMAGE="${RPM_IMAGE:-fedora:41}"
DOCKER="${DOCKER:-docker}"

die() { printf 'verify-release: FAIL: %s\n' "$*" >&2; exit 1; }
step() { printf '\n== %s\n' "$*"; }
ok() { printf 'ok: %s\n' "$*"; }

[ $# -ge 1 ] || die "usage: $0 vX.Y.Z [--latest] [--chart C.D.E | --chart-if-published]"
TAG=$1; shift
case "$TAG" in v[0-9]*.[0-9]*.[0-9]*) : ;; *) die "tag must look like vX.Y.Z, got '$TAG'" ;; esac
V=${TAG#v}
CHECK_LATEST=0 CHART="" CHART_IF_PUBLISHED=0
while [ $# -gt 0 ]; do
    case "$1" in
        --latest) CHECK_LATEST=1 ;;
        --chart) [ $# -ge 2 ] || die "--chart needs a chart version"; CHART=$2; shift ;;
        --chart-if-published) CHART_IF_PUBLISHED=1 ;;
        *) die "unknown argument '$1'" ;;
    esac
    shift
done
for c in curl jq cosign; do command -v "$c" >/dev/null || die "$c is not on PATH"; done
if [ -n "$CHART" ] || [ "$CHART_IF_PUBLISHED" = 1 ]; then command -v helm >/dev/null || die "helm is not on PATH"; fi

# Keep the token for api.github.com, out of the environment of everything
# the script runs (docker, install.sh, the installed binary, cosign, helm).
API_TOKEN="${GH_TOKEN:-}"
unset GH_TOKEN GITHUB_TOKEN

# Keep the caller's Docker endpoint (a context other than the default socket)
# before HOME and DOCKER_CONFIG move to the temporary directory.
if [ -z "${DOCKER_HOST:-}" ]; then
    DOCKER_HOST=$("$DOCKER" context inspect -f '{{.Endpoints.docker.Host}}' 2>/dev/null || true)
    [ -n "$DOCKER_HOST" ] && export DOCKER_HOST
fi

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
export HOME="$WORK/home" TUF_ROOT="$WORK/tuf" DOCKER_CONFIG="$WORK/docker" \
    XDG_CONFIG_HOME="$WORK/xdg/config" XDG_DATA_HOME="$WORK/xdg/data" XDG_CACHE_HOME="$WORK/xdg/cache" \
    HELM_CACHE_HOME="$WORK/helm/cache" HELM_CONFIG_HOME="$WORK/helm/config" HELM_DATA_HOME="$WORK/helm/data"
mkdir -p "$HOME" "$DOCKER_CONFIG" "$XDG_CONFIG_HOME" "$XDG_DATA_HOME" "$WORK/dl" "$WORK/pkg"
echo '{}' > "$DOCKER_CONFIG/config.json"
"$DOCKER" version >/dev/null 2>&1 || die "docker does not answer (DOCKER_HOST=${DOCKER_HOST:-unset})"

if command -v sha256sum >/dev/null; then sha256() { sha256sum "$1" | cut -d' ' -f1; }
else sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }; fi

DL="https://github.com/$REPO/releases/download/$TAG"
ID="https://github.com/Cyoda/cyoda-go/.github/workflows/release.yml@refs/tags/$TAG"
ISSUER="https://token.actions.githubusercontent.com"

fetch() { curl -fsSL --retry 3 --retry-connrefused -o "$2" "$1" || die "download failed: $1"; }
github_api() {
    if [ -n "$API_TOKEN" ]; then
        curl -fsSL -H "Authorization: Bearer $API_TOKEN" "https://api.github.com/repos/$REPO/$1"
    else
        curl -fsSL "https://api.github.com/repos/$REPO/$1"
    fi
}
# check_sum FILE: FILE's SHA-256 is the one SHA256SUMS lists for its name.
check_sum() {
    want=$(awk -v n="$(basename "$1")" '$2 == n {print $1}' "$WORK/dl/SHA256SUMS")
    [ -n "$want" ] || die "$(basename "$1") is not listed in SHA256SUMS"
    [ "$(sha256 "$1")" = "$want" ] || die "$(basename "$1") does not match SHA256SUMS"
}
# cosign_or_die LOG MESSAGE COSIGN-ARGS...: run cosign; on failure show why.
cosign_or_die() {
    log=$1 msg=$2; shift 2
    cosign "$@" > "$log.out" 2> "$log" || { tail -n 5 "$log" >&2; die "$msg"; }
}
# verify_image REF: cosign-verify REF against this tag's release run; print
# the digest the signature names.
verify_image() {
    cosign_or_die "$WORK/cosign-image.log" "cosign does not verify $1 for $TAG" \
        verify "$1" --certificate-identity "$ID" --certificate-oidc-issuer "$ISSUER" \
        --certificate-github-workflow-trigger push --certificate-github-workflow-ref "refs/tags/$TAG"
    d=$(jq -r '.[0].critical.image."docker-manifest-digest"' "$WORK/cosign-image.log.out")
    case "$d" in sha256:*) printf '%s' "$d" ;; *) die "no digest in the cosign output for $1" ;; esac
}
# run_version IMAGE@DIGEST: the image runs and reports version V.
run_version() {
    out=$("$DOCKER" run --rm "$1" --version 2> "$WORK/docker.log") \
        || { tail -n 5 "$WORK/docker.log" >&2; die "$1 does not run"; }
    case "$out" in "cyoda version $V "*) printf '%s' "$out" ;; *) die "$1 reports: $out" ;; esac
}

step "SHA256SUMS is signed by the release workflow for $TAG"
for f in SHA256SUMS SHA256SUMS.sig SHA256SUMS.pem; do fetch "$DL/$f" "$WORK/dl/$f"; done
cosign_or_die "$WORK/cosign-sums.log" "cosign does not verify SHA256SUMS for $TAG" \
    verify-blob --certificate "$WORK/dl/SHA256SUMS.pem" --signature "$WORK/dl/SHA256SUMS.sig" \
    --certificate-identity "$ID" --certificate-oidc-issuer "$ISSUER" \
    --certificate-github-workflow-trigger push --certificate-github-workflow-ref "refs/tags/$TAG" \
    "$WORK/dl/SHA256SUMS"
ok "SHA256SUMS verified"

step "the help asset carries version $V"
fetch "$DL/cyoda_help_$V.json" "$WORK/dl/cyoda_help_$V.json"
check_sum "$WORK/dl/cyoda_help_$V.json"
got=$(jq -r .version "$WORK/dl/cyoda_help_$V.json") || die "cyoda_help_$V.json is not JSON"
[ "$got" = "$V" ] || die "cyoda_help_$V.json says version '$got'"
ok "cyoda_help_$V.json version $got"

if [ -n "${INSTALL_SH:-}" ]; then
    step "install.sh at $INSTALL_SH (not the release's) installs $TAG with signatures required"
    cp "$INSTALL_SH" "$WORK/dl/install.sh"
else
    step "install.sh from the release installs $TAG with signatures required"
    fetch "$DL/install.sh" "$WORK/dl/install.sh"
    # SHA256SUMS lists install.sh from v0.9.0 on; earlier releases did not.
    if [ "$(printf '%s\n' 0.9.0 "$V" | sort -V | head -n1)" = "0.9.0" ]; then
        check_sum "$WORK/dl/install.sh"
    fi
fi
CYODA_VERSION="$TAG" CYODA_COSIGN_VERIFY=required CYODA_INSTALL_DIR="$WORK/bin" \
    sh "$WORK/dl/install.sh" > "$WORK/install.log" 2>&1 || { cat "$WORK/install.log" >&2; die "install.sh failed"; }
grep -q 'Signatures verified (cosign)' "$WORK/install.log" || { cat "$WORK/install.log" >&2; die "install.sh did not verify signatures"; }
"$WORK/bin/cyoda" --version | grep -q "^cyoda version $V " || die "installed binary: $("$WORK/bin/cyoda" --version)"
ok "$("$WORK/bin/cyoda" --version)"

ARCH=$("$DOCKER" version -f '{{.Server.Arch}}') || die "docker does not report its architecture"
step ".deb and .rpm ($ARCH) install in clean containers"
for p in deb rpm; do
    fetch "$DL/cyoda_linux_${ARCH}.$p" "$WORK/pkg/cyoda_linux_${ARCH}.$p"
    check_sum "$WORK/pkg/cyoda_linux_${ARCH}.$p"
done
# stdout is the command's output; Docker's own messages (an image pull) go
# to stderr, which is shown only on failure.
out=$("$DOCKER" run --rm -v "$WORK/pkg:/pkg:ro" "$DEB_IMAGE" sh -c \
    "dpkg -i /pkg/cyoda_linux_${ARCH}.deb >/dev/null && grep -q '^CYODA_STORAGE_BACKEND=sqlite' /etc/cyoda/cyoda.env && cyoda --version" 2> "$WORK/docker.log") \
    || { tail -n 5 "$WORK/docker.log" >&2; die ".deb did not install in $DEB_IMAGE"; }
case "$out" in "cyoda version $V "*) ok ".deb: $out" ;; *) die ".deb installed: $out" ;; esac
out=$("$DOCKER" run --rm -v "$WORK/pkg:/pkg:ro" "$RPM_IMAGE" sh -c \
    "rpm -i /pkg/cyoda_linux_${ARCH}.rpm && grep -q '^CYODA_STORAGE_BACKEND=sqlite' /etc/cyoda/cyoda.env && cyoda --version" 2> "$WORK/docker.log") \
    || { tail -n 5 "$WORK/docker.log" >&2; die ".rpm did not install in $RPM_IMAGE"; }
case "$out" in "cyoda version $V "*) ok ".rpm: $out" ;; *) die ".rpm installed: $out" ;; esac

step "the image $IMAGE:$V verifies and runs by its digest"
D=$(verify_image "$IMAGE:$V")
out=$(run_version "$IMAGE@$D")
ok "$IMAGE@$D: $out"

if [ "$CHECK_LATEST" = 1 ]; then
    step "$TAG is GitHub's Latest release, and the unpinned routes give it"
    latest=$(github_api releases/latest | jq -r .tag_name) || die "api.github.com did not answer for the Latest release"
    if [ "$latest" != "$TAG" ]; then
        case "$latest" in
            v[0-9]*) newer=$(printf '%s\n' "${latest#v}" "$V" | sort -V | tail -n1) ;;
            *) newer=$V ;;
        esac
        if [ "$newer" = "$V" ]; then
            die "GitHub's Latest release is '$latest', not $TAG; restore it: gh release edit $TAG --repo $REPO --latest"
        fi
        die "$TAG is not the newest stable release (Latest is $latest): check it without --latest"
    fi
    fetch "https://github.com/$REPO/releases/latest/download/SHA256SUMS" "$WORK/latest-SHA256SUMS"
    cmp -s "$WORK/latest-SHA256SUMS" "$WORK/dl/SHA256SUMS" || die "releases/latest/download/SHA256SUMS is not $TAG's"
    fetch "https://github.com/$REPO/releases/latest/download/install.sh" "$WORK/latest-install.sh"
    cmp -s "$WORK/latest-install.sh" "$WORK/dl/install.sh" || [ -n "${INSTALL_SH:-}" ] \
        || die "releases/latest/download/install.sh is not $TAG's"
    [ -z "${INSTALL_SH:-}" ] || cp "$INSTALL_SH" "$WORK/latest-install.sh"
    CYODA_COSIGN_VERIFY=required CYODA_INSTALL_DIR="$WORK/latest-bin" \
        sh "$WORK/latest-install.sh" > "$WORK/latest-install.log" 2>&1 || { cat "$WORK/latest-install.log" >&2; die "the unpinned install.sh failed"; }
    "$WORK/latest-bin/cyoda" --version | grep -q "^cyoda version $V " || die "the unpinned install.sh installed $("$WORK/latest-bin/cyoda" --version)"
    # `latest` is built and signed separately (its own digest) by the same
    # release run: verify it against this tag and run it.
    DLATEST=$(verify_image "$IMAGE:latest")
    out=$(run_version "$IMAGE@$DLATEST")
    ok "Latest is $TAG; latest/download SHA256SUMS and install.sh, the unpinned install.sh and $IMAGE:latest give $V"
fi

if [ -n "$CHART" ] || [ "$CHART_IF_PUBLISHED" = 1 ]; then
    helm repo add cyoda-verify https://cyoda.github.io/cyoda-go >/dev/null || die "helm repo add failed"
fi
# pull_chart C: the chart C downloads from the repository and renders.
pull_chart() {
    mkdir -p "$WORK/chart-$1"
    helm pull cyoda-verify/cyoda --version "$1" -d "$WORK/chart-$1" > /dev/null 2> "$WORK/helm.log" \
        || { tail -n 5 "$WORK/helm.log" >&2; die "chart $1 is listed but does not download"; }
    helm template cyoda "$WORK/chart-$1/cyoda-$1.tgz" --set postgres.existingSecret=x \
        --set jwt.existingSecret=x --set cluster.hmacSecret.existingSecret=x \
        --set monitoring.metricsBearer.existingSecret=x --set gateway.enabled=false > /dev/null 2> "$WORK/helm.log" \
        || { tail -n 5 "$WORK/helm.log" >&2; die "chart $1 does not render"; }
}
if [ -n "$CHART" ]; then
    step "the Helm repository serves chart $CHART with appVersion $V"
    # GitHub Pages can serve the new index a few minutes after the chart
    # release: retry for up to five minutes before failing.
    app=""
    for _ in 1 2 3 4 5 6 7 8 9 10; do
        helm repo update cyoda-verify >/dev/null 2>&1 || true
        app=$(helm search repo cyoda-verify/cyoda --version "$CHART" -o json | jq -r '.[0].app_version // empty') || app=""
        [ -n "$app" ] && break
        sleep 30
    done
    [ -n "$app" ] || die "chart $CHART is not served after five minutes"
    [ "$app" = "$V" ] || die "chart $CHART has appVersion '$app', not $V"
    pull_chart "$CHART"
    ok "chart $CHART, appVersion $app, downloads and renders"
elif [ "$CHART_IF_PUBLISHED" = 1 ]; then
    step "every chart with appVersion $V downloads and renders"
    charts=$(helm search repo cyoda-verify/cyoda --versions -o json | jq -r --arg v "$V" '[.[] | select(.app_version == $v) | .version] | join(" ")') \
        || die "helm search failed"
    if [ -n "$charts" ]; then
        for c in $charts; do pull_chart "$c"; done
        ok "chart(s) $charts with appVersion $V download and render"
    else
        ok "no chart has appVersion $V (a release may ship without one)"
    fi
fi

printf '\nverify-release: %s passed every check.\n' "$TAG"
