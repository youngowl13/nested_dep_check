package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"io/fs"
)

// ---------------------------------------------------------------------------
// 1) findFile EXACTLY as you originally provided
// ---------------------------------------------------------------------------

func findFile(root, target string) string {
	var found string
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Name() == target {
			found = path
			return fs.SkipDir
		}
		return nil
	})
	return found
}

// ---------------------------------------------------------------------------
// 2) Utilities: isCopyleft, parseLicenseLine, removeCaretTilde
// ---------------------------------------------------------------------------

func isCopyleft(license string) bool {
	copyleftLicenses := []string{
		"GPL","GNU GENERAL PUBLIC LICENSE","LGPL","GNU LESSER GENERAL PUBLIC LICENSE",
		"AGPL","GNU AFFERO GENERAL PUBLIC LICENSE","MPL","MOZILLA PUBLIC LICENSE",
		"CC-BY-SA","CREATIVE COMMONS ATTRIBUTION-SHAREALIKE","EPL","ECLIPSE PUBLIC LICENSE",
		"OFL","OPEN FONT LICENSE","CPL","COMMON PUBLIC LICENSE","OSL","OPEN SOFTWARE LICENSE",
	}
	up := strings.ToUpper(license)
	for _, kw := range copyleftLicenses {
		if strings.Contains(up, kw) {
			return true
		}
	}
	return false
}

func parseLicenseLine(line string) string {
	known := []string{
		"MIT","ISC","BSD","APACHE","ARTISTIC","ZLIB","WTFPL","CDDL","UNLICENSE","EUPL",
		"MPL","CC0","LGPL","AGPL","BSD-2-CLAUSE","BSD-3-CLAUSE","X11",
	}
	up := strings.ToUpper(line)
	for _, kw := range known {
		if strings.Contains(up, kw) {
			return kw
		}
	}
	return ""
}

func removeCaretTilde(ver string) string {
	ver = strings.TrimSpace(ver)
	return strings.TrimLeft(ver, "^~")
}

// ---------------------------------------------------------------------------
// 3) Node BFS: parse package.json => sub-sub from registry => fallback
// ---------------------------------------------------------------------------

type NodeDependency struct {
	Name       string
	Version    string
	License    string
	Details    string
	Copyleft   bool
	Transitive []*NodeDependency
	Language   string
}

func fallbackNpmLicenseMultiLine(pkgName string) string {
	url := "https://www.npmjs.com/package/" + pkgName
	resp, err := http.Get(url)
	if err != nil || resp.StatusCode != 200 {
		return ""
	}
	defer resp.Body.Close()

	var lines []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if scanner.Err() != nil {
		return ""
	}
	for i := 0; i < len(lines); i++ {
		if strings.Contains(strings.ToLower(lines[i]), "license") {
			lic := parseLicenseLine(lines[i])
			if lic != "" {
				return lic
			}
			for j := i + 1; j < len(lines) && j <= i+10; j++ {
				lic2 := parseLicenseLine(lines[j])
				if lic2 != "" {
					return lic2
				}
			}
		}
	}
	return ""
}

func findNpmLicense(verData map[string]interface{}) string {
	if l, ok := verData["license"].(string); ok && l != "" {
		return l
	}
	if lm, ok := verData["license"].(map[string]interface{}); ok {
		if t, ok := lm["type"].(string); ok && t != "" {
			return t
		}
		if nm, ok := lm["name"].(string); ok && nm != "" {
			return nm
		}
	}
	if arr, ok := verData["licenses"].([]interface{}); ok && len(arr) > 0 {
		if obj, ok := arr[0].(map[string]interface{}); ok {
			if t, ok := obj["type"].(string); ok && t != "" {
				return t
			}
			if nm, ok := obj["name"].(string); ok && nm != "" {
				return nm
			}
		}
	}
	return "Unknown"
}

