package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"time"
)

type webhookPayload struct {
	ID         int    `json:"id"`
	Type       string `json:"type"`
	Status     string `json:"status"`
	TargetRepo string `json:"target_repo"`
	Payload    string `json:"payload"`
}

// FireWebhook posts a task status notification to the configured webhook_url.
// It is a no-op when webhook_url is not set. The HTTP call runs in a goroutine
// with a 5-second timeout so it never blocks the caller.
func FireWebhook(db *sql.DB, id int, status string) {
	url := GetConfig(db, "webhook_url", "")
	if url == "" {
		return
	}

	var taskType, targetRepo, payload string
	err := db.QueryRow(
		`SELECT type, target_repo, payload FROM BountyBoard WHERE id = ?`, id,
	).Scan(&taskType, &targetRepo, &payload)
	if err != nil {
		return
	}

	const maxPayload = 500
	if len(payload) > maxPayload {
		payload = payload[:maxPayload] + "…"
	}

	body, err := json.Marshal(webhookPayload{
		ID:         id,
		Type:       taskType,
		Status:     status,
		TargetRepo: targetRepo,
		Payload:    payload,
	})
	if err != nil {
		return
	}

	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		client.Post(url, "application/json", bytes.NewReader(body)) //nolint:errcheck
	}()
}
