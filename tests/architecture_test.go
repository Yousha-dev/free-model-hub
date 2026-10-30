package tests

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// This file uses static analysis to guard layer boundaries, provider directory whitelists, and test layouts.

const vendorImportPrefix = "free-model-hub/internal/vendors/"

func TestNoCrossVendorImports(t *testing.T) {
	root := filepath.Join("..", "internal", "vendors")
	errWalk := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		owner := vendorOwner(path)
		if owner == "" {
			return nil
		}
		for _, imp := range fileImports(t, path) {
			if !strings.HasPrefix(imp, vendorImportPrefix) {
				continue
			}
			target := strings.Split(strings.TrimPrefix(imp, vendorImportPrefix), "/")[0]
			if target != owner {
				t.Errorf("%s: provider package %q must not import another provider package %q", path, owner, target)
			}
		}
		return nil
	})
	if errWalk != nil {
		t.Fatalf("walk vendors: %v", errWalk)
	}
}

func TestPluginAppVendorImportsOnlyInCompositionRoot(t *testing.T) {
	dir := filepath.Join("..", "internal", "app")
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		t.Fatalf("read app: %v", errRead)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		if entry.Name() == "vendors.go" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		for _, imp := range fileImports(t, path) {
			if strings.HasPrefix(imp, vendorImportPrefix) {
				t.Errorf("%s: only the composition root vendors.go can directly import specific providers", path)
			}
		}
	}
}

func vendorOwner(path string) string {
	parts := strings.Split(filepath.ToSlash(path), "/internal/vendors/")
	if len(parts) != 2 {
		return ""
	}
	return strings.Split(parts[1], "/")[0]
}

func fileImports(t *testing.T, path string) []string {
	t.Helper()
	file, errParse := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if errParse != nil {
		t.Fatalf("parse %s: %v", path, errParse)
	}
	out := make([]string, 0, len(file.Imports))
	for _, spec := range file.Imports {
		value, errUnquote := strconv.Unquote(spec.Path.Value)
		if errUnquote != nil {
			t.Fatalf("unquote import in %s: %v", path, errUnquote)
		}
		out = append(out, value)
	}
	return out
}

func TestTaskEngineVendorNeutral(t *testing.T) {
	dir := filepath.Join("..", "internal", "tasks")
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		t.Fatalf("read tasks: %v", errRead)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		for _, imp := range fileImports(t, path) {
			if strings.HasPrefix(imp, vendorImportPrefix) || imp == "free-model-hub/internal/admin" {
				t.Errorf("%s: task engine must not depend on providers or management interfaces (%s)", path, imp)
			}
		}
	}

	coreTask := filepath.Join("..", "internal", "core", "task.go")
	raw, errRead := os.ReadFile(coreTask)
	if errRead != nil {
		t.Fatalf("read core/task.go: %v", errRead)
	}
	lower := strings.ToLower(string(raw))
	for _, banned := range []string{"school", "growth"} {
		if strings.Contains(lower, banned) {
			t.Errorf("internal/core/task.go: generic task contract must not contain provider-specific identifiers %q", banned)
		}
	}
}

func TestCoreDoesNotImportUpperLayers(t *testing.T) {
	entries, errRead := os.ReadDir(filepath.Join("..", "internal", "core"))
	if errRead != nil {
		t.Fatalf("read core: %v", errRead)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join("..", "internal", "core", entry.Name())
		for _, imp := range fileImports(t, path) {
			if strings.HasPrefix(imp, "free-model-hub/internal/") {
				t.Errorf("%s: internal/core can only depend on cpasdk and standard library (found %s)", path, imp)
			}
		}
	}
}

func TestTestLayout(t *testing.T) {
	rootEntries, errRead := os.ReadDir(".")
	if errRead != nil {
		t.Fatalf("read tests root: %v", errRead)
	}
	for _, entry := range rootEntries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		if entry.Name() != "architecture_test.go" {
			t.Errorf("tests/%s: integration tests should be placed in tests/integration/, tests/ root should only keep architecture guards", entry.Name())
		}
	}

	integrationEntries, errRead := os.ReadDir(filepath.Join("integration"))
	if errRead != nil {
		t.Fatalf("read tests/integration: %v", errRead)
	}
	for _, entry := range integrationEntries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") && !strings.HasSuffix(entry.Name(), "_test.go") {
			t.Errorf("tests/integration/%s: integration test directory should not contain non-test source code", entry.Name())
		}
	}
}

