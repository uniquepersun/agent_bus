#!/usr/bin/env bash
# Agent Bus - General Quickstart & Multi-Agent Connector
set -e

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN_DIR="$DIR/bin"

# Helper: Detect LAN IP (macOS and Linux)
get_ip() {
  local ip=""
  if command -v ipconfig >/dev/null 2>&1; then
    ip=$(ipconfig getifaddr en0 2>/dev/null || ipconfig getifaddr en1 2>/dev/null || true)
  fi
  if [ -z "$ip" ] && command -v hostname >/dev/null 2>&1; then
    ip=$(hostname -I 2>/dev/null | awk '{print $1}' || true)
  fi
  if [ -z "$ip" ]; then
    ip="localhost"
  fi
  echo "$ip"
}

# Helper: Ensure binaries are compiled
ensure_build() {
  if [ ! -f "$BIN_DIR/busd" ] || [ ! -f "$BIN_DIR/bus-daemon" ] || [ ! -f "$BIN_DIR/bus" ]; then
    echo "⚙️  Building Agent Bus binaries..."
    export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"
    (cd "$DIR" && go build -o bin/ ./cmd/...)
    echo "✅ Binaries built successfully."
  fi
}

usage() {
  echo ""
  echo "🚌 Agent Bus - Generalized Multi-Agent Manager"
  echo "-----------------------------------------------"
  echo "Usage: ./quickstart.sh [command] [options]"
  echo ""
  echo "Commands:"
  echo "  host                           Start the central server on this machine"
  echo "  join <role> [server] [repo]    Join as ANY role (e.g. frontend, backend, database, devops)"
  echo ""
  echo "Role Shortcuts:"
  echo "  frontend [server-url]          Shortcut for: join frontend"
  echo "  backend  [server-url]          Shortcut for: join backend"
  echo "  database [server-url]          Shortcut for: join database"
  echo ""
  echo "Utilities:"
  echo "  demo                           Launch a 3-agent local demo (frontend, backend, database)"
  echo "  status [server-url]            Check connectivity, registered agents & contracts"
  echo "  stop                           Stop all background bus processes"
  echo ""
  echo "Examples:"
  echo "  ./quickstart.sh host"
  echo "  ./quickstart.sh database http://10.115.30.19:7070"
  echo "  ./quickstart.sh join auth@priya http://10.115.30.19:7070 ~/projects/auth-service"
  echo ""
  exit 1
}

# 1. Start Server (Host)
cmd_host() {
  ensure_build
  local ip
  ip=$(get_ip)
  local port="7070"

  echo ""
  echo "=================================================="
  echo " 🚀 AGENT BUS CENTRAL SERVER"
  echo "=================================================="
  echo " Local Dashboard:   http://localhost:$port"
  if [ "$ip" != "localhost" ]; then
    echo " Network Dashboard: http://$ip:$port"
  fi
  echo "=================================================="
  echo ""
  echo "👉 Commands for your teammates to join:"
  echo "   Backend:  ./quickstart.sh backend  http://$ip:$port"
  echo "   Frontend: ./quickstart.sh frontend http://$ip:$port"
  echo "   Database: ./quickstart.sh database http://$ip:$port"
  echo "   Custom:   ./quickstart.sh join <any-role> http://$ip:$port"
  echo "=================================================="
  echo ""

  cd "$DIR"
  exec "$BIN_DIR/busd" -addr "0.0.0.0:$port"
}

# 2. General Join (Supports ANY role: frontend, backend, database, devops, etc.)
cmd_join() {
  ensure_build

  local role_input="$1"
  shift || true

  if [ -z "$role_input" ]; then
    read -rp "Enter your role (e.g. frontend, backend, database): " role_input
  fi

  # Normalize role and id
  local role=""
  local agent_id=""
  if [[ "$role_input" == *"@"* ]]; then
    agent_id="$role_input"
    role="${role_input%%@*}"
  else
    role="$role_input"
    local human="${USER:-teammate}"
    # Map common default identities if defined in bus.json
    if [ "$role" = "frontend" ]; then
      agent_id="frontend@teammate"
    elif [ "$role" = "backend" ]; then
      agent_id="backend@abhay"
    elif [ "$role" = "database" ] || [ "$role" = "db" ]; then
      role="database"
      agent_id="database@teammate3"
    else
      agent_id="${role}@${human}"
    fi
  fi

  local server="${1:-http://localhost:7070}"
  local repo="${2:-$HOME/Desktop/test-$role}"

  # Mandate resolution
  local mandate_file="$DIR/mandates/${role}.md"
  if [ ! -f "$mandate_file" ]; then
    # Auto-generate custom mandate if one doesn't exist yet
    mkdir -p "$DIR/mandates"
    cat <<EOF > "$mandate_file"
Scope: ${role}/
I care about: stability, performance, backward compatibility
Auto-accept: additive changes, non-breaking modifications
Ask me: breaking contract changes, deleting resources, auth/security changes
EOF
    echo "📝 Created default mandate for '$role' in $mandate_file"
  fi

  mkdir -p "$repo"
  echo ""
  echo "=================================================="
  echo " 💻 CONNECTING AS: $agent_id"
  echo " Server:  $server"
  echo " Repo:    $repo"
  echo " Mandate: $mandate_file"
  echo "=================================================="

  # Install Claude Code hooks if Claude / binary is available
  if [ -x "$BIN_DIR/bus-hook" ]; then
    echo "📦 Configuring Claude Code hooks in $repo..."
    "$BIN_DIR/bus-hook" install -server "$server" -as "$agent_id" -project "$repo" >/dev/null 2>&1 || true
    echo "✅ Hooks installed."
  fi

  if command -v claude >/dev/null 2>&1; then
    (cd "$repo" && claude mcp add agent-bus -- "$BIN_DIR/bus-mcp" -server "$server" -as "$agent_id" -role worker >/dev/null 2>&1 || true)
  fi

  echo "🚀 Starting Daemon for $agent_id (press Ctrl+C to stop)..."
  cd "$DIR"
  exec "$BIN_DIR/bus-daemon" \
    -as "$agent_id" \
    -mandate "$mandate_file" \
    -repo "$repo" \
    -server "$server"
}

