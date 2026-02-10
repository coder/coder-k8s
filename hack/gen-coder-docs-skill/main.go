package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	beginDocsTreeMarker = "<!-- BEGIN DOCS_TREE -->"
	endDocsTreeMarker   = "<!-- END DOCS_TREE -->"
	beginSnapshotMarker = "<!-- BEGIN SNAPSHOT -->"
	endSnapshotMarker   = "<!-- END SNAPSHOT -->"
	upstreamRepo        = "coder/coder"
)

type options struct {
	sourceDocsRoot string
	destDocsRoot   string
	skillMDPath    string
	coderSHA       string
	snapshotOut    string
}

type manifest struct {
	Versions []string `json:"versions"`
	Routes   []route  `json:"routes"`
}

type route struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Path        string   `json:"path"`
	State       []string `json:"state"`
	Children    []route  `json:"children"`
}

type snapshotMetadata struct {
	UpstreamRepo string `json:"upstream_repo"`
	UpstreamSHA  string `json:"upstream_sha"`
	GeneratedAt  string `json:"generated_at"`
}

func main() {
	log.SetFlags(0)

	opts := parseFlags()
	if err := validatePreconditions(opts); err != nil {
		log.Fatalf("precondition failed: %v", err)
	}

	generatedAt := time.Now().UTC().Format(time.RFC3339)
	if err := run(opts, generatedAt); err != nil {
		log.Fatalf("generator failed: %v", err)
	}
}

func parseFlags() options {
	var opts options

	flag.StringVar(&opts.sourceDocsRoot, "source-docs-root", "", "path to upstream coder/coder/docs directory")
	flag.StringVar(&opts.destDocsRoot, "dest-docs-root", "", "destination path for synced docs snapshot")
	flag.StringVar(&opts.skillMDPath, "skill-md", "", "path to SKILL.md to inject generated sections into")
	flag.StringVar(&opts.coderSHA, "coder-sha", "", "upstream coder/coder commit SHA")
	flag.StringVar(&opts.snapshotOut, "snapshot-out", "", "path to write SNAPSHOT.json")
	flag.Parse()

	if flag.NArg() != 0 {
		log.Fatalf("unexpected positional arguments: %v", flag.Args())
	}

	if strings.TrimSpace(opts.sourceDocsRoot) == "" {
		log.Fatalf("missing required --source-docs-root flag")
	}
	if strings.TrimSpace(opts.destDocsRoot) == "" {
		log.Fatalf("missing required --dest-docs-root flag")
	}
	if strings.TrimSpace(opts.skillMDPath) == "" {
		log.Fatalf("missing required --skill-md flag")
	}
	if strings.TrimSpace(opts.coderSHA) == "" {
		log.Fatalf("missing required --coder-sha flag")
	}
	if strings.TrimSpace(opts.snapshotOut) == "" {
		log.Fatalf("missing required --snapshot-out flag")
	}

	if len(opts.coderSHA) < 12 {
		log.Fatalf("--coder-sha must be at least 12 characters, got %d", len(opts.coderSHA))
	}

	return opts
}

func validatePreconditions(opts options) error {
	if err := requireDirExists(opts.sourceDocsRoot, "--source-docs-root"); err != nil {
		return err
	}
	if err := requireFileExists(filepath.Join(opts.sourceDocsRoot, "manifest.json"), "manifest.json under --source-docs-root"); err != nil {
		return err
	}
	if err := requireFileExists(opts.skillMDPath, "--skill-md"); err != nil {
		return err
	}
	return nil
}

func run(opts options, generatedAt string) error {
	if err := syncDocsSnapshot(opts.sourceDocsRoot, opts.destDocsRoot); err != nil {
		return fmt.Errorf("sync docs snapshot: %w", err)
	}

	manifestPath := filepath.Join(opts.destDocsRoot, "manifest.json")
	m, err := parseManifest(manifestPath)
	if err != nil {
		return fmt.Errorf("parse synced manifest: %w", err)
	}

	docsTree, err := renderDocsTree(m.Routes, opts.destDocsRoot)
	if err != nil {
		return fmt.Errorf("render docs tree: %w", err)
	}

	if err := injectGeneratedSections(opts.skillMDPath, docsTree, opts.coderSHA, generatedAt); err != nil {
		return fmt.Errorf("inject generated sections into SKILL.md: %w", err)
	}

	if err := writeSnapshot(opts.snapshotOut, opts.coderSHA, generatedAt); err != nil {
		return fmt.Errorf("write snapshot json: %w", err)
	}

	return nil
}

