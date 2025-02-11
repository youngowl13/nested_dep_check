package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"io/fs"
)

// --------------------- Helper Functions ---------------------

// isCopyleft returns true if the license string contains any copyleft keywords.
func isCopyleft(license string) bool {
	copyleftLicenses := []string{
		"GPL",
		"GNU GENERAL PUBLIC LICENSE",
		"LGPL",
		"GNU LESSER GENERAL PUBLIC LICENSE",
		"AGPL",
		"GNU AFFERO GENERAL PUBLIC LICENSE",
		"MPL",
		"MOZILLA PUBLIC LICENSE",
		"CC-BY-SA",
		"CREATIVE COMMONS ATTRIBUTION-SHAREALIKE",
		"EPL",
		"ECLIPSE PUBLIC LICENSE",
		"OFL",
		"OPEN FONT LICENSE",
		"CPL",
		"COMMON PUBLIC LICENSE",
		"OSL",
		"OPEN SOFTWARE LICENSE",
	}
	license = strings.ToUpper(license)
	for _, kw := range copyleftLicenses {
		if strings.Contains(license, kw) {
			return true
		}
	}
	return false
}

// isCopyleftLicense is simply an alias for isCopyleft.
func isCopyleftLicense(license string) bool {
	return isCopyleft(license)
}

// ToUpper converts a string to uppercase.
func ToUpper(s string) string {
	return strings.ToUpper(s)
}

// findFile searches for the given target file starting at root.
func findFile(root, target string) string {
	var found string
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Name() == target {
			found = path
			return filepath.SkipDir
		}
		return nil
	})
	return found
}

// parseVariables scans file content for variable definitions (e.g., def cameraxVersion = "1.1.0-alpha05").
func parseVariables(content string) map[string]string {
	varMap := make(map[string]string)
	re := regexp.MustCompile(`(?m)^\s*def\s+(\w+)\s*=\s*["']([^"']+)["']`)
	matches := re.FindAllStringSubmatch(content, -1)
	for _, match := range matches {
		varMap[match[1]] = match[2]
	}
	return varMap
}

// --------------------- Node.js Dependency Resolution ---------------------

type NodeDependency struct {
	Name       string            `json:"name"`
	Version    string            `json:"version"`
	License    string            `json:"license"`
	Details    string            `json:"details"`
	Copyleft   bool              `json:"copyleft"`
	Transitive []*NodeDependency `json:"transitive,omitempty"`
	Language   string            `json:"language"`
}

func resolveNodeDependency(pkgName, version string, visited map[string]bool) (*NodeDependency, error) {
	key := pkgName + "@" + version
	if visited[key] {
		return nil, nil // cycle detected
	}
	visited[key] = true

	url := fmt.Sprintf("https://registry.npmjs.org/%s", pkgName)
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	ver := version
	if ver == "" {
		if dt, ok := data["dist-tags"].(map[string]interface{}); ok {
			if latest, ok := dt["latest"].(string); ok {
				ver = latest
			}
		}
	}
	versions, ok := data["versions"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("versions not found for %s", pkgName)
	}
	versionData, ok := versions[ver].(map[string]interface{})
	if !ok {
		if dt, ok := data["dist-tags"].(map[string]interface{}); ok {
			if latest, ok := dt["latest"].(string); ok {
				versionData, ok = versions[latest].(map[string]interface{})
				if ok {
					ver = latest
				} else {
					return nil, fmt.Errorf("version data not found for %s", pkgName)
				}
			}
		}
	}
	lic := "Unknown"
	if l, ok := versionData["license"].(string); ok {
		lic = l
	} else if l, ok := versionData["license"].(map[string]interface{}); ok {
		if t, ok := l["type"].(string); ok {
			lic = t
		}
	}
	details := fmt.Sprintf("https://www.npmjs.com/package/%s", pkgName)
	var trans []*NodeDependency
	if deps, ok := versionData["dependencies"].(map[string]interface{}); ok {
		for dep, depVerRange := range deps {
			depVer, ok := depVerRange.(string)
			if !ok {
				log.Printf("Warning: invalid version for dependency %s of %s, skipping", dep, pkgName)
				continue
			}
			tdep, err := resolveNodeDependency(dep, depVer, visited)
			if err != nil {
				log.Printf("Error resolving transitive dependency %s of %s: %v", dep, pkgName, err)
				continue
			}
			if tdep != nil {
				trans = append(trans, tdep)
			}
		}
	}
	return &NodeDependency{
		Name:       pkgName,
		Version:    ver,
		License:    lic,
		Details:    details,
		Copyleft:   isCopyleftLicense(lic),
		Transitive: trans,
		Language:   "node",
	}, nil
}

