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

/////////////////////////////////////////////////////////////////
// 1) Utility: Copyleft detection & file-finding
/////////////////////////////////////////////////////////////////

// isCopyleft checks if license text likely indicates a copyleft license.
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

// findFile recursively locates 'target' starting from 'root'.
func findFile(root, target string) string {
	var found string
	filepath.WalkDir(root, func(path string,d fs.DirEntry,err error)error{
		if err==nil && d.Name()==target {
			found = path
			return filepath.SkipDir
		}
		return nil
	})
	return found
}

// removeCaretTilde strips ^ or ~ from version
func removeCaretTilde(ver string) string {
	ver = strings.TrimSpace(ver)
	return strings.TrimLeft(ver,"^~")
}

/////////////////////////////////////////////////////////////////
// 2) Node fallback: fetch the npm webpage, search near "license"
/////////////////////////////////////////////////////////////////

// fallbackNpmLicenseHTML fetches https://www.npmjs.com/package/<pkg>
// and tries to find "license" ignoring case. Then up to 500 chars after it
// we search for known license keywords (MIT, BSD, APACHE, etc.).
func fallbackNpmLicenseHTML(pkg string) string {
	url := "https://www.npmjs.com/package/" + pkg
	resp, err := http.Get(url)
	if err!=nil || resp.StatusCode!=200 {
		return ""
	}
	defer resp.Body.Close()

	// Read entire HTML into memory (like you do with `curl <pkg>`)
	body, err2 := io.ReadAll(resp.Body)
	if err2!=nil {
		return ""
	}

	lower := strings.ToLower(string(body))
	idx := strings.Index(lower, "license") // case-insensitive search
	if idx==-1 {
		return ""
	}
	// We'll check up to 500 characters after "license"
	end := idx+500
	if end>len(lower) {
		end = len(lower)
	}
	snippet := lower[idx:end]

	known := []string{"MIT","BSD","APACHE","ISC","ARTISTIC","ZLIB","WTFPL",
		"CDDL","UNLICENSE","EUPL","MPL","CC0","LGPL","AGPL"}
	for _,k := range known {
		// do a case-insensitive check:
		if strings.Contains(snippet, strings.ToLower(k)) {
			return k
		}
	}
	return ""
}

/////////////////////////////////////////////////////////////////
// 3) Node and Python structs & resolution logic
/////////////////////////////////////////////////////////////////

type NodeDependency struct {
	Name string
	Version string
	License string
	Details string
	Copyleft bool
	Transitive []*NodeDependency
	Language string
}

func parseNodeDependencies(path string) ([]*NodeDependency,error){
	raw,err := os.ReadFile(path)
	if err!=nil{return nil,err}
	var pkg map[string]interface{}
	if err:=json.Unmarshal(raw,&pkg);err!=nil{return nil,err}
	deps, _ := pkg["dependencies"].(map[string]interface{})
	if deps==nil{
		return nil, fmt.Errorf("no dependencies found in package.json")
	}
	visited := map[string]bool{}
	var out []*NodeDependency
	for nm,ver := range deps {
		vstr,_ := ver.(string)
		d,e:= resolveNodeDependency(nm, removeCaretTilde(vstr), visited)
		if e==nil && d!=nil{
			out=append(out,d)
		}
	}
	return out,nil
}

