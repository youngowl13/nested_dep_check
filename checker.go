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
	"regexp"
	"strings"
	"sync"

	"io/fs"
)

//============================================================================
// 1) Copyleft detection for color-coding
//============================================================================
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

//============================================================================
// 2) findFile, removeCaretTilde
//============================================================================
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

func removeCaretTilde(ver string) string {
	ver = strings.TrimSpace(ver)
	return strings.TrimLeft(ver, "^~")
}

//============================================================================
// 3) Minimal dynamic fallback: 1 line after "license"
//============================================================================

var tagRe = regexp.MustCompile(`<[^>]+>`)

// minimalLicenseSnippet returns up to 2 lines: the line with “license” + next line
// Then we remove <tags> and trim to 200 chars.
func minimalLicenseSnippet(lines []string, i int) string {
	snippetLines := []string{lines[i]} // always keep line i
	if i+1 < len(lines) {
		snippetLines = append(snippetLines, lines[i+1]) // up to 1 line after
	}
	combined := strings.Join(snippetLines, " ")

	// remove any <tag> 
	clean := tagRe.ReplaceAllString(combined, "")
	// trim leading/trailing
	clean = strings.TrimSpace(clean)
	// limit to 200 chars
	if len(clean) > 200 {
		clean = clean[:200] + "..."
	}
	return clean
}

// fallbackNpmLicense fetches npmjs.com/package/<pkgName>, scanning line by line
// If we see “license”, we combine that line + next line, remove HTML tags, limit 200 chars
func fallbackNpmLicense(pkgName string) string {
	url := "https://www.npmjs.com/package/" + pkgName
	resp, err := http.Get(url)
	if err!=nil || resp.StatusCode!=200 {
		return ""
	}
	defer resp.Body.Close()

	var lines []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if e:= scanner.Err(); e!=nil{ return "" }

	for i:=0; i< len(lines); i++ {
		if strings.Contains(strings.ToLower(lines[i]), "license") {
			return minimalLicenseSnippet(lines,i)
		}
	}
	return ""
}

// fallbackPyPiLicense does the same for pypi.org/project/<pkgName>
func fallbackPyPiLicense(pkgName string) string {
	url := "https://pypi.org/project/" + pkgName
	resp, err := http.Get(url)
	if err!=nil || resp.StatusCode!=200 {
		return ""
	}
	defer resp.Body.Close()

	var lines []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if e:= scanner.Err(); e!=nil{ return "" }

	for i:=0; i< len(lines); i++ {
		if strings.Contains(strings.ToLower(lines[i]), "license") {
			return minimalLicenseSnippet(lines,i)
		}
	}
	return ""
}

//============================================================================
// 4) Node logic: parse, fallback
//============================================================================
type NodeDependency struct {
	Name      string
	Version   string
	License   string
	Details   string
	Copyleft  bool
	Transitive []*NodeDependency
	Language string
}

func parseNodeDependencies(path string) ([]*NodeDependency, error) {
	raw,err := os.ReadFile(path)
	if err!=nil{return nil,err}
	var pkg map[string]interface{}
	if e:=json.Unmarshal(raw,&pkg); e!=nil{
		return nil,e
	}
	deps, _ := pkg["dependencies"].(map[string]interface{})
	if deps==nil{
		return nil,fmt.Errorf("no dependencies found in package.json")
	}
	visited:= map[string]bool{}
	var out []*NodeDependency
	for nm, ver := range deps {
		str,_ := ver.(string)
		nd,e:= resolveNodeDependency(nm, removeCaretTilde(str), visited)
		if e==nil && nd!=nil{
			out=append(out, nd)
		}
	}
	return out,nil
}

func resolveNodeDependency(pkg,ver string, visited map[string]bool)(*NodeDependency,error){
	key:= pkg+"@"+ver
	if visited[key]{return nil,nil}
	visited[key]=true

	regURL:= "https://registry.npmjs.org/"+pkg
	resp,err:= http.Get(regURL)
	if err!=nil{return nil,err}
	defer resp.Body.Close()

	var data map[string]interface{}
	if e:= json.NewDecoder(resp.Body).Decode(&data); e!=nil{
		return nil,e
	}
	if ver=="" {
		if dist,ok:= data["dist-tags"].(map[string]interface{}); ok {
			if lat,ok:= dist["latest"].(string); ok{
				ver=lat
			}
		}
	}
	license:= "Unknown"
	var trans []*NodeDependency

	if vs, ok:= data["versions"].(map[string]interface{}); ok {
		if verData, ok:= vs[ver].(map[string]interface{}); ok {
			license= findNpmLicense(verData)
			if deps,ok:= verData["dependencies"].(map[string]interface{}); ok {
				for subName, subVer:= range deps {
					sv,_:= subVer.(string)
					ch,e2:= resolveNodeDependency(subName,removeCaretTilde(sv), visited)
					if e2==nil && ch!=nil{
						trans=append(trans,ch)
					}
				}
			}
		}
	}
	if license=="Unknown" {
		fb:= fallbackNpmLicense(pkg)
		if fb!=""{
			license= fb
		}
	}
	return &NodeDependency{
		Name: pkg,Version: ver,License: license,
		Details:"https://www.npmjs.com/package/"+pkg,
		Copyleft:isCopyleft(license),
		Transitive: trans,
		Language:"node",
	},nil
}

