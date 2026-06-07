-- Per-project agent config. A single nullable JSON column on projects holds the
-- agent settings (model, permissions, adapter-specific keys) AO resolves into
-- LaunchConfig.Config at spawn. NULL means unset; a non-NULL value is a JSON
-- object. One blob per project keeps the registry's "SQLite twin of the YAML
-- config" shape rather than splitting agent config into its own table.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE projects ADD COLUMN agent_config TEXT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE projects DROP COLUMN agent_config;
-- +goose StatementEnd