func resolveNodeDependency(pkgName,version string, visited map[string]bool)(*NodeDependency,error){
	key := pkgName+"@"+version
	if visited[key]{return nil,nil}
	visited[key]=true

	url := "https://registry.npmjs.org/" + pkgName
	resp, err := http.Get(url)
	if err!=nil{return nil,err}
	defer resp.Body.Close()

	var data map[string]interface{}
	if err:=json.NewDecoder(resp.Body).Decode(&data);err!=nil{
		return nil,err
	}
	if version=="" {
		if dist, ok:=data["dist-tags"].(map[string]interface{});ok{
			if lat, ok:=dist["latest"].(string);ok{
				version=lat
			}
		}
	}

	// Try the registry's known license first
	license := "Unknown"
	var trans []*NodeDependency

	if vs,ok:= data["versions"].(map[string]interface{});ok{
		if verData,ok:=vs[version].(map[string]interface{});ok{
			license = findNpmLicense(verData)
			// sub-dependencies
			if deps,ok:=verData["dependencies"].(map[string]interface{});ok{
				for nm,dv := range deps {
					dstr,_ := dv.(string)
					ch,e2 := resolveNodeDependency(nm, removeCaretTilde(dstr), visited)
					if e2==nil && ch!=nil{
						trans=append(trans, ch)
					}
				}
			}
		}
	}
	// fallback if unknown
	if license=="Unknown" {
		lic2 := fallbackNpmLicenseHTML(pkgName)
		if lic2!="" {
			license = lic2
		}
	}

	return &NodeDependency{
		Name: pkgName, Version: version, License: license,
		Details:"https://www.npmjs.com/package/"+pkgName,
		Copyleft: isCopyleft(license),
		Transitive: trans,
		Language:"node",
	},nil
}

func findNpmLicense(verData map[string]interface{}) string {
	if l,ok:=verData["license"].(string);ok && l!="" {
		return l
	}
	if lm,ok:=verData["license"].(map[string]interface{});ok {
		if t,ok:=lm["type"].(string);ok && t!="" {
			return t
		}
		if n,ok:=lm["name"].(string);ok && n!=""{
			return n
		}
	}
	if arr,ok:=verData["licenses"].([]interface{});ok && len(arr)>0 {
		if obj,ok:=arr[0].(map[string]interface{});ok {
			if t,ok:=obj["type"].(string);ok && t!="" {
				return t
			}
			if n,ok:=obj["name"].(string);ok && n!="" {
				return n
			}
		}
	}
	return "Unknown"
}

// Python:

type PythonDependency struct{
	Name string
	Version string
	License string
	Details string
	Copyleft bool
	Transitive []*PythonDependency
	Language string
}

