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
	"strings"
	"sync"

	"io/fs"

	"github.com/PuerkitoBio/goquery"
)

// isCopyleft checks if a license text likely indicates a copyleft license.
func isCopyleft(license string) bool {
	copyleft := []string{
		"GPL","GNU GENERAL PUBLIC LICENSE","LGPL","GNU LESSER GENERAL PUBLIC LICENSE",
		"AGPL","GNU AFFERO GENERAL PUBLIC LICENSE","MPL","MOZILLA PUBLIC LICENSE",
		"CC-BY-SA","CREATIVE COMMONS ATTRIBUTION-SHAREALIKE","EPL","ECLIPSE PUBLIC LICENSE",
		"OFL","OPEN FONT LICENSE","CPL","COMMON PUBLIC LICENSE","OSL","OPEN SOFTWARE LICENSE",
	}
	up := strings.ToUpper(license)
	for _, kw := range copyleft {
		if strings.Contains(up, kw) {
			return true
		}
	}
	return false
}

// findFile recursively locates 'target' starting at 'root'.
func findFile(root, target string) string {
	var found string
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err==nil && d.Name()==target {
			found = path
			return filepath.SkipDir
		}
		return nil
	})
	return found
}

// removeCaretTilde strips ^ or ~ from a version string.
func removeCaretTilde(ver string) string {
	ver = strings.TrimSpace(ver)
	return strings.TrimLeft(ver, "^~")
}

// fallbackNpmWebsiteLicense attempts to parse the npm webpage for "License" info using goquery.
func fallbackNpmWebsiteLicense(pkgName string) string {
	url := "https://www.npmjs.com/package/" + pkgName
	resp, err := http.Get(url)
	if err!=nil || resp.StatusCode!=200{
		return ""
	}
	defer resp.Body.Close()

	doc, err2 := goquery.NewDocumentFromReader(resp.Body)
	if err2!=nil{
		return ""
	}
	var foundLicense string
	// We look for an <h3> or some text "License", then a sibling <p> with e.g. "MIT".
	doc.Find("h3").EachWithBreak(func(i int, s *goquery.Selection) bool {
		txt := strings.TrimSpace(s.Text())
		if strings.EqualFold(txt, "License") {
			p := s.Next()
			if goquery.NodeName(p)=="p" {
				pText := strings.TrimSpace(p.Text())
				if pText!="" {
					foundLicense = pText
				}
			}
			return false // break
		}
		return true
	})
	if foundLicense!=""{
		return foundLicense
	}
	// Alternative fallback: search entire doc for "License" plus known keywords
	doc.Find("div").Each(func(i int, sel *goquery.Selection){
		divtxt := strings.ToUpper(sel.Text())
		if strings.Contains(divtxt, "LICENSE"){
			patterns := []string{"MIT","BSD","APACHE","ISC","ARTISTIC","ZLIB","WTFPL",
				"CDDL","UNLICENSE","EUPL","MPL","CC0","LGPL","AGPL"}
			for _, pat := range patterns {
				if strings.Contains(divtxt, pat) {
					foundLicense=pat
					return
				}
			}
		}
	})
	return foundLicense
}

// ------------------- Node Dependencies -------------------

// NodeDependency holds Node package info + sub-dependencies (Transitive).
type NodeDependency struct {
	Name       string
	Version    string
	License    string
	Details    string
	Copyleft   bool
	Transitive []*NodeDependency
	Language   string
}

func parseNodeDependencies(path string) ([]*NodeDependency, error) {
	raw, err := os.ReadFile(path)
	if err!=nil{return nil,err}
	var pkg map[string]interface{}
	if err:=json.Unmarshal(raw,&pkg); err!=nil{return nil,err}
	deps, _ := pkg["dependencies"].(map[string]interface{})
	if deps==nil{
		return nil, fmt.Errorf("no dependencies found in package.json")
	}
	visited := map[string]bool{}
	var results []*NodeDependency
	for nm, ver := range deps {
		vstr, _ := ver.(string)
		nd, e := resolveNodeDependency(nm, removeCaretTilde(vstr), visited)
		if e==nil && nd!=nil{
			results=append(results, nd)
		}
	}
	return results,nil
}

