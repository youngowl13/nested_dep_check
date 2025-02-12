package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"io/fs"
)

//--------------------------------------------------------------------------------
// 1) Utilities: isCopyleft, parseLicenseLine, removeCaretTilde, findNpmLicense
//--------------------------------------------------------------------------------

// isCopyleft checks if a license text indicates a copyleft license.
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

// parseLicenseLine tries to see if line has a known license substring
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

// remove ^ or ~ from version
func removeCaretTilde(ver string) string {
	ver = strings.TrimSpace(ver)
	return strings.TrimLeft(ver, "^~")
}

// findNpmLicense tries to read "license"/"licenses" in the registry "versions" data
func findNpmLicense(verData map[string]interface{}) string {
	if l,ok:= verData["license"].(string); ok && l!="" {
		return l
	}
	if lm,ok:= verData["license"].(map[string]interface{}); ok {
		if t,ok:= lm["type"].(string); ok && t!="" {
			return t
		}
		if nm,ok:= lm["name"].(string); ok && nm!="" {
			return nm
		}
	}
	if arr,ok:= verData["licenses"].([]interface{}); ok && len(arr)>0 {
		if obj,ok:= arr[0].(map[string]interface{}); ok {
			if t,ok:= obj["type"].(string); ok && t!="" {
				return t
			}
			if nm,ok:= obj["name"].(string); ok && nm!="" {
				return nm
			}
		}
	}
	return "Unknown"
}

//--------------------------------------------------------------------------------
// 2) findFile(...) - Locates target in root recursively
//--------------------------------------------------------------------------------

func findFile(root, target string) string {
	var found string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Name() == target {
			found = p
			return fs.SkipDir
		}
		return nil
	})
	return found
}

//--------------------------------------------------------------------------------
// 3) Node logic: read package.json -> registry -> fallback multi-line
//--------------------------------------------------------------------------------

type NodeDependency struct {
	Name       string
	Version    string
	License    string
	Details    string
	Copyleft   bool
	Transitive []*NodeDependency
	Language   string
}