func parseNodeDependencies(nodeFile string) ([]*NodeDependency, error) {
	raw, err := os.ReadFile(nodeFile)
	if err != nil {
		return nil, err
	}
	var pkg map[string]interface{}
	if e := json.Unmarshal(raw, &pkg); e != nil {
		return nil, e
	}
	deps, _ := pkg["dependencies"].(map[string]interface{})
	if deps == nil {
		return nil, fmt.Errorf("no dependencies found in package.json")
	}
	visited := make(map[string]bool)
	var results []*NodeDependency
	for nm, ver := range deps {
		vstr, _ := ver.(string)
		nd, e := resolveNodeDependency(nm, removeCaretTilde(vstr), visited)
		if e == nil && nd != nil {
			results = append(results, nd)
		}
	}
	return results, nil
}

func resolveNodeDependency(pkgName, version string, visited map[string]bool) (*NodeDependency, error) {
	key := pkgName + "@" + version
	if visited[key] {
		return nil, nil
	}
	visited[key] = true

	url := "https://registry.npmjs.org/" + pkgName
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data map[string]interface{}
	if e := json.NewDecoder(resp.Body).Decode(&data); e != nil {
		return nil, e
	}
	if version == "" {
		if dist, ok := data["dist-tags"].(map[string]interface{}); ok {
			if lat, ok := dist["latest"].(string); ok {
				version = lat
			}
		}
	}
	license := "Unknown"
	var trans []*NodeDependency

	if vs, ok := data["versions"].(map[string]interface{}); ok {
		if verData, ok := vs[version].(map[string]interface{}); ok {
			license = findNpmLicense(verData)
			if deps, ok := verData["dependencies"].(map[string]interface{}); ok {
				for subName, subVer := range deps {
					sv, _ := subVer.(string)
					ch, e2 := resolveNodeDependency(subName, removeCaretTilde(sv), visited)
					if e2 == nil && ch != nil {
						trans = append(trans, ch)
					}
				}
			}
		}
	}
	if license == "Unknown" {
		if fb := fallbackNpmLicenseMultiLine(pkgName); fb != "" {
			license = fb
		}
	}
	nd := &NodeDependency{
		Name:       pkgName,
		Version:    version,
		License:    license,
		Details:    "https://www.npmjs.com/package/" + pkgName,
		Copyleft:   isCopyleft(license),
		Transitive: trans,
		Language:   "node",
	}
	return nd, nil
}

// ---------------------------------------------------------------------------
// 4) Python BFS: parse lines => BFS from PyPI => pass "" for subVer
// (with the new parsePyRequiresDistLine that discards constraints, environment markers, etc.)
// ---------------------------------------------------------------------------

type PythonDependency struct {
	Name       string
	Version    string
	License    string
	Details    string
	Copyleft   bool
	Transitive []*PythonDependency
	Language   string
}

func parsePythonDependencies(reqFile string) ([]*PythonDependency, error) {
	f, err := os.Open(reqFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	reqs, err := parseRequirements(f)
	if err != nil {
		return nil, err
	}
	visited := make(map[string]bool)
	var results []*PythonDependency
	for _, r := range reqs {
		d, e2 := resolvePythonDependency(r.name, r.version, visited)
		if e2 == nil && d != nil {
			results = append(results, d)
		} else if e2 != nil {
			log.Println("Python parse error for", r.name, ":", e2)
		}
	}
	return results, nil
}

type requirement struct {
	name, version string
}

func parseRequirements(r io.Reader) ([]requirement, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(raw), "\n")
	var out []requirement
	for _, line := range lines {
		sline := strings.TrimSpace(line)
		if sline == "" || strings.HasPrefix(sline, "#") {
			continue
		}
		p := strings.Split(sline, "==")
		if len(p) != 2 {
			p = strings.Split(sline, ">=")
			if len(p) != 2 {
				log.Println("Invalid python requirement line:", sline)
				continue
			}
		}
		nm := strings.TrimSpace(p[0])
		ver := strings.TrimSpace(p[1])
		out = append(out, requirement{nm, ver})
	}
	return out, nil
}