func resolveNodeDependency(pkgName, version string, visited map[string]bool)(*NodeDependency, error){
	key := pkgName+"@"+version
	if visited[key]{
		return nil,nil
	}
	visited[key] = true

	resp,err:=http.Get("https://registry.npmjs.org/"+pkgName)
	if err!=nil{return nil,err}
	defer resp.Body.Close()
	var data map[string]interface{}
	if err:=json.NewDecoder(resp.Body).Decode(&data); err!=nil{
		return nil,err
	}
	if version==""{
		if dist,ok:=data["dist-tags"].(map[string]interface{});ok{
			if latest,ok:=dist["latest"].(string);ok{
				version=latest
			}
		}
	}
	license:="Unknown"
	var trans []*NodeDependency
	if vs,ok:=data["versions"].(map[string]interface{});ok{
		if verData,ok:=vs[version].(map[string]interface{});ok{
			license=findNpmLicense(verData)
			if deps,ok:=verData["dependencies"].(map[string]interface{});ok{
				for d,dv := range deps{
					str2,_:=dv.(string)
					ch,e2:=resolveNodeDependency(d, removeCaretTilde(str2), visited)
					if e2==nil && ch!=nil{
						trans=append(trans,ch)
					}
				}
			}
		}
	}
	// fallback
	if license=="Unknown"{
		webLic := fallbackNpmWebsiteLicense(pkgName)
		if webLic!=""{
			license=webLic
		}
	}
	return &NodeDependency{
		Name: pkgName, Version:version, License:license,
		Details:"https://www.npmjs.com/package/"+pkgName,
		Copyleft:isCopyleft(license),
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

// ------------------- Python Dependencies -------------------

type PythonDependency struct{
	Name string
	Version string
	License string
	Details string
	Copyleft bool
	Transitive []*PythonDependency
	Language string
}

func parsePythonDependencies(path string) ([]*PythonDependency, error) {
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
			d,e := resolvePythonDependency(nm,vr,map[string]bool{})
			if e!=nil{errChan<-e;return}
			if d!=nil{depChan<-*d}
		}(r.name, r.version)
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
	if err:=json.NewDecoder(resp.Body).Decode(&data);err!=nil{return nil,err}
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
	for _, ln:=range lines{
		line:=strings.TrimSpace(ln)
		if line==""||strings.HasPrefix(line,"#"){continue}
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

// Flatten for summary table

type FlatDep struct{
	Name string
	Version string
	License string
	Details string
	Language string
	Parent string
}

func flattenNodeAll(nds []*NodeDependency, parent string)[]FlatDep{
	var out []FlatDep
	for _,nd:=range nds{
		out=append(out,FlatDep{
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
	for _,pd:=range pds{
		out=append(out,FlatDep{
			Name:pd.Name,Version:pd.Version,License:pd.License,
			Details:pd.Details,Language:pd.Language,Parent:parent,
		})
		if len(pd.Transitive)>0{
			out=append(out, flattenPyAll(pd.Transitive, pd.Name)...)
		}
	}
	return out
}

// We want an HTML nested <details> approach for each NodeDependency and PythonDependency
// so no scripts are required to expand/collapse sub-dependencies.

func buildNodeDependencyHTML(nd *NodeDependency) string {
	// We'll produce <details><summary>NAME@VER (License: ???)</summary> then children
	summaryText := fmt.Sprintf("%s@%s (License: %s)", nd.Name, nd.Version, nd.License)
	var sb strings.Builder
	sb.WriteString("<details>")
	sb.WriteString("<summary>")
	sb.WriteString(template.HTMLEscapeString(summaryText))
	sb.WriteString("</summary>\n")

	if len(nd.Transitive)>0 {
		sb.WriteString("<ul>\n")
		for _,child:=range nd.Transitive{
			sb.WriteString("<li>")
			sb.WriteString(buildNodeDependencyHTML(child))
			sb.WriteString("</li>\n")
		}
		sb.WriteString("</ul>\n")
	}
	sb.WriteString("</details>\n")
	return sb.String()
}

func buildPythonDependencyHTML(pd *PythonDependency) string {
	summaryText := fmt.Sprintf("%s@%s (License: %s)", pd.Name, pd.Version, pd.License)
	var sb strings.Builder
	sb.WriteString("<details>")
	sb.WriteString("<summary>")
	sb.WriteString(template.HTMLEscapeString(summaryText))
	sb.WriteString("</summary>\n")

	if len(pd.Transitive)>0 {
		sb.WriteString("<ul>\n")
		for _,child:=range pd.Transitive{
			sb.WriteString("<li>")
			sb.WriteString(buildPythonDependencyHTML(child))
			sb.WriteString("</li>\n")
		}
		sb.WriteString("</ul>\n")
	}
	sb.WriteString("</details>\n")
	return sb.String()
}

// If no direct deps, we produce a single details node: "No Node dependencies"
func buildNodeTreeHTML(nodes []*NodeDependency) string {
	if len(nodes)==0 {
		return `<p>No Node dependencies found.</p>`
	}
	var sb strings.Builder
	for _,nd:=range nodes{
		sb.WriteString(buildNodeDependencyHTML(nd))
	}
	return sb.String()
}
func buildPythonTreeHTML(nodes []*PythonDependency) string {
	if len(nodes)==0 {
		return `<p>No Python dependencies found.</p>`
	}
	var sb strings.Builder
	for _,pd:=range nodes{
		sb.WriteString(buildPythonDependencyHTML(pd))
	}
	return sb.String()
}

// We produce a single HTML with summary table + Node detail tree + Python detail tree, all static (<details>).
type ReportData struct{
	Summary string
	FlatDeps []FlatDep
	NodeRoot []*NodeDependency
	PyRoot   []*PythonDependency
}

var htmlTemplate=`
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
  {{range .FlatDeps}}
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

<h2>Node.js Dependencies Tree</h2>
<div>
  {{.NodeHTML}}
</div>

<h2>Python Dependencies Tree</h2>
<div>
  {{.PyHTML}}
</div>

</body>
</html>
`

// We define a minimal struct to pass to the template, including pre-built HTML for the <details> trees.
type CombinedHTMLData struct{
	Summary string
	FlatDeps []FlatDep
	NodeHTML template.HTML
	PyHTML   template.HTML
}

func main(){
	// 1) parse Node
	nFile:=findFile(".","package.json")
	var nodeDeps []*NodeDependency
	if nFile!=""{
		nd,err:=parseNodeDependencies(nFile)
		if err==nil{nodeDeps=nd}else{log.Println("Node parse error:",err)}
	}

	// 2) parse Python
	pFile:=findFile(".","requirements.txt")
	if pFile==""{
		pFile=findFile(".","requirement.txt")
	}
	var pyDeps []*PythonDependency
	if pFile!=""{
		pd,err:=parsePythonDependencies(pFile)
		if err==nil{pyDeps=pd}else{log.Println("Python parse error:",err)}
	}

	// 3) Flatten for table
	fn := flattenNodeAll(nodeDeps,"Direct")
	fp := flattenPyAll(pyDeps,"Direct")
	allFlat := append(fn,fp...)

	// 4) Count how many are copyleft
	copyleftCount:=0
	for _,dep:=range allFlat {
		if isCopyleft(dep.License){copyleftCount++}
	}
	summary := fmt.Sprintf("%d direct Node.js deps, %d direct Python deps, Copyleft:%d",
		len(nodeDeps), len(pyDeps), copyleftCount)

	// 5) Build nested <details> for Node
	nodeHTML := buildNodeTreeHTML(nodeDeps)
	// 6) Build nested <details> for Python
	pyHTML   := buildPythonTreeHTML(pyDeps)

	// 7) Prepare data for template
	data := CombinedHTMLData{
		Summary: summary,
		FlatDeps: allFlat,
		NodeHTML: template.HTML(nodeHTML),
		PyHTML:   template.HTML(pyHTML),
	}

	// 8) Render final single HTML
	tmpl,err := template.New("report").Funcs(template.FuncMap{
		"isCopyleft": isCopyleft,
	}).Parse(htmlTemplate)
	if err!=nil{
		log.Println("Template parse error:", err)
		os.Exit(1)
	}
	out,err2 := os.Create("dependency-license-report.html")
	if err2!=nil{
		log.Println("Create file error:",err2)
		os.Exit(1)
	}
	defer out.Close()

	if e:=tmpl.Execute(out, data);e!=nil{
		log.Println("Template exec error:", e)
		os.Exit(1)
	}

	fmt.Println("dependency-license-report.html generated successfully!")
	fmt.Println("No JavaScript-based graphs. Using <details> for nested trees. No local-file script blocking.")
}
