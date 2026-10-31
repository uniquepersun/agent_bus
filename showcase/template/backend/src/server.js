// Tasks API. In-memory data stands in for Postgres (see db/schema.sql).
const http = require("http");

const users = {
  u1: { id: "u1", name: "Asha Rao", email: "asha@example.com" },
  u2: { id: "u2", name: "Ben Ortiz", email: "ben@example.com" },
};

const tasks = [
  { id: "t1", title: "Write the API docs", done: false, assignee_id: "u1" },
  { id: "t2", title: "Fix the login bug", done: true, assignee_id: "u2" },
];

function send(res, status, body) {
  res.writeHead(status, { "Content-Type": "application/json", "Access-Control-Allow-Origin": "*" });
  res.end(JSON.stringify(body));
}

function readJSON(req) {
  return new Promise((resolve, reject) => {
    let raw = "";
    req.on("data", (chunk) => (raw += chunk));
    req.on("end", () => {
      try {
        resolve(JSON.parse(raw || "{}"));
      } catch (err) {
        reject(err);
      }
    });
  });
}

const server = http.createServer(async (req, res) => {
  const url = new URL(req.url, "http://localhost");

  const user = url.pathname.match(/^\/users\/([^/]+)$/);
  if (req.method === "GET" && user) {
    const found = users[user[1]];
    return found ? send(res, 200, found) : send(res, 404, { error: "user not found" });
  }

  if (req.method === "GET" && url.pathname === "/tasks") {
    return send(res, 200, tasks);
  }

  if (req.method === "POST" && url.pathname === "/tasks") {
    let body;
    try {
      body = await readJSON(req);
    } catch {
      return send(res, 400, { error: "invalid JSON" });
    }
    if (!body.title || !users[body.assignee_id]) {
      return send(res, 400, { error: "title and a valid assignee_id are required" });
    }
    const task = { id: "t" + (tasks.length + 1), title: body.title, done: false, assignee_id: body.assignee_id };
    tasks.push(task);
    return send(res, 201, task);
  }

  send(res, 404, { error: "not found" });
});

const port = Number(process.env.PORT || 3000);
server.listen(port, () => console.log(`Tasks API on http://localhost:${port}`));