// layerRule describes the dependency boundary of an internal package.
//
// An explicit table is used instead of individual ifs: when refactoring, adding or removing internal packages must update this table,
// so that "forgetting to update layer rules" fails immediately in CI rather than relying on human memory.
type layerRule struct {
	pkg string
	// forbidden lists internal packages that this package is **not allowed** to import (precise to the first-level directory).
	forbidden []string
	// When allowlist is non-empty, the import set must fall entirely within it (used for provider packages).
	allowlist []string
}

// layerRules defines the architectural layer contract of the repository.
//
// Dependency direction is top-down: app -> admin / tasks / auth / hostbridge / vendors -> core.
// Packages within the same layer do not depend on each other (admin and tasks, tasks and auth are both forbidden).
//
// Provider packages use allowlist instead of forbidden: their dependency surface must **precisely** be
// core (contracts) + hostbridge (host communication) + logger (logging), and nothing more—
// this rule blocks any possibility of vendors inversely depending on auth, state, tasks, or admin.
var layerRules = []layerRule{
	{pkg: "core", forbidden: []string{"admin", "app", "auth", "hostbridge", "logger", "state", "tasks", "vendors"}},
	{pkg: "logger"},
	{pkg: "state", forbidden: []string{"admin", "app", "auth", "hostbridge", "tasks", "vendors"}},
	{pkg: "hostbridge", forbidden: []string{"admin", "app", "auth", "state", "tasks", "vendors"}},
	{pkg: "auth", forbidden: []string{"admin", "app", "tasks", "vendors"}},
	{pkg: "tasks", forbidden: []string{"admin", "app", "vendors"}},
	{pkg: "admin", forbidden: []string{"app"}},
	{pkg: "vendors/autoclaw", allowlist: []string{"core", "hostbridge", "logger"}},
	{pkg: "vendors/cline", allowlist: []string{"core", "hostbridge", "logger"}},
	{pkg: "vendors/codearts", allowlist: []string{"core", "hostbridge", "logger"}},
	{pkg: "vendors/minimaxcode", allowlist: []string{"core", "hostbridge", "logger"}},
	{pkg: "vendors/opencodezen", allowlist: []string{"core", "hostbridge", "logger"}},
	{pkg: "vendors/openaicompat", allowlist: []string{"core", "hostbridge", "logger"}},
	{pkg: "vendors/opencodego", allowlist: []string{"core", "hostbridge", "logger", "ocgo"}},
	{pkg: "ocgo", forbidden: []string{"admin", "app", "auth", "core", "hostbridge", "logger", "state", "tasks", "vendors"}},
	{pkg: "vendors/qoder", allowlist: []string{"core", "hostbridge", "logger"}},
	{pkg: "vendors/raccoon", allowlist: []string{"core", "hostbridge", "logger"}},
	{pkg: "vendors/trae", allowlist: []string{"core", "hostbridge", "logger"}},
	{pkg: "vendors/workbuddy", allowlist: []string{"core", "hostbridge", "logger"}},
	{pkg: "vendors/zcode", allowlist: []string{"core", "hostbridge", "logger"}},
}

// internalPackages is a whitelist of allowed directory names directly under internal/.
//
// Its purpose is to **discover issues**: when a new package is added but its boundary is forgotten in layerRules,
// the whitelist intercepts it first and prompts for rule completion, rather than letting it quietly enter the dependency graph.
var internalPackages = map[string]bool{
	"admin": true, "app": true, "auth": true, "core": true,
	"hostbridge": true, "logger": true, "ocgo": true, "state": true, "tasks": true, "vendors": true,
}

func TestInternalPackageAllowlist(t *testing.T) {
	entries, errRead := os.ReadDir(filepath.Join("..", "internal"))
	if errRead != nil {
		t.Fatalf("read internal: %v", errRead)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !internalPackages[entry.Name()] {
			t.Errorf("internal/%s: new internal packages must sync internalPackages and layerRules (layer guards require explicit boundary declaration)", entry.Name())
		}
	}
}

