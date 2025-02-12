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

// isCopyleft checks if license text is likely copyleft
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

// findFile recursively locates 'target' in 'root' directory
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

// removeCaretTilde strips leading ^ or ~ from a version string
func removeCaretTilde(ver string) string {
	ver = strings.TrimSpace(ver)
	return strings.TrimLeft(ver, "^~")
}

// fallbackNpmWebsiteLicense uses goquery to parse the npm page's right column for "License"
func fallbackNpmWebsiteLicense(pkg string) string {
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
	var foundLicense string
	// The right column typically has <p>, <span>, or <strong> for "License" label, then next or child node says "MIT"
	doc.Find("div").Each(func(i int, s *goquery.Selection) {
		// We look for text "License" within that block
		// Then see if there's a line with "MIT" or "BSD" or something
		// This is somewhat naive but better than a random approach
		divText := strings.ToUpper(s.Text())
		if strings.Contains(divText, "LICENSE") {
			// Attempt to find known keywords in that text
			patterns := []string{"MIT","BSD","APACHE","ISC","ARTISTIC","ZLIB","WTFPL","CDDL","UNLICENSE","EUPL","MPL","CC0","LGPL","AGPL"}
			for _, pat := range patterns {
				if strings.Contains(divText, pat) {
					foundLicense = pat
					return
				}
			}
		}
	})
	return foundLicense
}

// ------------------- Node Dependencies -------------------

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
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	if version == "" {
		if dist, ok := data["dist-tags"].(map[string]interface{}); ok {
			if latest, ok := dist["latest"].(string); ok {
				version = latest
			}
		}
	}

	license := "Unknown"
	var trans []*NodeDependency

	if vs, ok := data["versions"].(map[string]interface{}); ok {
		if verData, ok := vs[version].(map[string]interface{}); ok {
			license = findNpmLicense(verData)
			if deps, ok := verData["dependencies"].(map[string]interface{}); ok {
				for d, dv := range deps {
					str, _ := dv.(string)
					child, e2 := resolveNodeDependency(d, removeCaretTilde(str), visited)
					if e2 == nil && child != nil {
						trans = append(trans, child)
					}
				}
			}
		}
	}

	if license == "Unknown" {
		// fallback: parse the npm webpage
		l2 := fallbackNpmWebsiteLicense(pkgName)
		if l2 != "" {
			license = l2
		}
	}

	return &NodeDependency{
		Name:     pkgName,
		Version:  version,
		License:  license,
		Details:  "https://www.npmjs.com/package/" + pkgName,
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
		if n, ok := lm["name"].(string); ok && n != "" {
			return n
		}
	}
	if arr, ok := verData["licenses"].([]interface{}); ok && len(arr) > 0 {
		if obj, ok := arr[0].(map[string]interface{}); ok {
			if t, ok := obj["type"].(string); ok && t != "" {
				return t
			}
			if n, ok := obj["name"].(string); ok && n != "" {
				return n
			}
		}
	}
	return "Unknown"
}

// -------------------- Python Deps --------------------

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
		go func(nm, ver string) {
			defer wg.Done()
			dp, e := resolvePythonDependency(nm, ver, map[string]bool{})
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
		log.Println("Python parse error:", e)
	}
	return results, nil
}

func resolvePythonDependency(pkgName, version string, visited map[string]bool) (*PythonDependency, error) {
	if visited[pkgName+"@"+version] {
		return nil, nil
	}
	visited[pkgName+"@"+version] = true

	resp, err := http.Get("https://pypi.org/pypi/" + pkgName + "/json")
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
		return nil, fmt.Errorf("info missing for %s", pkgName)
	}
	if version == "" {
		if v2, ok := info["version"].(string); ok {
			version = v2
		}
	}
	license := "Unknown"
	if l, ok := info["license"].(string); ok && l != "" {
		license = l
	}
	return &PythonDependency{
		Name: pkgName, Version: version, License: license,
		Details: "https://pypi.org/pypi/"+pkgName+"/json",
		Copyleft: isCopyleft(license),
		Language:"python",
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
			Name: nd.Name, Version: nd.Version, License: nd.License,
			Details: nd.Details, Language: nd.Language, Parent: parent,
		})
		if len(nd.Transitive) > 0 {
			out = append(out, flattenNodeAll(nd.Transitive, nd.Name)...)
		}
	}
	return out
}

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

// If no direct node or python, put a single stub
func buildTreeJSON(node []*NodeDependency, py []*PythonDependency) (string,string,error){
	if len(node)==0{
		node=[]*NodeDependency{
			{Name:"No Node dependencies",Version:"",License:"",Details:"",Language:"node"},
		}
	}
	if len(py)==0{
		py=[]*PythonDependency{
			{Name:"No Python dependencies",Version:"",License:"",Details:"",Language:"python"},
		}
	}
	nroot:=map[string]interface{}{
		"Name":"Node.js Dependencies",
		"Version":"",
		"Transitive":node,
	}
	proot:=map[string]interface{}{
		"Name":"Python Dependencies",
		"Version":"",
		"Transitive":py,
	}
	nb, e:=json.MarshalIndent(nroot,"","  ")
	if e!=nil{return"","",e}
	pb,e2:=json.MarshalIndent(proot,"","  ")
	if e2!=nil{return"","",e2}
	return string(nb),string(pb),nil
}

// Single HTML with everything
type SinglePageData struct{
	Summary string
	Deps []FlatDep
	NodeJSON string
	PythonJSON string
}

