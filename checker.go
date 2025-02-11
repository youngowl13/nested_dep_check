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
)

// isCopyleft checks if a license text likely indicates copyleft.
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

// findFile recursively searches root for target.
func findFile(root, target string) string {
	var found string
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err==nil && d.Name()==target {
			found=path
			return filepath.SkipDir
		}
		return nil
	})
	return found
}

// -------------------- Node.js Dependencies --------------------

type NodeDependency struct {
	Name       string
	Version    string
	License    string
	Details    string
	Copyleft   bool
	Transitive []*NodeDependency
	Language   string
}

func resolveNodeDependency(pkg, ver string, visited map[string]bool) (*NodeDependency, error) {
	if visited[pkg+"@"+ver] { return nil, nil }
	visited[pkg+"@"+ver] = true

	resp, err := http.Get("https://registry.npmjs.org/" + pkg)
	if err!=nil { return nil, err }
	defer resp.Body.Close()

	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err!=nil { return nil, err }

	if ver=="" {
		if dist, ok := data["dist-tags"].(map[string]interface{}); ok {
			if latest, ok := dist["latest"].(string); ok {
				ver = latest
			}
		}
	}
	vs, _ := data["versions"].(map[string]interface{})
	license := "Unknown"
	var trans []*NodeDependency
	if vs!=nil {
		if verData, ok := vs[ver].(map[string]interface{}); ok {
			license = findLicenseNPM(verData)
			if deps, ok := verData["dependencies"].(map[string]interface{}); ok {
				for d, dv := range deps {
					dstr, _ := dv.(string)
					nd, e := resolveNodeDependency(d, removeCaretTilde(dstr), visited)
					if e==nil && nd!=nil {
						trans = append(trans, nd)
					}
				}
			}
		}
	}
	return &NodeDependency{
		Name: pkg, Version: ver,
		License: license,
		Details: "https://www.npmjs.com/package/"+pkg,
		Copyleft: isCopyleft(license),
		Transitive: trans,
		Language: "node",
	}, nil
}

func parseNodeDependencies(path string) ([]*NodeDependency, error) {
	raw, err := os.ReadFile(path)
	if err!=nil { return nil, err }
	var dat map[string]interface{}
	if err := json.Unmarshal(raw, &dat); err!=nil { return nil, err }
	deps, _ := dat["dependencies"].(map[string]interface{})
	if deps==nil {
		return nil, fmt.Errorf("no dependencies found in package.json")
	}
	visited := map[string]bool{}
	var results []*NodeDependency
	for nm, ver := range deps {
		str, _ := ver.(string)
		nd, e := resolveNodeDependency(nm, removeCaretTilde(str), visited)
		if e==nil && nd!=nil {
			results=append(results, nd)
		}
	}
	return results,nil
}

func findLicenseNPM(verData map[string]interface{}) string {
	// Attempt "license" as string.
	if l, ok := verData["license"].(string); ok && l!="" {
		return l
	}
	// If "license" is an object or "licenses" is an array.
	if lmap, ok := verData["license"].(map[string]interface{}); ok {
		if t,ok := lmap["type"].(string);ok && t!="" {
			return t
		}
	}
	// If "licenses" is an array of objects with "type" or "name".
	if arr, ok := verData["licenses"].([]interface{}); ok && len(arr)>0 {
		if licObj, ok := arr[0].(map[string]interface{}); ok {
			if t, ok := licObj["type"].(string); ok && t!="" {
				return t
			} else if n, ok := licObj["name"].(string); ok && n!="" {
				return n
			}
		}
	}
	return "Unknown"
}

func removeCaretTilde(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimLeft(v, "^~")
	return v
}

// -------------------- Python Dependencies --------------------

type PythonDependency struct {
	Name       string
	Version    string
	License    string
	Details    string
	Copyleft   bool
	Transitive []*PythonDependency
	Language   string
}

func resolvePythonDependency(pkg, ver string, visited map[string]bool) (*PythonDependency, error) {
	if visited[pkg+"@"+ver] { return nil, nil }
	visited[pkg+"@"+ver] = true

	resp, err := http.Get("https://pypi.org/pypi/"+pkg+"/json")
	if err!=nil { return nil, err }
	defer resp.Body.Close()

	if resp.StatusCode!=200 {
		return nil, fmt.Errorf("PyPI status:%d", resp.StatusCode)
	}
	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err!=nil {
		return nil, err
	}
	info, _ := data["info"].(map[string]interface{})
	if info==nil {
		return nil, fmt.Errorf("info missing for %s", pkg)
	}
	if ver=="" {
		if v2, ok := info["version"].(string); ok {
			ver = v2
		}
	}
	license := "Unknown"
	if l, ok := info["license"].(string); ok && l!="" {
		license = l
	}
	return &PythonDependency{
		Name: pkg, Version: ver, License: license,
		Details: "https://pypi.org/pypi/"+pkg+"/json",
		Copyleft: isCopyleft(license),
		Language:"python",
	}, nil
}

