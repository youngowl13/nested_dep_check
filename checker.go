package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"io/fs"
)

// ----------------------------------------------------------------------
// 1) Check or Install Python on Ubuntu
// ----------------------------------------------------------------------

// ensurePythonOnUbuntu checks if python3 is installed; if not, we do sudo apt-get update & install.
func ensurePythonOnUbuntu() error {
	// 1) Check presence of python3
	_, err := exec.LookPath("python3")
	if err == nil {
		log.Println("python3 is already installed.")
		return nil
	}

	log.Println("python3 not found, installing on Ubuntu via apt-get ...")
	cmd := exec.Command("sudo", "apt-get", "update")
	if e := cmd.Run(); e!=nil {
		return fmt.Errorf("sudo apt-get update failed: %v", e)
	}
	cmd2 := exec.Command("sudo", "apt-get", "install", "-y", "python3", "python3-venv", "python3-pip")
	if e := cmd2.Run(); e!=nil {
		return fmt.Errorf("sudo apt-get install python3 python3-venv python3-pip failed: %v", e)
	}
	return nil
}

// ----------------------------------------------------------------------
// 2) Copyleft + license-line helpers
// ----------------------------------------------------------------------

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

// remove ^ or ~ from version
func removeCaretTilde(ver string) string {
	ver = strings.TrimSpace(ver)
	return strings.TrimLeft(ver, "^~")
}

func findFile(root, target string) string {
	var found string
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err==nil && d.Name()==target {
			found= path
			return fs.SkipDir
		}
		return nil
	})
	return found
}

// ----------------------------------------------------------------------
// 3) Node logic: parse package.json => registry => fallback
// ----------------------------------------------------------------------

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
	if err!=nil || resp.StatusCode != 200 {
		return ""
	}
	defer resp.Body.Close()

	var lines []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		lines= append(lines, sc.Text())
	}
	if sc.Err()!=nil {
		return ""
	}
	for i:=0; i< len(lines); i++ {
		if strings.Contains(strings.ToLower(lines[i]), "license") {
			lic := parseLicenseLine(lines[i])
			if lic!="" {
				return lic
			}
			for j:= i+1; j< len(lines) && j<=i+10; j++ {
				lic2:= parseLicenseLine(lines[j])
				if lic2!=""{
					return lic2
				}
			}
		}
	}
	return ""
}

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

func parseNodeDependencies(path string) ([]*NodeDependency, error) {
	raw,err:= os.ReadFile(path)
	if err!=nil{return nil,err}
	var pkg map[string]interface{}
	if e:= json.Unmarshal(raw,&pkg); e!=nil{
		return nil,e
	}
	deps, _ := pkg["dependencies"].(map[string]interface{})
	if deps==nil {
		return nil, fmt.Errorf("no dependencies found in package.json")
	}
	visited := make(map[string]bool)
	var results []*NodeDependency
	for nm, ver := range deps {
		vstr,_:= ver.(string)
		nd,e := resolveNodeDependency(nm, removeCaretTilde(vstr), visited)
		if e==nil && nd!=nil {
			results= append(results, nd)
		}
	}
	return results,nil
}

func resolveNodeDependency(pkgName, version string, visited map[string]bool)(*NodeDependency,error){
	key:= pkgName+"@"+version
	if visited[key] {
		return nil,nil
	}
	visited[key]=true

	regURL := "https://registry.npmjs.org/"+pkgName
	resp,err:= http.Get(regURL)
	if err!=nil{ return nil, err }
	defer resp.Body.Close()

	var data map[string]interface{}
	if e:= json.NewDecoder(resp.Body).Decode(&data); e!=nil{
		return nil,e
	}
	if version=="" {
		if dist,ok:= data["dist-tags"].(map[string]interface{}); ok {
			if lat,ok:= dist["latest"].(string); ok {
				version= lat
			}
		}
	}
	license := "Unknown"
	var trans []*NodeDependency

	if vs,ok:= data["versions"].(map[string]interface{}); ok {
		if verData,ok:= vs[version].(map[string]interface{}); ok {
			license= findNpmLicense(verData)
			if deps,ok:= verData["dependencies"].(map[string]interface{}); ok {
				for subName, subVer := range deps {
					sv,_:= subVer.(string)
					ch,e2:= resolveNodeDependency(subName, removeCaretTilde(sv), visited)
					if e2==nil && ch!=nil {
						trans= append(trans, ch)
					}
				}
			}
		}
	}
	if license=="Unknown" {
		if fb:= fallbackNpmLicenseMultiLine(pkgName); fb!="" {
			license= fb
		}
	}
	nd:= &NodeDependency{
		Name: pkgName, Version: version, 
		License: license,
		Details: "https://www.npmjs.com/package/"+pkgName,
		Copyleft: isCopyleft(license),
		Transitive: trans,
		Language:"node",
	}
	return nd,nil
}

