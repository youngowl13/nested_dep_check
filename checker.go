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

// isCopyleftLicense is an alias for isCopyleft.
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

// parseVariables scans file content for variable definitions.
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

	// For simplicity, transitive resolution for Python is not fully implemented.
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

// --------------------- Flatten Dependencies ---------------------

type FlatDep struct {
	Name     string
	Version  string
	License  string
	Details  string
	Language string
	Parent   string
}

func flattenNodeDeps(nds []*NodeDependency, parent string) []FlatDep {
	var flats []FlatDep
	for _, nd := range nds {
		flat := FlatDep{
			Name:     nd.Name,
			Version:  nd.Version,
			License:  nd.License,
			Details:  nd.Details,
			Language: nd.Language,
			Parent:   parent,
		}
		flats = append(flats, flat)
		if len(nd.Transitive) > 0 {
			trans := flattenNodeDeps(nd.Transitive, nd.Name)
			flats = append(flats, trans...)
		}
	}
	return flats
}

func flattenPythonDeps(pds []*PythonDependency, parent string) []FlatDep {
	var flats []FlatDep
	for _, pd := range pds {
		flat := FlatDep{
			Name:     pd.Name,
			Version:  pd.Version,
			License:  pd.License,
			Details:  pd.Details,
			Language: pd.Language,
			Parent:   parent,
		}
		flats = append(flats, flat)
		if len(pd.Transitive) > 0 {
			trans := flattenPythonDeps(pd.Transitive, pd.Name)
			flats = append(flats, trans...)
		}
	}
	return flats
}

// --------------------- JSON for Graph Visualization ---------------------

func dependencyTreeJSON(nodeDeps []*NodeDependency, pythonDeps []*PythonDependency) (string, string, error) {
	// Wrap each dependency array in a dummy root so that D3.js has a single root.
	dummyNode := map[string]interface{}{
		"Name":       "Node.js Dependencies",
		"Version":    "",
		"Transitive": nodeDeps,
	}
	dummyPython := map[string]interface{}{
		"Name":       "Python Dependencies",
		"Version":    "",
		"Transitive": pythonDeps,
	}
	nodeJSONBytes, err := json.MarshalIndent(dummyNode, "", "  ")
	if err != nil {
		return "", "", err
	}
	pythonJSONBytes, err := json.MarshalIndent(dummyPython, "", "  ")
	if err != nil {
		return "", "", err
	}
	return string(nodeJSONBytes), string(pythonJSONBytes), nil
}

// --------------------- Copyleft Summary ---------------------

func hasCopyleftTransitiveNode(dep *NodeDependency) bool {
	for _, t := range dep.Transitive {
		if t.Copyleft || hasCopyleftTransitiveNode(t) {
			return true
		}
	}
	return false
}

func countCopyleftTransitivesNode(deps []*NodeDependency) int {
	count := 0
	for _, d := range deps {
		if hasCopyleftTransitiveNode(d) {
			count++
		}
	}
	return count
}

func hasCopyleftTransitivePython(dep *PythonDependency) bool {
	for _, t := range dep.Transitive {
		if t.Copyleft || hasCopyleftTransitivePython(t) {
			return true
		}
	}
	return false
}

func countCopyleftTransitivesPython(deps []*PythonDependency) int {
	count := 0
	for _, d := range deps {
		if hasCopyleftTransitivePython(d) {
			count++
		}
	}
	return count
}

// --------------------- Report Template Data and HTML Report Generation ---------------------

type ReportTemplateData struct {
	Summary         string
	FlatDeps        []FlatDep
	NodeTreeJSON    template.JS
	PythonTreeJSON  template.JS
}

