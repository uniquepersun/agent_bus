CREATE TABLE users (
  id    UUID PRIMARY KEY,
  name  TEXT NOT NULL,
  email TEXT NOT NULL UNIQUE
);

CREATE TABLE tasks (
  id          UUID PRIMARY KEY,
  title       TEXT NOT NULL,
  done        BOOLEAN NOT NULL DEFAULT FALSE,
  assignee_id UUID NOT NULL REFERENCES users(id)
);