// parsePyRequiresDistLine => discards environment markers, version constraints
// to keep only the raw package name
func parsePyRequiresDistLine(line string) (string, string) {
	parts := strings.FieldsFunc(line, func(r rune) bool {
		// keep [a-zA-Z0-9._-], discard everything else
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			return false
		}
		return true
	})
	if len(parts) > 0 {
		name := strings.TrimSpace(parts[0])
		return name, ""
	}
	return "", ""
}

func resolvePythonDependency(pkgName, version string, visited map[string]bool) (*PythonDependency, error) {
	key := strings.ToLower(pkgName) + "@" + version
	if visited[key] {
		return nil, nil
	}
	visited[key] = true

	url := "https://pypi.org/pypi/" + pkgName + "/json"
	log.Printf("DEBUG: Fetching PyPI data for package: %s", pkgName)
	resp, err := http.Get(url)
	if err != nil {
		log.Printf("ERROR: HTTP GET error for package: %s: %v", pkgName, err)
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		log.Printf("ERROR: PyPI returned status %d for package: %s", resp.StatusCode, pkgName)
		return nil, fmt.Errorf("PyPI returned status: %d for package: %s", resp.StatusCode, pkgName)
	}
	var data map[string]interface{}
	if e := json.NewDecoder(resp.Body).Decode(&data); e != nil {
		log.Printf("ERROR: JSON decode error for package: %s: %v", pkgName, e)
		return nil, fmt.Errorf("JSON decode error from PyPI for package: %s: %w", pkgName, e)
	}
	info, _ := data["info"].(map[string]interface{})
	if info == nil {
		log.Printf("ERROR: 'info' section missing in PyPI data for %s", pkgName)
		return nil, fmt.Errorf("info section missing in PyPI data for %s", pkgName)
	}
	if version == "" {
		if v2, ok := info["version"].(string); ok {
			version = v2
		}
	}
	license := "Unknown"
	if l, ok := info["license"].(string); ok && l != "" {
		license = l
	} else {
		log.Printf("WARNING: License information not found on PyPI for package: %s@%s", pkgName, version)
	}

	var trans []*PythonDependency
	if distArr, ok := info["requires_dist"].([]interface{}); ok && len(distArr) > 0 {
		log.Printf("DEBUG: Processing requires_dist for package: %s@%s", pkgName, version)
		for _, x := range distArr {
			line, ok := x.(string)
			if !ok {
				log.Printf("WARNING: requires_dist item is not a string: %#v in package %s", x, pkgName)
				continue
			}
			subName, subVer := parsePyRequiresDistLine(line)
			if subName == "" {
				log.Printf("WARNING: parsePyRequiresDistLine failed for line: '%s' in package %s", line, pkgName)
				continue
			}
			log.Printf("DEBUG: Resolving transitive dependency: %s (discarded constraints: %s) of %s@%s", subName, subVer, pkgName, version)
			// pass "" for version in BFS call
			ch, e2 := resolvePythonDependency(subName, "", visited)
			if e2 != nil {
				log.Printf("ERROR: Error resolving transitive dependency %s of %s: %v", subName, pkgName, e2)
			}
			if e2 == nil && ch != nil {
				trans = append(trans, ch)
			}
		}
	} else {
		log.Printf("DEBUG: requires_dist missing or empty for package: %s@%s", pkgName, version)
	}

	py := &PythonDependency{
		Name:      pkgName,
		Version:   version,
		License:   license,
		Details:   "https://pypi.org/project/" + pkgName,
		Copyleft:  isCopyleft(license),
		Transitive: trans,
		Language:  "python",
	}
	return py, nil
}

// ---------------------------------------------------------------------------
// 5) Flatten + <details> expansions => single HTML
//  **CHANGES**: We set the parent param properly in flattenNodeAll, flattenPyAll
//  so that transitive dependencies appear with correct Parent name
// ---------------------------------------------------------------------------