func parsePythonDependencies(path string)([]*PythonDependency,error){
	f,err:=os.Open(path)
	if err!=nil{return nil,err}
	defer f.Close()

	reqs, err:=parseRequirements(f)
	if err!=nil{return nil,err}
	var results []*PythonDependency
	var wg sync.WaitGroup
	depChan:=make(chan PythonDependency)
	errChan:=make(chan error)

	for _, r:=range reqs {
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

func resolvePythonDependency(pkgName,version string, visited map[string]bool)(*PythonDependency,error){
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
		Name:pkgName,Version:version,License:license,
		Details:"https://pypi.org/pypi/"+pkgName+"/json",
		Copyleft: isCopyleft(license),
		Language:"python",
	},nil
}

type requirement struct{name,version string}
func parseRequirements(r io.Reader)([]requirement,error){
	raw,err:=io.ReadAll(r)
	if err!=nil{return nil,err}
	lines := strings.Split(string(raw),"\n")
	var out []requirement
	for _,ln:=range lines{
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

/////////////////////////////////////////////////////////////////
// 4) Flatten for table + nested <details> for the "graph"
/////////////////////////////////////////////////////////////////

type FlatDep struct {
	Name string
	Version string
	License string
	Details string
	Language string
	Parent string
}

func flattenNodeAll(nds []*NodeDependency, parent string)[]FlatDep{
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
func flattenPyAll(pds []*PythonDependency, parent string)[]FlatDep{
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

// Build nested <details> tree for Node

func buildNodeTreeHTML(nd *NodeDependency) string {
	summary := fmt.Sprintf("%s@%s (License: %s)", nd.Name, nd.Version, nd.License)
	var sb strings.Builder
	sb.WriteString("<details>\n<summary>")
	sb.WriteString(template.HTMLEscapeString(summary))
	sb.WriteString("</summary>\n")

	if len(nd.Transitive)>0 {
		sb.WriteString("<ul>\n")
		for _,ch:=range nd.Transitive{
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
	for _,nd:=range nodes{
		sb.WriteString(buildNodeTreeHTML(nd))
	}
	return sb.String()
}

// Build nested <details> tree for Python

func buildPythonTreeHTML(pd *PythonDependency) string {
	summary := fmt.Sprintf("%s@%s (License: %s)", pd.Name, pd.Version, pd.License)
	var sb strings.Builder
	sb.WriteString("<details>\n<summary>")
	sb.WriteString(template.HTMLEscapeString(summary))
	sb.WriteString("</summary>\n")

	if len(pd.Transitive)>0 {
		sb.WriteString("<ul>\n")
		for _,ch:=range pd.Transitive{
			sb.WriteString("<li>")
			sb.WriteString(buildPythonTreeHTML(ch))
			sb.WriteString("</li>\n")
		}
		sb.WriteString("</ul>\n")
	}
	sb.WriteString("</details>\n")
	return sb.String()
}
func buildPythonTreesHTML(pyDeps []*PythonDependency) string {
	if len(pyDeps)==0{
		return "<p>No Python dependencies found.</p>"
	}
	var sb strings.Builder
	for _,pd:=range pyDeps{
		sb.WriteString(buildPythonTreeHTML(pd))
	}
	return sb.String()
}

/////////////////////////////////////////////////////////////////
// 5) Single HTML with summary table + <details> expansions
/////////////////////////////////////////////////////////////////

var reportTemplate=`
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
<tr><th>Name</th><th>Version</th><th>License</th><th>Parent</th><th>Language</th><th>Details</th></tr>
{{range .Deps}}
<tr>
<td>{{.Name}}</td>
<td>{{.Version}}</td>
<td class="{{if eq .License "Unknown"}}unknown{{else if isCopyleft .License}}copyleft{{else}}non-copyleft{{end}}">{{.License}}</td>
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

func main(){
	// 1) Parse Node
	nf := findFile(".","package.json")
	var nodeDeps []*NodeDependency
	if nf!=""{
		nd,err:=parseNodeDependencies(nf)
		if err==nil{nodeDeps=nd}else{log.Println("Node parse error:", err)}
	}
	// 2) Parse Python
	pf := findFile(".","requirements.txt")
	if pf==""{
		pf=findFile(".","requirement.txt")
	}
	var pyDeps []*PythonDependency
	if pf!=""{
		pd,err:=parsePythonDependencies(pf)
		if err==nil{pyDeps=pd}else{log.Println("Python parse error:",err)}
	}
	// 3) Flatten for table
	fn := flattenNodeAll(nodeDeps,"Direct")
	fp := flattenPyAll(pyDeps,"Direct")
	allFlat := append(fn, fp...)
	// 4) Count copyleft
	copyleftCount:=0
	for _,dep:=range allFlat{
		if isCopyleft(dep.License){
			copyleftCount++
		}
	}
	summary:=fmt.Sprintf("%d direct Node.js deps, %d direct Python deps, copyleft:%d",
		len(nodeDeps), len(pyDeps), copyleftCount)

	// 5) Build nested <details> HTML
	nodeHTML := buildNodeTreesHTML(nodeDeps)
	pyHTML := buildPythonTreesHTML(pyDeps)

	// 6) Prepare final data & template
	data := struct{
		Summary string
		Deps []FlatDep
		NodeHTML template.HTML
		PyHTML template.HTML
	}{
		Summary: summary,
		Deps: allFlat,
		NodeHTML: template.HTML(nodeHTML),
		PyHTML: template.HTML(pyHTML),
	}

	tmpl, err:= template.New("report").Funcs(template.FuncMap{
		"isCopyleft": isCopyleft,
	}).Parse(reportTemplate)
	if err!=nil{
		log.Println("Template parse error:", err)
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
	fmt.Println("dependency-license-report.html generated. Fallback scrapes npm lines near 'license' for known keywords.")
}
