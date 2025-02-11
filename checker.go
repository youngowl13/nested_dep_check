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

// isCopyleft checks if a license text is likely copyleft
func isCopyleft(license string) bool {
	copyleftLicenses:=[]string{
		"GPL","GNU GENERAL PUBLIC LICENSE","LGPL","GNU LESSER GENERAL PUBLIC LICENSE",
		"AGPL","GNU AFFERO GENERAL PUBLIC LICENSE","MPL","MOZILLA PUBLIC LICENSE",
		"CC-BY-SA","CREATIVE COMMONS ATTRIBUTION-SHAREALIKE","EPL","ECLIPSE PUBLIC LICENSE",
		"OFL","OPEN FONT LICENSE","CPL","COMMON PUBLIC LICENSE","OSL","OPEN SOFTWARE LICENSE",
	}
	up:=strings.ToUpper(license)
	for _,kw:=range copyleftLicenses {
		if strings.Contains(up,kw){return true}
	}
	return false
}

// findFile searches directory tree for target
func findFile(root,target string)string{
	var found string
	filepath.WalkDir(root,func(path string,d fs.DirEntry,err error)error{
		if err==nil && d.Name()==target{
			found=path
			return filepath.SkipDir
		}
		return nil
	})
	return found
}

// removeCaretTilde strips leading ^ or ~ from version string
func removeCaretTilde(ver string)string{
	ver=strings.TrimSpace(ver)
	ver=strings.TrimLeft(ver,"^~")
	return ver
}

//------------------- Node.js dependencies -------------------

type NodeDependency struct{
	Name string
	Version string
	License string
	Details string
	Copyleft bool
	Transitive []*NodeDependency
	Language string
}

func parseNodeDependencies(path string)([]*NodeDependency,error){
	data,err:=os.ReadFile(path)
	if err!=nil{return nil,err}
	var pkg map[string]interface{}
	if err:=json.Unmarshal(data,&pkg);err!=nil{return nil,err}
	deps, _ := pkg["dependencies"].(map[string]interface{})
	if deps==nil{
		return nil,fmt.Errorf("no dependencies found in package.json")
	}
	visited:=map[string]bool{}
	var results []*NodeDependency
	for name,ver:=range deps{
		vstr,_:=ver.(string)
		n, e:=resolveNodeDependency(name,removeCaretTilde(vstr),visited)
		if e==nil && n!=nil{
			results=append(results,n)
		}
	}
	return results,nil
}

func resolveNodeDependency(pkg,ver string,visited map[string]bool)(*NodeDependency,error){
	if visited[pkg+"@"+ver]{return nil,nil}
	visited[pkg+"@"+ver]=true

	url:="https://registry.npmjs.org/"+pkg
	resp,err:=http.Get(url)
	if err!=nil{return nil,err}
	defer resp.Body.Close()

	var data map[string]interface{}
	if err:=json.NewDecoder(resp.Body).Decode(&data);err!=nil{return nil,err}

	if ver==""{
		if dist,ok:=data["dist-tags"].(map[string]interface{});ok{
			if latest, ok:=dist["latest"].(string);ok{
				ver=latest
			}
		}
	}
	license:="Unknown"
	var trans []*NodeDependency
	if vs,ok:=data["versions"].(map[string]interface{});ok{
		if verData,ok:=vs[ver].(map[string]interface{});ok{
			license=findLicenseNPM(verData)
			if deps,ok:=verData["dependencies"].(map[string]interface{});ok{
				for dep,dv:=range deps{
					dstr,_:=dv.(string)
					nd,ee:=resolveNodeDependency(dep,removeCaretTilde(dstr),visited)
					if ee==nil && nd!=nil{
						trans=append(trans,nd)
					}
				}
			}
		}
	}
	return &NodeDependency{
		Name:pkg,Version:ver,
		License:license,
		Details:"https://www.npmjs.com/package/"+pkg,
		Copyleft:isCopyleft(license),
		Transitive:trans,
		Language:"node",
	},nil
}

func findLicenseNPM(verData map[string]interface{}) string {
	// "license" as string
	if l, ok := verData["license"].(string); ok && l!=""{
		return l
	}
	// "license" as object
	if lm, ok := verData["license"].(map[string]interface{}); ok{
		if t,ok:=lm["type"].(string);ok && t!=""{
			return t
		}
		if n,ok:=lm["name"].(string);ok && n!=""{
			return n
		}
	}
	// "licenses" array
	if arr,ok:=verData["licenses"].([]interface{});ok{
		if len(arr)>0{
			if licObj,ok:=arr[0].(map[string]interface{});ok{
				if t,ok:=licObj["type"].(string);ok&&t!=""{return t}
				if nm,ok:=licObj["name"].(string);ok&&nm!=""{return nm}
			}
		}
	}
	return "Unknown"
}

