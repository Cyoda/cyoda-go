#!/bin/bash
#
# Start a multi-node cyoda-go cluster with nginx load balancer.
# Handles secret generation, nginx config, and docker-compose orchestration.
#
# Usage: ./start-cluster.sh [NUM_NODES]       (default: 3)
#        ./start-cluster.sh --nodes NUM_NODES
#

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

# Parse arguments: accept both positional and --nodes/--profile flags
NUM_NODES=3
PROFILE="${PROFILE:-postgres}"
while [[ $# -gt 0 ]]; do
    case "$1" in
        --nodes)  NUM_NODES="$2"; shift 2 ;;
        --nodes=*) NUM_NODES="${1#*=}"; shift ;;
        --profile)  PROFILE="$2"; shift 2 ;;
        --profile=*) PROFILE="${1#*=}"; shift ;;
        -d|--detach) EXTRA_ARGS+=("$1"); shift ;;
        [0-9]*) NUM_NODES="$1"; shift ;;
        *) EXTRA_ARGS+=("$1"); shift ;;
    esac
done

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

if [[ "$NUM_NODES" -lt 1 || "$NUM_NODES" -gt 20 ]]; then
    log_error "--nodes must be between 1 and 20 (got $NUM_NODES)"
    exit 1
fi

case "$PROFILE" in
    postgres|sqlite|memory) ;;
    *) log_error "--profile must be one of: postgres, sqlite, memory (got '$PROFILE')"; exit 1 ;;
esac

# sqlite and memory are single-node backends — each container gets isolated
# storage, so multi-node would be N independent instances behind the LB, not
# a real cluster. Only postgres shares state across nodes.
if [[ "$NUM_NODES" -gt 1 && "$PROFILE" != "postgres" ]]; then
    log_error "Profile '$PROFILE' does not support multi-node (per-node isolated storage)."
    log_error "Use --nodes 1 with this profile, or --profile postgres for a real cluster."
    exit 1
fi

log_info "Preparing cyoda cluster with $NUM_NODES node(s) — profile: $PROFILE"

# ── Secrets (generate once, persist to .env, reuse on restart) ────────
#
# Load order matters: profile overlay BEFORE base .env so user-supplied
# CYODA_* values take precedence over previously-persisted auto-generated
# values. Precedence: overlay CYODA_* > persisted .env > auto-generated.
ENV_FILE="$SCRIPT_DIR/.env"
PROFILE_ENV_FILE="$SCRIPT_DIR/.env.$PROFILE"

if [[ -f "$PROFILE_ENV_FILE" ]]; then
    log_info "Loading profile overlay from $PROFILE_ENV_FILE"
    # shellcheck disable=SC1090
    source "$PROFILE_ENV_FILE"
fi

if [[ -f "$ENV_FILE" ]]; then
    log_info "Loading persisted secrets from $ENV_FILE"
    # shellcheck disable=SC1090
    source "$ENV_FILE"
else
    log_info "First run — generating secrets and saving to $ENV_FILE"
fi

# Resolve each with full precedence chain: overlay CYODA_* > persisted > default/generate.
JWT_KEY_B64="${CYODA_JWT_SIGNING_KEY:-${JWT_KEY_B64:-$(openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 2>/dev/null | base64 | tr -d '\n')}}"
HMAC_SECRET="${CYODA_HMAC_SECRET:-${HMAC_SECRET:-$(openssl rand -hex 32)}}"
BOOTSTRAP_CLIENT_ID="${CYODA_BOOTSTRAP_CLIENT_ID:-${BOOTSTRAP_CLIENT_ID:-m2m.user}}"
BOOTSTRAP_CLIENT_SECRET="${CYODA_BOOTSTRAP_CLIENT_SECRET:-${BOOTSTRAP_CLIENT_SECRET:-$(openssl rand -hex 32)}}"
BOOTSTRAP_TENANT_ID="${CYODA_BOOTSTRAP_TENANT_ID:-${BOOTSTRAP_TENANT_ID:-riskblocs}}"
BOOTSTRAP_ROLES="${CYODA_BOOTSTRAP_ROLES:-${BOOTSTRAP_ROLES:-ROLE_ADMIN,ROLE_M2M}}"

# Persist for next run
cat > "$ENV_FILE" <<ENVEOF
# Auto-generated cluster config — stable across restarts. Delete this file to regenerate.
JWT_KEY_B64=${JWT_KEY_B64}
HMAC_SECRET=${HMAC_SECRET}
BOOTSTRAP_CLIENT_ID=${BOOTSTRAP_CLIENT_ID}
BOOTSTRAP_CLIENT_SECRET=${BOOTSTRAP_CLIENT_SECRET}
BOOTSTRAP_TENANT_ID=${BOOTSTRAP_TENANT_ID}
BOOTSTRAP_ROLES=${BOOTSTRAP_ROLES}
ENVEOF

