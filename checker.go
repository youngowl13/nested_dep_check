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

	"github.com/PuerkitoBio/goquery"
)

// isCopyleft checks if license text likely indicates a copyleft license.
func isCopyleft(license string) bool {
	copyleft := []string{
		"GPL", "GNU GENERAL PUBLIC LICENSE", "LGPL", "GNU LESSER GENERAL PUBLIC LICENSE",
		"AGPL", "GNU AFFERO GENERAL PUBLIC LICENSE", "MPL", "MOZILLA PUBLIC LICENSE",
		"CC-BY-SA", "CREATIVE COMMONS ATTRIBUTION-SHAREALIKE", "EPL", "ECLIPSE PUBLIC LICENSE",
		"OFL", "OPEN FONT LICENSE", "CPL", "COMMON PUBLIC LICENSE", "OSL", "OPEN SOFTWARE LICENSE",
	}
	up := strings.ToUpper(license)
	for _, kw := range copyleft {
		if strings.Contains(up, kw) {
			return true
		}
	}
	return false
}

// findFile searches recursively for target
func findFile(root, target string) string {
	var found string
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Name() == target {
			found = path
			return filepath.SkipDir
		}
		return nil
	})
	return found
}

// removeCaretTilde strips leading ^ or ~ from version.
func removeCaretTilde(ver string) string {
	return strings.TrimLeft(strings.TrimSpace(ver), "^~")
}

// fetchNpmHTMLLicense uses goquery to parse the npmjs.com webpage looking for a section near “License”.
func fetchNpmHTMLLicense(pkg string) string {
	url := "https://www.npmjs.com/package/" + pkg
	resp, err := http.Get(url)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return ""
	}
	var license string
	// Example approach: find an h3 with text “License”, then read the next sibling or .next() element's text
	doc.Find("h3").EachWithBreak(func(i int, s *goquery.Selection) bool {
		if strings.EqualFold(strings.TrimSpace(s.Text()), "License") {
			next := s.Next()
			if goquery.NodeName(next) == "p" {
				license = strings.TrimSpace(next.Text())
			}
			return false
		}
		return true
	})
	return license
}

// ------------------------ Node.js ------------------------

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
	if err != nil {
		return nil, err
	}
	var pkg map[string]interface{}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return nil, err
	}
	deps, _ := pkg["dependencies"].(map[string]interface{})
	if deps == nil {
		return nil, fmt.Errorf("no dependencies found in package.json")
	}
	visited := map[string]bool{}
	var results []*NodeDependency
	for nm, ver := range deps {
		str, _ := ver.(string)
		nd, e := resolveNodeDependency(nm, removeCaretTilde(str), visited)
		if e == nil && nd != nil {
			results = append(results, nd)
		}
	}
	return results, nil
}

func resolveNodeDependency(pkg, ver string, visited map[string]bool) (*NodeDependency, error) {
	key := pkg + "@" + ver
	if visited[key] {
		return nil, nil
	}
	visited[key] = true

	resp, err := http.Get("https://registry.npmjs.org/" + pkg)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	if ver == "" {
		if dist, ok := data["dist-tags"].(map[string]interface{}); ok {
			if latest, ok := dist["latest"].(string); ok {
				ver = latest
			}
		}
	}
	license := "Unknown"
	var trans []*NodeDependency
	if vs, ok := data["versions"].(map[string]interface{}); ok {
		if verData, ok := vs[ver].(map[string]interface{}); ok {
			license = findNpmLicense(verData)
			if deps, ok := verData["dependencies"].(map[string]interface{}); ok {
				for d, dv := range deps {
					str, _ := dv.(string)
					nd, e := resolveNodeDependency(d, removeCaretTilde(str), visited)
					if e == nil && nd != nil {
						trans = append(trans, nd)
					}
				}
			}
		}
	}
	if license == "Unknown" {
		webLic := fetchNpmHTMLLicense(pkg)
		if webLic != "" {
			license = webLic
		}
	}
	return &NodeDependency{
		Name: pkg, Version: ver,
		License: license,
		Details: "https://www.npmjs.com/package/" + pkg,
		Copyleft: isCopyleft(license),
		Transitive: trans,
		Language: "node",
	}, nil
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

// ------------------------ Python ------------------------

type PythonDependency struct {
	Name       string
	Version    string
	License    string
	Details    string
	Copyleft   bool
	Transitive []*PythonDependency
	Language   string
}

func parsePythonDependencies(path string) ([]*PythonDependency, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	reqs, err := parseRequirements(f)
	if err != nil {
		return nil, err
	}
	var results []*PythonDependency
	var wg sync.WaitGroup
	depChan := make(chan PythonDependency)
	errChan := make(chan error)
	for _, r := range reqs {
		wg.Add(1)
		go func(nm, vr string) {
			defer wg.Done()
			dp, e := resolvePythonDependency(nm, vr, map[string]bool{})
			if e != nil {
				errChan <- e
				return
			}
			if dp != nil {
				depChan <- *dp
			}
		}(r.name, r.version)
	}
	go func() {
		wg.Wait()
		close(depChan)
		close(errChan)
	}()
	for d := range depChan {
		results = append(results, &d)
	}
	for e := range errChan {
		log.Println("Python error:", e)
	}
	return results, nil
}

func resolvePythonDependency(pkg, ver string, visited map[string]bool) (*PythonDependency, error) {
	if visited[pkg+"@"+ver] {
		return nil, nil
	}
	visited[pkg+"@"+ver] = true
	resp, err := http.Get("https://pypi.org/pypi/" + pkg + "/json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("PyPI status: %d", resp.StatusCode)
	}
	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	info, _ := data["info"].(map[string]interface{})
	if info == nil {
		return nil, fmt.Errorf("info missing for %s", pkg)
	}
	if ver == "" {
		if v2, ok := info["version"].(string); ok {
			ver = v2
		}
	}
	license := "Unknown"
	if l, ok := info["license"].(string); ok && l != "" {
		license = l
	}
	return &PythonDependency{
		Name: pkg, Version: ver, License: license,
		Details: "https://pypi.org/pypi/" + pkg + "/json",
		Copyleft: isCopyleft(license),
		Language: "python",
	}, nil
}

type requirement struct{name,version string}
func parseRequirements(r io.Reader)([]requirement,error){
	raw,err:=io.ReadAll(r)
	if err!=nil{return nil,err}
	lines:=strings.Split(string(raw),"\n")
	var out []requirement
	for _,ln:=range lines{
		line:=strings.TrimSpace(ln)
		if line==""||strings.HasPrefix(line,"#"){continue}
		p:=strings.Split(line,"==")
		if len(p)!=2{
			p=strings.Split(line,">=")
			if len(p)!=2{
				log.Println("Invalid requirement:",line)
				continue
			}
		}
		out=append(out,requirement{
			name:strings.TrimSpace(p[0]),
			version:strings.TrimSpace(p[1]),
		})
	}
	return out,nil
}

// Flatten

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
			out=append(out,flattenNodeAll(nd.Transitive,nd.Name)...)
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
			out=append(out,flattenPyAll(pd.Transitive,pd.Name)...)
		}
	}
	return out
}

