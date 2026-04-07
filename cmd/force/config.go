package main

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"

	"force-orchestrator/internal/store"
)

func cmdConfig(db *sql.DB, args []string) {
	subCmd := ""
	if len(args) >= 1 {
		subCmd = args[0]
	}
	switch subCmd {
	case "get":
		if len(args) < 2 {
			fmt.Println("Usage: force config get <key>")
			os.Exit(1)
		}
		val := store.GetConfig(db, args[1], "")
		if val == "" {
			fmt.Printf("%s is not set\n", args[1])
		} else {
			fmt.Printf("%s = %s\n", args[1], val)
		}
	case "set":
		if len(args) < 3 {
			fmt.Println("Usage: force config set <key> <value>")
			os.Exit(1)
		}
		cfgKey, cfgVal := args[1], args[2]
		// Validate integer-only keys to catch bad values before they cause
		// cryptic claude CLI errors at runtime.
		integerKeys := map[string]bool{
			"num_astromechs": true,
			"num_captain":    true,
			"num_council":    true,
			"max_concurrent": true,
			"spawn_delay_ms": true,
			"batch_size":     true,
			"max_turns":      true,
		}
		if integerKeys[cfgKey] {
			if n, err := strconv.Atoi(cfgVal); err != nil || n < 0 {
				fmt.Printf("Error: '%s' requires a non-negative integer, got: %s\n", cfgKey, cfgVal)
				os.Exit(1)
			}
		}
		knownKeys := map[string]string{
			"num_astromechs": "integer (number of astromech agents to spawn)",
			"num_captain":    "integer (number of captain agents to spawn, default 1)",
			"num_council":    "integer (number of Jedi Council reviewers to spawn)",
			"max_concurrent": "integer (max simultaneous CodeEdit tasks)",
			"spawn_delay_ms": "integer (ms to sleep between successive task claims, 0=off)",
			"batch_size":     "integer (max tasks claimed fleet-wide per 60s window, 0=off)",
			"max_turns":      "integer (max claude CLI turns per task, default 40)",
			"estop":          "true/false (emergency stop — use 'force estop'/'force resume' instead)",
			"rl_hits_*":      "integer (persisted rate-limit hit count per agent — managed automatically)",
			"webhook_url":    "string (URL to POST task status notifications to on Completed/Failed/Escalated)",
		}
		if _, ok := knownKeys[cfgKey]; !ok {
			fmt.Printf("Warning: '%s' is not a known config key.\nKnown keys:\n", cfgKey)
			for k, desc := range knownKeys {
				fmt.Printf("  %-20s %s\n", k, desc)
			}
			fmt.Println("Setting anyway — unknown keys are ignored by the daemon.")
		}
		store.SetConfig(db, cfgKey, cfgVal)
		fmt.Printf("%s = %s\n", cfgKey, cfgVal)
	case "list", "":
		rows, cfgErr := db.Query(`SELECT key, value FROM SystemConfig ORDER BY key`)
		if cfgErr != nil {
			fmt.Printf("DB error: %v\n", cfgErr)
			os.Exit(1)
		}
		defer rows.Close()
		found := false
		for rows.Next() {
			found = true
			var k, v string
			rows.Scan(&k, &v)
			fmt.Printf("%-25s = %s\n", k, v)
		}
		if !found {
			fmt.Println("No config values set. Defaults are in effect.")
		}
	default:
		fmt.Printf("Unknown config subcommand: %s\n", args[0])
		fmt.Println("Usage: force config [list|get <key>|set <key> <value>]")
		os.Exit(1)
	}
}
