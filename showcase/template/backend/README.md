# Tasks backend

A small tasks API used to showcase Agent Bus. `npm start` serves it on port 3000.

- `GET /users/:id`
- `GET /tasks`
- `POST /tasks` with `{ "title", "assignee_id" }`

The shared API, schema, and environment contracts live in `contracts/` (written by Agent Bus; do not edit).