// TestDeclaredPackagesHaveBoundaryRules ensures the whitelist and boundary rules back each other up:
//
//   - Non-composition-root packages in internalPackages must have boundaries defined in layerRules (otherwise adding
//     a package and only updating the whitelist would bypass dependency checks);
//   - Non-vendor packages in layerRules must also be in internalPackages (otherwise deleting
//     a whitelist entry leaves a directory under internal/ that is no longer declared).
//
// app is the composition root, and vendors are covered by individual provider entries; both are excluded by convention.
func TestDeclaredPackagesHaveBoundaryRules(t *testing.T) {
	covered := make(map[string]bool, len(layerRules))
	for _, rule := range layerRules {
		covered[strings.Split(rule.pkg, "/")[0]] = true
	}
	for pkg := range internalPackages {
		if pkg == "app" || pkg == "vendors" {
			continue
		}
		if !covered[pkg] {
			t.Errorf("internal/%s is allowlisted but has no layerRules boundary", pkg)
		}
	}
	for pkg := range covered {
		if pkg == "vendors" {
			continue
		}
		if !internalPackages[pkg] {
			t.Errorf("internal/%s has a layerRules boundary but is missing from internalPackages", pkg)
		}
	}
}

func TestLayerBoundaries(t *testing.T) {
	root := filepath.Join("..", "internal")
	for _, rule := range layerRules {
		dir := filepath.Join(root, filepath.FromSlash(rule.pkg))
		entries, errRead := os.ReadDir(dir)
		if errRead != nil {
			t.Fatalf("read %s: %v", rule.pkg, errRead)
		}
		banned := make(map[string]bool, len(rule.forbidden))
		for _, name := range rule.forbidden {
			banned[name] = true
		}
		allowed := make(map[string]bool, len(rule.allowlist))
		for _, name := range rule.allowlist {
			allowed[name] = true
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			for _, imp := range fileImports(t, path) {
				if !strings.HasPrefix(imp, "free-model-hub/internal/") {
					continue
				}
				target := strings.TrimPrefix(imp, "free-model-hub/internal/")
				if banned[strings.Split(target, "/")[0]] {
					t.Errorf("%s: internal/%s must not depend on internal/%s", path, rule.pkg, target)
				}
				if len(allowed) > 0 && !allowed[target] && !allowed[strings.Split(target, "/")[0]] {
					t.Errorf("%s: internal/%s can only depend on %v, actually depended on %s",
						path, rule.pkg, rule.allowlist, target)
				}
			}
		}
	}
}

// TestVendorsNeverWriteCredentialsDirectly guards that "there is only one path for credential write-back".
//
// The layer allowlist only restricts vendors to depending on core/hostbridge/logger, but hostbridge
// provides both HTTP bridges and host callbacks, so the import graph alone cannot tell if "this vendor is calling
// host.auth.save". Credential write-back (merge → host.auth.save → persistence) must be done
// uniformly by the keep-alive orchestration in internal/tasks, while providers only implement core.Refresher to refresh tokens—
// otherwise, every added provider would introduce a drifting write-back implementation.
func TestVendorsNeverWriteCredentialsDirectly(t *testing.T) {
	forbidden := []string{"MethodHostAuthSave", "MethodAuthSave", "host.auth.save"}
	root := filepath.Join("..", "internal", "vendors")
	errWalk := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, errRead := os.ReadFile(path)
		if errRead != nil {
			return errRead
		}
		body := stripComments(string(raw))
		for _, token := range forbidden {
			if strings.Contains(body, token) {
				t.Errorf("%s: providers must not write back credentials themselves (found %q) -- token refresh only implements core.Refresher, write-back is uniformly orchestrated by internal/tasks", path, token)
			}
		}
		return nil
	})
	if errWalk != nil {
		t.Fatalf("walk vendors: %v", errWalk)
	}
}

// ---------------------------------------------------------------------------
// Provider directory structure and file whitelist guard
// ---------------------------------------------------------------------------

// vendorRequiredFiles / vendorOptionalFiles are the closed whitelist for internal/vendors/<name>.
//
// Filenames define responsibilities: adding a new role requires updating this and the README "Architecture and Directory Standards" first, before writing code.
// There can be only one login.go; login channels must not be split into login_*.go source files. Tests and assets/ are handled separately.
var vendorRequiredFiles = []string{
	"adapter.go",
	"chat.go",
	"credential.go",
	"models.go",
	"errors.go",
	"login.go",
}

var vendorOptionalFiles = []string{
	"tasks.go",
}

var vendorDirNamePattern = regexp.MustCompile(`^[a-z][a-z0-9]*$`)

var embedDirectivePattern = regexp.MustCompile(`(?m)^//go:embed[ \t]+([^\r\n]+)$`)