// build JSON for Node & Python
func buildTreeJSON(node []*NodeDependency, py []*PythonDependency)(string,string,error){
	nroot:=map[string]interface{}{"Name":"Node.js Dependencies","Version":"","Transitive":node}
	proot:=map[string]interface{}{"Name":"Python Dependencies","Version":"","Transitive":py}
	nb, e:=json.MarshalIndent(nroot,"","  ")
	if e!=nil{return"","",e}
	pb, e2:=json.MarshalIndent(proot,"","  ")
	if e2!=nil{return"","",e2}
	return string(nb), string(pb), nil
}

// single HTML with 3 "pages"
type SinglePageData struct{
	Summary string
	Deps []FlatDep
	NodeJSON string
	PythonJSON string
}

var singleTmpl=`<!DOCTYPE html><html><head><meta charset="UTF-8"><title>Dependency License Report</title>
<style>
body{font-family:Arial,sans-serif;margin:20px}
h1,h2{color:#2c3e50}
nav a{margin-right:10px;cursor:pointer;color:blue;text-decoration:underline}
.page{display:none} .page.active{display:block}
table{width:100%;border-collapse:collapse;margin-bottom:20px}
th,td{border:1px solid #ddd;padding:8px;text-align:left}
th{background:#f2f2f2}
.copyleft{background:#f8d7da;color:#721c24}
.non-copyleft{background:#d4edda;color:#155724}
.unknown{background:#fff3cd;color:#856404}
</style>
</head><body>
<h1>Dependency License Report</h1>
<nav>
<a onclick="showPage('summary')">Summary</a>
<a onclick="showPage('node')">Node Graph</a>
<a onclick="showPage('python')">Python Graph</a>
</nav>

<div id="summary" class="page active">
<h2>Summary</h2>
<p>{{.Summary}}</p>
<h2>Dependencies</h2>
<table><tr>
<th>Name</th><th>Version</th><th>License</th><th>Parent</th>
<th>Language</th><th>Details</th>
</tr>
{{range .Deps}}<tr>
<td>{{.Name}}</td>
<td>{{.Version}}</td>
<td class="{{if eq .License "Unknown"}}unknown{{else if isCopyleft .License}}copyleft{{else}}non-copyleft{{end}}">
{{.License}}</td>
<td>{{.Parent}}</td>
<td>{{.Language}}</td>
<td><a href="{{.Details}}" target="_blank">View</a></td>
</tr>{{end}}
</table>
</div>

<div id="node" class="page">
<h2>Node.js Collapsible Tree</h2>
<div id="nodeGraph"></div>
</div>

<div id="python" class="page">
<h2>Python Collapsible Tree</h2>
<div id="pythonGraph"></div>
</div>

<script src="https://d3js.org/d3.v6.min.js"></script>
<script>
function showPage(pg){
document.getElementById("summary").classList.remove("active");
document.getElementById("node").classList.remove("active");
document.getElementById("python").classList.remove("active");
document.getElementById(pg).classList.add("active");
}
function collapsibleTree(data, sel){
var m={top:20,right:90,bottom:30,left:90},w=660-m.left-m.right,h=500-m.top-m.bottom;
var svg=d3.select(sel).append("svg").attr("width",w+m.left+m.right).attr("height",h+m.top+m.bottom)
.append("g").attr("transform","translate("+m.left+","+m.top+")");
var tree=d3.tree().size([h,w]);
var root=d3.hierarchy(data,function(d){return d.Transitive;});
root.x0=h/2;root.y0=0;update(root);
function update(source){
var treed=tree(root),nodes=treed.descendants(),links=nodes.slice(1);
nodes.forEach(function(d){d.y=d.depth*180});
var node=svg.selectAll("g.node").data(nodes,function(d){return d.id||(d.id=Math.random())});
var ne=node.enter().append("g").attr("class","node")
.attr("transform",function(d){return"translate("+source.y0+","+source.x0+")";})
.on("click",click);
ne.append("circle").attr("class","node").attr("r",1e-6)
.style("fill",function(d){return d._children?"lightsteelblue":"#fff"})
.style("stroke","steelblue").style("stroke-width","3");
ne.append("text").attr("dy",".35em")
.attr("x",function(d){return d.children||d._children?-13:13;})
.style("text-anchor",function(d){return d.children||d._children?"end":"start";})
.text(function(d){return d.data.Name+"@"+d.data.Version;});
var nodeUpdate=ne.merge(node);
nodeUpdate.transition().duration(200)
.attr("transform",function(d){return"translate("+d.y+","+d.x+")";});
nodeUpdate.select("circle.node").attr("r",10)
.style("fill",function(d){return d._children?"lightsteelblue":"#fff"});
var nodeExit=node.exit().transition().duration(200)
.attr("transform",function(d){return"translate("+source.y+","+source.x+")";}).remove();
nodeExit.select("circle").attr("r",1e-6);
var link=svg.selectAll("path.link").data(links,function(d){return d.id||(d.id=Math.random())});
var linkEnter=link.enter().insert("path","g").attr("class","link")
.attr("d",function(d){var o={x:source.x0,y:source.y0};return diag(o,o)});
var linkUpdate=linkEnter.merge(link);
linkUpdate.transition().duration(200).attr("d",function(d){return diag(d,d.parent)});
link.exit().transition().duration(200).attr("d",function(d){
var o={x:source.x,y:source.y};return diag(o,o)}).remove();
nodes.forEach(function(d){d.x0=d.x;d.y0=d.y});
function diag(s,d){return"M"+s.y+","+s.x
+"C"+(s.y+50)+","+s.x+" "+(d.y+50)+","+d.x+" "+d.y+","+d.x}
}
function click(ev,d){
if(d.children){d._children=d.children;d.children=null;}
else{d.children=d._children;d._children=null;}
update(d);
}
}
var nodeData = NODE_JSON;
var pythonData = PYTHON_JSON;
collapsibleTree(nodeData,"#nodeGraph");
collapsibleTree(pythonData,"#pythonGraph");
</script>
</body></html>`