func syncDocsSnapshot(sourceRoot, destRoot string) error {
	if err := os.RemoveAll(destRoot); err != nil {
		return fmt.Errorf("remove destination root %q: %w", destRoot, err)
	}
	if err := os.MkdirAll(destRoot, 0o755); err != nil {
		return fmt.Errorf("create destination root %q: %w", destRoot, err)
	}

	walkErr := filepath.WalkDir(sourceRoot, func(currentPath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(sourceRoot, currentPath)
		if err != nil {
			return fmt.Errorf("compute relative path for %q: %w", currentPath, err)
		}
		if relPath == "." {
			return nil
		}

		if hasPathComponent(relPath, "images") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			return nil
		}
		if !shouldCopyFile(currentPath) {
			return nil
		}

		destPath := filepath.Join(destRoot, relPath)
		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			return fmt.Errorf("create parent directory for %q: %w", destPath, err)
		}
		if err := copyTextFileWithNormalizedLF(currentPath, destPath); err != nil {
			return fmt.Errorf("copy %q -> %q: %w", currentPath, destPath, err)
		}
		return nil
	})
	if walkErr != nil {
		return fmt.Errorf("walk source docs root %q: %w", sourceRoot, walkErr)
	}

	if err := requireFileExists(filepath.Join(destRoot, "manifest.json"), "synced manifest.json"); err != nil {
		return err
	}

	return nil
}

func shouldCopyFile(filePath string) bool {
	base := filepath.Base(filePath)
	ext := strings.ToLower(filepath.Ext(base))
	return base == "manifest.json" || ext == ".md" || ext == ".json"
}

func hasPathComponent(relPath, component string) bool {
	parts := strings.Split(relPath, string(filepath.Separator))
	for _, part := range parts {
		if part == component {
			return true
		}
	}
	return false
}

func copyTextFileWithNormalizedLF(srcPath, destPath string) error {
	content, err := os.ReadFile(srcPath)
	if err != nil {
		return fmt.Errorf("read source file %q: %w", srcPath, err)
	}
	content = bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n"))
	if err := os.WriteFile(destPath, content, 0o644); err != nil {
		return fmt.Errorf("write destination file %q: %w", destPath, err)
	}
	return nil
}

func parseManifest(manifestPath string) (manifest, error) {
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return manifest{}, fmt.Errorf("read manifest %q: %w", manifestPath, err)
	}

	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return manifest{}, fmt.Errorf("decode manifest json: %w", err)
	}
	return m, nil
}

func renderDocsTree(routes []route, destDocsRoot string) (string, error) {
	lines := make([]string, 0, len(routes))
	if err := appendRouteLines(routes, destDocsRoot, 0, &lines); err != nil {
		return "", err
	}
	return strings.Join(lines, "\n"), nil
}

func appendRouteLines(routes []route, destDocsRoot string, level int, lines *[]string) error {
	for _, r := range routes {
		if routeStateContains(r.State, "hidden") {
			// Hidden routes and their descendants are intentionally omitted from the rendered tree.
			continue
		}

		routePath, err := normalizeRoutePath(r.Path)
		if err != nil {
			return fmt.Errorf("invalid route path for %q: %w", r.Title, err)
		}
		if err := assertRouteFileExists(destDocsRoot, routePath); err != nil {
			return err
		}

		title := strings.TrimSpace(r.Title)
		if title == "" {
			return fmt.Errorf("manifest route at path %q has empty title", routePath)
		}
		if len(r.Children) > 0 {
			title = "**" + title + "**"
		}

		line := fmt.Sprintf("%s- %s (`%s`) → `references/docs/%s`", strings.Repeat("  ", level), title, routePath, routePath)
		description := strings.TrimSpace(r.Description)
		if description != "" {
			line += " — " + description
		}
		*lines = append(*lines, line)

		if err := appendRouteLines(r.Children, destDocsRoot, level+1, lines); err != nil {
			return err
		}
	}
	return nil
}

func normalizeRoutePath(routePath string) (string, error) {
	trimmed := strings.TrimSpace(routePath)
	if trimmed == "" {
		return "", fmt.Errorf("path is empty")
	}

	trimmed = strings.TrimPrefix(trimmed, "./")
	cleanPath := path.Clean(trimmed)
	if cleanPath == "." {
		return "", fmt.Errorf("path %q resolves to current directory", routePath)
	}
	if strings.HasPrefix(cleanPath, "/") || strings.HasPrefix(cleanPath, "../") || strings.Contains(cleanPath, "/../") {
		return "", fmt.Errorf("path %q escapes docs root", routePath)
	}
	return cleanPath, nil
}