// fallbackNpmLicenseMultiLine fetches npmjs.com/package/<pkg>, scanning up to 10 lines after "license"
func fallbackNpmLicenseMultiLine(pkgName string) string {
	url := "https://www.npmjs.com/package/" + pkgName
	resp, err := http.Get(url)
	if err != nil || resp.StatusCode != 200 {
		return ""
	}
	defer resp.Body.Close()

	var lines []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if sc.Err() != nil {
		return ""
	}
	for i := 0; i < len(lines); i++ {
		lower := strings.ToLower(lines[i])
		if strings.Contains(lower, "license") {
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

// parseNodeDependencies reads package.json's "dependencies", then calls registry.
func parseNodeDependencies(nodeFile string) ([]*NodeDependency, error) {
	raw, err := os.ReadFile(nodeFile)
	if err!=nil{return nil,err}
	var pkg map[string]interface{}
	if e:= json.Unmarshal(raw,&pkg); e!=nil{
		return nil,e
	}
	deps, _ := pkg["dependencies"].(map[string]interface{})
	if deps==nil{
		return nil, fmt.Errorf("no dependencies found in package.json")
	}
	visited := make(map[string]bool)
	var results []*NodeDependency
	for nm, ver := range deps {
		vstr, _ := ver.(string)
		nd, e := resolveNodeDependency(nm, removeCaretTilde(vstr), visited)
		if e==nil && nd!=nil {
			results=append(results, nd)
		}
	}
	return results,nil
}

func resolveNodeDependency(pkgName, version string, visited map[string]bool)(*NodeDependency, error){
	key := pkgName+"@"+version
	if visited[key] {
		return nil,nil
	}
	visited[key]=true

	regURL:= "https://registry.npmjs.org/"+pkgName
	resp,err:= http.Get(regURL)
	if err!=nil{return nil,err}
	defer resp.Body.Close()

	var data map[string]interface{}
	if e:= json.NewDecoder(resp.Body).Decode(&data); e!=nil{
		return nil,e
	}
	if version=="" {
		if dist, ok:= data["dist-tags"].(map[string]interface{}); ok {
			if lat, ok:= dist["latest"].(string); ok {
				version= lat
			}
		}
	}
	license:="Unknown"
	var trans []*NodeDependency

	if vs, ok:= data["versions"].(map[string]interface{}); ok {
		if verData, ok:= vs[version].(map[string]interface{}); ok {
			license= findNpmLicense(verData)
			if deps, ok:= verData["dependencies"].(map[string]interface{}); ok {
				for dName, dVer := range deps {
					sv,_:= dVer.(string)
					ch,e2:= resolveNodeDependency(dName, removeCaretTilde(sv), visited)
					if e2==nil && ch!=nil {
						trans=append(trans,ch)
					}
				}
			}
		}
	}
	if license=="Unknown" {
		lic2:= fallbackNpmLicenseMultiLine(pkgName)
		if lic2!="" {
			license= lic2
		}
	}
	nd:= &NodeDependency{
		Name: pkgName,
		Version: version,
		License: license,
		Details: "https://www.npmjs.com/package/"+pkgName,
		Copyleft: isCopyleft(license),
		Transitive: trans,
		Language:"node",
	}
	return nd, nil
}

//--------------------------------------------------------------------------------
// 4) Python logic: local environment sub-sub-dependencies via pipdeptree --json-tree
//--------------------------------------------------------------------------------

type PyPackageNode struct {
	Package struct {
		Key string `json:"key"`
		InstalledVersion string `json:"installed_version"`
	} `json:"package"`
	Dependencies []PyPackageNode `json:"dependencies"`
}

type PythonDependency struct {
	Name       string
	Version    string
	License    string
	Details    string
	Copyleft   bool
	Transitive []*PythonDependency
	Language   string
}

// parsePythonDependencies runs pipdeptree --json-tree, then rec build the tree. 
func parsePythonDependencies(_ string) ([]*PythonDependency, error) {
	cmd := exec.Command("pipdeptree", "--json-tree")
	out, err := cmd.Output()
	if err!=nil{
		return nil, fmt.Errorf("error calling pipdeptree: %v", err)
	}

	var topNodes []PyPackageNode
	if e:= json.Unmarshal(out,&topNodes); e!=nil{
		return nil, fmt.Errorf("unmarshal pipdeptree: %v", e)
	}

	cache := make(map[string]*PythonDependency)
	visited := make(map[string]bool)
	var results []*PythonDependency
	for _, tn := range topNodes {
		pd := buildPyDepTree(tn, cache, visited)
		if pd!=nil {
			results=append(results, pd)
		}
	}
	return results,nil
}

// buildPyDepTree transforms a PyPackageNode -> PythonDependency sub-tree
func buildPyDepTree(node PyPackageNode, cache map[string]*PythonDependency, visited map[string]bool) *PythonDependency {
	nameUp := strings.ToUpper(node.Package.Key)
	if visited[nameUp] {
		if p,ok:= cache[nameUp]; ok {
			return p
		}
		return nil
	}
	visited[nameUp]=true

	license := findPythonInstalledLicense(node.Package.Key)
	pd := &PythonDependency{
		Name: node.Package.Key,
		Version: node.Package.InstalledVersion,
		License: license,
		Details: "local python env - pipdeptree",
		Copyleft: isCopyleft(license),
		Language:"python",
	}
	cache[nameUp]= pd

	for _, child := range node.Dependencies {
		chDep:= buildPyDepTree(child, cache, visited)
		if chDep!=nil {
			pd.Transitive= append(pd.Transitive, chDep)
		}
	}
	return pd
}

// findPythonInstalledLicense does "pip show <pkg>" => parse "Location: ...", then read METADATA. 
func findPythonInstalledLicense(pkgName string) string {
	cmd := exec.Command("pip","show", pkgName)
	out, err := cmd.Output()
	if err!=nil{
		return "Unknown"
	}
	location:=""
	sc:= bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line:= sc.Text()
		if strings.HasPrefix(line,"Location: ") {
			location= strings.TrimSpace(strings.TrimPrefix(line,"Location: "))
			break
		}
	}
	if sc.Err()!=nil || location=="" {
		return "Unknown"
	}
	pkgLower:= strings.ToLower(pkgName)
	entries, err2:= os.ReadDir(location)
	if err2!=nil{
		return "Unknown"
	}
	for _, e := range entries {
		if !e.IsDir(){continue}
		dnLower:= strings.ToLower(e.Name())
		if strings.HasPrefix(dnLower, pkgLower+"-") && strings.Contains(dnLower,".dist-info") {
			metaPath:= filepath.Join(location, e.Name(), "METADATA")
			if st,er:= os.Stat(metaPath); er==nil && !st.IsDir(){
				lic:= parseMetadataLicense(metaPath)
				if lic!="" {
					return lic
				}
			}
			pkgInfo:= filepath.Join(location, e.Name(), "PKG-INFO")
			if st,er:= os.Stat(pkgInfo); er==nil && !st.IsDir(){
				lic:= parseMetadataLicense(pkgInfo)
				if lic!="" {
					return lic
				}
			}
		}
	}
	return "Unknown"
}

// parseMetadataLicense reads lines for "License:..." or "Classifier: License ::..."
func parseMetadataLicense(path string) string {
	f,err:= os.Open(path)
	if err!=nil{
		return ""
	}
	defer f.Close()

	sc:= bufio.NewScanner(f)
	license:=""
	for sc.Scan() {
		line:= sc.Text()
		if strings.HasPrefix(line,"License:") {
			val:= strings.TrimSpace(strings.TrimPrefix(line,"License:"))
			if val!=""{
				license= val
				break
			}
		} else if strings.HasPrefix(line,"Classifier: License ::") {
			val := strings.TrimSpace(strings.TrimPrefix(line,"Classifier:"))
			// e.g. "License :: OSI Approved :: MIT License"
			if val!="" {
				if lic := parseLicenseLine(val); lic!="" {
					license= lic
					break
				}
			}
		}
	}
	return license
}

/////////////////////////////////////////////////////////////////////////////
// 4) Flatten + <details> expansions => final HTML
/////////////////////////////////////////////////////////////////////////////

type FlatDep struct {
	Name     string
	Version  string
	License  string
	Details  string
	Language string
	Parent   string
}

// Node flatten
func flattenNodeAll(nds []*NodeDependency, parent string) []FlatDep {
	var out []FlatDep
	for _, nd := range nds {
		out = append(out, FlatDep{
			Name: nd.Name, Version: nd.Version, License: nd.License,
			Details: nd.Details, Language: nd.Language, Parent: parent,
		})
		if len(nd.Transitive) > 0 {
			out = append(out, flattenNodeAll(nd.Transitive, nd.Name)...)
		}
	}
	return out
}

// Python flatten
func flattenPyAll(pds []*PythonDependency, parent string) []FlatDep {
	var out []FlatDep
	for _, pd := range pds {
		out = append(out, FlatDep{
			Name: pd.Name, Version: pd.Version, License: pd.License,
			Details: pd.Details, Language: pd.Language, Parent: parent,
		})
		if len(pd.Transitive) > 0 {
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
	if len(nd.Transitive)>0 {
		sb.WriteString("<ul>\n")
		for _,ch:= range nd.Transitive {
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
	if len(nodes)==0{
		return "<p>No Node dependencies found.</p>"
	}
	var sb strings.Builder
	for _, nd:= range nodes {
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
	if len(pd.Transitive)>0 {
		sb.WriteString("<ul>\n")
		for _,ch:= range pd.Transitive {
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
	if len(py)==0{
		return "<p>No Python dependencies found.</p>"
	}
	var sb strings.Builder
	for _, pd := range py {
		sb.WriteString(buildPythonTreeHTML(pd))
	}
	return sb.String()
}

/////////////////////////////////////////////////////////////////////////////
// 5) Single HTML template
/////////////////////////////////////////////////////////////////////////////

var reportTemplate = `
<!DOCTYPE html>
<html>
<head>
<meta charset="UTF-8">
<title>Dependency License Report</title>
<style>
body{font-family:Arial,sans-serif;margin:20px;}
h1,h2{color:#2c3e50;}
table{width:100%;border-collapse:collapse;margin-bottom:20px;}
th,td{border:1px solid #ddd;padding:8px;text-align:left;}
th{background:#f2f2f2;}
.copyleft{background:#f8d7da;color:#721c24;}
.non-copyleft{background:#d4edda;color:#155724;}
.unknown{background:#ffff99;color:#333;}
details{margin:4px 0;}
summary{cursor:pointer;font-weight:bold;}
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
  <td><a href="{{.Details}}" target="_blank">View</a></td>
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

//--------------------------------------------------------------------------------
// 6) main
//--------------------------------------------------------------------------------

func main() {
	// 1) Node
	nodeFile := findFile(".", "package.json")
	var nodeDeps []*NodeDependency
	if nodeFile != "" {
		nd, err := parseNodeDependencies(nodeFile)
		if err==nil {
			nodeDeps= nd
		} else {
			log.Println("Node parse error:", err)
		}
	}

	// 2) Python
	pyDeps, err := parsePythonDependencies("requirements.txt")
	if err!=nil {
		log.Println("Python parse error:", err)
	}

	// 3) Flatten
	fn := flattenNodeAll(nodeDeps,"Direct")
	fp := flattenPyAll(pyDeps,"Direct")
	allDeps := append(fn, fp...)

	// 4) Count Copyleft
	countCopyleft := 0
	for _, d := range allDeps {
		if isCopyleft(d.License) {
			countCopyleft++
		}
	}
	summary := fmt.Sprintf("%d Node top-level deps, %d Python top-level packages from pipdeptree, copyleft:%d",
		len(nodeDeps), len(pyDeps), countCopyleft)

	// 5) Build expansions
	nodeHTML := buildNodeTreesHTML(nodeDeps)
	pyHTML := buildPythonTreesHTML(pyDeps)

	// 6) Final data
	data := struct {
		Summary string
		Deps    []FlatDep
		NodeHTML template.HTML
		PyHTML   template.HTML
	}{
		Summary: summary,
		Deps: allDeps,
		NodeHTML: template.HTML(nodeHTML),
		PyHTML: template.HTML(pyHTML),
	}

	// parse template
	tmpl, e := template.New("report").Funcs(template.FuncMap{
		"isCopyleft": isCopyleft,
	}).Parse(reportTemplate)
	if e!=nil {
		log.Fatal("Template parse error:", e)
	}

	out, e2 := os.Create("dependency-license-report.html")
	if e2!=nil {
		log.Fatal("Create file error:", e2)
	}
	defer out.Close()

	if e3 := tmpl.Execute(out, data); e3!=nil {
		log.Fatal("Template exec error:", e3)
	}

	fmt.Println("dependency-license-report.html generated successfully!")
	fmt.Println("- Node: multi-line fallback. Sub-sub from registry JSON.")
	fmt.Println("- Python: pipdeptree --json-tree for sub-sub, plus local METADATA license.")
	fmt.Println("- Unknown => no info found, red => copyleft, green => known license.")
}
