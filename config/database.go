package config

import (
	"github.com/goravel/framework/contracts/database/driver"
	sqlitefacades "github.com/goravel/sqlite/facades"
	"smart-mzcmc/app/facades"
)

func init() {
	config := facades.Config()
	config.Add("database", map[string]any{
		"default": config.Env("DB_CONNECTION", "sqlite"),
		"connections": map[string]any{
			"sqlite": map[string]any{
				"database": config.Env("DB_DATABASE", "database/smart-mzcmc.db"),
				"prefix":   "",
				"singular": false,
				"via": func() (driver.Driver, error) {
					return sqlitefacades.Sqlite("sqlite")
				},
			},
		},
		"pool": map[string]any{
			"max_idle_conns":    10,
			"max_open_conns":    100,
			"conn_max_idletime": 3600,
			"conn_max_lifetime": 3600,
		},
		"slow_threshold": 200,
		"migrations": map[string]any{
			"table": "migrations",
		},
	})
}