func parseNodeDependencies(filePath string) ([]*NodeDependency, error) {
	file, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	var data map[string]interface{}
	if err := json.Unmarshal(file, &data); err != nil {
		return nil, err
	}
	deps, ok := data["dependencies"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("no dependencies found in package.json")
	}
	var results []*NodeDependency
	visited := make(map[string]bool)
	for name, ver := range deps {
		versionStr, ok := ver.(string)
		if !ok {
			versionStr = ""
		}
		versionStr = strings.TrimPrefix(versionStr, "^")
		nodeDep, err := resolveNodeDependency(name, versionStr, visited)
		if err == nil && nodeDep != nil {
			results = append(results, nodeDep)
		}
	}
	return results, nil
}

// --------------------- Python Dependency Resolution ---------------------

type PythonDependency struct {
	Name       string              `json:"name"`
	Version    string              `json:"version"`
	License    string              `json:"license"`
	Details    string              `json:"details"`
	Copyleft   bool                `json:"copyleft"`
	Transitive []*PythonDependency `json:"transitive,omitempty"`
	Language   string              `json:"language"`
}

func resolvePythonDependency(pkgName, version string, visited map[string]bool) (*PythonDependency, error) {
	key := pkgName + "@" + version
	if visited[key] {
		return nil, nil // cycle detected
	}
	visited[key] = true
	url := fmt.Sprintf("https://pypi.org/pypi/%s/json", pkgName)
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("PyPI returned status: %s", resp.Status)
	}

	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	info, ok := data["info"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("info not found for %s", pkgName)
	}

	ver := version
	if ver == "" {
		if rel, ok := info["version"].(string); ok {
			ver = rel
		} else {
			return nil, fmt.Errorf("version not found for %s", pkgName)
		}
	}
	releases, ok := data["releases"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("releases not found for %s", pkgName)
	}
	if _, ok := releases[ver].([]interface{}); !ok {
		if rel, ok := info["version"].(string); ok {
			ver = rel
		} else {
			return nil, fmt.Errorf("version %s not found for %s", ver, pkgName)
		}
	}

	lic := "Unknown"
	if l, ok := info["license"].(string); ok {
		lic = l
	}

	details := url

	// For simplicity, transitive resolution for Python is not implemented fully.
	return &PythonDependency{
		Name:     pkgName,
		Version:  ver,
		License:  lic,
		Details:  details,
		Copyleft: isCopyleft(lic),
		Language: "python",
	}, nil
}

func parsePythonDependencies(filePath string) ([]*PythonDependency, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("could not open requirements.txt: %w", err)
	}
	defer file.Close()

	requirements, err := parseRequirements(file)
	if err != nil {
		return nil, err
	}

	var resolvedDeps []*PythonDependency
	var wg sync.WaitGroup
	depChan := make(chan PythonDependency)
	errChan := make(chan error)

	for _, req := range requirements {
		wg.Add(1)
		go func(r requirement) {
			defer wg.Done()
			dep, err := resolvePythonDependency(r.name, r.version, make(map[string]bool))
			if err != nil {
				errChan <- fmt.Errorf("resolving %s@%s: %w", r.name, r.version, err)
				return
			}
			if dep != nil {
				depChan <- *dep
			}
		}(req)
	}

	go func() {
		wg.Wait()
		close(depChan)
		close(errChan)
	}()

	for dep := range depChan {
		resolvedDeps = append(resolvedDeps, &dep)
	}
	for err := range errChan {
		log.Println(err)
	}

	return resolvedDeps, nil
}

type requirement struct {
	name    string
	version string // Simplified; doesn't handle complex specifiers
}