func parsePythonDependencies(path string) ([]*PythonDependency, error) {
	f, err := os.Open(path)
	if err!=nil { return nil, err }
	defer f.Close()
	reqs, err := parseRequirements(f)
	if err!=nil { return nil, err }
	var results []*PythonDependency
	var wg sync.WaitGroup
	depChan := make(chan PythonDependency)
	errChan := make(chan error)
	for _, r := range reqs {
		wg.Add(1)
		go func(nm, vr string) {
			defer wg.Done()
			dp, e := resolvePythonDependency(nm, vr, map[string]bool{})
			if e!=nil { errChan<-e; return }
			if dp!=nil { depChan<-*dp }
		}(r.name, r.version)
	}
	go func(){
		wg.Wait()
		close(depChan)
		close(errChan)
	}()
	for d := range depChan {
		results=append(results,&d)
	}
	for e := range errChan {
		log.Println(e)
	}
	return results,nil
}

type requirement struct{name,version string}

func parseRequirements(r io.Reader) ([]requirement, error) {
	var ret []requirement
	raw, err := io.ReadAll(r)
	if err!=nil { return nil, err }
	lines := strings.Split(string(raw), "\n")
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if trim=="" || strings.HasPrefix(trim,"#") { continue }
		parts := strings.Split(trim, "==")
		if len(parts)!=2 {
			parts = strings.Split(trim, ">=")
			if len(parts)!=2 {
				log.Println("Warning invalid line:", trim)
				continue
			}
		}
		ret=append(ret, requirement{name:strings.TrimSpace(parts[0]),version:strings.TrimSpace(parts[1])})
	}
	return ret,nil
}

// Flatten

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
		out = append(out, FlatDep{
			Name: nd.Name, Version: nd.Version,
			License: nd.License, Details: nd.Details,
			Language: nd.Language, Parent: parent,
		})
		if len(nd.Transitive)>0 {
			out = append(out, flattenNodeAll(nd.Transitive, nd.Name)...)
		}
	}
	return out
}

func flattenPythonAll(pds []*PythonDependency, parent string) []FlatDep {
	var out []FlatDep
	for _, pd := range pds {
		out = append(out, FlatDep{
			Name: pd.Name, Version: pd.Version,
			License: pd.License, Details: pd.Details,
			Language: pd.Language, Parent: parent,
		})
		if len(pd.Transitive)>0 {
			out = append(out, flattenPythonAll(pd.Transitive, pd.Name)...)
		}
	}
	return out
}

// Graph

func dependencyTreeJSONFinal(node []*NodeDependency, py []*PythonDependency) (string,string,error){
	nroot := map[string]interface{}{
		"Name":"Node.js Dependencies",
		"Version":"",
		"Transitive":node,
	}
	proot := map[string]interface{}{
		"Name":"Python Dependencies",
		"Version":"",
		"Transitive":py,
	}
	nb, e := json.MarshalIndent(nroot,"","  ")
	if e!=nil { return "","",e }
	pb, e2 := json.MarshalIndent(proot,"","  ")
	if e2!=nil { return "","",e2 }
	return string(nb), string(pb), nil
}

// Template

type ReportData struct {
	Summary string
	Deps []FlatDep
	NodeJS template.JS
	Python template.JS
}