// TestVendorDirectoryLayout guards the closed whitelist of the vendors directory.
//
// It checks: directory names, required files, login files, package names, and disallows subpackages/miscellaneous files/symlinks.
// When adding a new file role, vendorRequiredFiles/vendorOptionalFiles in this file and the README must be updated simultaneously,
// otherwise this test will fail—acting as the ratchet that prevents standards from drifting again.
func TestVendorDirectoryLayout(t *testing.T) {
	root, rootEntries := vendorRootEntries(t)
	for _, entry := range rootEntries {
		if !entry.IsDir() {
			t.Errorf("internal/vendors/%s: vendors root directory only allows provider subdirectories", entry.Name())
		}
	}

	allowed := slices.Concat(vendorRequiredFiles, vendorOptionalFiles)
	for _, vendor := range vendorDirectoryNames(rootEntries) {
		t.Run(vendor, func(t *testing.T) {
			if !vendorDirNamePattern.MatchString(vendor) {
				t.Errorf("internal/vendors/%s: directory name must be lowercase letters/numbers", vendor)
			}
			dir := filepath.Join(root, vendor)
			entries, errRead := os.ReadDir(dir)
			if errRead != nil {
				t.Fatalf("read %s: %v", dir, errRead)
			}
			present := make(map[string]bool, len(entries))
			for _, entry := range entries {
				name := entry.Name()
				entryPath := filepath.Join(dir, name)
				if entry.Type()&os.ModeSymlink != 0 {
					t.Errorf("%s: symbolic links are not allowed in vendor directory", entryPath)
					continue
				}
				if entry.IsDir() {
					if name != "assets" {
						t.Errorf("%s: vendor directory only allows assets/ subdirectory, found %q", dir, name)
					}
					continue
				}
				present[name] = true
				if strings.HasSuffix(name, "_test.go") {
					continue
				}
				if !strings.HasSuffix(name, ".go") {
					t.Errorf("%s: vendor directory only allows .go files and assets/, found %q", entryPath, name)
					continue
				}
				if strings.HasPrefix(name, "login_") && strings.HasSuffix(name, ".go") {
					t.Errorf("%s: login channels must be merged into a single login.go, login_*.go source files are forbidden", entryPath)
					continue
				}
				if slices.Contains(vendorRequiredFiles, name) || slices.Contains(vendorOptionalFiles, name) {
					continue
				}
				t.Errorf("%s: not in vendor file whitelist; allowed %v + *_test.go + assets/", entryPath, allowed)
			}
			for _, name := range vendorRequiredFiles {
				if !present[name] {
					t.Errorf("%s: missing required file %s", dir, name)
				}
			}
			assertVendorPackageNames(t, dir, vendor)
		})
	}
}

// TestVendorAssetsAreEmbedded guards the two-way consistency between assets/ and go:embed:
// every asset must be referenced, and every embed path must successfully match an asset.
func TestVendorAssetsAreEmbedded(t *testing.T) {
	root, rootEntries := vendorRootEntries(t)
	for _, vendor := range vendorDirectoryNames(rootEntries) {
		t.Run(vendor, func(t *testing.T) {
			dir := filepath.Join(root, vendor)
			assetsDir := filepath.Join(dir, "assets")
			info, errStat := os.Stat(assetsDir)
			if os.IsNotExist(errStat) {
				return
			}
			if errStat != nil {
				t.Fatalf("stat %s: %v", assetsDir, errStat)
			}
			if !info.IsDir() {
				t.Fatalf("%s: assets must be a directory", assetsDir)
			}
			assets := vendorAssetFiles(t, dir, assetsDir)
			if len(assets) == 0 {
				t.Errorf("%s: assets/ exists but has no files", assetsDir)
				return
			}
			patterns := vendorEmbedPatterns(t, dir)
			matched := make(map[string]bool, len(assets))
			for _, pattern := range patterns {
				if !safeEmbedPattern(t, dir, pattern) {
					continue
				}
				hit := false
				for _, asset := range assets {
					if embedPatternMatches(pattern, asset) {
						matched[asset] = true
						hit = true
					}
				}
				if !hit {
					t.Errorf("%s: go:embed %q did not match any files under assets/", dir, pattern)
				}
			}
			for _, asset := range assets {
				if !matched[asset] {
					t.Errorf("%s: assets file is not referenced by any go:embed", filepath.Join(dir, filepath.FromSlash(asset)))
				}
			}
		})
	}
}