var reportTemplate = `
<!DOCTYPE html>
<html>
<head>
    <meta charset="UTF-8">
    <title>Dependency License Report</title>
    <style>
        body { font-family: Arial, sans-serif; margin: 20px; }
        h1, h2 { color: #2c3e50; }
        table { width: 100%; border-collapse: collapse; margin-bottom: 20px; }
        th, td { border: 1px solid #ddd; padding: 8px; text-align: left; }
        th { background-color: #f2f2f2; }
        .copyleft { background-color: #ffcccc; color: #a10000; }
        .non-copyleft { background-color: #d4edda; color: #155724; }
        .unknown { background-color: #fff3cd; color: #856404; }
    </style>
</head>
<body>
    <h1>Dependency License Report</h1>
    <h2>Summary</h2>
    <p>{{.Summary}}</p>
    <h2>Dependencies Table</h2>
    <table>
        <tr>
            <th>Dependency</th>
            <th>Version</th>
            <th>License</th>
            <th>Parent Dependency</th>
            <th>Language</th>
            <th>Details</th>
        </tr>
        {{range .FlatDeps}}
        <tr>
            <td>{{.Name}}</td>
            <td>{{.Version}}</td>
            <td class="{{if eq .License "Unknown"}}unknown{{else if isCopyleft .License}}copyleft{{else}}non-copyleft{{end}}">
                {{.License}}
            </td>
            <td>{{.Parent}}</td>
            <td>{{.Language}}</td>
            <td><a href="{{.Details}}" target="_blank">View</a></td>
        </tr>
        {{end}}
    </table>
    <h2>Dependency Graph Visualization</h2>
    <p>The graphs below represent the dependency trees for Node.js and Python.</p>
    <h3>Node.js Dependency Tree</h3>
    <div id="nodeGraph"></div>
    <h3>Python Dependency Tree</h3>
    <div id="pythonGraph"></div>
    <script src="https://d3js.org/d3.v6.min.js"></script>
    <script>
    var nodeData = {{.NodeTreeJSON}};
    var pythonData = {{.PythonTreeJSON}};
    
    function renderTree(data, elementId) {
        var margin = {top: 20, right: 90, bottom: 30, left: 90},
            width = 660 - margin.left - margin.right,
            height = 500 - margin.top - margin.bottom;
    
        var svg = d3.select("#" + elementId).append("svg")
            .attr("width", width + margin.left + margin.right)
            .attr("height", height + margin.top + margin.bottom)
          .append("g")
            .attr("transform", "translate(" + margin.left + "," + margin.top + ")");
    
        var treemap = d3.tree().size([height, width]);
    
        var root = d3.hierarchy(data, function(d) { return d.Transitive; });
    
        root = treemap(root);
    
        var link = svg.selectAll(".link")
            .data(root.descendants().slice(1))
          .enter().append("path")
            .attr("class", "link")
            .attr("d", function(d) {
                return "M" + d.y + "," + d.x
                    + "C" + (d.parent.y + 50) + "," + d.x
                    + " " + (d.parent.y + 50) + "," + d.parent.x
                    + " " + d.parent.y + "," + d.parent.x;
            })
            .attr("fill", "none")
            .attr("stroke", "#ccc");
    
        var node = svg.selectAll(".node")
            .data(root.descendants())
          .enter().append("g")
            .attr("class", function(d) { 
                return "node" + (d.children ? " node--internal" : " node--leaf"); })
            .attr("transform", function(d) { 
                return "translate(" + d.y + "," + d.x + ")"; });
    
        node.append("circle")
            .attr("r", 10)
            .attr("fill", "#fff")
            .attr("stroke", "steelblue")
            .attr("stroke-width", "3");
    
        node.append("text")
            .attr("dy", ".35em")
            .attr("x", function(d) { return d.children ? -13 : 13; })
            .style("text-anchor", function(d) { 
                return d.children ? "end" : "start"; })
            .text(function(d) { return d.data.Name + "@" + d.data.Version; });
    }
    
    renderTree(nodeData, "nodeGraph");
    renderTree(pythonData, "pythonGraph");
    </script>
</body>
</html>
`

func dependencyTreeJSON(nodeDeps []*NodeDependency, pythonDeps []*PythonDependency) (string, string, error) {
	// Wrap each dependency array in a dummy root so that D3.js has a single root.
	dummyNode := map[string]interface{}{
		"Name":       "Node.js Dependencies",
		"Version":    "",
		"Transitive": nodeDeps,
	}
	dummyPython := map[string]interface{}{
		"Name":       "Python Dependencies",
		"Version":    "",
		"Transitive": pythonDeps,
	}
	nodeJSONBytes, err := json.MarshalIndent(dummyNode, "", "  ")
	if err != nil {
		return "", "", err
	}
	pythonJSONBytes, err := json.MarshalIndent(dummyPython, "", "  ")
	if err != nil {
		return "", "", err
	}
	return string(nodeJSONBytes), string(pythonJSONBytes), nil
}