// ----------------------------------------------------------------------
// 4) Python approach: create venv, install pipdeptree, parse sub-sub BFS
// ----------------------------------------------------------------------

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

// create a local venv => pip install -r => pip install pipdeptree => pipdeptree --json-tree => parse BFS
func parsePythonDependencies(reqFile string)([]*PythonDependency, error){
	// We'll place the venv in venvpython
	venvDir := "venvpython"
	if _,err:= os.Stat(venvDir); err==nil {
		log.Printf("Removing existing venv: %s", venvDir)
		os.RemoveAll(venvDir)
	}
	// create venv: python3 -m venv venvpython
	cmdVenv := exec.Command("python3","-m","venv", venvDir)
	if e:= cmdVenv.Run(); e!=nil {
		return nil, fmt.Errorf("error creating venv: %v", e)
	}

	pipPath := filepath.Join(venvDir,"bin","pip")
	if _,err:= os.Stat(pipPath); err!=nil{
		return nil, fmt.Errorf("pip not found in venv: %v", err)
	}

	// install from requirements
	cmdInstall := exec.Command(pipPath, "install", "-r", reqFile)
	if e:= cmdInstall.Run(); e!=nil{
		return nil, fmt.Errorf("pip install -r %s failed: %v", reqFile, e)
	}

	// install pipdeptree
	cmdDepTree := exec.Command(pipPath, "install","pipdeptree")
	if e:= cmdDepTree.Run(); e!=nil{
		return nil, fmt.Errorf("pip install pipdeptree failed: %v", e)
	}

	// pipdeptree --json-tree
	pipdeptreeExe := filepath.Join(venvDir,"bin","pipdeptree")
	out, err := exec.Command(pipdeptreeExe,"--json-tree").Output()
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
			results= append(results, pd)
		}
	}
	return results,nil
}

func buildPyDepTree(node PyPackageNode, cache map[string]*PythonDependency, visited map[string]bool) *PythonDependency {
	nameUp := strings.ToUpper(node.Package.Key)
	if visited[nameUp] {
		if p,ok:= cache[nameUp]; ok {
			return p
		}
		return nil
	}
	visited[nameUp]=true

	license := "Unknown"  // If you'd like, you can parse local METADATA for real license.

	py := &PythonDependency{
		Name: node.Package.Key,
		Version: node.Package.InstalledVersion,
		License: license,
		Details: "local venv + pipdeptree",
		Copyleft: isCopyleft(license),
		Language:"python",
	}
	cache[nameUp]= py

	for _, child := range node.Dependencies {
		chDep:= buildPyDepTree(child, cache, visited)
		if chDep!=nil {
			py.Transitive= append(py.Transitive,chDep)
		}
	}
	return py
}

// ----------------------------------------------------------------------
// 5) Flatten + <details> expansions => single HTML
// ----------------------------------------------------------------------

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
		out=append(out, FlatDep{
			Name: nd.Name, Version: nd.Version, License: nd.License,
			Details: nd.Details, Language: nd.Language, Parent: parent,
		})
		if len(nd.Transitive)>0 {
			out= append(out, flattenNodeAll(nd.Transitive, nd.Name)...)
		}
	}
	return out
}

