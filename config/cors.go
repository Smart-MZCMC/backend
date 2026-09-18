package config

import (
	"smart-mzcmc/app/facades"
)

func init() {
	config := facades.Config()
	config.Add("cors", map[string]any{
		"paths":                []string{"*"},
		"allowed_methods":      []string{"*"},
		"allowed_origins":      []string{"*"},
		"allowed_headers":      []string{"*"},
		"exposed_headers":      []string{"*"},
		"max_age":              86400,
		"supports_credentials": false,
	})
}
