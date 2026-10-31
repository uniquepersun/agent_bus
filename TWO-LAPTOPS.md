# Running Agent Bus on two laptops

This guide sets up the demo on two laptops: **Laptop A** runs the bus server and the backend agent, **Laptop B** runs the frontend agent. Allow about 10 minutes the first time.

## 1. Before you start (both laptops)

- Both laptops on the **same Wi-Fi network** (a phone hotspot works well at events).
- A signed-in coding agent CLI: the Antigravity CLI (`agy`) or Claude Code (`claude`). Each laptop can use either.
- `git` and `curl` (preinstalled on macOS).
- Optional: [blob](https://github.com/uniquepersun/blob), the game to play while the agents work.
  ```sh
  brew tap uniquepersun/blob https://github.com/uniquepersun/blob && brew install uniquepersun/blob/blob
  ```

## 2. Download (both laptops)

From the [latest release](https://github.com/uniquepersun/agent_bus/releases/latest), download the archive for your machine and unpack it:

| Machine | Archive |
|---|---|
| Mac with Apple Silicon (M1 or later) | `agent-bus-<version>-darwin-arm64.tar.gz` |
| Intel Mac | `agent-bus-<version>-darwin-amd64.tar.gz` |
| Linux x86-64 | `agent-bus-<version>-linux-amd64.tar.gz` |

```sh
cd ~/Downloads
tar -xzf agent-bus-*-darwin-arm64.tar.gz
cd agent-bus-*-darwin-arm64
xattr -dr com.apple.quarantine .      # macOS only: allows the downloaded binaries to run
```

All commands below are run from this folder.

## 3. Laptop A: start the bus

```sh
./demo.sh server -fresh
```

It prints the address of the bus, for example `http://192.168.1.20:7070`. Write it down; Laptop B needs it.

- If macOS asks whether `busd` may accept incoming connections, click **Allow**.
- Open the printed address in a browser: this is the live dashboard. Put it on the projector.
- Leave this terminal running.

## 4. Laptop A: start the backend side

Open a **second terminal** on Laptop A, in the same folder:

```sh
./demo.sh laptop -server http://<bus-address>:7070 -as backend@abhay -project ~/agentbus-demo/backend
```

Add `-agent claude` to use Claude Code instead of agy.

This installs the agent integration into the project folder, starts the background daemon, and opens the chat with your agent (`bus chat` for agy, or Claude Code).

## 5. Laptop B: start the frontend side

First check that Laptop B can reach the bus:

```sh
curl http://<bus-address>:7070/api/state
```

It should print JSON. If it hangs, see Troubleshooting. Then:

```sh
./demo.sh laptop -server http://<bus-address>:7070 -as frontend@teammate -project ~/agentbus-demo/frontend
```

To give the frontend agent some code to update, create `~/agentbus-demo/frontend/src/UserCard.tsx`:

```tsx
type User = { id: string; name: string; email: string };

export function UserCard({ user }: { user: User }) {
  return (
    <div className="card">
      <h2>{user.name}</h2>
    </div>
  );
}
```

## 6. Run the demo

1. **Laptop B (frontend chat):** give the agent a small task, for example
   `Add a <p> that shows user.email under the heading in src/UserCard.tsx.`
2. **Laptop A (backend chat):** ask for the cross-boundary change:
   `Our users.name column was renamed to display_name for i18n. Propose the matching change to the User schema in openapi.yaml on the agent bus, as a blocking proposal. Do not edit any files.`
   A `PROPOSE` line appears in both chats and a card appears on the dashboard.
3. **Laptop B:** the frontend liaison checks its mandate (renames need approval) and asks its human. The chat shows `QUESTION FOR YOU (#2)`. Answer it:
   `/answer 2 fine, but keep name for one release`
4. **Both chats and the dashboard:** the liaisons settle it on their own: `COUNTER`, `ACCEPT`, `DECIDED`.
5. **Laptop B:** the chat shows `update delivered to the agent`, and the frontend agent updates `UserCard.tsx` to use `display_name`. Open the file to show it.
6. Close on `~/agentbus-demo/frontend/DECISIONS.md`: the permanent record of the decision.

Each agy liaison turn takes one to two minutes (Claude Code is faster), so the whole run takes about 5 to 8 minutes. Talk through the dashboard while the cards move.

**With blob:** run `blob` in another terminal tab on each laptop. The game pauses with a card when your liaison has a question for you; answer in the chat, then keep playing.

## 7. Useful commands

Run these from the release folder, adding `-server http://<bus-address>:7070`:

| Command | Purpose |
|---|---|
| `./bin/bus -server URL threads` | List threads and their states |
| `./bin/bus -server URL show <thread>` | A thread's full history with diffs |
| `./bin/bus -server URL tail` | Follow every event live |
| `./bin/bus -server URL -human <name> answer <thread> <seq> "text"` | Answer a question outside the chat |
| `./bin/bus -server URL -human <name> resolve <thread> <seq>` | Decide an escalated thread |

In the chat: `/answer`, `/pick`, `/threads`, `/show <thread>`, `/help`, `/quit`.

## 8. Reset between rehearsals

1. Stop everything with `Ctrl+C` (or `/quit` in the chats).
2. Laptop A: `./demo.sh server -fresh` starts with an empty ledger.
3. Both laptops: restore the demo code, for example by recreating `UserCard.tsx` as above, and delete `contracts/`, `DECISIONS.md` and `.agentbus/` in the project folder.

## Troubleshooting

- **Laptop B cannot reach the bus:** confirm both laptops are on the same network, use the address printed by `demo.sh server` (not `localhost`), and allow `busd` in the macOS firewall (System Settings, Network, Firewall). Venue Wi-Fi often blocks connections between devices; switch to a phone hotspot.
- **"agent ... is not registered on the bus":** the identity must match `bus.json` on Laptop A. To use other names, edit `bus.json` there and restart with `./demo.sh server -fresh`.
- **agy turns end with no output:** headless agy was denied a tool. The chat shows `[agent] ...` with the rule it needs. Grant extra commands with `-allow-command "npm test"` on `demo.sh laptop`.
- **Nothing happens after a proposal:** read the daemon log at `<project>/.agentbus/daemon.log` and the liaison transcripts in `<project>/.agentbus/logs/`.
- **"cannot reach the bus" from demo.sh laptop:** start `demo.sh server` first, and check the `-server` address.

The first time `demo.sh laptop` runs with agy, it adds the permission rules headless agy needs to `~/.gemini/antigravity-cli/settings.json`: access to the agent-bus tools and to write files in the project folder.
