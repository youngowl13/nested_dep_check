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
	"sync"

	"io/fs"
)

// isCopyleft checks if the license text likely indicates a copyleft.
func isCopyleft(lic string) bool {
	copyleft := []string{
		"GPL","GNU GENERAL PUBLIC LICENSE","LGPL","GNU LESSER GENERAL PUBLIC LICENSE",
		"AGPL","GNU AFFERO GENERAL PUBLIC LICENSE","MPL","MOZILLA PUBLIC LICENSE",
		"CC-BY-SA","CREATIVE COMMONS ATTRIBUTION-SHAREALIKE","EPL","ECLIPSE PUBLIC LICENSE",
		"OFL","OPEN FONT LICENSE","CPL","COMMON PUBLIC LICENSE","OSL","OPEN SOFTWARE LICENSE",
	}
	up := strings.ToUpper(lic)
	for _, kw := range copyleft {
		if strings.Contains(up, kw) {
			return true
		}
	}
	return false
}

// findFile searches recursively for 'target'.
func findFile(root, target string) string {
	var found string
	filepath.WalkDir(root, func(path string,d fs.DirEntry,err error)error{
		if err==nil && d.Name()==target {
			found=path
			return filepath.SkipDir
		}
		return nil
	})
	return found
}

// removeCaretTilde strips leading ^ or ~ from version
func removeCaretTilde(ver string) string {
	ver=strings.TrimSpace(ver)
	return strings.TrimLeft(ver,"^~")
}

// fallbackNpmLicenseByHTML fetches https://www.npmjs.com/package/<pkgName>
// and scans line by line for "license" ignoring case. Then tries to extract known keywords.
func fallbackNpmLicenseByHTML(pkgName string) string {
	url := "https://www.npmjs.com/package/" + pkgName
	resp, err := http.Get(url)
	if err!=nil || resp.StatusCode!=200 {
		return ""
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(strings.ToLower(line), "license") {
			lic := parseLicenseLine(line)
			if lic!="" {
				return lic
			}
		}
	}
	return ""
}

// parseLicenseLine tries to spot a known license keyword in that line.
func parseLicenseLine(line string) string {
	known := []string{
		"MIT","BSD","APACHE","ISC","ARTISTIC","ZLIB","WTFPL","CDDL","UNLICENSE","EUPL",
		"MPL","CC0","LGPL","AGPL",
	}
	up := strings.ToUpper(line)
	for _,kw := range known {
		if strings.Contains(up, kw) {
			return kw
		}
	}
	return ""
}

// -------------------- Node Dependencies --------------------

type NodeDependency struct {
	Name       string
	Version    string
	License    string
	Details    string
	Copyleft   bool
	Transitive []*NodeDependency
	Language   string
}

func parseNodeDependencies(path string)([]*NodeDependency,error){
	raw,err:=os.ReadFile(path)
	if err!=nil{return nil,err}
	var data map[string]interface{}
	if err:=json.Unmarshal(raw,&data);err!=nil{return nil,err}
	deps, _ := data["dependencies"].(map[string]interface{})
	if deps==nil{
		return nil,fmt.Errorf("no dependencies found in package.json")
	}
	visited := map[string]bool{}
	var results []*NodeDependency
	for nm, ver := range deps {
		vstr, _ := ver.(string)
		d, e := resolveNodeDependency(nm, removeCaretTilde(vstr), visited)
		if e==nil && d!=nil{
			results=append(results,d)
		}
	}
	return results,nil
}

func resolveNodeDependency(pkgName, version string, visited map[string]bool)(*NodeDependency,error){
	key := pkgName+"@"+version
	if visited[key]{return nil,nil}
	visited[key]=true

	// fetch registry JSON
	url := "https://registry.npmjs.org/" + pkgName
	resp, err:= http.Get(url)
	if err!=nil{return nil,err}
	defer resp.Body.Close()

	var regData map[string]interface{}
	if err:=json.NewDecoder(resp.Body).Decode(&regData);err!=nil{
		return nil,err
	}
	if version==""{
		if dist, ok:=regData["dist-tags"].(map[string]interface{});ok{
			if lat,ok:=dist["latest"].(string);ok{
				version=lat
			}
		}
	}

	license := "Unknown"
	var trans []*NodeDependency

	if vs,ok:=regData["versions"].(map[string]interface{});ok{
		if verData,ok:=vs[version].(map[string]interface{});ok{
			license = findNpmLicense(verData)
			if deps,ok:=verData["dependencies"].(map[string]interface{});ok{
				for dName, dVer := range deps {
					dstr,_:= dVer.(string)
					child, e2 := resolveNodeDependency(dName, removeCaretTilde(dstr), visited)
					if e2==nil && child!=nil{
						trans=append(trans, child)
					}
				}
			}
		}
	}

	if license=="Unknown"{
		// fallback: fetch HTML from npm site, parse lines for "license"
		webLic := fallbackNpmLicenseByHTML(pkgName)
		if webLic!=""{
			license=webLic
		}
	}

	return &NodeDependency{
		Name: pkgName,
		Version: version,
		License: license,
		Details: "https://www.npmjs.com/package/"+pkgName,
		Copyleft: isCopyleft(license),
		Transitive: trans,
		Language:"node",
	},nil
}

