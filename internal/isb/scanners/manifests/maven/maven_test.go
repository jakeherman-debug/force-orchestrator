package maven

import (
	"os"
	"path/filepath"
	"testing"
)

func read(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return body
}

func TestParser_Detect(t *testing.T) {
	p := Parser{}
	for path, want := range map[string]bool{
		"pom.xml":          true,
		"build.gradle":     true,
		"build.gradle.kts": true,
		"package.json":     false,
		"go.mod":           false,
	} {
		if got := p.Detect(path); got != want {
			t.Errorf("Detect(%q)=%v want %v", path, got, want)
		}
	}
}

func TestParser_Pom_Direct(t *testing.T) {
	deps, err := Parser{}.Parse("pom.xml", read(t, "pom.xml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := map[string]string{}
	for _, d := range deps {
		got[d.Name] = d.Version
	}
	if got["org.springframework.boot:spring-boot-starter-web"] != "3.2.0" {
		t.Errorf("missing spring-boot-starter-web@3.2.0: %v", got)
	}
	if got["com.fasterxml.jackson.core:jackson-databind"] != "2.16.0" {
		t.Errorf("missing jackson-databind@2.16.0: %v", got)
	}
	// ${junit.version} is unresolved → empty Version (by design).
	if v, ok := got["org.junit.jupiter:junit-jupiter"]; !ok {
		t.Errorf("expected junit-jupiter present: %v", got)
	} else if v != "" {
		t.Errorf("expected empty version for unresolved ${junit.version}, got %q", v)
	}
}

func TestParser_BuildGradle_Groovy(t *testing.T) {
	deps, err := Parser{}.Parse("build.gradle", read(t, "build.gradle"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := map[string]string{}
	for _, d := range deps {
		got[d.Name] = d.Version
	}
	if got["org.springframework.boot:spring-boot-starter-web"] != "3.2.0" {
		t.Errorf("missing spring-boot-starter-web: %v", got)
	}
	if got["org.junit.jupiter:junit-jupiter"] != "5.10.1" {
		t.Errorf("missing junit-jupiter@5.10.1: %v", got)
	}
	if got["org.postgresql:postgresql"] != "42.7.1" {
		t.Errorf("map-style postgresql dep missing: %v", got)
	}
	if _, ok := got["org.example:commented-out"]; ok {
		t.Errorf("commented-out dep should be ignored: %v", got)
	}
}

func TestParser_BuildGradle_Kotlin(t *testing.T) {
	deps, err := Parser{}.Parse("build.gradle.kts", read(t, "build.gradle.kts"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := map[string]string{}
	for _, d := range deps {
		got[d.Name] = d.Version
	}
	if got["org.springframework.boot:spring-boot-starter-web"] != "3.2.0" {
		t.Errorf("kotlin DSL: missing spring-boot: %v", got)
	}
	if got["org.junit.jupiter:junit-jupiter"] != "5.10.1" {
		t.Errorf("kotlin DSL: missing junit: %v", got)
	}
}

func TestParser_DiffAddsCommonsLang(t *testing.T) {
	before := read(t, "pom.xml")
	after := read(t, "pom.xml.after")
	added, removed, err := Parser{}.ParseDiff("pom.xml", before, after)
	if err != nil {
		t.Fatalf("ParseDiff: %v", err)
	}
	found := false
	for _, d := range added {
		if d.Name == "org.apache.commons:commons-lang3" && d.Version == "3.14.0" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected commons-lang3@3.14.0 in added: %+v", added)
	}
	if len(removed) != 0 {
		t.Errorf("no removed deps expected: %+v", removed)
	}
}

func TestParser_Malformed_DoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic on malformed: %v", r)
		}
	}()
	_, err := Parser{}.Parse("pom.xml", []byte("<not valid xml"))
	if err == nil {
		t.Errorf("expected error on malformed pom.xml")
	}
}