func generateSingleHTML(data SinglePageData) error {
	t, e := template.New("single").Funcs(template.FuncMap{
		"isCopyleft": isCopyleft,
	}).Parse(singleTmpl)
	if e!=nil{return e}
	out, e2 := os.Create("dependency-license-report.html")
	if e2!=nil{return e2}
	defer out.Close()
	html := t.Execute(out, data)
	return html
}

// main
func main(){
	// parse Node
	nf:=findFile(".","package.json")
	var nodeDeps []*NodeDependency
	if nf!=""{
		nd,err:=parseNodeDependencies(nf)
		if err==nil{nodeDeps=nd}else{log.Println("Node parse err:",err)}
	}
	// parse Python
	pf:=findFile(".","requirements.txt")
	if pf==""{pf=findFile(".","requirement.txt")}
	var pyDeps []*PythonDependency
	if pf!=""{
		pd,err:=parsePythonDependencies(pf)
		if err==nil{pyDeps=pd}else{log.Println("Python parse err:",err)}
	}
	// flatten
	fn:=flattenNodeAll(nodeDeps,"Direct")
	fp:=flattenPyAll(pyDeps,"Direct")
	allFlat:=append(fn,fp...)
	totalCleft:=0
	for _,f:=range allFlat{
		if isCopyleft(f.License){totalCleft++}
	}
	summary:=fmt.Sprintf("%d direct Node.js deps, %d direct Python deps. Copyleft:%d",
		len(nodeDeps), len(pyDeps), totalCleft)
	// build JSON
	njson,pjson,err:=buildTreeJSON(nodeDeps,pyDeps)
	if err!=nil{
		log.Println("Build JSON err:",err)
		njson,pjson="[]","[]"
	}
	// produce single HTML with “pages”
	pageData := SinglePageData{
		Summary:summary,
		Deps:allFlat,
		NodeJSON:njson,
		PythonJSON:pjson,
	}
	if e:=generateSingleHTML(pageData); e!=nil{
		log.Println("Generate single HTML error:", e)
		os.Exit(1)
	}
	fmt.Println("dependency-license-report.html generated with 3 pages (Summary/Node/Python).")
}