type FlatDep struct {
	Name     string
	Version  string
	License  string
	Details  string
	Language string
	Parent   string
}

// Flatten Node
func flattenNodeAll(nds []*NodeDependency, parent string) []FlatDep {
	var out []FlatDep
	for _, nd := range nds {
		// for each direct child in 'nds', set its parent to the 'parent' param
		out = append(out, FlatDep{
			Name:    nd.Name,
			Version: nd.Version,
			License: nd.License,
			Details: nd.Details,
			Language: nd.Language,
			Parent: parent, // Ensure the parent is set here
		})
		if len(nd.Transitive) > 0 {
			// for each transitive child, we pass nd.Name as the new parent
			out = append(out, flattenNodeAll(nd.Transitive, nd.Name)...)
		}
	}
	return out
}

// Flatten Python
func flattenPyAll(pds []*PythonDependency, parent string) []FlatDep {
	var out []FlatDep
	for _, pd := range pds {
		out = append(out, FlatDep{
			Name:    pd.Name,
			Version: pd.Version,
			License: pd.License,
			Details: pd.Details,
			Language: pd.Language,
			Parent: parent, // ensure the parent is set here
		})
		if len(pd.Transitive) > 0 {
			// for each transitive child, we pass pd.Name as the new parent
			out = append(out, flattenPyAll(pd.Transitive, pd.Name)...)
		}
	}
	return out
}

func buildNodeTreeHTML(nd *NodeDependency) string {
	sum := fmt.Sprintf("%s@%s (License: %s)", nd.Name, nd.Version, nd.License)
	var sb strings.Builder
	sb.WriteString("<details><summary>")
	sb.WriteString(template.HTMLEscapeString(sum))
	sb.WriteString("</summary>\n")
	if len(nd.Transitive) > 0 {
		sb.WriteString("<ul>\n")
		for _, ch := range nd.Transitive {
			sb.WriteString("<li>")
			sb.WriteString(buildNodeTreeHTML(ch))
			sb.WriteString("</li>\n")
		}
		sb.WriteString("</ul>\n")
	}
	sb.WriteString("</details>\n")
	return sb.String()
}

func buildNodeTreesHTML(nodes []*NodeDependency) string {
	if len(nodes) == 0 {
		return "<p>No Node dependencies found.</p>"
	}
	var sb strings.Builder
	for _, nd := range nodes {
		sb.WriteString(buildNodeTreeHTML(nd))
	}
	return sb.String()
}

// expansions for Python BFS
func buildPythonTreeHTML(pd *PythonDependency) string {
	sum := fmt.Sprintf("%s@%s (License: %s)", pd.Name, pd.Version, pd.License)
	var sb strings.Builder
	sb.WriteString("<details><summary>")
	sb.WriteString(template.HTMLEscapeString(sum))
	sb.WriteString("</summary>\n")
	if len(pd.Transitive) > 0 {
		sb.WriteString("<ul>\n")
		for _, ch := range pd.Transitive {
			sb.WriteString("<li>")
			sb.WriteString(buildPythonTreeHTML(ch))
			sb.WriteString("</li>\n")
		}
		sb.WriteString("</ul>\n")
	}
	sb.WriteString("</details>\n")
	return sb.String()
}
func buildPythonTreesHTML(py []*PythonDependency) string {
	if len(py) == 0 {
		return "<p>No Python dependencies found.</p>"
	}
	var sb strings.Builder
	for _, pd := range py {
		sb.WriteString(buildPythonTreeHTML(pd))
	}
	return sb.String()
}