// Python flatten
func flattenPyAll(pds []*PythonDependency, parent string) []FlatDep {
	var out []FlatDep
	for _, pd := range pds {
		out= append(out, FlatDep{
			Name: pd.Name, Version: pd.Version, License: pd.License,
			Details: pd.Details, Language: pd.Language, Parent: parent,
		})
		if len(pd.Transitive)>0 {
			out= append(out, flattenPyAll(pd.Transitive, pd.Name)...)
		}
	}
	return out
}

// Node expansions
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
	if len(nodes)==0 {
		return "<p>No Node dependencies found.</p>"
	}
	var sb strings.Builder
	for _, nd:= range nodes {
		sb.WriteString(buildNodeTreeHTML(nd))
	}
	return sb.String()
}

// Python expansions sub-sub BFS
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
	if len(py)==0 {
		return "<p>No Python dependencies found or installed.</p>"
	}
	var sb strings.Builder
	for _, pd:= range py {
		sb.WriteString(buildPythonTreeHTML(pd))
	}
	return sb.String()
}

// single HTML
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
  <td>{{.Details}}</td>
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

// ----------------------------------------------------------------------
// 7) main
// ----------------------------------------------------------------------

func main() {
	// 1) Ensure python3 is installed on Ubuntu
	err := ensurePythonOnUbuntu()
	if err!=nil {
		log.Fatalf("Error ensuring python installed: %v", err)
	}

	// 2) Node approach
	nodeFile := findFile(".", "package.json")
	var nodeDeps []*NodeDependency
	if nodeFile != "" {
		nd, e := parseNodeDependencies(nodeFile)
		if e==nil {
			nodeDeps = nd
		} else {
			log.Println("Node parse error:", e)
		}
	}

	// 3) Python approach: check if we have requirements.txt or requirement.txt
	pyReq := findFile(".", "requirements.txt")
	if pyReq=="" {
		pyReq= findFile(".", "requirement.txt")
	}
	var pyDeps []*PythonDependency
	if pyReq!="" {
		pd, e2 := parsePythonDependencies(pyReq)
		if e2==nil {
			pyDeps= pd
		} else {
			log.Println("Python parse error:", e2)
		}
	}

	// 4) Flatten
	fn := flattenNodeAll(nodeDeps, "Direct")
	fp := flattenPyAll(pyDeps, "Direct")
	allDeps := append(fn, fp...)

	// 5) Count copyleft
	countCleft:=0
	for _, d:= range allDeps {
		if isCopyleft(d.License) {
			countCleft++
		}
	}
	summary := fmt.Sprintf("%d Node top-level deps, %d Python top-level packages (sub-sub BFS), Copyleft:%d",
		len(nodeDeps), len(pyDeps), countCleft)

	// 6) Build expansions
	nodeHTML:= buildNodeTreesHTML(nodeDeps)
	pyHTML  := buildPythonTreesHTML(pyDeps)

	// 7) Data => HTML
	data := struct{
		Summary string
		Deps []FlatDep
		NodeHTML template.HTML
		PyHTML template.HTML
	}{
		Summary: summary,
		Deps: allDeps,
		NodeHTML: template.HTML(nodeHTML),
		PyHTML: template.HTML(pyHTML),
	}

	tmpl, e:= template.New("report").Funcs(template.FuncMap{
		"isCopyleft": isCopyleft,
	}).Parse(reportTemplate)
	if e!=nil {
		log.Fatal("Template parse error:", e)
	}

	out, e2:= os.Create("dependency-license-report.html")
	if e2!=nil{
		log.Fatal("Create file error:", e2)
	}
	defer out.Close()

	if e3:= tmpl.Execute(out, data); e3!=nil {
		log.Fatal("Template exec error:", e3)
	}

	fmt.Println("dependency-license-report.html generated successfully!")
	fmt.Println("- On Ubuntu: if python3 missing, we do 'sudo apt-get update/install' automatically.")
	fmt.Println("- Then create venv, pip install -r, pip install pipdeptree => sub-sub BFS.")
	fmt.Println("- Node uses your multi-line fallback for sub-sub. Enjoy!")
}
