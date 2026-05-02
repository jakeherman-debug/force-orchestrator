package store

import (
	"os"
	"path/filepath"
	"testing"
)

const sampleMIT = `MIT License

Copyright (c) 2024

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files...

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY...`

const sampleApache = `                                 Apache License
                           Version 2.0, January 2004`

func TestSetGetRepositoryLicense_RoundTrip(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	AddRepo(db, "myrepo", "/tmp/nope", "desc")

	if err := SetRepositoryLicense(db, "myrepo", "MIT"); err != nil {
		t.Fatalf("SetRepositoryLicense: %v", err)
	}
	got, err := GetRepositoryLicense(db, "myrepo")
	if err != nil {
		t.Fatalf("GetRepositoryLicense: %v", err)
	}
	if got != "MIT" {
		t.Errorf("expected MIT, got %q", got)
	}
}

func TestSetRepositoryLicense_MissingRepo_Errors(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	if err := SetRepositoryLicense(db, "no-such-repo", "MIT"); err == nil {
		t.Errorf("expected error for missing repo")
	}
}

func TestAddRepo_DetectsLicenseFromDisk(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "LICENSE"), []byte(sampleMIT), 0644); err != nil {
		t.Fatalf("write LICENSE: %v", err)
	}

	AddRepo(db, "withlicense", dir, "demo")

	got, err := GetRepositoryLicense(db, "withlicense")
	if err != nil {
		t.Fatalf("GetRepositoryLicense: %v", err)
	}
	if got != "MIT" {
		t.Errorf("expected MIT detection on AddRepo, got %q", got)
	}
}

func TestAddRepo_NoLicenseFile_LeavesUnknown(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	dir := t.TempDir()
	AddRepo(db, "nolicense", dir, "no LICENSE file")

	got, err := GetRepositoryLicense(db, "nolicense")
	if err != nil {
		t.Fatalf("GetRepositoryLicense: %v", err)
	}
	if got != "Unknown" {
		t.Errorf("expected Unknown when no LICENSE file, got %q", got)
	}
}

// TestBackfillRepositoryLicenses_Stamps_OnMigration confirms the
// backfill scans Repositories rows with license='' and runs SPDX
// against on-disk LICENSE files. Idempotent: a re-run after
// detection does NOT re-stamp.
func TestBackfillRepositoryLicenses_Stamps(t *testing.T) {
	db := InitHolocronDSN(":memory:")
	defer db.Close()

	dirA := t.TempDir()
	if err := os.WriteFile(filepath.Join(dirA, "LICENSE"), []byte(sampleApache), 0644); err != nil {
		t.Fatalf("write A: %v", err)
	}
	dirB := t.TempDir()
	if err := os.WriteFile(filepath.Join(dirB, "LICENSE.md"), []byte(sampleMIT), 0644); err != nil {
		t.Fatalf("write B: %v", err)
	}
	dirC := t.TempDir() // no LICENSE file

	// Insert directly to simulate pre-D5 rows (license='').
	for name, p := range map[string]string{"a": dirA, "b": dirB, "c": dirC} {
		if _, err := db.Exec(`INSERT INTO Repositories (name, local_path, mode, license) VALUES (?, ?, 'write', '')`, name, p); err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
	}

	backfillRepositoryLicenses(db)

	for name, want := range map[string]string{"a": "Apache-2.0", "b": "MIT", "c": ""} {
		got, err := GetRepositoryLicense(db, name)
		if err != nil {
			t.Fatalf("Get %s: %v", name, err)
		}
		if got != want {
			t.Errorf("%s: want license=%q got %q", name, want, got)
		}
	}

	// Idempotence: running again must not change anything (and not
	// clobber any operator-set values).
	if err := SetRepositoryLicense(db, "c", "OperatorSetCustom"); err != nil {
		t.Fatalf("SetRepositoryLicense: %v", err)
	}
	backfillRepositoryLicenses(db)
	got, _ := GetRepositoryLicense(db, "c")
	if got != "OperatorSetCustom" {
		t.Errorf("backfill clobbered operator-set value: got %q", got)
	}
}
