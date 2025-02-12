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

// isCopyleft checks license text for known copyleft strings
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

// findFile searches for target
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

// removeCaretTilde strips leading ^ or ~
func removeCaretTilde(ver string) string {
	return strings.TrimLeft(strings.TrimSpace(ver),"^~")
}

// fallbackNpmWebsiteLicense tries to parse HTML from npmjs.com for your snippet
func fallbackNpmWebsiteLicense(pkg string) string {
	resp, err := http.Get("https://www.npmjs.com/package/" + pkg)
	if err!=nil||resp.StatusCode!=200{
		return ""
	}
	defer resp.Body.Close()

	doc, err2 := goquery.NewDocumentFromReader(resp.Body)
	if err2!=nil{
		return ""
	}
	var found string
	// Specifically look for <h3>License</h3> and the adjacent <p> with e.g. class "f2874b88"
	// adapt if npm changes the structure
	doc.Find("h3").EachWithBreak(func(i int, s *goquery.Selection) bool {
		txt := strings.TrimSpace(s.Text())
		if strings.EqualFold(txt, "License") {
			// typically next sibling is <p class="f2874b88 ...">MIT</p>
			p := s.Next()
			if goquery.NodeName(p)=="p" {
				licenseText := strings.TrimSpace(p.Text())
				if licenseText!="" {
					found=licenseText
				}
			}
			return false // break
		}
		return true
	})
	if found=="" {
		// try an alternative approach: search "License" in the div text
		doc.Find("div").Each(func(i int, sel *goquery.Selection){
			divtxt := strings.ToUpper(sel.Text())
			if strings.Contains(divtxt, "LICENSE"){
				// parse known patterns
				patterns := []string{"MIT","BSD","APACHE","ISC","ARTISTIC","ZLIB","WTFPL",
					"CDDL","UNLICENSE","EUPL","MPL","CC0","LGPL","AGPL"}
				for _, p := range patterns {
					if strings.Contains(divtxt, p) {
						found=p
						return
					}
				}
			}
		})
	}
	return found
}

// ------------------- Node.js -------------------

type NodeDependency struct{
	Name,Version,License,Details string
	Copyleft bool
	Transitive []*NodeDependency
	Language string
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
	visited:=map[string]bool{}
	var out []*NodeDependency
	for nm,ver:=range deps{
		str,_:=ver.(string)
		d,e:=resolveNodeDependency(nm, removeCaretTilde(str), visited)
		if e==nil&&d!=nil{
			out=append(out,d)
		}
	}
	return out,nil
}

func resolveNodeDependency(pkg,ver string, visited map[string]bool)(*NodeDependency,error){
	key:=pkg+"@"+ver
	if visited[key]{return nil,nil}
	visited[key]=true

	resp,err:=http.Get("https://registry.npmjs.org/"+pkg)
	if err!=nil{return nil,err}
	defer resp.Body.Close()

	var regData map[string]interface{}
	if err:=json.NewDecoder(resp.Body).Decode(&regData);err!=nil{return nil,err}
	if ver==""{
		if dist,ok:=regData["dist-tags"].(map[string]interface{});ok{
			if lat,ok:=dist["latest"].(string);ok{
				ver=lat
			}
		}
	}
	license:="Unknown"
	var trans []*NodeDependency
	if vs,ok:=regData["versions"].(map[string]interface{});ok{
		if verData,ok:=vs[ver].(map[string]interface{});ok{
			license=findNpmLicense(verData)
			if deps,ok:=verData["dependencies"].(map[string]interface{});ok{
				for nm,v2:=range deps{
					str2,_:=v2.(string)
					ch,e2:=resolveNodeDependency(nm, removeCaretTilde(str2), visited)
					if e2==nil&&ch!=nil{
						trans=append(trans,ch)
					}
				}
			}
		}
	}
	if license=="Unknown"{
		// fallback to npm webpage parse
		lic2 := fallbackNpmWebsiteLicense(pkg)
		if lic2!=""{
			license=lic2
		}
	}
	return &NodeDependency{
		Name: pkg,Version:ver,License:license,
		Details:"https://www.npmjs.com/package/"+pkg,
		Copyleft:isCopyleft(license),
		Transitive:trans,
		Language:"node",
	},nil
}