// TestEveryVendorHasLayerRule ensures that "adding a vendor directory" cannot bypass dependency boundary checks.
//
// TestLayerBoundaries only iterates over layerRules; if a new vendor is not registered, its imports
// would be completely unconstrained. Here, the directory set is used to reversely validate layerRules and lock down the allowlist.
func TestEveryVendorHasLayerRule(t *testing.T) {
	dirs := vendorDirectories(t)
	rules := make(map[string]layerRule)
	for _, rule := range layerRules {
		if !strings.HasPrefix(rule.pkg, "vendors/") {
			continue
		}
		name := strings.TrimPrefix(rule.pkg, "vendors/")
		if strings.Contains(name, "/") {
			t.Errorf("layerRules: %s should no longer declare vendor subpackages; vendor must be a single package", rule.pkg)
			continue
		}
		if _, exists := rules[name]; exists {
			t.Errorf("layerRules: vendors/%s duplicate declaration", name)
			continue
		}
		rules[name] = rule
	}
	want := []string{"core", "hostbridge", "logger"}
	for _, dir := range dirs {
		rule, ok := rules[dir]
		if !ok {
			t.Errorf("internal/vendors/%s: missing layerRules boundary; new providers must explicitly declare allowlist %v", dir, want)
			continue
		}
		expected := want
		if dir == "opencodego" {
			// OpenCode Go additionally uses its ported protocol translators.
			expected = append(slices.Clone(want), "ocgo")
		}
		if !sameStringSet(rule.allowlist, expected) {
			t.Errorf("internal/vendors/%s: allowlist = %v, must be exactly %v", dir, rule.allowlist, want)
		}
	}
	stale := make([]string, 0)
	for name := range rules {
		if !slices.Contains(dirs, name) {
			stale = append(stale, name)
		}
	}
	slices.Sort(stale)
	for _, name := range stale {
		t.Errorf("layerRules declared vendors/%s, but the directory does not exist", name)
	}
}

// TestVendorRegistrationMatchesDirectories guards the composition root:
// Every vendor directory must be imported by internal/app/vendors.go and registered in BuiltinFactories.
func TestVendorRegistrationMatchesDirectories(t *testing.T) {
	dirs := vendorDirectories(t)
	appFile := filepath.Join("..", "internal", "app", "vendors.go")
	imported := make(map[string]bool)
	for _, imp := range fileImports(t, appFile) {
		if !strings.HasPrefix(imp, vendorImportPrefix) {
			continue
		}
		name := strings.Split(strings.TrimPrefix(imp, vendorImportPrefix), "/")[0]
		imported[name] = true
	}
	for _, dir := range dirs {
		if !imported[dir] {
			t.Errorf("internal/vendors/%s: directory exists but internal/app/vendors.go is not importing/registering it", dir)
		}
	}
	stale := make([]string, 0)
	for name := range imported {
		if !slices.Contains(dirs, name) {
			stale = append(stale, name)
		}
	}
	slices.Sort(stale)
	for _, name := range stale {
		t.Errorf("internal/app/vendors.go imported vendors/%s, but the directory does not exist", name)
	}
	body := functionBody(t, appFile, "BuiltinFactories")
	for _, dir := range dirs {
		if !strings.Contains(body, dir+".Instances") {
			t.Errorf("internal/app/vendors.go: BuiltinFactories has not registered %s.Instances", dir)
		}
	}
}

func vendorRootEntries(t *testing.T) (string, []os.DirEntry) {
	t.Helper()
	root := filepath.Join("..", "internal", "vendors")
	entries, errRead := os.ReadDir(root)
	if errRead != nil {
		t.Fatalf("read vendors: %v", errRead)
	}
	return root, entries
}

func vendorDirectories(t *testing.T) []string {
	t.Helper()
	_, entries := vendorRootEntries(t)
	return vendorDirectoryNames(entries)
}

func vendorDirectoryNames(entries []os.DirEntry) []string {
	dirs := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, entry.Name())
		}
	}
	slices.Sort(dirs)
	return dirs
}

func assertVendorPackageNames(t *testing.T, dir, want string) {
	t.Helper()
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		t.Fatalf("read %s: %v", dir, errRead)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		filePath := filepath.Join(dir, entry.Name())
		file, errParse := parser.ParseFile(token.NewFileSet(), filePath, nil, parser.PackageClauseOnly)
		if errParse != nil {
			t.Fatalf("parse %s: %v", filePath, errParse)
		}
		if file.Name.Name != want {
			t.Errorf("%s: package %q does not match directory name %q", filePath, file.Name.Name, want)
		}
	}
}