func findNpmLicense(verData map[string]interface{})string{
	if l,ok:=verData["license"].(string);ok&&l!=""{
		return l
	}
	if lm,ok:=verData["license"].(map[string]interface{});ok{
		if t,ok:=lm["type"].(string);ok&&t!=""{return t}
		if n,ok:=lm["name"].(string);ok&&n!=""{return n}
	}
	if arr,ok:=verData["licenses"].([]interface{});ok&&len(arr)>0{
		if obj,ok:=arr[0].(map[string]interface{});ok{
			if t,ok:=obj["type"].(string);ok&&t!=""{return t}
			if n,ok:=obj["name"].(string);ok&&n!=""{return n}
		}
	}
	return "Unknown"
}

// -------------------- Python Dependencies --------------------

type PythonDependency struct {
	Name string
	Version string
	License string
	Details string
	Copyleft bool
	Transitive []*PythonDependency
	Language string
}

func parsePythonDependencies(path string)([]*PythonDependency, error){
	f,err:=os.Open(path)
	if err!=nil{return nil,err}
	defer f.Close()

	reqs, err:=parseRequirements(f)
	if err!=nil{return nil,err}
	var results []*PythonDependency
	var wg sync.WaitGroup
	depChan:=make(chan PythonDependency)
	errChan:=make(chan error)

	for _, r := range reqs {
		wg.Add(1)
		go func(nm,vr string){
			defer wg.Done()
			d,e:=resolvePythonDependency(nm,vr,map[string]bool{})
			if e!=nil{errChan<-e;return}
			if d!=nil{depChan<-*d}
		}(r.name,r.version)
	}
	go func(){
		wg.Wait()
		close(depChan)
		close(errChan)
	}()
	for d:=range depChan{
		results=append(results,&d)
	}
	for e:=range errChan{
		log.Println("Python parse error:", e)
	}
	return results,nil
}

func resolvePythonDependency(pkgName, version string, visited map[string]bool)(*PythonDependency,error){
	key:=pkgName+"@"+version
	if visited[key]{return nil,nil}
	visited[key]=true

	resp,err:=http.Get("https://pypi.org/pypi/"+pkgName+"/json")
	if err!=nil{return nil,err}
	defer resp.Body.Close()
	if resp.StatusCode!=200{
		return nil,fmt.Errorf("PyPI status:%d",resp.StatusCode)
	}
	var data map[string]interface{}
	if err:=json.NewDecoder(resp.Body).Decode(&data);err!=nil{
		return nil,err
	}
	info,_:=data["info"].(map[string]interface{})
	if info==nil{
		return nil,fmt.Errorf("info missing for %s",pkgName)
	}
	if version==""{
		if v2,ok:=info["version"].(string);ok{
			version=v2
		}
	}
	license:="Unknown"
	if l,ok:=info["license"].(string);ok&&l!=""{
		license=l
	}
	return &PythonDependency{
		Name:pkgName,
		Version:version,
		License:license,
		Details:"https://pypi.org/pypi/"+pkgName+"/json",
		Copyleft:isCopyleft(license),
		Language:"python",
	},nil
}

type requirement struct{name,version string}
func parseRequirements(r io.Reader)([]requirement,error){
	raw,err:=io.ReadAll(r)
	if err!=nil{return nil,err}
	lines:=strings.Split(string(raw),"\n")
	var out []requirement
	for _, ln := range lines {
		line:=strings.TrimSpace(ln)
		if line==""||strings.HasPrefix(line,"#"){
			continue
		}
		p:=strings.Split(line,"==")
		if len(p)!=2{
			p=strings.Split(line,">=")
			if len(p)!=2{
				log.Println("Invalid python requirement:", line)
				continue
			}
		}
		out=append(out, requirement{
			name:strings.TrimSpace(p[0]),
			version:strings.TrimSpace(p[1]),
		})
	}
	return out,nil
}

// Flatten for the summary table

type FlatDep struct {
	Name string
	Version string
	License string
	Details string
	Language string
	Parent string
}

func flattenNodeAll(nds []*NodeDependency, parent string) []FlatDep {
	var out []FlatDep
	for _, nd := range nds {
		out=append(out, FlatDep{
			Name:nd.Name,Version:nd.Version,License:nd.License,
			Details:nd.Details,Language:nd.Language,Parent:parent,
		})
		if len(nd.Transitive)>0{
			out=append(out, flattenNodeAll(nd.Transitive, nd.Name)...)
		}
	}
	return out
}
func flattenPyAll(pds []*PythonDependency, parent string) []FlatDep {
	var out []FlatDep
	for _, pd := range pds {
		out=append(out, FlatDep{
			Name:pd.Name,Version:pd.Version,License:pd.License,
			Details:pd.Details,Language:pd.Language,Parent:parent,
		})
		if len(pd.Transitive)>0{
			out=append(out, flattenPyAll(pd.Transitive, pd.Name)...)
		}
	}
	return out
}

// We'll show nested <details> for Node/Python so no script is needed.

