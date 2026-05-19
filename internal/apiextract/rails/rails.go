// Package rails provides a ProviderExtractor for Rails routes.rb files.
// It handles the most common Rails DSL patterns: direct route declarations
// (get/post/put/patch/delete), resources, namespace blocks, root, and match.
// Dynamic patterns (procs, mountable engines) are intentionally skipped to
// stay within the ≥ 95% accuracy target without false positives.
package rails

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strings"

	"force-orchestrator/internal/store"
)

const (
	apiKind       = "http_route"
	extractorName = "rails-routes"
)

// Extractor implements apiextract.ProviderExtractor for Rails routes.rb files.
type Extractor struct{}

// Kind returns "http_route".
func (e *Extractor) Kind() string { return apiKind }

// ExtractorName returns "rails-routes".
func (e *Extractor) ExtractorName() string { return extractorName }

// Extract parses a Rails routes.rb file and returns CrossRepoAPI rows.
func (e *Extractor) Extract(repoName, filePath string, content []byte) ([]store.CrossRepoAPI, error) {
	p := &parser{
		repoName: repoName,
		filePath: filePath,
	}
	return p.parse(content)
}

// parser holds the incremental state while scanning routes.rb.
type parser struct {
	repoName string
	filePath string
}

// Rails route verb patterns — these match the start of a trimmed line.
var (
	// Direct verb: get '/path', to: 'controller#action'
	// or:          get '/path' => 'controller#action'
	reVerb = regexp.MustCompile(`^(get|post|put|patch|delete)\s+['"]([^'"]+)['"]`)

	// resources :name or resources :name, only: [...] etc.
	reResources = regexp.MustCompile(`^resources\s+:(\w+)`)

	// namespace :ns do
	reNamespace = regexp.MustCompile(`^namespace\s+:(\w+)`)

	// root 'controller#action' or root to: 'controller#action'
	reRoot = regexp.MustCompile(`^root\b`)

	// match '/path', via: [:get, :post] or via: :get
	reMatch = regexp.MustCompile(`^match\s+['"]([^'"]+)['"]`)
	reVia   = regexp.MustCompile(`via:\s*(\[:[^\]]+\]|:\w+)`)
	reViaEl = regexp.MustCompile(`:(\w+)`)
)

// Standard Rails resourceful routes produced by `resources :name`.
// Order: index, show, new, create, edit, update (PATCH), update (PUT), destroy.
var resourceRoutes = []struct {
	method string
	suffix string // appended to /:plural or /:plural/:id
}{
	{"GET", ""},           // index
	{"POST", ""},          // create
	{"GET", "/new"},       // new
	{"GET", "/:id"},       // show
	{"GET", "/:id/edit"},  // edit
	{"PATCH", "/:id"},     // update (PATCH)
	{"PUT", "/:id"},       // update (PUT)
	{"DELETE", "/:id"},    // destroy
}