# ── Ports (from env, falling back to single-node defaults) ───────────
HTTP_PORT="${CYODA_HTTP_PORT:-8123}"
GRPC_PORT="${CYODA_GRPC_PORT:-9123}"
GOSSIP_PORT=7946
LB_HTTP_PORT="$HTTP_PORT"
LB_GRPC_PORT="$GRPC_PORT"

# ── Build local image from source ────────────────────────────────────
# The canonical Dockerfile (deploy/docker/Dockerfile) is a distroless runtime
# that expects a pre-staged binary at $TARGETPLATFORM/cyoda — the goreleaser
# convention. Mirror the staging pattern from scripts/dev/run-docker-dev.sh:
# build the binary once for the host arch, stage it, then buildx the image
# and reference it by tag from compose. One image, N containers share it.
LOCAL_IMAGE_TAG="ghcr.io/cyoda/cyoda:multi-node-dev"

case "$(uname -m)" in
    x86_64|amd64) HOST_ARCH=amd64 ;;
    aarch64|arm64) HOST_ARCH=arm64 ;;
    *) log_error "unsupported arch: $(uname -m)"; exit 1 ;;
esac
HOST_PLATFORM="linux/$HOST_ARCH"

BUILDCTX="$PROJECT_ROOT/.buildctx"
trap 'rm -rf "$BUILDCTX"' EXIT
rm -rf "$BUILDCTX"
mkdir -p "$BUILDCTX/$HOST_PLATFORM"

log_info "Building cyoda binary for $HOST_PLATFORM..."
# Build from PROJECT_ROOT so `go` discovers this repo's go.work, not one
# that might exist in the invoker's cwd.
(cd "$PROJECT_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH="$HOST_ARCH" \
    go build -ldflags="-s -w" -o "$BUILDCTX/$HOST_PLATFORM/cyoda" \
    ./cmd/cyoda)

log_info "Building image $LOCAL_IMAGE_TAG..."
docker buildx build --load \
    --platform "$HOST_PLATFORM" \
    -t "$LOCAL_IMAGE_TAG" \
    -f "$PROJECT_ROOT/deploy/docker/Dockerfile" \
    "$BUILDCTX" > /dev/null

# ── Seed nodes: first min(N,3) ──────────────────────────────────────
SEED_COUNT=$((NUM_NODES < 3 ? NUM_NODES : 3))
SEED_LIST=""
for i in $(seq 1 "$SEED_COUNT"); do
    [[ -n "$SEED_LIST" ]] && SEED_LIST+=","
    SEED_LIST+="cyoda-node-${i}:${GOSSIP_PORT}"
done

# ── Generate nginx.conf ─────────────────────────────────────────────
generate_nginx_conf() {
    local num_nodes=$1
    local nginx_conf="$SCRIPT_DIR/nginx.conf"

    log_info "Generating nginx.conf for $num_nodes nodes..."

    cat > "$nginx_conf" << 'NGINX_HEADER'
worker_processes auto;

events {
    worker_connections 4096;
    multi_accept on;
}

http {
    log_format upstream_log '[$time_local] '
                          '$remote_addr -> $upstream_addr '
                          '"$request" $status '
                          'upstream_status=$upstream_status '
                          'upstream_time=$upstream_response_time';

    access_log /dev/stdout upstream_log;
    error_log  /dev/stderr;

    client_max_body_size 16m;

    # HTTP upstream for cyoda nodes
    upstream minicyoda_http {
NGINX_HEADER

    for i in $(seq 1 "$num_nodes"); do
        echo "        server cyoda-node-${i}:${HTTP_PORT} max_fails=3 fail_timeout=30s;" >> "$nginx_conf"
    done

    cat >> "$nginx_conf" << 'NGINX_HTTP_FOOTER'
        keepalive 32;
        keepalive_timeout 300s;
        keepalive_requests 10000;
    }

    # gRPC upstream for cyoda nodes
    upstream minicyoda_grpc {
NGINX_HTTP_FOOTER

    for i in $(seq 1 "$num_nodes"); do
        echo "        server cyoda-node-${i}:${GRPC_PORT} max_fails=3 fail_timeout=30s;" >> "$nginx_conf"
    done

    cat >> "$nginx_conf" << NGINX_FOOTER
    }

    # HTTP server
    server {
        listen ${HTTP_PORT};
        server_name localhost;

        location /health {
            access_log off;
            return 200 'OK';
            add_header Content-Type text/plain;
        }

        location / {
            proxy_pass http://minicyoda_http;
            proxy_http_version 1.1;
            proxy_set_header Host \$host;
            proxy_set_header X-Real-IP \$remote_addr;
            proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
            proxy_set_header X-Forwarded-Proto \$scheme;
            proxy_set_header X-Tx-Token \$http_x_tx_token;
            proxy_connect_timeout 10s;
            proxy_send_timeout 60s;
            proxy_read_timeout 60s;
            proxy_buffer_size 128k;
            proxy_buffers 4 256k;
            proxy_busy_buffers_size 256k;
        }
    }

    # gRPC server (HTTP/2 for grpc_pass)
    server {
        listen ${GRPC_PORT} http2;
        server_name localhost;

        location / {
            grpc_pass grpc://minicyoda_grpc;
            grpc_read_timeout 3600s;
            grpc_send_timeout 3600s;
            grpc_connect_timeout 10s;
            grpc_socket_keepalive on;
            grpc_set_header X-Real-IP \$remote_addr;
            grpc_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
            grpc_next_upstream off;
            error_page 502 503 504 = @grpc_error;
        }

        location @grpc_error {
            internal;
            default_type application/grpc;
            add_header grpc-status 14;
            add_header grpc-message "Backend service temporarily unavailable";
            add_header content-type application/grpc;
            return 204;
        }
    }
}
NGINX_FOOTER
}

# ── Profile → compose fragments ───────────────────────────────────────
# Four variables parameterize the compose template:
#   BACKEND_ENV      — CYODA_STORAGE_BACKEND + connection vars, indented 2sp.
#   BACKEND_SERVICE  — extra top-level service (postgres) or empty.
#   NODE_DEPENDS     — per-node depends_on block or empty.
#   TOP_VOLUMES      — named volume declarations under the top-level volumes:.
# NODE_VOLS (per-node volume mount) is computed inline per node.
case "$PROFILE" in
    postgres)
        BACKEND_ENV='  CYODA_STORAGE_BACKEND: "postgres"
  CYODA_POSTGRES_URL: "postgres://minicyoda:minicyoda@postgres:5432/minicyoda?sslmode=disable"
  CYODA_POSTGRES_AUTO_MIGRATE: "true"'
        BACKEND_SERVICE='  postgres:
    image: postgres:17-alpine
    container_name: minicyoda-postgres
    environment:
      POSTGRES_DB: minicyoda
      POSTGRES_USER: minicyoda
      POSTGRES_PASSWORD: minicyoda
    ports:
      - "127.0.0.1:5432:5432"
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U minicyoda -d minicyoda"]
      interval: 2s
      timeout: 5s
      retries: 10
    networks:
      - minicyoda-network
