#!/usr/bin/env bash
# Agent Bus launcher: one command per machine.
#
#   ./demo.sh server                         start the bus and dashboard on this machine
#   ./demo.sh laptop -as ID -project DIR     set up and start one person's side, then open their agent
#   ./demo.sh rehearse                       run both sides on this machine (agy), in two Terminal windows
#
#   ./demo.sh showcase server                the showcase tasks app: bus with its contracts and mandates
#   ./demo.sh showcase backend|frontend -server URL [-agent claude] [-project DIR]
#                                            create the showcase project (default ~/Desktop/test-<side>)
#                                            if needed, then start that side
#
# laptop options:
#   -as ID              agent identity from bus.json, e.g. backend@abhay or frontend@teammate
#   -project DIR        the code repository this person's agent works in
#   -server URL         bus address (default http://localhost:7070)
#   -agent agy|claude   coding agent for this side (default agy)
#   -mandate FILE       mandate file (default mandates/<backend|frontend>.md from the identity)
#   -allow-command CMD  agy: a command the worker may run, e.g. "npm test" (repeatable; grep is always allowed)
#
# server options:
#   -port N             port to listen on (default 7070)
#   -fresh              start from an empty ledger
#   -config FILE        agents and contracts (default bus.json)
#   -db FILE            ledger file (default bus.db)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN="$ROOT/bin"

say() { printf '\n== %s\n' "$*"; }
die() { printf 'demo.sh: %s\n' "$*" >&2; exit 1; }

build() {
  # Release archives ship prebuilt binaries; build from source otherwise.
  if [ -x "$BIN/busd" ] && [ ! -f "$ROOT/go.mod" ]; then return; fi
  command -v go >/dev/null || { [ -x "$BIN/busd" ] && return; die "Go is required to build (https://go.dev/dl)"; }
  say "Building"
  (cd "$ROOT" && go build -o "$BIN/" ./cmd/...)
}

lan_ip() {
  ipconfig getifaddr en0 2>/dev/null || ipconfig getifaddr en1 2>/dev/null || hostname -I 2>/dev/null | awk '{print $1}' || echo localhost
}

cmd_server() {
  local port=7070 fresh=0 config=bus.json db=bus.db
  while [ $# -gt 0 ]; do
    case "$1" in
      -port) port="$2"; shift 2 ;;
      -fresh) fresh=1; shift ;;
      -config) config="$2"; shift 2 ;;
      -db) db="$2"; shift 2 ;;
      *) die "unknown server option $1" ;;
    esac
  done
  build
  [ "$fresh" = 1 ] && rm -f "$ROOT/$db" "$ROOT/$db"-*
  local ip; ip="$(lan_ip)"
  say "Agent Bus server"
  echo "Dashboard:      http://$ip:$port   (open this on the projector)"
  echo "Bus address:    http://$ip:$port   (give this to each laptop as -server)"
  echo "Stop with Ctrl+C."
  cd "$ROOT" && exec "$BIN/busd" -addr "0.0.0.0:$port" -config "$config" -db "$db"
}

cmd_laptop() {
  local as="" project="" server="http://localhost:7070" agent="agy" mandate="" commands=(-allow-command grep)
  while [ $# -gt 0 ]; do
    case "$1" in
      -as) as="$2"; shift 2 ;;
      -project) project="$2"; shift 2 ;;
      -server) server="$2"; shift 2 ;;
      -agent) agent="$2"; shift 2 ;;
      -mandate) mandate="$2"; shift 2 ;;
      -allow-command) commands+=(-allow-command "$2"); shift 2 ;;
      *) die "unknown laptop option $1" ;;
    esac
  done
  [ -n "$as" ] || die "-as is required, e.g. -as frontend@teammate"
  [ -n "$project" ] || die "-project is required: the repository this agent works in"
  case "$agent" in agy|claude) ;; *) die "-agent must be agy or claude" ;; esac
  command -v "$agent" >/dev/null || die "$agent is not installed or not on PATH"
  mkdir -p "$project"
  project="$(cd "$project" && pwd)"
  [ -d "$project/.git" ] || git -C "$project" init -q
  if [ -z "$mandate" ]; then mandate="$ROOT/mandates/${as%%@*}.md"; fi
  [ -f "$mandate" ] || die "mandate $mandate not found; pass -mandate"
  build

  curl -fsS "$server/api/state" >/dev/null || die "cannot reach the bus at $server; start it with ./demo.sh server"

  say "Installing the $agent worker integration for $as in $project"
  if [ "$agent" = agy ]; then
    "$BIN/bus-hook" install -agent agy -server "$server" -as "$as" -project "$project" "${commands[@]}"
  else
    "$BIN/bus-hook" install -server "$server" -as "$as" -project "$project"
    (cd "$project" && claude mcp remove agent-bus >/dev/null 2>&1 || true
     cd "$project" && claude mcp add agent-bus -- "$BIN/bus-mcp" -server "$server" -as "$as" -role worker)
  fi
  for ignore in .agentbus/ .agents/plugins/agent-bus/; do
    grep -qxF "$ignore" "$project/.gitignore" 2>/dev/null || echo "$ignore" >> "$project/.gitignore"
  done

  say "Starting the daemon ($agent liaisons); log: $project/.agentbus/daemon.log"
  mkdir -p "$project/.agentbus"
  local blob_flag=()
  if command -v blob >/dev/null; then
    blob_flag=(-blob)
    echo "blob is installed: run 'blob' in another tab to play; it pauses when your liaison needs you."
  fi
  "$BIN/bus-daemon" -server "$server" -as "$as" -mandate "$mandate" -repo "$project" -liaison "$agent" ${blob_flag[@]+"${blob_flag[@]}"} \
    > "$project/.agentbus/daemon.log" 2>&1 &
  local daemon=$!
  trap 'kill $daemon 2>/dev/null' EXIT INT TERM
  sleep 1
  kill -0 "$daemon" 2>/dev/null || { cat "$project/.agentbus/daemon.log"; die "the daemon failed to start"; }

  if [ "$agent" = agy ]; then
    say "Opening the chat. Talk to your agent; bus activity appears inline. /help for commands."
    "$BIN/bus" chat -server "$server" -as "$as" -dir "$project"
  else
    say "Opening Claude Code. Answer your liaison with: $BIN/bus -server $server -human <name> answer <thread> <seq> \"...\" (or use the dashboard)"
    (cd "$project" && claude)
  fi
}