func vendorAssetFiles(t *testing.T, dir, assetsDir string) []string {
	t.Helper()
	assets := make([]string, 0)
	errWalk := filepath.Walk(assetsDir, func(assetPath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, errRel := filepath.Rel(dir, assetPath)
		if errRel != nil {
			return errRel
		}
		assets = append(assets, filepath.ToSlash(rel))
		return nil
	})
	if errWalk != nil {
		t.Fatalf("walk %s: %v", assetsDir, errWalk)
	}
	slices.Sort(assets)
	return assets
}

func vendorEmbedPatterns(t *testing.T, dir string) []string {
	t.Helper()
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		t.Fatalf("read %s: %v", dir, errRead)
	}
	patterns := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		filePath := filepath.Join(dir, entry.Name())
		raw, errReadFile := os.ReadFile(filePath)
		if errReadFile != nil {
			t.Fatalf("read %s: %v", filePath, errReadFile)
		}
		for _, match := range embedDirectivePattern.FindAllStringSubmatch(string(raw), -1) {
			for _, field := range strings.Fields(match[1]) {
				field = strings.Trim(field, "`\"")
				if field != "" {
					patterns = append(patterns, field)
				}
			}
		}
	}
	slices.Sort(patterns)
	return patterns
}

func safeEmbedPattern(t *testing.T, dir, pattern string) bool {
	t.Helper()
	invalid := strings.HasPrefix(pattern, "/") || filepath.IsAbs(pattern)
	if !invalid {
		for _, segment := range strings.Split(filepath.ToSlash(pattern), "/") {
			if segment == ".." {
				invalid = true
				break
			}
		}
	}
	if invalid {
		t.Errorf("%s: go:embed path must be a relative path within the vendor directory: %q", dir, pattern)
		return false
	}
	return true
}

func embedPatternMatches(pattern, asset string) bool {
	if matched, _ := path.Match(pattern, asset); matched {
		return true
	}
	// go:embed also accepts directory names; directories recursively include all files beneath them.
	prefix := strings.TrimSuffix(filepath.ToSlash(pattern), "/")
	return prefix != "" && strings.HasPrefix(asset, prefix+"/")
}

func sameStringSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	set := make(map[string]bool, len(got))
	for _, value := range got {
		set[value] = true
	}
	for _, value := range want {
		if !set[value] {
			return false
		}
	}
	return true
}

func functionBody(t *testing.T, filePath, name string) string {
	t.Helper()
	fset := token.NewFileSet()
	file, errParse := parser.ParseFile(fset, filePath, nil, 0)
	if errParse != nil {
		t.Fatalf("parse %s: %v", filePath, errParse)
	}
	raw, errRead := os.ReadFile(filePath)
	if errRead != nil {
		t.Fatalf("read %s: %v", filePath, errRead)
	}
	for _, decl := range file.Decls {
		fn, okFunc := decl.(*ast.FuncDecl)
		if !okFunc || fn.Name.Name != name || fn.Body == nil {
			continue
		}
		start := fset.Position(fn.Body.Lbrace).Offset
		end := fset.Position(fn.Body.Rbrace).Offset
		return string(raw[start:end])
	}
	t.Fatalf("%s：未找到函数 %s", filePath, name)
	return ""
}

// stripComments removes comments from Go source code, leaving only compilable code text.
//
// The guard needs to find symbols in the code, not in the comments explaining those symbols—otherwise
// if a developer explains in a comment "host.auth.save should not appear here", they will trip the guard themselves.
func stripComments(src string) string {
	var out strings.Builder
	for i := 0; i < len(src); {
		switch {
		case strings.HasPrefix(src[i:], "//"):
			end := strings.IndexByte(src[i:], '\n')
			if end < 0 {
				i = len(src)
			} else {
				i += end
			}
		case strings.HasPrefix(src[i:], "/*"):
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				i = len(src)
			} else {
				i += end + 4
			}
			out.WriteByte(' ')
		case src[i] == '\'' || src[i] == '"' || src[i] == '`':
			quote := src[i]
			out.WriteByte(quote)
			i++
			for i < len(src) {
				if quote != '`' && src[i] == '\\' {
					out.WriteByte(src[i])
					i++
					if i < len(src) {
						out.WriteByte(src[i])
						i++
					}
					continue
				}
				out.WriteByte(src[i])
				if src[i] == quote {
					i++
					break
				}
				i++
			}
		default:
			out.WriteByte(src[i])
			i++
		}
	}
	return out.String()
}
