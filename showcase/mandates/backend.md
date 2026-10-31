Scope: the backend repository (src/, db/), schema.sql, openapi.yaml, env.md
I care about: no breaking API changes without a migration path, Postgres only, every table change has a matching API change
Auto-accept: additive optional fields, new endpoints, new optional environment variables
Ask me: removing or renaming fields, changing a field's type, auth changes, new dependencies