func findNpmLicense(verData map[string]interface{})string{
	if l, ok:=verData["license"].(string);ok&&l!=""{
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

// ------------------- Python -------------------

type PythonDependency struct{
	Name,Version,License,Details string
	Copyleft bool
	Transitive []*PythonDependency
	Language string
}

func parsePythonDependencies(path string)([]*PythonDependency,error){
	f,err:=os.Open(path)
	if err!=nil{return nil,err}
	defer f.Close()
	reqs,err:=parseRequirements(f)
	if err!=nil{return nil,err}
	var results []*PythonDependency
	var wg sync.WaitGroup
	depChan:=make(chan PythonDependency)
	errChan:=make(chan error)
	for _, r:=range reqs{
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

func resolvePythonDependency(pkg,ver string, visited map[string]bool)(*PythonDependency,error){
	if visited[pkg+"@"+ver]{return nil,nil}
	visited[pkg+"@"+ver]=true

	resp,err:=http.Get("https://pypi.org/pypi/"+pkg+"/json")
	if err!=nil{return nil,err}
	defer resp.Body.Close()
	if resp.StatusCode!=200{
		return nil,fmt.Errorf("PyPI status:%d",resp.StatusCode)
	}
	var data map[string]interface{}
	if err:=json.NewDecoder(resp.Body).Decode(&data);err!=nil{return nil,err}
	info,_:=data["info"].(map[string]interface{})
	if info==nil{
		return nil,fmt.Errorf("info missing for %s",pkg)
	}
	if ver==""{
		if v2,ok:=info["version"].(string);ok{
			ver=v2
		}
	}
	license:="Unknown"
	if l,ok:=info["license"].(string);ok&&l!=""{
		license=l
	}
	return &PythonDependency{
		Name:pkg, Version:ver, License:license,
		Details:"https://pypi.org/pypi/"+pkg+"/json",
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

type FlatDep struct{
	Name,Version,License,Details,Language,Parent string
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

// buildTreeJSON - if no direct node or python, put a single stub so user sees something
func buildTreeJSON(node []*NodeDependency, py []*PythonDependency)(string,string,error){
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
		"Name":"Node.js Dependencies","Version":"","Transitive":node,
	}
	proot:=map[string]interface{}{
		"Name":"Python Dependencies","Version":"","Transitive":py,
	}
	nb,e:=json.MarshalIndent(nroot,"","  ")
	if e!=nil{return"","",e}
	pb,e2:=json.MarshalIndent(proot,"","  ")
	if e2!=nil{return"","",e2}
	return string(nb),string(pb),nil
}

// Single HTML
type SinglePageData struct{
	Summary string
	Deps []FlatDep
	NodeJSON,PythonJSON string
}

var singleTmpl=`<!DOCTYPE html><html><head><meta charset="UTF-8">
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
  width:1200px;
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
  var margin={top:20,right:200,bottom:30,left:200},
      width=1200 - margin.left - margin.right,
      height=800 - margin.top - margin.bottom;

  var svg=d3.select(container).append("svg")
    .attr("width",width+margin.left+margin.right)
    .attr("height",height+margin.top+margin.bottom)
    .append("g")
    .attr("transform","translate("+margin.left+","+margin.top+")");

  var treemap=d3.tree().size([height,width]);
  var root=d3.hierarchy(data,function(d){return d.Transitive;});
  root.x0=height/2; root.y0=0;

  update(root);

  function update(source){
    var treeData=treemap(root),
        nodes=treeData.descendants(),
        links=nodes.slice(1);

    // Extra wide horizontal spacing
    nodes.forEach(function(d){d.y=d.depth*600;});

    var node=svg.selectAll("g.node").data(nodes,function(d){return d.id||(d.id=Math.random())});
    var nodeEnter=node.enter().append("g").attr("class","node")
    .attr("transform",function(d){return"translate("+source.y0+","+source.x0+")";})
    .on("click",click);

    nodeEnter.append("circle").attr("class","node").attr("r",1e-6)
    .style("fill",function(d){return d._children?"lightsteelblue":"#fff"})
    .style("stroke","steelblue").style("stroke-width","3");

    nodeEnter.append("text").attr("dy",".35em")
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

    var link=svg.selectAll("path.link").data(links,function(d){return d.id||(d.id=Math.random())});
    var linkEnter=link.enter().insert("path","g").attr("class","link")
    .attr("d",function(d){var o={x:source.x0,y:source.y0};return diag(o,o)});
    var linkUpdate=linkEnter.merge(link);
    linkUpdate.transition().duration(200)
      .attr("d",function(d){return diag(d,d.parent)});
    link.exit().transition().duration(200)
      .attr("d",function(d){var o={x:source.x,y:source.y};return diag(o,o)})
      .remove();
    nodes.forEach(function(d){d.x0=d.x; d.y0=d.y;});

    function diag(s,d){
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

var nodeData = NODE_JSON;
var pythonData= PYTHON_JSON;

collapsibleTree(nodeData,"#nodeGraph");
collapsibleTree(pythonData,"#pythonGraph");
</script>
</body></html>`

func generateSingleHTML(rep SinglePageData) error {
	t, e := template.New("report").Funcs(template.FuncMap{
		"isCopyleft": isCopyleft,
	}).Parse(singleTmpl)
	if e!=nil{return e}
	f, e2 := os.Create("dependency-license-report.html")
	if e2!=nil{return e2}
	defer f.Close()
	return t.Execute(f, rep)
}

func main(){
	// 1) Parse Node
	nFile:=findFile(".","package.json")
	var nodeDeps []*NodeDependency
	if nFile!=""{
		nd,err:=parseNodeDependencies(nFile)
		if err==nil{nodeDeps=nd}else{log.Println("Node parse error:",err)}
	}

	// 2) Parse Python
	pFile:=findFile(".","requirements.txt")
	if pFile==""{pFile=findFile(".","requirement.txt")}
	var pyDeps []*PythonDependency
	if pFile!=""{
		pd,err:=parsePythonDependencies(pFile)
		if err==nil{pyDeps=pd}else{log.Println("Python parse error:",err)}
	}

	// 3) Flatten
	flatNode := flattenNodeAll(nodeDeps,"Direct")
	flatPy   := flattenPyAll(pyDeps,"Direct")
	allDeps  := append(flatNode, flatPy...)

	// 4) Count copyleft
	countCleft:=0
	for _,dep := range allDeps {
		if isCopyleft(dep.License){countCleft++}
	}
	summary := fmt.Sprintf("%d direct Node.js deps, %d direct Python deps, Copyleft: %d",
		len(nodeDeps),len(pyDeps),countCleft)

	// 5) Build JSON for D3
	njson, pjson, err := buildTreeJSON(nodeDeps, pyDeps)
	if err!=nil{
		log.Println("Error building JSON for trees:", err)
		njson,pjson="{}","{}"
	}

	// 6) Single HTML
	reportData := SinglePageData{
		Summary: summary,
		Deps: allDeps,
		NodeJSON: njson,
		PythonJSON: pjson,
	}
	if e:=generateSingleHTML(reportData); e!=nil{
		log.Println("Error generating HTML:", e)
		os.Exit(1)
	}
	fmt.Println("dependency-license-report.html generated. Large horizontal spacing, goquery fallback. Yellow for Unknown.")
}
