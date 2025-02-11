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

// findFile recursively searches the directory tree for target.
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

// --------------------- Node.js ---------------------

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
	key := pkg + "@" + ver
	if visited[key] {
		return nil, nil
	}
	visited[key] = true
	url := "https://registry.npmjs.org/" + pkg
	resp, err := http.Get(url)
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
	trans := []*NodeDependency{}
	if vs, ok := data["versions"].(map[string]interface{}); ok {
		if verData, ok := vs[ver].(map[string]interface{}); ok {
			if l, ok := verData["license"].(string); ok {
				license = l
			} else if lm, ok := verData["license"].(map[string]interface{}); ok {
				if t, ok := lm["type"].(string); ok {
					license = t
				}
			}
			if deps, ok := verData["dependencies"].(map[string]interface{}); ok {
				for dep, dv := range deps {
					if dver, ok := dv.(string); ok {
						nd, e := resolveNodeDependency(dep, dver, visited)
						if e == nil && nd != nil {
							trans = append(trans, nd)
						}
					}
				}
			}
		}
	}
	return &NodeDependency{
		Name: pkg, Version: ver, License: license,
		Details: "https://www.npmjs.com/package/" + pkg,
		Copyleft: isCopyleft(license),
		Transitive: trans,
		Language: "node",
	}, nil
}

func parseNodeDependencies(path string) ([]*NodeDependency, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, err
	}
	deps, _ := obj["dependencies"].(map[string]interface{})
	if deps == nil {
		return nil, fmt.Errorf("no dependencies found in package.json")
	}
	var results []*NodeDependency
	visited := map[string]bool{}
	for name, ver := range deps {
		vstr, _ := ver.(string)
		vstr = strings.TrimPrefix(vstr, "^")
		nd, e := resolveNodeDependency(name, vstr, visited)
		if e == nil && nd != nil {
			results = append(results, nd)
		}
	}
	return results, nil
}

// --------------------- Python ---------------------

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
	key := pkg + "@" + ver
	if visited[key] {
		return nil, nil
	}
	visited[key] = true
	url := "https://pypi.org/pypi/" + pkg + "/json"
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("PyPI status: %s", resp.Status)
	}
	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	info, _ := data["info"].(map[string]interface{})
	if info == nil {
		return nil, fmt.Errorf("info not found for %s", pkg)
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
		Name: pkg, Version: ver,
		License: license,
		Details: "https://pypi.org/pypi/" + pkg + "/json",
		Copyleft: isCopyleft(license),
		Language: "python",
	}, nil
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
		go func(n, v string) {
			defer wg.Done()
			dep, e := resolvePythonDependency(n, v, map[string]bool{})
			if e != nil {
				errChan <- e
				return
			}
			if dep != nil {
				depChan <- *dep
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
		log.Println(e)
	}
	return results, nil
}

type requirement struct{name,version string}

func parseRequirements(r io.Reader)([]requirement,error){
	var ret []requirement
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		l := strings.TrimSpace(line)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		p := strings.Split(l, "==")
		if len(p)!=2 {
			p = strings.Split(l, ">=")
			if len(p)!=2 {
				log.Printf("Warning: invalid line: %s", l)
				continue
			}
		}
		ret = append(ret, requirement{
			name: strings.TrimSpace(p[0]),
			version: strings.TrimSpace(p[1]),
		})
	}
	return ret, nil
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

func flattenAllNodeDeps(nds []*NodeDependency, parent string) []FlatDep {
	var out []FlatDep
	for _, nd := range nds {
		out = append(out, FlatDep{
			Name: nd.Name, Version: nd.Version,
			License: nd.License, Details: nd.Details,
			Language: nd.Language, Parent: parent,
		})
		if len(nd.Transitive) > 0 {
			out = append(out, flattenAllNodeDeps(nd.Transitive, nd.Name)...)
		}
	}
	return out
}

func flattenAllPythonDeps(pds []*PythonDependency, parent string) []FlatDep {
	var out []FlatDep
	for _, pd := range pds {
		out = append(out, FlatDep{
			Name: pd.Name, Version: pd.Version,
			License: pd.License, Details: pd.Details,
			Language: pd.Language, Parent: parent,
		})
		if len(pd.Transitive) > 0 {
			out = append(out, flattenAllPythonDeps(pd.Transitive, pd.Name)...)
		}
	}
	return out
}

// Graph

func dependencyTreeJSONFinal(node []*NodeDependency, py []*PythonDependency) (string, string, error) {
	nroot := map[string]interface{}{
		"Name": "Node.js Dependencies",
		"Version": "",
		"Transitive": node,
	}
	proot := map[string]interface{}{
		"Name": "Python Dependencies",
		"Version": "",
		"Transitive": py,
	}
	nb, e := json.MarshalIndent(nroot,"","  ")
	if e != nil { return "", "", e }
	pb, e2 := json.MarshalIndent(proot,"","  ")
	if e2 != nil { return "", "", e2 }
	return string(nb), string(pb), nil
}

// Template

type ReportData struct{
	Summary string
	Deps []FlatDep
	NodeJS template.JS
	Python template.JS
}