// The final HTML with color-coded table
var reportTemplate = `
<!DOCTYPE html>
<html>
<head>
<meta charset="UTF-8">
<title>Dependency License Report</title>
<style>
body{font-family:Arial,sans-serif;margin:20px}
h1,h2{color:#2c3e50}
table{width:100%;border-collapse:collapse;margin-bottom:20px}
th,td{border:1px solid #ddd;padding:8px;text-align:left}
th{background:#f2f2f2}
.copyleft{background:#f8d7da;color:#721c24}
.non-copyleft{background:#d4edda;color:#155724}
.unknown{background:#ffff99;color:#333}
details{margin:4px 0}
summary{cursor:pointer;font-weight:bold}
</style>
</head>
<body>
<h1>Dependency License Report</h1>

<h2>Summary</h2>
<p>{{.Summary}}</p>

<h2>Dependencies Table</h2>
<table>
<tr>
  <th>Name</th>
  <th>Version</th>
  <th>License</th>
  <th>Parent</th>
  <th>Language</th>
  <th>Details</th>
</tr>
{{range .Deps}}
<tr>
  <td>{{.Name}}</td>
  <td>{{.Version}}</td>
  <td class="{{if eq .License "Unknown"}}unknown{{else if isCopyleft .License}}copyleft{{else}}non-copyleft{{end}}">
    {{.License}}
  </td>
  <td>{{.Parent}}</td>
  <td>{{.Language}}</td>
  <td><a href="{{.Details}}" target="_blank">{{.Details}}</a></td>
</tr>
{{end}}
</table>

<h2>Node.js Dependencies</h2>
<div>
{{.NodeHTML}}
</div>

<h2>Python Dependencies</h2>
<div>
{{.PyHTML}}
</div>

</body>
</html>
`

func main() {
	// 1) Node approach
	nodeFile := findFile(".", "package.json")
	var nodeDeps []*NodeDependency
	if nodeFile != "" {
		nd, err := parseNodeDependencies(nodeFile)
		if err == nil {
			nodeDeps = nd
		} else {
			log.Println("Node parse error:", err)
		}
	}

	// 2) Python approach
	pyFile := findFile(".", "requirements.txt")
	if pyFile == "" {
		pyFile = findFile(".", "requirement.txt")
	}
	var pyDeps []*PythonDependency
	if pyFile != "" {
		pd, err := parsePythonDependencies(pyFile)
		if err == nil {
			pyDeps = pd
		} else {
			log.Println("Python parse error:", err)
		}
	}

	// 3) Flatten
	// The parent param is "Direct" for top-level
	fn := flattenNodeAll(nodeDeps, "Direct")
	fp := flattenPyAll(pyDeps, "Direct")
	allDeps := append(fn, fp...)

	// 4) Count copyleft
	copyleftCount := 0
	for _, d := range allDeps {
		if isCopyleft(d.License) {
			copyleftCount++
		}
	}
	summary := fmt.Sprintf("Node top-level: %d, Python top-level: %d, Copyleft: %d",
		len(nodeDeps), len(pyDeps), copyleftCount)

	// 5) expansions for the nested <details>
	nodeHTML := buildNodeTreesHTML(nodeDeps)
	pyHTML := buildPythonTreesHTML(pyDeps)

	data := struct {
		Summary string
		Deps    []FlatDep
		NodeHTML template.HTML
		PyHTML   template.HTML
	}{
		Summary: summary,
		Deps:    allDeps,
		NodeHTML: template.HTML(nodeHTML),
		PyHTML:   template.HTML(pyHTML),
	}

	tmpl, e := template.New("report").Funcs(template.FuncMap{
		"isCopyleft": isCopyleft,
	}).Parse(reportTemplate)
	if e != nil {
		log.Fatal("Template parse error:", e)
	}
	out, e2 := os.Create("dependency-license-report.html")
	if e2 != nil {
		log.Fatal("Create file error:", e2)
	}
	defer out.Close()

	if e3 := tmpl.Execute(out, data); e3 != nil {
		log.Fatal("Template exec error:", e3)
	}

	fmt.Println("dependency-license-report.html generated!")
}