func parseRequirements(r io.Reader) ([]requirement, error) {
	var requirements []requirement
	content, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "==")
		if len(parts) != 2 {
			parts = strings.Split(line, ">=")
			if len(parts) != 2 {
				log.Printf("Warning: invalid requirement line: %s, skipping", line)
				continue
			}
		}
		name := strings.TrimSpace(parts[0])
		version := strings.TrimSpace(parts[1])
		requirements = append(requirements, requirement{name: name, version: version})
	}
	return requirements, nil
}

// --------------------- HTML Report Generation ---------------------

type ReportData struct {
	NodeDeps   []*NodeDependency
	PythonDeps []*PythonDependency
}

var reportTemplate = `
<!DOCTYPE html>
<html>
<head>
    <meta charset="UTF-8">
    <title>Dependency License Report</title>
    <style>
        body { font-family: Arial, sans-serif; margin: 20px; }
        h1 { color: #2c3e50; }
        ul { list-style-type: none; padding-left: 20px; }
        li { margin: 4px 0; border-left: 3px solid #ddd; padding-left: 10px; }
        .copyleft { color: #721c24; background-color: #f8d7da; padding: 2px 4px; border-radius: 3px; }
        .non-copyleft { color: #155724; background-color: #d4edda; padding: 2px 4px; border-radius: 3px; }
        .unknown { color: #856404; background-color: #fff3cd; padding: 2px 4px; border-radius: 3px; }
        .language { font-style: italic; color: #555; }
    </style>
</head>
<body>
    <h1>Dependency License Report</h1>
    <h2>Node.js Dependencies</h2>
    {{if .NodeDeps}}
        {{template "depList" .NodeDeps}}
    {{else}}
        <p>No Node.js dependencies found.</p>
    {{end}}
    <h2>Python Dependencies</h2>
    {{if .PythonDeps}}
        {{template "depList" .PythonDeps}}
    {{else}}
        <p>No Python dependencies found.</p>
    {{end}}
</body>
</html>

{{define "depList"}}
<ul>
{{range .}}
    <li>
        <strong>{{.Name}}@{{.Version}}</strong> - License:
        {{if .Copyleft}}<span class="copyleft">{{.License}}</span>{{else if eq .License "Unknown"}}<span class="unknown">{{.License}}</span>{{else}}<span class="non-copyleft">{{.License}}</span>{{end}}
        [<a href="{{.Details}}" target="_blank">Details</a>] <span class="language">({{.Language}})</span>
        {{if .Transitive}}
            {{template "depList" .Transitive}}
        {{end}}
    </li>
{{end}}
</ul>
{{end}}
`

func generateHTMLReport(data ReportData) error {
	tmpl, err := template.New("report").Parse(reportTemplate)
	if err != nil {
		return err
	}
	reportFile := "dependency-license-report.html"
	f, err := os.Create(reportFile)
	if err != nil {
		return err
	}
	defer f.Close()
	return tmpl.Execute(f, data)
}

// --------------------- Main ---------------------

func main() {
	// Locate Node.js package.json.
	nodeFile := findFile(".", "package.json")
	var nodeDeps []*NodeDependency
	if nodeFile != "" {
		nd, err := parseNodeDependencies(nodeFile)
		if err != nil {
			log.Println("Error parsing Node.js dependencies:", err)
		} else {
			nodeDeps = nd
		}
	}

	// Locate Python requirements file.
	pythonFile := findFile(".", "requirements.txt")
	if pythonFile == "" {
		pythonFile = findFile(".", "requirement.txt")
	}
	var pythonDeps []*PythonDependency
	if pythonFile != "" {
		pd, err := parsePythonDependencies(pythonFile)
		if err != nil {
			log.Println("Error parsing Python dependencies:", err)
		} else {
			pythonDeps = pd
		}
	}

	reportData := ReportData{
		NodeDeps:   nodeDeps,
		PythonDeps: pythonDeps,
	}

	if err := generateHTMLReport(reportData); err != nil {
		log.Println("Error generating report:", err)
		os.Exit(1)
	}
	fmt.Println("Dependency license report generated: dependency-license-report.html")
}