var tpl =`<!DOCTYPE html><html><head><meta charset="UTF-8">
<title>Dependency License Report</title>
<style>
body{font-family:Arial,sans-serif;margin:20px}h1,h2{color:#2c3e50}
table{width:100%;border-collapse:collapse;margin-bottom:20px}
th,td{border:1px solid #ddd;padding:8px;text-align:left}
th{background:#f2f2f2}
.copyleft{background:#f8d7da;color:#721c24}
.non-copyleft{background:#d4edda;color:#155724}
.unknown{background:#fff3cd;color:#856404}
</style></head><body>
<h1>Dependency License Report</h1><h2>Summary</h2><p>{{.Summary}}</p>
<h2>Dependencies</h2><table><tr>
<th>Name</th><th>Version</th><th>License</th><th>Parent</th>
<th>Language</th><th>Details</th></tr>
{{range .Deps}}<tr>
<td>{{.Name}}</td><td>{{.Version}}</td>
<td class="{{if eq .License "Unknown"}}unknown{{else if isCopyleft .License}}copyleft{{else}}non-copyleft{{end}}">{{.License}}</td>
<td>{{.Parent}}</td><td>{{.Language}}</td>
<td><a href="{{.Details}}" target="_blank">View</a></td></tr>{{end}}
</table>
<h2>Graph Visualization</h2>
<h3>Node.js Tree</h3><div id="nodeGraph"></div>
<h3>Python Tree</h3><div id="pythonGraph"></div>
<script src="https://d3js.org/d3.v6.min.js"></script>
<script>
var nodeData={{.NodeJS}};
var pythonData={{.Python}};
function render(data,id){
var m={top:20,right:90,bottom:30,left:90},w=660-m.left-m.right,h=500-m.top-m.bottom;
var svg=d3.select("#"+id).append("svg").attr("width",w+m.left+m.right)
.attr("height",h+m.top+m.bottom).append("g")
.attr("transform","translate("+m.left+","+m.top+")");
var tree=d3.tree().size([h,w]);
var root=d3.hierarchy(data,function(d){return d.Transitive;});
root=tree(root);
var link=svg.selectAll(".link").data(root.descendants().slice(1))
.enter().append("path").attr("class","link")
.attr("d",function(d){return"M"+d.y+","+d.x
+"C"+(d.parent.y+50)+","+d.x+" "+(d.parent.y+50)+","+d.parent.x
+" "+d.parent.y+","+d.parent.x;})
.attr("fill","none").attr("stroke","#ccc");
var node=svg.selectAll(".node").data(root.descendants())
.enter().append("g")
.attr("class",function(d){return"node"+(d.children?" node--internal":" node--leaf");})
.attr("transform",function(d){return"translate("+d.y+","+d.x+")";});
node.append("circle").attr("r",10).attr("fill","#fff")
.attr("stroke","steelblue").attr("stroke-width","3");
node.append("text").attr("dy",".35em")
.attr("x",function(d){return d.children?-13:13;})
.style("text-anchor",function(d){return d.children?"end":"start";})
.text(function(d){return d.data.Name+"@"+d.data.Version;});
}
render(nodeData,"nodeGraph");
render(pythonData,"pythonGraph");
</script>
</body></html>`

func generateHTML(data ReportData) error {
	t, e := template.New("rep").Funcs(template.FuncMap{"isCopyleft":isCopyleft}).Parse(tpl)
	if e!=nil {return e}
	f, e2 := os.Create("dependency-license-report.html")
	if e2!=nil {return e2}
	defer f.Close()
	return t.Execute(f, data)
}

func main(){
	nodeFile := findFile(".","package.json")
	var nodeDeps []*NodeDependency
	if nodeFile!=""{
		nd,err:=parseNodeDependencies(nodeFile)
		if err==nil{nodeDeps=nd}else{log.Println("Node parse:",err)}
	}
	pythonFile:=findFile(".","requirements.txt")
	if pythonFile==""{
		pythonFile=findFile(".","requirement.txt")
	}
	var pyDeps []*PythonDependency
	if pythonFile!=""{
		pd,err:=parsePythonDependencies(pythonFile)
		if err==nil{pyDeps=pd}else{log.Println("Python parse:",err)}
	}
	fn:=flattenAllNodeDeps(nodeDeps,"Direct")
	fp:=flattenAllPythonDeps(pyDeps,"Direct")
	flatAll:=append(fn,fp...)
	totalCopyleft:=0
	for _,d:=range flatAll{
		if isCopyleft(d.License){totalCopyleft++}
	}
	summary:=fmt.Sprintf("%d direct Node.js deps, %d direct Python deps, total copyleft:%d",
		len(nodeDeps),len(pyDeps),totalCopyleft)
	njson,pjson,err:=dependencyTreeJSONFinal(nodeDeps,pyDeps)
	if err!=nil{
		njson,pjson="[]","[]"
		log.Println("JSON Error:",err)
	}
	rep := ReportData{Summary:summary,Deps:flatAll,
		NodeJS:template.JS(njson),Python:template.JS(pjson)}
	if e:=generateHTML(rep); e!=nil{
		log.Println("Generate HTML error:", e)
		os.Exit(1)
	}
	fmt.Println("dependency-license-report.html generated.")
}