var htmlTemplate = `<!DOCTYPE html>
<html><head><meta charset="UTF-8"><title>Dependency License Report</title>
<style>
body{font-family:Arial,sans-serif;margin:20px}
h1,h2{color:#2c3e50}
table{width:100%;border-collapse:collapse;margin-bottom:20px}
th,td{border:1px solid #ddd;padding:8px;text-align:left}
th{background:#f2f2f2}
.copyleft{background:#f8d7da;color:#721c24}
.non-copyleft{background:#d4edda;color:#155724}
.unknown{background:#fff3cd;color:#856404}
</style></head>
<body>
<h1>Dependency License Report</h1>
<h2>Summary</h2><p>{{.Summary}}</p>
<h2>Dependencies</h2>
<table><tr>
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
<h2>Dependency Graph Visualization</h2>
<h3>Node.js Tree</h3><div id="nodeGraph"></div>
<h3>Python Tree</h3><div id="pythonGraph"></div>
<script src="https://d3js.org/d3.v6.min.js"></script>
<script>
var nodeData={{.NodeJS}};
var pythonData={{.Python}};
function collapsibleTree(data,selector){
var margin={top:20,right:90,bottom:30,left:90},
width=660-margin.left-margin.right,
height=500-margin.top-margin.bottom;
var svg=d3.select(selector).append("svg")
.attr("width",width+margin.left+margin.right)
.attr("height",height+margin.top+margin.bottom)
.append("g").attr("transform","translate("+margin.left+","+margin.top+")");
var treemap=d3.tree().size([height,width]);
var root=d3.hierarchy(data,function(d){return d.Transitive;});
root.x0=height/2;root.y0=0;
update(root);
function update(source){
var treeData=treemap(root),nodes=treeData.descendants(),links=treeData.descendants().slice(1);
nodes.forEach(function(d){d.y=d.depth*180});
var node=svg.selectAll("g.node").data(nodes,function(d){return d.id||(d.id=(Math.random()*1000000))});
var nodeEnter=node.enter().append("g").attr("class","node")
.attr("transform",function(d){return"translate("+source.y0+","+source.x0+")";})
.on("click",click);
nodeEnter.append("circle").attr("class","node").attr("r",1e-6)
.style("fill",function(d){return d._children?"lightsteelblue":"#fff"})
.style("stroke","steelblue").style("stroke-width","3");
nodeEnter.append("text").attr("dy",".35em").attr("x",function(d){return d.children||d._children?-13:13;})
.style("text-anchor",function(d){return d.children||d._children?"end":"start";})
.text(function(d){return d.data.Name+"@"+d.data.Version;});
var nodeUpdate=nodeEnter.merge(node);
nodeUpdate.transition().duration(200)
.attr("transform",function(d){return"translate("+d.y+","+d.x+")";});
nodeUpdate.select("circle.node").attr("r",10)
.style("fill",function(d){return d._children?"lightsteelblue":"#fff"});
var nodeExit=node.exit().transition().duration(200)
.attr("transform",function(d){return"translate("+source.y+","+source.x+")";})
.remove();
nodeExit.select("circle").attr("r",1e-6);
var link=svg.selectAll("path.link").data(links,function(d){return d.id|| (d.id=(Math.random()*1000000))});
var linkEnter=link.enter().insert("path","g")
.attr("class","link")
.attr("d",function(d){
var o={x:source.x0,y:source.y0};return diagonal(o,o)});
var linkUpdate=linkEnter.merge(link);
linkUpdate.transition().duration(200).attr("d",function(d){
return diagonal(d,d.parent)});
link.exit().transition().duration(200).attr("d",function(d){
var o={x:source.x,y:source.y};return diagonal(o,o)})
.remove();
nodes.forEach(function(d){d.x0=d.x;d.y0=d.y});
function diagonal(s,d){return"M"+s.y+","+s.x
+"C"+(s.y+50)+","+s.x+" "+(d.y+50)+","+d.x+" "+d.y+","+d.x}
}
function click(event,d){
if(d.children){d._children=d.children;d.children=null;}else{d.children=d._children;d._children=null;}
update(d);
}
}
collapsibleTree(nodeData,"#nodeGraph");
collapsibleTree(pythonData,"#pythonGraph");
</script>
</body></html>
`

func generateHTMLReport(d ReportData) error {
	t, e := template.New("report").Funcs(template.FuncMap{
		"isCopyleft": isCopyleft,
	}).Parse(htmlTemplate)
	if e!=nil {return e}
	out, e2 := os.Create("dependency-license-report.html")
	if e2!=nil {return e2}
	defer out.Close()
	return t.Execute(out, d)
}

func main(){
	nodeFile:=findFile(".","package.json")
	var nodeDeps []*NodeDependency
	if nodeFile!=""{
		nd,err:=parseNodeDependencies(nodeFile)
		if err==nil{nodeDeps=nd}else{log.Println("Node parse error:",err)}
	}
	pyFile:=findFile(".","requirements.txt")
	if pyFile==""{pyFile=findFile(".","requirement.txt")}
	var pyDeps []*PythonDependency
	if pyFile!=""{
		pd,err:=parsePythonDependencies(pyFile)
		if err==nil{pyDeps=pd}else{log.Println("Python parse error:",err)}
	}
	fn:=flattenAllNodeDeps(nodeDeps,"Direct")
	fp:=flattenAllPythonDeps(pyDeps,"Direct")
	allFlat:=append(fn,fp...)
	totalCleft:=0
	for _,x:=range allFlat{
		if isCopyleft(x.License){totalCleft++}
	}
	sum:=fmt.Sprintf("%d direct Node.js deps, %d direct Python deps. Copyleft: %d",
		len(nodeDeps),len(pyDeps),totalCleft)
	nj,pj,err:=dependencyTreeJSONFinal(nodeDeps,pyDeps)
	if err!=nil{
		log.Println("Error building JSON:",err)
		nj,pj="[]","[]"
	}
	rep:=ReportData{Summary:sum,Deps:allFlat,NodeJS:template.JS(nj),Python:template.JS(pj)}
	if err:=generateHTMLReport(rep);err!=nil{
		log.Println("Error generating HTML:",err)
		os.Exit(1)
	}
	fmt.Println("dependency-license-report.html generated.")
}
