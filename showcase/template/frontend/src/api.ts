// Client for the Tasks API. Types mirror contracts/openapi.yaml.
export type User = { id: string; name: string; email: string };

export type Task = { id: string; title: string; done: boolean; assignee_id: string };

const BASE = process.env.API_BASE_URL ?? "http://localhost:3000";

export async function fetchUser(id: string): Promise<User> {
  const res = await fetch(`${BASE}/users/${id}`);
  return res.json();
}

export async function fetchTasks(): Promise<Task[]> {
  const res = await fetch(`${BASE}/tasks`);
  return res.json();
}

export async function createTask(title: string, assigneeId: string): Promise<Task> {
  const res = await fetch(`${BASE}/tasks`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ title, assignee_id: assigneeId }),
  });
  return res.json();
}
