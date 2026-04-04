package agents

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	igit "force-orchestrator/internal/git"
	"force-orchestrator/internal/store"
)

// dogCooldowns defines how often each built-in dog may run.
var dogCooldowns = map[string]time.Duration{
	"git-hygiene":    30 * time.Minute,
	"db-vacuum":      6 * time.Hour,
	"holonet-rotate": 24 * time.Hour,
	"mail-cleanup":   12 * time.Hour,
}

// dogOrder determines the execution order of dogs within each inquisitor cycle.
var dogOrder = []string{"git-hygiene", "db-vacuum", "holonet-rotate", "mail-cleanup"}

// RunDogs checks each built-in dog against its cooldown and runs any that are due.
// Called by SpawnInquisitor on every inquisitor cycle.
func RunDogs(db *sql.DB, logger interface{ Printf(string, ...any) }) {
	for _, dogName := range dogOrder {
		cooldown := dogCooldowns[dogName]
		last := store.DogLastRun(db, dogName)
		if last != "" {
			var lastT time.Time
			if err := (&lastT).UnmarshalText([]byte(last)); err == nil {
				if time.Since(lastT) < cooldown {
					continue
				}
			} else {
				t, err := time.ParseInLocation("2006-01-02 15:04:05", last, time.UTC)
				if err == nil && time.Since(t) < cooldown {
					continue
				}
			}
		}
		logger.Printf("Dog %s: running (cooldown %v)", dogName, cooldown)
		store.DogMarkRun(db, dogName)
		if err := runDog(db, dogName, logger); err != nil {
			logger.Printf("Dog %s: error — %v", dogName, err)
			store.SendMail(db, "inquisitor", "operator",
				fmt.Sprintf("[DOG FAILURE] %s", dogName),
				fmt.Sprintf("Watchdog '%s' failed during its scheduled run.\n\nError: %v\n\nThis may indicate a system health issue requiring attention.", dogName, err),
				0, store.MailTypeAlert)
		} else {
			logger.Printf("Dog %s: done", dogName)
		}
	}
}

func runDog(db *sql.DB, name string, logger interface{ Printf(string, ...any) }) error {
	switch name {
	case "git-hygiene":
		return dogGitHygiene(db, logger)
	case "db-vacuum":
		return dogDBVacuum(db, logger)
	case "holonet-rotate":
		return dogHolonetRotate(logger)
	case "mail-cleanup":
		return dogMailCleanup(db, logger)
	default:
		return fmt.Errorf("unknown dog: %s", name)
	}
}

func dogGitHygiene(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	rows, err := db.Query(`SELECT name, local_path FROM Repositories`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name, path string
		rows.Scan(&name, &path)
		if _, statErr := os.Stat(path); statErr != nil {
			logger.Printf("ERROR: Dog git-hygiene: repo '%s' path not accessible (%s) — check registration with: force repos", name, path)
			continue
		}
		if out, gitErr := igit.RunCmd(path, "fetch", "--prune", "--quiet"); gitErr != nil {
			logger.Printf("Dog git-hygiene: fetch failed for %s: %s", name, out)
		} else {
			logger.Printf("Dog git-hygiene: fetched %s", name)
		}
		igit.RunCmd(path, "gc", "--auto", "--quiet")
	}
	return nil
}

const vacuumThresholdBytes = 100 * 1024 * 1024 // 100 MB

func dogDBVacuum(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	// WAL checkpointing is handled by SpawnInquisitor every 5 minutes — no need to repeat here.
	db.Exec(`ANALYZE`)

	// Only VACUUM when the database file exceeds the threshold; VACUUM holds an
	// exclusive lock for its entire duration, so we avoid it during normal operation.
	var seq int
	var dbName, dbFile string
	db.QueryRow(`PRAGMA database_list`).Scan(&seq, &dbName, &dbFile)
	if dbFile != "" {
		info, err := os.Stat(dbFile)
		if err == nil && info.Size() < vacuumThresholdBytes {
			logger.Printf("Dog db-vacuum: skipping VACUUM (%.1f MB < threshold)", float64(info.Size())/1024/1024)
			return nil
		}
	}

	_, err := db.Exec(`VACUUM`)
	return err
}

const holonetMaxBytes = 50 * 1024 * 1024 // 50 MB

func dogHolonetRotate(logger interface{ Printf(string, ...any) }) error {
	const path = "holonet.jsonl"
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	if info.Size() < holonetMaxBytes {
		return nil
	}
	// Use millisecond precision to prevent same-second filename collisions.
	stamp := time.Now().UTC().Format("20060102-150405.000")
	archivePath := filepath.Join("holonet-" + stamp + ".jsonl")
	if renErr := os.Rename(path, archivePath); renErr != nil {
		return renErr
	}
	logger.Printf("Dog holonet-rotate: rotated %s → %s (%.1f MB)", path, archivePath, float64(info.Size())/1024/1024)
	return nil
}

func dogMailCleanup(db *sql.DB, logger interface{ Printf(string, ...any) }) error {
	res, err := db.Exec(`
		DELETE FROM Fleet_Mail
		WHERE read_at = ''
		  AND task_id != 0
		  AND created_at < datetime('now', '-48 hours')
		  AND task_id IN (
		      SELECT id FROM BountyBoard WHERE status IN ('Completed', 'Failed', 'Escalated')
		  )`)
	if err != nil {
		return err
	}
	stale, _ := res.RowsAffected()

	res2, err := db.Exec(`DELETE FROM Fleet_Mail WHERE read_at != '' AND created_at < datetime('now', '-30 days')`)
	if err != nil {
		return err
	}
	old, _ := res2.RowsAffected()

	if stale > 0 || old > 0 {
		logger.Printf("Dog mail-cleanup: removed %d stale unread + %d old read messages", stale, old)
	}
	return nil
}

// DogStatus holds the status of a single watchdog.
type DogStatus struct {
	Name     string
	Cooldown time.Duration
	LastRun  string
	NextRun  string
	RunCount int
}

// ListDogs returns the name, last-run time, and run count for all known dogs.
func ListDogs(db *sql.DB) []DogStatus {
	var result []DogStatus
	for _, name := range dogOrder {
		cooldown := dogCooldowns[name]
		var lastRun string
		var count int
		db.QueryRow(`SELECT last_run_at, run_count FROM Dogs WHERE name = ?`, name).Scan(&lastRun, &count)
		var nextRun string
		if lastRun != "" {
			t, err := time.ParseInLocation("2006-01-02 15:04:05", lastRun, time.UTC)
			if err == nil {
				next := t.Add(cooldown)
				if time.Now().Before(next) {
					nextRun = fmt.Sprintf("in %v", next.Sub(time.Now()).Round(time.Minute))
				} else {
					nextRun = "overdue"
				}
			}
		} else {
			nextRun = "never run"
		}
		result = append(result, DogStatus{
			Name:     name,
			Cooldown: cooldown,
			LastRun:  lastRun,
			NextRun:  nextRun,
			RunCount: count,
		})
	}
	return result
}