//------------------ Python dependencies ------------------

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
	reqs,err:=parseRequirements(f)
	if err!=nil{return nil,err}
	var results []*PythonDependency
	var wg sync.WaitGroup
	depChan:=make(chan PythonDependency)
	errChan:=make(chan error)
	for _,r:=range reqs{
		wg.Add(1)
		go func(nm,vs string){
			defer wg.Done()
			dp,e:=resolvePythonDependency(nm,vs,map[string]bool{})
			if e!=nil{
				errChan<-e
				return
			}
			if dp!=nil{
				depChan<-*dp
			}
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
		log.Println("Py error:",e)
	}
	return results,nil
}

func resolvePythonDependency(pkg,ver string,visited map[string]bool)(*PythonDependency,error){
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
	info, _ := data["info"].(map[string]interface{})
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
		Name:pkg,Version:ver,License:license,
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
	var ret []requirement
	for _,ln:=range lines{
		line:=strings.TrimSpace(ln)
		if line==""||strings.HasPrefix(line,"#"){continue}
		parts:=strings.Split(line,"==")
		if len(parts)!=2{
			parts=strings.Split(line,">=")
			if len(parts)!=2{
				log.Println("Invalid requirement line:",line)
				continue
			}
		}
		ret=append(ret,requirement{name:strings.TrimSpace(parts[0]),
			version:strings.TrimSpace(parts[1])})
	}
	return ret,nil
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

func flattenAllNodeDeps(nds []*NodeDependency,parent string)[]FlatDep{
	var out []FlatDep
	for _,nd:=range nds{
		out=append(out,FlatDep{
			Name:nd.Name,Version:nd.Version,
			License:nd.License,Details:nd.Details,
			Language:nd.Language,Parent:parent,
		})
		if len(nd.Transitive)>0{
			out=append(out,flattenAllNodeDeps(nd.Transitive,nd.Name)...)
		}
	}
	return out
}

func flattenAllPythonDeps(pds []*PythonDependency,parent string)[]FlatDep{
	var out []FlatDep
	for _,pd:=range pds{
		out=append(out,FlatDep{
			Name:pd.Name,Version:pd.Version,
			License:pd.License,Details:pd.Details,
			Language:pd.Language,Parent:parent,
		})
		if len(pd.Transitive)>0{
			out=append(out,flattenAllPythonDeps(pd.Transitive,pd.Name)...)
		}
	}
	return out
}

// Graph

func dependencyTreeJSONFinal(node []*NodeDependency,py []*PythonDependency)(string,string,error){
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
	nb,e:=json.MarshalIndent(nroot,"","  ")
	if e!=nil{return"","",e}
	pb,e2:=json.MarshalIndent(proot,"","  ")
	if e2!=nil{return"","",e2}
	return string(nb),string(pb),nil
}

// Template

type ReportData struct{
	Summary string
	Deps []FlatDep
	NodeJS template.JS
	Python template.JS
}

var tpl=`<!DOCTYPE html><html><head><meta charset="UTF-8">
<title>Dependency License Report</title>
<style>
body{font-family:Arial,sans-serif;margin:20px}
h1,h2{color:#2c3e50}
table{width:100%;border-collapse:collapse;margin-bottom:20px}
th,td{border:1px solid #ddd;padding:8px;text-align:left}
th{background:#f2f2f2}
.copyleft{background:#f8d7da;color:#721c24}
.non-copyleft{background:#d4edda;color:#155724}
.unknown{background:#fff3cd;color:#856404}
</style></head><body>
<h1>Dependency License Report</h1><h2>Summary</h2><p>{{.Summary}}</p>
<h2>Dependencies</h2><table><tr>
<th>Name</th><th>Version</th><th>License</th>
<th>Parent</th><th>Language</th><th>Details</th>
</tr>{{range .Deps}}<tr>
<td>{{.Name}}</td><td>{{.Version}}</td>
<td class="{{if eq .License "Unknown"}}unknown{{else if isCopyleft .License}}copyleft{{else}}non-copyleft{{end}}">
{{.License}}</td>
<td>{{.Parent}}</td><td>{{.Language}}</td>
<td><a href="{{.Details}}" target="_blank">View</a></td>
</tr>{{end}}</table>
<h2>Dependency Graph Visualization</h2>
<h3>Node.js Tree</h3><div id="nodeGraph"></div>
<h3>Python Tree</h3><div id="pythonGraph"></div>
<script src="https://d3js.org/d3.v6.min.js"></script>
<script>
var nodeData={{.NodeJS}};
var pythonData={{.Python}};
function collapsibleTree(data,sel){
var m={top:20,right:90,bottom:30,left:90},w=660-m.left-m.right,h=500-m.top-m.bottom;
var svg=d3.select(sel).append("svg")
.attr("width",w+m.left+m.right).attr("height",h+m.top+m.bottom)
.append("g").attr("transform","translate("+m.left+","+m.top+")");
var tree=d3.tree().size([h,w]);
var root=d3.hierarchy(data,function(d){return d.Transitive;});
root.x0=h/2;root.y0=0;update(root);
function update(source){
var treed=tree(root),nodes=treed.descendants(),links=nodes.slice(1);
nodes.forEach(function(d){d.y=d.depth*180});
var node=svg.selectAll("g.node")
.data(nodes,function(d){return d.id||(d.id=Math.random())});
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
.attr("transform",function(d){return"translate("+source.y+","+source.x+")";}).remove();
nodeExit.select("circle").attr("r",1e-6);
var link=svg.selectAll("path.link").data(links,function(d){return d.id||(d.id=Math.random())});
var linkEnter=link.enter().insert("path","g").attr("class","link")
.attr("d",function(d){var o={x:source.x0,y:source.y0};return diagonal(o,o)});
var linkUpdate=linkEnter.merge(link);
linkUpdate.transition().duration(200).attr("d",function(d){return diagonal(d,d.parent)});
link.exit().transition().duration(200).attr("d",function(d){
var o={x:source.x,y:source.y};return diagonal(o,o)}).remove();
nodes.forEach(function(d){d.x0=d.x;d.y0=d.y});
function diagonal(s,d){return"M"+s.y+","+s.x
+"C"+(s.y+50)+","+s.x+" "+(d.y+50)+","+d.x+" "+d.y+","+d.x}
}
function click(e,d){
if(d.children){d._children=d.children;d.children=null;}else{d.children=d._children;d._children=null;}
update(d);
}
}
collapsibleTree(nodeData,"#nodeGraph");
collapsibleTree(pythonData,"#pythonGraph");
</script></body></html>`

func generateHTML(data ReportData) error {
	t, e := template.New("report").Funcs(template.FuncMap{
		"isCopyleft": isCopyleft,
	}).Parse(tpl)
	if e!=nil{return e}
	out, e2 := os.Create("dependency-license-report.html")
	if e2!=nil{return e2}
	defer out.Close()
	return t.Execute(out, data)
}

func main(){
	nodeFile:=findFile(".","package.json")
	var nodeDeps []*NodeDependency
	if nodeFile!=""{
		nd,err:=parseNodeDependencies(nodeFile)
		if err==nil{nodeDeps=nd}else{log.Println("Node parse err:",err)}
	}
	pyFile:=findFile(".","requirements.txt")
	if pyFile==""{
		pyFile=findFile(".","requirement.txt")
	}
	var pyDeps []*PythonDependency
	if pyFile!=""{
		pd,err:=parsePythonDependencies(pyFile)
		if err==nil{pyDeps=pd}else{log.Println("Python parse err:",err)}
	}
	fn:=flattenAllNodeDeps(nodeDeps,"Direct")
	fp:=flattenAllPythonDeps(pyDeps,"Direct")
	allFlat:=append(fn,fp...)
	tCleft:=0
	for _,f:=range allFlat{
		if isCopyleft(f.License){tCleft++}
	}
	summary:=fmt.Sprintf("%d direct Node.js deps, %d direct Python deps, copyleft:%d",
		len(nodeDeps),len(pyDeps),tCleft)
	njson,pjson,err:=dependencyTreeJSONFinal(nodeDeps,pyDeps)
	if err!=nil{
		log.Println("Building JSON err:",err)
		njson,pjson="[]","[]"
	}
	rep:=ReportData{Summary:summary,Deps:allFlat,NodeJS:template.JS(njson),Python:template.JS(pjson)}
	if e:=generateHTML(rep);e!=nil{
		log.Println("Generate HTML error:", e)
		os.Exit(1)
	}
	fmt.Println("dependency-license-report.html generated.")
}