var singleHTMLTmpl=`<!DOCTYPE html><html><head><meta charset="UTF-8">
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
.graph-container{
  margin:0 auto;
  width:1200px; /* center the graph with wide space */
}
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
<td class="{{if eq .License "Unknown"}}unknown{{else if isCopyleft .License}}copyleft{{else}}non-copyleft{{end}}">
{{.License}}</td>
<td>{{.Parent}}</td>
<td>{{.Language}}</td>
<td><a href="{{.Details}}" target="_blank">View</a></td>
</tr>
{{end}}
</table>

<div class="graph-container">
<h2>Node.js Collapsible Tree</h2>
<div id="nodeGraph"></div>
</div>

<br/><br/>

<div class="graph-container">
<h2>Python Collapsible Tree</h2>
<div id="pythonGraph"></div>
</div>

<script src="https://d3js.org/d3.v6.min.js"></script>
<script>
function collapsibleTree(data, container){
  // wide layout
  var margin={top:20,right:200,bottom:30,left:200},
      width=1200 - margin.left - margin.right,
      height=800 - margin.top - margin.bottom;

  var svg=d3.select(container).append("svg")
    .attr("width",width+margin.left+margin.right)
    .attr("height",height+margin.top+margin.bottom)
    .append("g")
    .attr("transform","translate("+margin.left+","+margin.top+")");

  var treemap = d3.tree().size([height, width]);
  var root = d3.hierarchy(data, function(d){return d.Transitive;});
  root.x0 = height/2; root.y0=0;
  update(root);

  function update(source){
    var treeData=treemap(root),
        nodes=treeData.descendants(),
        links=nodes.slice(1);

    // large horizontal spacing
    nodes.forEach(function(d){d.y=d.depth*300;});

    // node
    var node=svg.selectAll("g.node").data(nodes, function(d){return d.id||(d.id=Math.random());});
    var nodeEnter=node.enter().append("g")
      .attr("class","node")
      .attr("transform",function(d){return"translate("+source.y0+","+source.x0+")";})
      .on("click",click);

    nodeEnter.append("circle")
      .attr("class","node")
      .attr("r",1e-6)
      .style("fill",function(d){return d._children?"lightsteelblue":"#fff"})
      .style("stroke","steelblue").style("stroke-width","3");

    nodeEnter.append("text")
      .attr("dy",".35em")
      .attr("x",function(d){return d.children||d._children?-13:13;})
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

    // link
    var link=svg.selectAll("path.link").data(links,function(d){return d.id||(d.id=Math.random());});
    var linkEnter=link.enter().insert("path","g").attr("class","link")
      .attr("d",function(d){
        var o={x:source.x0,y:source.y0};return diagonal(o,o);
      });
    var linkUpdate=linkEnter.merge(link);
    linkUpdate.transition().duration(200)
      .attr("d",function(d){return diagonal(d,d.parent);});
    link.exit().transition().duration(200)
      .attr("d",function(d){
        var o={x:source.x,y:source.y};return diagonal(o,o);
      }).remove();
    nodes.forEach(function(d){
      d.x0=d.x; d.y0=d.y;
    });
    function diagonal(s,d){
      return"M"+s.y+","+s.x
        +"C"+(s.y+50)+","+s.x
        +" "+(d.y+50)+","+d.x
        +" "+d.y+","+d.x;
    }
  }
  function click(event,d){
    if(d.children){d._children=d.children;d.children=null;}
    else{d.children=d._children;d._children=null;}
    update(d);
  }
}

// Build the trees
var nodeData = NODE_JSON;
var pythonData= PYTHON_JSON;
collapsibleTree(nodeData,"#nodeGraph");
collapsibleTree(pythonData,"#pythonGraph");
</script>
</body>
</html>`

func generateSingleHTML(data SinglePageData) error {
	tmpl, err := template.New("single").Funcs(template.FuncMap{
		"isCopyleft": isCopyleft,
	}).Parse(singleHTMLTmpl)
	if err!=nil{return err}
	f, e2 := os.Create("dependency-license-report.html")
	if e2!=nil{return e2}
	defer f.Close()
	return tmpl.Execute(f, data)
}

func main(){
	// 1) parse Node
	nf:=findFile(".","package.json")
	var nodeDeps []*NodeDependency
	if nf!=""{
		nd,err:=parseNodeDependencies(nf)
		if err==nil{nodeDeps=nd}else{log.Println("Node parse error:",err)}
	}
	// 2) parse Python
	pf:=findFile(".","requirements.txt")
	if pf==""{pf=findFile(".","requirement.txt")}
	var pyDeps []*PythonDependency
	if pf!=""{
		pd,err:=parsePythonDependencies(pf)
		if err==nil{pyDeps=pd}else{log.Println("Python parse error:",err)}
	}
	// 3) flatten
	fn:=flattenNodeAll(nodeDeps,"Direct")
	fp:=flattenPyAll(pyDeps,"Direct")
	allFlattened := append(fn,fp...)
	// 4) count copyleft
	copyleftCount:=0
	for _,dep:=range allFlattened{
		if isCopyleft(dep.License){copyleftCount++}
	}
	summary:=fmt.Sprintf("%d direct Node.js deps, %d direct Python deps, copyleft:%d",
		len(nodeDeps), len(pyDeps), copyleftCount)

	// 5) build tree JSON
	njson, pjson, err := buildTreeJSON(nodeDeps, pyDeps)
	if err!=nil{
		log.Println("Error building JSON:", err)
		njson,pjson="{}","{}"
	}

	// 6) generate single HTML
	sp := SinglePageData{
		Summary: summary,
		Deps: allFlattened,
		NodeJSON: njson,
		PythonJSON: pjson,
	}
	if e:=generateSingleHTML(sp);e!=nil{
		log.Println("Error generating HTML:", e)
		os.Exit(1)
	}
	fmt.Println("dependency-license-report.html generated. Single page with wide, centered graphs & improved fallback license via goquery.")
}
