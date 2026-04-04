package main

import (
	"database/sql"

	"force-orchestrator/internal/dashboard"
)

func cmdDashboard(db *sql.DB, port int) {
	dashboard.RunDashboard(db, port)
}