cmd_rehearse() {
  build
  local base="$HOME/agentbus-demo"
  mkdir -p "$base/frontend/src" "$base/backend"
  if [ ! -f "$base/frontend/src/UserCard.tsx" ]; then
    cat > "$base/frontend/src/UserCard.tsx" <<'TSX'
type User = { id: string; name: string; email: string };

export function UserCard({ user }: { user: User }) {
  return (
    <div className="card">
      <h2>{user.name}</h2>
    </div>
  );
}
TSX
  fi
  say "Starting the bus with a fresh ledger"
  (cd "$ROOT" && rm -f bus.db bus.db-* && "$BIN/busd" -addr 127.0.0.1:7070 > "$base/busd.log" 2>&1 &)
  sleep 1
  curl -fsS http://localhost:7070/api/state >/dev/null || die "the bus did not start; see $base/busd.log"
  local fe="cd '$ROOT' && ./demo.sh laptop -as frontend@teammate -project '$base/frontend'"
  local be="cd '$ROOT' && ./demo.sh laptop -as backend@abhay -project '$base/backend'"
  if [ "$(uname)" = Darwin ]; then
    osascript -e "tell application \"Terminal\" to do script \"$fe\"" >/dev/null
    osascript -e "tell application \"Terminal\" to do script \"$be\"" >/dev/null
    open http://localhost:7070
    say "Opened the dashboard and two Terminal windows (frontend and backend)."
  else
    say "Open two terminals and run:"
    echo "  $fe"
    echo "  $be"
  fi
  cat <<'EOF'

Try it:
  1. In the backend window: "Our users.name column was renamed to display_name for i18n.
     Propose the matching change to the User schema in openapi.yaml on the agent bus, as a
     blocking proposal. Do not edit any files."
  2. In the frontend window, answer the QUESTION FOR YOU with: /answer <seq> fine, but keep name for one release
  3. Watch both windows and the dashboard; the frontend agent updates src/UserCard.tsx.

Stop the bus afterwards with: pkill -f "busd -addr 127.0.0.1:7070"
EOF
}

cmd_showcase() {
  local side="${1:-}"; shift || true
  case "$side" in
    server)
      cmd_server -config showcase/bus.json -db showcase.db "$@" ;;
    backend|frontend)
      local id project="$HOME/Desktop/test-$side" args=()
      id="$(grep -o "\"$side@[^\"]*\"" "$ROOT/showcase/bus.json" | head -1 | tr -d '"')"
      while [ $# -gt 0 ]; do
        case "$1" in
          -project) project="$2"; shift 2 ;;
          *) args+=("$1"); shift ;;
        esac
      done
      mkdir -p "$project"
      if [ ! -d "$project/src" ]; then
        say "Creating the showcase $side project in $project"
        cp -Rn "$ROOT/showcase/template/$side/." "$project/"
      fi
      cmd_laptop -as "$id" -project "$project" -mandate "$ROOT/showcase/mandates/$side.md" ${args[@]+"${args[@]}"} ;;
    *) die "usage: ./demo.sh showcase server | backend -server URL | frontend -server URL" ;;
  esac
}

case "${1:-}" in
  server) shift; cmd_server "$@" ;;
  showcase) shift; cmd_showcase "$@" ;;
  laptop) shift; cmd_laptop "$@" ;;
  rehearse) shift; cmd_rehearse "$@" ;;
  *) sed -n '2,28p' "$0" | sed 's/^# \{0,1\}//'; exit 1 ;;
esac