func buildNodeTreeHTML(nd *NodeDependency) string {
	summary := fmt.Sprintf("%s@%s (License: %s)", nd.Name, nd.Version, nd.License)
	var sb strings.Builder
	sb.WriteString("<details>\n<summary>")
	sb.WriteString(template.HTMLEscapeString(summary))
	sb.WriteString("</summary>\n")

	if len(nd.Transitive)>0{
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
func buildPythonTreeHTML(pd *PythonDependency) string {
	summary := fmt.Sprintf("%s@%s (License: %s)", pd.Name, pd.Version, pd.License)
	var sb strings.Builder
	sb.WriteString("<details>\n<summary>")
	sb.WriteString(template.HTMLEscapeString(summary))
	sb.WriteString("</summary>\n")

	if len(pd.Transitive)>0{
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
func buildNodeTreesHTML(nodes []*NodeDependency) string {
	if len(nodes)==0{
		return "<p>No Node dependencies.</p>"
	}
	var sb strings.Builder
	for _,nd := range nodes {
		sb.WriteString(buildNodeTreeHTML(nd))
	}
	return sb.String()
}
func buildPythonTreesHTML(nodes []*PythonDependency) string {
	if len(nodes)==0{
		return "<p>No Python dependencies.</p>"
	}
	var sb strings.Builder
	for _,pd := range nodes {
		sb.WriteString(buildPythonTreeHTML(pd))
	}
	return sb.String()
}

// If no node deps => single stub node

type CombinedData struct{
	Summary string
	Deps []FlatDep
	NodeDeps []*NodeDependency
	PyDeps   []*PythonDependency
	NodeHTML string
	PyHTML   string
}

var reportTmpl = `<!DOCTYPE html>
<html>
<head>
<meta charset="UTF-8"/>
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
<th>Name</th><th>Version</th><th>License</th><th>Parent</th><th>Language</th><th>Details</th>
</tr>
{{range .Deps}}
<tr>
<td>{{.Name}}</td>
<td>{{.Version}}</td>
<td class="{{if eq .License "Unknown"}}unknown{{else if isCopyleft .License}}copyleft{{else}}non-copyleft{{end}}">
{{.License}}</td>
<td>{{.Parent}}</td>
<td>{{.Language}}</td>
<td><a href="{{.Details}}" target="_blank">View</a></td>
</tr>
{{end}}
</table>

<h2>Node.js Dependencies (Nested Tree)</h2>
<div>
{{.NodeHTML}}
</div>

<h2>Python Dependencies (Nested Tree)</h2>
<div>
{{.PyHTML}}
</div>

</body>
</html>`

func main(){
	// 1) Parse Node
	nodeFile:=findFile(".","package.json")
	var nodeDeps []*NodeDependency
	if nodeFile!=""{
		nd,err:=parseNodeDependencies(nodeFile)
		if err==nil{nodeDeps=nd}else{log.Println("Node parse error:",err)}
	}

	// 2) Parse Python
	pyFile:=findFile(".","requirements.txt")
	if pyFile==""{
		pyFile=findFile(".","requirement.txt")
	}
	var pyDeps []*PythonDependency
	if pyFile!=""{
		pd,err:=parsePythonDependencies(pyFile)
		if err==nil{pyDeps=pd}else{log.Println("Python parse error:",err)}
	}

	// 3) Flatten for table
	fn:=flattenNodeAll(nodeDeps,"Direct")
	fp:=flattenPyAll(pyDeps,"Direct")
	allFlat := append(fn, fp...)

	// 4) Count Copyleft
	countCleft:=0
	for _,dep:=range allFlat{
		if isCopyleft(dep.License){countCleft++}
	}
	summary:=fmt.Sprintf("%d direct Node.js deps, %d direct Python deps, Copyleft:%d", len(nodeDeps),len(pyDeps),countCleft)

	// 5) Build <details> nested HTML for Node/Python
	nodeTreeHTML := buildNodeTreesHTML(nodeDeps)
	pyTreeHTML   := buildPythonTreesHTML(pyDeps)

	// 6) Template data
	data := struct{
		Summary string
		Deps []FlatDep
		NodeHTML template.HTML
		PyHTML   template.HTML
	}{
		Summary: summary,
		Deps: allFlat,
		NodeHTML: template.HTML(nodeTreeHTML),
		PyHTML:   template.HTML(pyTreeHTML),
	}

	// 7) Render single HTML
	tmpl, err:= template.New("report").Funcs(template.FuncMap{"isCopyleft":isCopyleft}).Parse(reportTmpl)
	if err!=nil{
		log.Println("Template parse error:",err)
		os.Exit(1)
	}
	out, e2 := os.Create("dependency-license-report.html")
	if e2!=nil{
		log.Println("Create file error:", e2)
		os.Exit(1)
	}
	defer out.Close()

	if e3:=tmpl.Execute(out, data); e3!=nil{
		log.Println("Template exec error:", e3)
		os.Exit(1)
	}

	fmt.Println("dependency-license-report.html generated!")
	fmt.Println("No JS-based graph. We use <details> for nested expansions. Fallback license scraping with naive line approach from npm official repo.")
}