func (p *parser) parse(content []byte) ([]store.CrossRepoAPI, error) {
	scanner := bufio.NewScanner(bytes.NewReader(content))

	var out []store.CrossRepoAPI
	lineNum := 0

	// Track namespace prefixes via a simple stack. Each entry is the prefix
	// accumulated so far (e.g. "/api", "/api/v1").
	type stackFrame struct {
		prefix string
		depth  int // brace depth when this frame opened
	}
	var nsStack []stackFrame
	braceDepth := 0

	currentPrefix := func() string {
		if len(nsStack) == 0 {
			return ""
		}
		return nsStack[len(nsStack)-1].prefix
	}

	for scanner.Scan() {
		lineNum++
		raw := scanner.Text()
		line := strings.TrimSpace(raw)

		// Track brace depth to know when namespaces end.
		// Count opens and closes; pop namespace frames when depth falls.
		opens := strings.Count(line, "do") // "do" is the block opener in Ruby
		// Actually track literal { and } as well as do/end pairs.
		// For simplicity use indent heuristic via "do" and "end".
		// We count "end" keywords to pop namespace stack.

		_ = opens // computed below with the inline approach

		// Count "do" block openers (namespace/scope/resources blocks).
		// We detect them via the namespace regex firing — depth tracked separately.
		opens2 := strings.Count(line, " do") + func() int {
			if strings.HasSuffix(line, " do") || line == "do" {
				return 1
			}
			return 0
		}()
		_ = opens2

		// Simpler depth model: count "do" on this line as +1, "end" as -1.
		doCount := 0
		if strings.Contains(line, " do") || strings.HasSuffix(line, " do") {
			doCount++
		}
		endCount := 0
		if line == "end" || strings.HasPrefix(line, "end ") || strings.HasPrefix(line, "end\t") {
			endCount++
		}

		braceDepth += doCount

		// Pop namespace frames whose depth is now above current braceDepth.
		for len(nsStack) > 0 && braceDepth <= nsStack[len(nsStack)-1].depth {
			nsStack = nsStack[:len(nsStack)-1]
		}

		// Skip comments and blank lines.
		if line == "" || strings.HasPrefix(line, "#") {
			if endCount > 0 {
				braceDepth -= endCount
			}
			continue
		}

		prefix := currentPrefix()

		switch {
		case reNamespace.MatchString(line):
			m := reNamespace.FindStringSubmatch(line)
			ns := m[1]
			nsStack = append(nsStack, stackFrame{
				prefix: prefix + "/" + ns,
				depth:  braceDepth - doCount, // depth before this line's "do"
			})

		case reResources.MatchString(line):
			m := reResources.FindStringSubmatch(line)
			name := m[1]
			base := prefix + "/" + name
			for _, rr := range resourceRoutes {
				path := store.NormalizeAPIPath(base + rr.suffix)
				identifier := fmt.Sprintf("%s %s", rr.method, path)
				out = append(out, store.CrossRepoAPI{
					RepoName:      p.repoName,
					APIKind:       apiKind,
					APIIdentifier: identifier,
					SourceFile:    p.filePath,
					SourceLine:    lineNum,
					Extractor:     extractorName,
					LastScannedAt: store.NowSQLite(),
				})
			}

		case reRoot.MatchString(line):
			out = append(out, store.CrossRepoAPI{
				RepoName:      p.repoName,
				APIKind:       apiKind,
				APIIdentifier: "GET " + prefix + "/",
				SourceFile:    p.filePath,
				SourceLine:    lineNum,
				Extractor:     extractorName,
				LastScannedAt: store.NowSQLite(),
			})

		case reMatch.MatchString(line):
			pathM := reMatch.FindStringSubmatch(line)
			rawPath := pathM[1]
			path := store.NormalizeAPIPath(prefix + rawPath)

			viaM := reVia.FindStringSubmatch(line)
			if viaM == nil {
				// No via: clause — skip (dynamic or incomplete).
				break
			}
			methods := reViaEl.FindAllStringSubmatch(viaM[1], -1)
			for _, mm := range methods {
				method := strings.ToUpper(mm[1])
				identifier := fmt.Sprintf("%s %s", method, path)
				out = append(out, store.CrossRepoAPI{
					RepoName:      p.repoName,
					APIKind:       apiKind,
					APIIdentifier: identifier,
					SourceFile:    p.filePath,
					SourceLine:    lineNum,
					Extractor:     extractorName,
					LastScannedAt: store.NowSQLite(),
				})
			}

		case reVerb.MatchString(line):
			m := reVerb.FindStringSubmatch(line)
			method := strings.ToUpper(m[1])
			rawPath := m[2]
			path := store.NormalizeAPIPath(prefix + rawPath)
			identifier := fmt.Sprintf("%s %s", method, path)
			out = append(out, store.CrossRepoAPI{
				RepoName:      p.repoName,
				APIKind:       apiKind,
				APIIdentifier: identifier,
				SourceFile:    p.filePath,
				SourceLine:    lineNum,
				Extractor:     extractorName,
				LastScannedAt: store.NowSQLite(),
			})
		}

		if endCount > 0 {
			braceDepth -= endCount
			if braceDepth < 0 {
				braceDepth = 0
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("rails extractor: scan %s: %w", p.filePath, err)
	}
	return out, nil
}