# 3. Check Status
cmd_status() {
  ensure_build
  local server="${1:-http://localhost:7070}"
  echo "🔍 Checking status on $server..."
  if curl -s --connect-timeout 2 "$server/api/contracts" >/dev/null 2>&1; then
    echo "✅ Bus server is reachable at $server"
    echo ""
    echo "📋 Registered Contracts:"
    curl -s "$server/api/contracts"
    echo ""
    echo ""
    echo "💬 Active Threads:"
    "$BIN_DIR/bus" -server "$server" threads || true
  else
    echo "❌ Could not connect to $server"
    echo "Make sure the server is started with: ./quickstart.sh host"
  fi
}

# 4. Stop running background bus processes
cmd_stop() {
  echo "Stopping background bus processes..."
  pkill -f "busd" 2>/dev/null && echo "Stopped busd" || echo "No busd running"
  pkill -f "bus-daemon" 2>/dev/null && echo "Stopped bus-daemon" || echo "No bus-daemon running"
  echo "Done."
}

# 5. One-click 3-Agent Demo (Frontend, Backend, Database)
cmd_demo() {
  ensure_build
  echo "🎭 Setting up 3-agent test simulation (Frontend, Backend, Database)..."
  cmd_stop >/dev/null 2>&1 || true
  sleep 1

  # Start busd in background
  cd "$DIR"
  nohup "$BIN_DIR/busd" -addr "0.0.0.0:7070" </dev/null >/dev/null 2>&1 &
  sleep 1

  echo "1️⃣  Server running at http://localhost:7070"

  local fe_dir="$HOME/Desktop/test-frontend"
  local be_dir="$HOME/Desktop/test-backend"
  local db_dir="$HOME/Desktop/test-database"
  mkdir -p "$fe_dir" "$be_dir" "$db_dir"

  # Start daemons for all 3 agents in background
  nohup "$BIN_DIR/bus-daemon" -as frontend@teammate -mandate "$DIR/mandates/frontend.md" -repo "$fe_dir" -server "http://localhost:7070" </dev/null >/dev/null 2>&1 &
  nohup "$BIN_DIR/bus-daemon" -as backend@abhay -mandate "$DIR/mandates/backend.md" -repo "$be_dir" -server "http://localhost:7070" </dev/null >/dev/null 2>&1 &
  nohup "$BIN_DIR/bus-daemon" -as database@teammate3 -mandate "$DIR/mandates/database.md" -repo "$db_dir" -server "http://localhost:7070" </dev/null >/dev/null 2>&1 &
  sleep 2

  echo "2️⃣  Frontend daemon running in: $fe_dir"
  echo "3️⃣  Backend daemon running in:  $be_dir"
  echo "4️⃣  Database daemon running in: $db_dir"
  echo ""
  echo "✅ All 3 agents are connected and syncing contracts (openapi.yaml, schema.sql, env.md)!"
  echo "Open dashboard: http://localhost:7070"
  echo "To stop this demo, run: ./quickstart.sh stop"
}

# Main routing
case "$1" in
  host|server)
    cmd_host
    ;;
  join)
    shift
    cmd_join "$@"
    ;;
  frontend|fe)
    shift
    cmd_join frontend "$@"
    ;;
  backend|be)
    shift
    cmd_join backend "$@"
    ;;
  database|db)
    shift
    cmd_join database "$@"
    ;;
  status)
    shift
    cmd_status "$@"
    ;;
  stop)
    cmd_stop
    ;;
  demo)
    cmd_demo
    ;;
  *)
    usage
    ;;
esac