// --------------------- Main Flattening ---------------------

type FlatDep struct {
	Name     string
	Version  string
	License  string
	Details  string
	Language string
	Parent   string
}

func flattenNodeDeps(nds []*NodeDependency, parent string) []FlatDep {
	var flats []FlatDep
	for _, nd := range nds {
		flat := FlatDep{
			Name:     nd.Name,
			Version:  nd.Version,
			License:  nd.License,
			Details:  nd.Details,
			Language: nd.Language,
			Parent:   parent,
		}
		flats = append(flats, flat)
		if len(nd.Transitive) > 0 {
			trans := flattenNodeDeps(nd.Transitive, nd.Name)
			flats = append(flats, trans...)
		}
	}
	return flats
}

func flattenPythonDeps(pds []*PythonDependency, parent string) []FlatDep {
	var flats []FlatDep
	for _, pd := range pds {
		flat := FlatDep{
			Name:     pd.Name,
			Version:  pd.Version,
			License:  pd.License,
			Details:  pd.Details,
			Language: pd.Language,
			Parent:   parent,
		}
		flats = append(flats, flat)
		if len(pd.Transitive) > 0 {
			trans := flattenPythonDeps(pd.Transitive, pd.Name)
			flats = append(flats, trans...)
		}
	}
	return flats
}

// --------------------- Copyleft Summary ---------------------

func hasCopyleftTransitiveNode(dep *NodeDependency) bool {
	for _, t := range dep.Transitive {
		if t.Copyleft || hasCopyleftTransitiveNode(t) {
			return true
		}
	}
	return false
}

func countCopyleftTransitivesNode(deps []*NodeDependency) int {
	count := 0
	for _, d := range deps {
		if hasCopyleftTransitiveNode(d) {
			count++
		}
	}
	return count
}

func hasCopyleftTransitivePython(dep *PythonDependency) bool {
	for _, t := range dep.Transitive {
		if t.Copyleft || hasCopyleftTransitivePython(t) {
			return true
		}
	}
	return false
}

func countCopyleftTransitivesPython(deps []*PythonDependency) int {
	count := 0
	for _, d := range deps {
		if hasCopyleftTransitivePython(d) {
			count++
		}
	}
	return count
}

// --------------------- Report Template Data and HTML Report Generation ---------------------

type ReportTemplateData struct {
	Summary         string
	FlatDeps        []FlatDep
	NodeTreeJSON    template.JS
	PythonTreeJSON  template.JS
}

func generateHTMLReport(data ReportTemplateData) error {
	tmpl, err := template.New("report").Funcs(template.FuncMap{
		"isCopyleft": isCopyleft,
	}).Parse(reportTemplate)
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

	// Flatten dependencies for the table.
	flatNode := flattenNodeDeps(nodeDeps, "Direct")
	flatPython := flattenPythonDeps(pythonDeps, "Direct")
	flatDeps := append(flatNode, flatPython...)

	// Create summary information.
	directNodeCount := len(nodeDeps)
	directPythonCount := len(pythonDeps)
	nodeCopyleftCount := countCopyleftTransitivesNode(nodeDeps)
	pythonCopyleftCount := countCopyleftTransitivesPython(pythonDeps)
	summary := fmt.Sprintf("%d direct Node.js dependencies (%d with transitive copyleft), %d direct Python dependencies (%d with transitive copyleft).",
		directNodeCount, nodeCopyleftCount, directPythonCount, pythonCopyleftCount)

	// Generate JSON for graph visualization.
	nodeJSON, pythonJSON, err := dependencyTreeJSON(nodeDeps, pythonDeps)
	if err != nil {
		log.Println("Error generating JSON for graph:", err)
		nodeJSON, pythonJSON = "[]", "[]"
	}

	reportData := ReportTemplateData{
		Summary:         summary,
		FlatDeps:        flatDeps,
		NodeTreeJSON:    template.JS(nodeJSON),
		PythonTreeJSON:  template.JS(pythonJSON),
	}

	if err := generateHTMLReport(reportData); err != nil {
		log.Println("Error generating report:", err)
		os.Exit(1)
	}
	fmt.Println("Dependency license report generated: dependency-license-report.html")
}