func findNpmLicense(verData map[string]interface{}) string {
	if l,ok:= verData["license"].(string); ok && l!=""{
		return l
	}
	if lm,ok:= verData["license"].(map[string]interface{}); ok {
		if t,ok:= lm["type"].(string); ok && t!=""{
			return t
		}
		if nm,ok:= lm["name"].(string); ok && nm!=""{
			return nm
		}
	}
	if arr,ok:= verData["licenses"].([]interface{}); ok && len(arr)>0 {
		if obj,ok:= arr[0].(map[string]interface{});ok{
			if t,ok:= obj["type"].(string); ok && t!=""{
				return t
			}
			if nm,ok:= obj["name"].(string); ok && nm!=""{
				return nm
			}
		}
	}
	return "Unknown"
}

//============================================================================
// 5) Python logic: parse, fallback, transitive
//============================================================================
type PythonDependency struct {
	Name string
	Version string
	License string
	Details string
	Copyleft bool
	Transitive []*PythonDependency
	Language string
}

func parsePythonDependencies(path string)([]*PythonDependency,error){
	f,err:= os.Open(path)
	if err!=nil{return nil,err}
	defer f.Close()

	reqs,err:= parseRequirements(f)
	if err!=nil{return nil,err}

	var results []*PythonDependency
	var wg sync.WaitGroup
	depChan:= make(chan PythonDependency)
	errChan:= make(chan error)

	for _, r:= range reqs{
		wg.Add(1)
		go func(nm,vr string){
			defer wg.Done()
			d,e:= resolvePythonDependency(nm,vr,map[string]bool{})
			if e!=nil{errChan<-e;return}
			if d!=nil{depChan<-*d}
		}(r.name,r.version)
	}
	go func(){
		wg.Wait()
		close(depChan)
		close(errChan)
	}()
	for d:= range depChan{
		results=append(results,&d)
	}
	for e:= range errChan{
		log.Println("Python parse error:", e)
	}
	return results,nil
}

func resolvePythonDependency(pkg,ver string, visited map[string]bool)(*PythonDependency,error){
	key:= pkg+"@"+ver
	if visited[key]{return nil,nil}
	visited[key]=true

	url:= "https://pypi.org/pypi/"+pkg+"/json"
	resp,err:= http.Get(url)
	if err!=nil{return nil,err}
	defer resp.Body.Close()

	if resp.StatusCode!=200{
		return nil,fmt.Errorf("PyPI status:%d", resp.StatusCode)
	}
	var data map[string]interface{}
	if e:= json.NewDecoder(resp.Body).Decode(&data); e!=nil{
		return nil,e
	}
	info,_:= data["info"].(map[string]interface{})
	if info==nil{
		return nil,fmt.Errorf("info missing for %s", pkg)
	}
	if ver=="" {
		if v2,ok:= info["version"].(string);ok{
			ver= v2
		}
	}
	license:="Unknown"
	if l,ok:= info["license"].(string); ok && l!=""{
		license= l
	}
	if license=="Unknown" {
		fb:= fallbackPyPiLicense(pkg)
		if fb!=""{
			license= fb
		}
	}
	// transitive from requires_dist
	var trans []*PythonDependency
	if distArr,ok:= info["requires_dist"].([]interface{}); ok && len(distArr)>0 {
		for _, item:= range distArr{
			if line,ok:= item.(string);ok{
				sn,sv:= parseDistRequirement(line)
				if sn!="" {
					ch,e2:= resolvePythonDependency(sn,sv, visited)
					if e2==nil && ch!=nil{
						trans=append(trans,ch)
					}
				}
			}
		}
	}

	return &PythonDependency{
		Name: pkg,Version: ver,License: license,
		Details:"https://pypi.org/project/"+pkg,
		Copyleft: isCopyleft(license),
		Transitive: trans,
		Language:"python",
	},nil
}

func parseDistRequirement(line string)(string,string){
	line= strings.TrimSpace(line)
	i:= strings.Index(line," ")
	if i<0 {
		return line,""
	}
	name:= line[:i]
	rest:= strings.TrimSpace(line[i:])
	rest= strings.Trim(rest,"()")
	return name,rest
}

type requirement struct{name,version string}

