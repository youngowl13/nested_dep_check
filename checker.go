package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html/template"
	"io/fs"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
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

// isCopyleftLicense is an alias to isCopyleft.
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

// parseVariables scans the file content for variable definitions.
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
}

func resolveNodeDependency(pkgName, version string, visited map[string]bool) (*NodeDependency, error) {
	key := pkgName + "@" + version
	if visited[key] {
		return nil, nil
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
		return nil, fmt.Errorf("version data not found for %s version %s", pkgName, ver)
	}
	lic := "Unknown"
	if l, ok := versionData["license"].(string); ok {
		lic = l
	}
	details := fmt.Sprintf("https://www.npmjs.com/package/%s", pkgName)
	var trans []*NodeDependency
	if deps, ok := versionData["dependencies"].(map[string]interface{}); ok {
		for dep, depVer := range deps {
			dv, ok := depVer.(string)
			if !ok {
				dv = ""
			}
			tdep, err := resolveNodeDependency(dep, dv, visited)
			if err == nil && tdep != nil {
				trans = append(trans, tdep)
			}
		}
	}
	nodeDep := &NodeDependency{
		Name:       pkgName,
		Version:    ver,
		License:    lic,
		Details:    details,
		Copyleft:   isCopyleftLicense(lic),
		Transitive: trans,
	}
	return nodeDep, nil
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
}

func resolvePythonDependency(pkgName, version string, visited map[string]bool) (*PythonDependency, error) {
	key := pkgName + "@" + version
	if visited[key] {
		return nil, nil
	}
	visited[key] = true
	url := fmt.Sprintf("https://pypi.org/pypi/%s/json", pkgName)
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	info, ok := data["info"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("info not found for %s", pkgName)
	}
	lic := "Unknown"
	if l, ok := info["license"].(string); ok {
		lic = l
	}
	details := url
	var trans []*PythonDependency
	if reqs, ok := info["requires_dist"].([]interface{}); ok {
		for _, r := range reqs {
			reqStr, ok := r.(string)
			if !ok {
				continue
			}
			parts := strings.Split(reqStr, " ")
			depName := parts[0]
			depVer := ""
			if len(parts) > 1 {
				depVer = strings.Trim(parts[1], " ()>=,<")
			}
			tdep, err := resolvePythonDependency(depName, depVer, visited)
			if err == nil && tdep != nil {
				trans = append(trans, tdep)
			}
		}
	}
	pyDep := &PythonDependency{
		Name:       pkgName,
		Version:    version,
		License:    lic,
		Details:    details,
		Copyleft:   isCopyleftLicense(lic),
		Transitive: trans,
	}
	return pyDep, nil
}

func parsePythonDependencies(filePath string) ([]*PythonDependency, error) {
	file, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(file), "\n")
	var results []*PythonDependency
	visited := make(map[string]bool)
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "==")
		pkgName := strings.TrimSpace(parts[0])
		versionStr := ""
		if len(parts) > 1 {
			versionStr = strings.TrimSpace(parts[1])
		}
		pyDep, err := resolvePythonDependency(pkgName, versionStr, visited)
		if err == nil && pyDep != nil {
			results = append(results, pyDep)
		}
	}
	return results, nil
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
        ul { list-style-type: none; }
        li { margin: 4px 0; }
        .copyleft { color: #721c24; background-color: #f8d7da; padding: 2px 4px; }
        .non-copyleft { color: #155724; background-color: #d4edda; padding: 2px 4px; }
        .unknown { color: #856404; background-color: #fff3cd; padding: 2px 4px; }
    </style>
</head>
<body>
    <h1>Dependency License Report</h1>
    <h2>Node.js Dependencies</h2>
    {{template "nodeList" .NodeDeps}}
    <h2>Python Dependencies</h2>
    {{template "pythonList" .PythonDeps}}
</body>
</html>

{{define "nodeList"}}
<ul>
{{range .}}
    <li>
        <strong>{{.Name}}@{{.Version}}</strong> - License: 
        {{if eq (ToUpper .License) "UNKNOWN"}}<span class="unknown">Unknown</span>{{else if isCopyleft .License}}<span class="copyleft">{{.License}}</span>{{else}}<span class="non-copyleft">{{.License}}</span>{{end}}
        [<a href="https://www.npmjs.com/package/{{.Name}}" target="_blank">Details</a>]
        {{if .Transitive}}
            {{template "nodeList" .Transitive}}
        {{end}}
    </li>
{{end}}
</ul>
{{end}}

{{define "pythonList"}}
<ul>
{{range .}}
    <li>
        <strong>{{.Name}}@{{.Version}}</strong> - License: 
        {{if eq (ToUpper .License) "UNKNOWN"}}<span class="unknown">Unknown</span>{{else if isCopyleft .License}}<span class="copyleft">{{.License}}</span>{{else}}<span class="non-copyleft">{{.License}}</span>{{end}}
        [<a href="{{.Details}}" target="_blank">Details</a>]
        {{if .Transitive}}
            {{template "pythonList" .Transitive}}
        {{end}}
    </li>
{{end}}
</ul>
{{end}}

{{define "ToUpper"}}{{. | ToUpper}}{{end}}
`

func generateHTMLReport(data ReportData) error {
	tmpl, err := template.New("report").Funcs(template.FuncMap{
		"ToUpper":           ToUpper,
		"isCopyleft":        isCopyleft,
		"isCopyleftLicense": isCopyleftLicense,
	}).Parse(reportTemplate)
	if err != nil {
		return fmt.Errorf("error parsing template: %v", err)
	}
	reportFile := "dependency-license-report.html"
	f, err := os.Create(reportFile)
	if err != nil {
		return fmt.Errorf("error creating report file: %v", err)
	}
	defer f.Close()
	if err := tmpl.Execute(f, data); err != nil {
		return fmt.Errorf("error executing template: %v", err)
	}
	fmt.Println("Dependency license report generated:", reportFile)
	return nil
}

// --------------------- Main ---------------------

func main() {
	// Locate Node.js package.json.
	nodeFile := findFile(".", "package.json")
	var nodeDeps []*NodeDependency
	if nodeFile != "" {
		nd, err := parseNodeDependencies(nodeFile)
		if err != nil {
			fmt.Println("Error parsing Node.js dependencies:", err)
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
			fmt.Println("Error parsing Python dependencies:", err)
		} else {
			pythonDeps = pd
		}
	}

	reportData := ReportData{
		NodeDeps:   nodeDeps,
		PythonDeps: pythonDeps,
	}

	if err := generateHTMLReport(reportData); err != nil {
		fmt.Println("Error generating report:", err)
		os.Exit(1)
	}
}