func assertRouteFileExists(destDocsRoot, routePath string) error {
	candidate := filepath.Join(destDocsRoot, filepath.FromSlash(routePath))
	rel, err := filepath.Rel(destDocsRoot, candidate)
	if err != nil {
		return fmt.Errorf("compute route relative path for %q: %w", routePath, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("route path %q escaped destination docs root %q", routePath, destDocsRoot)
	}

	info, err := os.Stat(candidate)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("manifest route path %q does not exist at %q", routePath, candidate)
		}
		return fmt.Errorf("stat manifest route path %q at %q: %w", routePath, candidate, err)
	}
	if info.IsDir() {
		return fmt.Errorf("manifest route path %q resolves to directory %q, expected file", routePath, candidate)
	}
	return nil
}

func injectGeneratedSections(skillMDPath, docsTree, coderSHA, generatedAt string) error {
	contentBytes, err := os.ReadFile(skillMDPath)
	if err != nil {
		return fmt.Errorf("read SKILL.md %q: %w", skillMDPath, err)
	}
	content := string(contentBytes)

	updated, err := replaceBetweenMarkers(content, beginDocsTreeMarker, endDocsTreeMarker, docsTree)
	if err != nil {
		return fmt.Errorf("replace DOCS_TREE block: %w", err)
	}

	snapshotText := fmt.Sprintf("- **Upstream**: `%s@%s`\n- **Generated**: `%s`", upstreamRepo, coderSHA[:12], generatedAt)
	updated, err = replaceBetweenMarkers(updated, beginSnapshotMarker, endSnapshotMarker, snapshotText)
	if err != nil {
		return fmt.Errorf("replace SNAPSHOT block: %w", err)
	}

	if err := os.WriteFile(skillMDPath, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("write SKILL.md %q: %w", skillMDPath, err)
	}
	return nil
}

func replaceBetweenMarkers(content, beginMarker, endMarker, replacement string) (string, error) {
	beginIdx := strings.Index(content, beginMarker)
	if beginIdx == -1 {
		return "", fmt.Errorf("missing begin marker %q", beginMarker)
	}
	if strings.LastIndex(content, beginMarker) != beginIdx {
		return "", fmt.Errorf("begin marker %q appears multiple times", beginMarker)
	}

	endIdx := strings.Index(content, endMarker)
	if endIdx == -1 {
		return "", fmt.Errorf("missing end marker %q", endMarker)
	}
	if strings.LastIndex(content, endMarker) != endIdx {
		return "", fmt.Errorf("end marker %q appears multiple times", endMarker)
	}

	sectionStart := beginIdx + len(beginMarker)
	if endIdx < sectionStart {
		return "", fmt.Errorf("marker order invalid: %q appears before %q", endMarker, beginMarker)
	}

	var b strings.Builder
	b.WriteString(content[:sectionStart])
	if !strings.HasSuffix(content[:sectionStart], "\n") {
		b.WriteString("\n")
	}

	trimmedReplacement := strings.TrimRight(replacement, "\n")
	if trimmedReplacement != "" {
		b.WriteString(trimmedReplacement)
		b.WriteString("\n")
	}

	b.WriteString(content[endIdx:])
	return b.String(), nil
}

func writeSnapshot(snapshotOut, coderSHA, generatedAt string) error {
	snapshot := snapshotMetadata{
		UpstreamRepo: upstreamRepo,
		UpstreamSHA:  coderSHA,
		GeneratedAt:  generatedAt,
	}

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("encode snapshot json: %w", err)
	}
	data = append(data, '\n')

	if err := os.MkdirAll(filepath.Dir(snapshotOut), 0o755); err != nil {
		return fmt.Errorf("create snapshot parent directory for %q: %w", snapshotOut, err)
	}
	if err := os.WriteFile(snapshotOut, data, 0o644); err != nil {
		return fmt.Errorf("write snapshot json %q: %w", snapshotOut, err)
	}
	return nil
}

func requireDirExists(pathValue, label string) error {
	info, err := os.Stat(pathValue)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s does not exist: %q", label, pathValue)
		}
		return fmt.Errorf("stat %s (%q): %w", label, pathValue, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s must be a directory: %q", label, pathValue)
	}
	return nil
}

// routeStateContains checks whether the state slice contains the given value.
func routeStateContains(states []string, target string) bool {
	for _, s := range states {
		if s == target {
			return true
		}
	}
	return false
}

func requireFileExists(pathValue, label string) error {
	info, err := os.Stat(pathValue)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s does not exist: %q", label, pathValue)
		}
		return fmt.Errorf("stat %s (%q): %w", label, pathValue, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s must be a file: %q", label, pathValue)
	}
	return nil
}
