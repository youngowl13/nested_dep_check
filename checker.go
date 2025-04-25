package main

import (
    "bufio"
    "encoding/json"
    "fmt"
    "html/template"
    "io"
    "io/fs"
    "log"
    "net/http"
    "os"
    "path/filepath"
    "sort"
    "strings"
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
        "GPL", "GNU GENERAL PUBLIC LICENSE", "LGPL", "GNU LESSER GENERAL PUBLIC LICENSE",
        "AGPL", "GNU AFFERO GENERAL PUBLIC LICENSE", "MPL", "MOZILLA PUBLIC LICENSE",
        "CC-BY-SA", "CREATIVE COMMONS ATTRIBUTION-SHAREALIKE", "EPL", "ECLIPSE PUBLIC LICENSE",
        "OFL", "OPEN FONT LICENSE", "CPL", "COMMON PUBLIC LICENSE", "OSL", "OPEN SOFTWARE LICENSE",
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
        "MIT", "ISC", "BSD", "APACHE", "ARTISTIC", "ZLIB", "WTFPL", "CDDL", "UNLICENSE", "EUPL",
        "MPL", "CC0", "LGPL", "AGPL", "BSD-2-CLAUSE", "BSD-3-CLAUSE", "X11",
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
            // check up to 10 lines below in case the text is spread out
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
    // If version is empty, use dist-tags.latest
    if version == "" {
        if dist, ok := data["dist-tags"].(map[string]interface{}); ok {
            if lat, ok := dist["latest"].(string); ok {
                version = lat
            }
        }
    }

    vs, _ := data["versions"].(map[string]interface{})
    if vs == nil {
        // no "versions" block => can't proceed
        return nil, nil
    }

    verData, ok := vs[version].(map[string]interface{})
    if !ok {
        // FALLBACK: if exact version doesn't exist (e.g. "1.0.1 || ^2.0.0"),
        // fallback to "latest"
        if dist, ok2 := data["dist-tags"].(map[string]interface{}); ok2 {
            if lat, ok2 := dist["latest"].(string); ok2 {
                if vMap, ok3 := vs[lat].(map[string]interface{}); ok3 {
                    log.Printf("Node fallback: Could not find exact version %s for %s, using 'latest' => %s",
                        version, pkgName, lat)
                    version = lat
                    verData = vMap
                    ok = true
                }
            }
        }
    }

    license := "Unknown"
    var trans []*NodeDependency

    if ok && verData != nil {
        license = findNpmLicense(verData)
        if deps, ok2 := verData["dependencies"].(map[string]interface{}); ok2 {
            for subName, subVer := range deps {
                sv, _ := subVer.(string)
                ch, e2 := resolveNodeDependency(subName, removeCaretTilde(sv), visited)
                if e2 == nil && ch != nil {
                    trans = append(trans, ch)
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
// 4) Python BFS with fallback: parse lines => BFS from PyPI => fallback
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
        // we handle "==" or ">="; ignoring everything else
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

// parsePyRequiresDistLine => discard environment markers, version constraints
// keep only the raw package name
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

    // If version not specified, use info["version"] (PyPI's "latest" in many cases)
    if version == "" {
        if v2, ok := info["version"].(string); ok {
            version = v2
        }
    }

    // --- NEW FALLBACK: If there's no release with the EXACT version,
    //     fall back to info["version"] (like "latest" in Node).
    releases, _ := data["releases"].(map[string]interface{})
    if releases != nil {
        if _, ok := releases[version]; !ok {
            // fallback to "latest" known to PyPI
            if infoVer, ok2 := info["version"].(string); ok2 && infoVer != "" {
                log.Printf("Python fallback: Could not find exact release %s for %s, using info.version => %s",
                    version, pkgName, infoVer)
                version = infoVer
            }
        }
    }

    // Now proceed with the BFS
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
            log.Printf("DEBUG: Resolving transitive dependency: %s (discarded constraints: %s) of %s@%s",
                subName, subVer, pkgName, version)
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
        Name:       pkgName,
        Version:    version,
        License:    license,
        Details:    "https://pypi.org/project/" + pkgName,
        Copyleft:   isCopyleft(license),
        Transitive: trans,
        Language:   "python",
    }
    return py, nil
}

// ---------------------------------------------------------------------------
// 5) Flatten with top-level tracking
// ---------------------------------------------------------------------------

type FlatDep struct {
    Name     string
    Version  string
    License  string
    Details  string
    Language string
    Parent   string
    TopLevel string
}

// Flatten Node (with top-level tracking)
func flattenNodeAllWithTop(nds []*NodeDependency) []FlatDep {
    var out []FlatDep
    for _, nd := range nds {
        // For each top-level Node dep, we set parent="Direct" and top=nd.Name
        out = append(out, flattenNodeOne(nd, "Direct", nd.Name)...)
    }
    return out
}

func flattenNodeOne(nd *NodeDependency, parent, top string) []FlatDep {
    fd := FlatDep{
        Name:     nd.Name,
        Version:  nd.Version,
        License:  nd.License,
        Details:  nd.Details,
        Language: nd.Language,
        Parent:   parent,
        TopLevel: top,
    }
    var out []FlatDep
    out = append(out, fd)
    for _, sub := range nd.Transitive {
        out = append(out, flattenNodeOne(sub, nd.Name, top)...)
    }
    return out
}

// Flatten Python (with top-level tracking)
func flattenPyAllWithTop(pds []*PythonDependency) []FlatDep {
    var out []FlatDep
    for _, pd := range pds {
        // For each top-level Python dep, we set parent="Direct" and top=pd.Name
        out = append(out, flattenPyOne(pd, "Direct", pd.Name)...)
    }
    return out
}

func flattenPyOne(pd *PythonDependency, parent, top string) []FlatDep {
    fd := FlatDep{
        Name:     pd.Name,
        Version:  pd.Version,
        License:  pd.License,
        Details:  pd.Details,
        Language: pd.Language,
        Parent:   parent,
        TopLevel: top,
    }
    var out []FlatDep
    out = append(out, fd)
    for _, sub := range pd.Transitive {
        out = append(out, flattenPyOne(sub, pd.Name, top)...)
    }
    return out
}

// ---------------------------------------------------------------------------
// BFS-expansion HTML for Node and Python
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Final HTML: two separate tables + BFS expansions + "Top-Level" column
// ---------------------------------------------------------------------------

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

<h2>Node Dependencies (from: {{.NodeFilePath}})</h2>
{{if eq (len .NodeDepsFlat) 0}}
<p>No Node dependencies found.</p>
{{else}}
<table>
<tr>
  <th>Name</th>
  <th>Version</th>
  <th>License</th>
  <th>Parent</th>
  <th>Top-Level</th>
  <th>Language</th>
  <th>Details</th>
</tr>
{{range .NodeDepsFlat}}
<tr>
  <td>{{.Name}}</td>
  <td>{{.Version}}</td>
  <td class="{{if eq .License "Unknown"}}unknown{{else if isCopyleft .License}}copyleft{{else}}non-copyleft{{end}}">
    {{.License}}
  </td>
  <td>{{.Parent}}</td>
  <td>{{.TopLevel}}</td>
  <td>{{.Language}}</td>
  <td><a href="{{.Details}}" target="_blank">{{.Details}}</a></td>
</tr>
{{end}}
</table>
{{end}}

<h3>Node BFS Expansions</h3>
<div>
{{.NodeHTML}}
</div>

<hr />

<h2>Python Dependencies (from: {{.PyFilePath}})</h2>
{{if eq (len .PyDepsFlat) 0}}
<p>No Python dependencies found.</p>
{{else}}
<table>
<tr>
  <th>Name</th>
  <th>Version</th>
  <th>License</th>
  <th>Parent</th>
  <th>Top-Level</th>
  <th>Language</th>
  <th>Details</th>
</tr>
{{range .PyDepsFlat}}
<tr>
  <td>{{.Name}}</td>
  <td>{{.Version}}</td>
  <td class="{{if eq .License "Unknown"}}unknown{{else if isCopyleft .License}}copyleft{{else}}non-copyleft{{end}}">
    {{.License}}
  </td>
  <td>{{.Parent}}</td>
  <td>{{.TopLevel}}</td>
  <td>{{.Language}}</td>
  <td><a href="{{.Details}}" target="_blank">{{.Details}}</a></td>
</tr>
{{end}}
</table>
{{end}}

<h3>Python BFS Expansions</h3>
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

    // 3) Flatten with top-level tracking
    nodeFlat := flattenNodeAllWithTop(nodeDeps)
    pyFlat := flattenPyAllWithTop(pyDeps)

    // 4) Sort each table so that copyleft first, unknown second, rest last
    sort.SliceStable(nodeFlat, func(i, j int) bool {
        li := nodeFlat[i].License
        lj := nodeFlat[j].License
        getGroup := func(l string) int {
            if isCopyleft(l) {
                return 1
            } else if l == "Unknown" {
                return 2
            }
            return 3
        }
        return getGroup(li) < getGroup(lj)
    })

    sort.SliceStable(pyFlat, func(i, j int) bool {
        li := pyFlat[i].License
        lj := pyFlat[j].License
        getGroup := func(l string) int {
            if isCopyleft(l) {
                return 1
            } else if l == "Unknown" {
                return 2
            }
            return 3
        }
        return getGroup(li) < getGroup(lj)
    })

    // 5) Build summary
    nodeTopCount := len(nodeDeps)
    pyTopCount := len(pyDeps)
    copyleftCount := 0
    for _, d := range nodeFlat {
        if isCopyleft(d.License) {
            copyleftCount++
        }
    }
    for _, d := range pyFlat {
        if isCopyleft(d.License) {
            copyleftCount++
        }
    }
    summary := fmt.Sprintf("Node top-level: %d, Python top-level: %d, Copyleft: %d",
        nodeTopCount, pyTopCount, copyleftCount)

    // 6) BFS expansions
    nodeHTML := buildNodeTreesHTML(nodeDeps)
    pyHTML := buildPythonTreesHTML(pyDeps)

    // 7) Execute final template
    data := struct {
        Summary      string
        NodeFilePath string
        PyFilePath   string
        NodeDepsFlat []FlatDep
        PyDepsFlat   []FlatDep
        NodeHTML     template.HTML
        PyHTML       template.HTML
    }{
        Summary:      summary,
        NodeFilePath: nodeFile,
        PyFilePath:   pyFile,
        NodeDepsFlat: nodeFlat,
        PyDepsFlat:   pyFlat,
        NodeHTML:     template.HTML(nodeHTML),
        PyHTML:       template.HTML(pyHTML),
    }

    tmpl, err := template.New("report").Funcs(template.FuncMap{
        "isCopyleft": isCopyleft,
    }).Parse(reportTemplate)
    if err != nil {
        log.Fatal("Template parse error:", err)
    }
    out, err := os.Create("dependency-license-report.html")
    if err != nil {
        log.Fatal("Create file error:", err)
    }
    defer out.Close()

    if err := tmpl.Execute(out, data); err != nil {
        log.Fatal("Template exec error:", err)
    }

    fmt.Println("dependency-license-report.html generated!")
}