'
        NODE_DEPENDS='    depends_on:
      postgres:
        condition: service_healthy'
        TOP_VOLUMES='  pgdata:'
        ;;
    sqlite)
        BACKEND_ENV='  CYODA_STORAGE_BACKEND: "sqlite"
  CYODA_SQLITE_PATH: "/var/lib/cyoda/cyoda.db"
  CYODA_SQLITE_AUTO_MIGRATE: "true"'
        BACKEND_SERVICE=''
        NODE_DEPENDS=''
        TOP_VOLUMES='  cyoda-data:'
        ;;
    memory)
        BACKEND_ENV='  CYODA_STORAGE_BACKEND: "memory"'
        BACKEND_SERVICE=''
        NODE_DEPENDS=''
        TOP_VOLUMES=''
        ;;
esac

# ── Generate docker-compose.generated.yml ────────────────────────────
generate_docker_compose() {
    local num_nodes=$1
    local compose_file="$SCRIPT_DIR/docker-compose.generated.yml"

    log_info "Generating docker-compose.generated.yml for $num_nodes nodes..."

    cat > "$compose_file" << COMPOSE_HEADER
# Auto-generated by start-cluster.sh — do not edit manually.
# Profile: ${PROFILE}

x-minicyoda-common: &minicyoda-common
  image: ${LOCAL_IMAGE_TAG}
  networks:
    - minicyoda-network

x-minicyoda-env: &minicyoda-env
  CYODA_HTTP_PORT: "${HTTP_PORT}"
  CYODA_GRPC_PORT: "${GRPC_PORT}"
  CYODA_LOG_LEVEL: "info"
${BACKEND_ENV}
  CYODA_IAM_MODE: "jwt"
  CYODA_JWT_SIGNING_KEY: "${JWT_KEY_B64}"
  CYODA_BOOTSTRAP_CLIENT_ID: "${BOOTSTRAP_CLIENT_ID}"
  CYODA_BOOTSTRAP_CLIENT_SECRET: "${BOOTSTRAP_CLIENT_SECRET}"
  CYODA_BOOTSTRAP_TENANT_ID: "${BOOTSTRAP_TENANT_ID}"
  CYODA_BOOTSTRAP_ROLES: "${BOOTSTRAP_ROLES}"
  CYODA_OTEL_ENABLED: "true"
  OTEL_EXPORTER_OTLP_ENDPOINT: "http://otel-backend:4318"

services:
  # WARNING: Grafana is unauthenticated by default. Do NOT expose to untrusted networks.
  otel-backend:
    image: grafana/otel-lgtm:latest
    container_name: minicyoda-otel
    ports:
      - "127.0.0.1:3000:3000"
    volumes:
      - ${PROJECT_ROOT}/scripts/grafana/dashboards:/otel-lgtm/grafana/dashboards/cyoda-go:ro
      - ${PROJECT_ROOT}/scripts/grafana/provisioning/dashboards/default.yml:/otel-lgtm/grafana/conf/provisioning/dashboards/cyoda-go.yml:ro
    networks:
      - minicyoda-network

${BACKEND_SERVICE}
  load-balancer:
    image: nginx:alpine
    container_name: minicyoda-lb
    ports:
      - "${LB_HTTP_PORT}:${LB_HTTP_PORT}"
      - "${LB_GRPC_PORT}:${LB_GRPC_PORT}"
    volumes:
      - ./nginx.conf:/etc/nginx/nginx.conf:ro
    depends_on:
COMPOSE_HEADER

    for i in $(seq 1 "$num_nodes"); do
        cat >> "$compose_file" << DEPENDS
      cyoda-node-${i}:
        condition: service_started
DEPENDS
    done

    cat >> "$compose_file" << 'LB_FOOTER'
    healthcheck:
      test: ["CMD", "wget", "-q", "--spider", "http://localhost:${LB_HTTP_PORT}/health"]
      interval: 10s
      timeout: 5s
      retries: 3
    networks:
      - minicyoda-network

LB_FOOTER

    # Per-node volume mount (sqlite needs a writable data dir; postgres/memory
    # do not). Declared in the template so `yq` or a reader can see it per node.
    if [[ "$PROFILE" == "sqlite" ]]; then
        NODE_VOLS='    volumes:
      - cyoda-data:/var/lib/cyoda'
    else
        NODE_VOLS=''
    fi

    # Generate each node
    for i in $(seq 1 "$num_nodes"); do
        if [[ "$num_nodes" -gt 1 ]]; then
            cat >> "$compose_file" << NODE_CLUSTER
  cyoda-node-${i}:
    <<: *minicyoda-common
    container_name: minicyoda-node${i}
    environment:
      <<: *minicyoda-env
      CYODA_CLUSTER_ENABLED: "true"
      CYODA_NODE_ID: "node-${i}"
      CYODA_NODE_ADDR: "http://cyoda-node-${i}:${HTTP_PORT}"
      CYODA_GOSSIP_ADDR: "0.0.0.0:${GOSSIP_PORT}"
      CYODA_SEED_NODES: "${SEED_LIST}"
      CYODA_HMAC_SECRET: "${HMAC_SECRET}"
      CYODA_TX_TTL: "60s"
${NODE_VOLS}
${NODE_DEPENDS}

NODE_CLUSTER
        else
            cat >> "$compose_file" << NODE_SINGLE
  cyoda-node-${i}:
    <<: *minicyoda-common
    container_name: minicyoda-node${i}
    environment:
      <<: *minicyoda-env
${NODE_VOLS}
${NODE_DEPENDS}

NODE_SINGLE
        fi
    done

    # Only emit the top-level `volumes:` block when there are volumes to
    # declare — memory profile has none, and `volumes:` with no children
    # trips some strict YAML parsers.
    if [[ -n "$TOP_VOLUMES" ]]; then
        cat >> "$compose_file" << VOLUMES_BLOCK
volumes:
${TOP_VOLUMES}

VOLUMES_BLOCK
    fi

    cat >> "$compose_file" << 'NETWORKS_BLOCK'
networks:
  minicyoda-network:
    driver: bridge
NETWORKS_BLOCK
}

# ── Generate ──────────────────────────────────────────────────────────
generate_nginx_conf "$NUM_NODES"
generate_docker_compose "$NUM_NODES"

log_info "Starting cluster with $NUM_NODES node(s)..."
log_info "Endpoints:"
log_info "  HTTP: http://localhost:${LB_HTTP_PORT}"
log_info "  gRPC: localhost:${LB_GRPC_PORT}"
if [[ "$NUM_NODES" -gt 1 ]]; then
    log_info "Cluster:"
    log_info "  Seed nodes: $(seq -f 'node-%.0f' -s ', ' 1 "$SEED_COUNT")"
    log_info "  HMAC secret: ${HMAC_SECRET:0:8}..."
fi

cd "$SCRIPT_DIR"
docker compose -f docker-compose.generated.yml up "${EXTRA_ARGS[@]}"