func parseRequirements(r io.Reader)([]requirement,error){
	raw,err:= io.ReadAll(r)
	if err!=nil{return nil,err}
	lines:= strings.Split(string(raw),"\n")
	var out []requirement
	for _, ln:= range lines {
		line:= strings.TrimSpace(ln)
		if line==""|| strings.HasPrefix(line,"#"){
			continue
		}
		p:= strings.Split(line,"==")
		if len(p)!=2 {
			p= strings.Split(line,">=")
			if len(p)!=2{
				log.Println("Invalid python requirement:", line)
				continue
			}
		}
		out=append(out, requirement{
			name: strings.TrimSpace(p[0]),
			version: strings.TrimSpace(p[1]),
		})
	}
	return out,nil
}

//============================================================================
// 6) Flatten + <details> expansions
//============================================================================
type FlatDep struct{
	Name string
	Version string
	License string
	Details string
	Language string
	Parent string
}

// Node flatten
func flattenNodeAll(nds []*NodeDependency, parent string) []FlatDep {
	var out []FlatDep
	for _, nd := range nds {
		out= append(out, FlatDep{
			Name:nd.Name,Version:nd.Version,License:nd.License,
			Details:nd.Details,Language:nd.Language,Parent:parent,
		})
		if len(nd.Transitive)>0{
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
			Name:pd.Name,Version:pd.Version,License:pd.License,
			Details:pd.Details,Language:pd.Language,Parent:parent,
		})
		if len(pd.Transitive)>0{
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

	if len(nd.Transitive)>0{
		sb.WriteString("<ul>\n")
		for _,ch:= range nd.Transitive{
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
	for _,nd:= range nodes {
		sb.WriteString(buildNodeTreeHTML(nd))
	}
	return sb.String()
}

// Python expansions
func buildPythonTreeHTML(pd *PythonDependency) string {
	sum:= fmt.Sprintf("%s@%s (License: %s)", pd.Name, pd.Version, pd.License)
	var sb strings.Builder
	sb.WriteString("<details><summary>")
	sb.WriteString(template.HTMLEscapeString(sum))
	sb.WriteString("</summary>\n")

	if len(pd.Transitive)>0{
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
	if len(py)==0{
		return "<p>No Python dependencies found.</p>"
	}
	var sb strings.Builder
	for _,pd:= range py {
		sb.WriteString(buildPythonTreeHTML(pd))
	}
	return sb.String()
}

//============================================================================
// 7) Single HTML
//============================================================================
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
<tr>
  <th>Name</th><th>Version</th><th>License</th><th>Parent</th><th>Language</th><th>Details</th>
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

//============================================================================
// 8) main
//============================================================================
func main(){
	// Node
	nf:= findFile(".", "package.json")
	var nodeDeps []*NodeDependency
	if nf!=""{
		nd,err:= parseNodeDependencies(nf)
		if err==nil{
			nodeDeps= nd
		} else {
			log.Println("Node parse error:", err)
		}
	}

	// Python
	pf:= findFile(".", "requirements.txt")
	if pf==""{
		pf= findFile(".", "requirement.txt")
	}
	var pyDeps []*PythonDependency
	if pf!=""{
		pd,err:= parsePythonDependencies(pf)
		if err==nil{
			pyDeps= pd
		} else {
			log.Println("Python parse error:", err)
		}
	}

	// Flatten
	fn:= flattenNodeAll(nodeDeps,"Direct")
	fp:= flattenPyAll(pyDeps,"Direct")
	allDeps:= append(fn,fp...)

	// Copyleft count
	countCLeft:=0
	for _,d:= range allDeps {
		if isCopyleft(d.License){
			countCLeft++
		}
	}
	summary:= fmt.Sprintf("%d direct Node.js deps, %d direct Python deps, copyleft:%d",
		len(nodeDeps), len(pyDeps), countCLeft)

	// expansions
	nodeHTML:= buildNodeTreesHTML(nodeDeps)
	pyHTML  := buildPythonTreesHTML(pyDeps)

	// final data
	data:= struct{
		Summary string
		Deps []FlatDep
		NodeHTML template.HTML
		PyHTML   template.HTML
	}{
		Summary: summary,
		Deps: allDeps,
		NodeHTML: template.HTML(nodeHTML),
		PyHTML: template.HTML(pyHTML),
	}

	// template
	tmpl, e:= template.New("report").Funcs(template.FuncMap{
		"isCopyleft": isCopyleft,
	}).Parse(reportTemplate)
	if e!=nil{
		log.Println("Template parse error:", e)
		os.Exit(1)
	}
	out, e2:= os.Create("dependency-license-report.html")
	if e2!=nil{
		log.Println("Create file error:", e2)
		os.Exit(1)
	}
	defer out.Close()

	if e3:= tmpl.Execute(out, data); e3!=nil{
		log.Println("Template exec error:", e3)
		os.Exit(1)
	}

	fmt.Println("dependency-license-report.html generated.")
	fmt.Println("- We store only 2 lines near 'license' (the line + next line).")
	fmt.Println("- We remove <tags> & limit to 200 chars. That should reduce irrelevant text.")
	fmt.Println("- Node & Python both do the same fallback approach.")
}
