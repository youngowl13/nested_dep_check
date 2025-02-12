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

// -------------------- 1) Copyleft detection --------------------
func isCopyleft(lic string) bool {
    copyleftLicenses := []string{
        "GPL","GNU GENERAL PUBLIC LICENSE","LGPL","GNU LESSER GENERAL PUBLIC LICENSE",
        "AGPL","GNU AFFERO GENERAL PUBLIC LICENSE","MPL","MOZILLA PUBLIC LICENSE",
        "CC-BY-SA","CREATIVE COMMONS ATTRIBUTION-SHAREALIKE","EPL","ECLIPSE PUBLIC LICENSE",
        "OFL","OPEN FONT LICENSE","CPL","COMMON PUBLIC LICENSE","OSL","OPEN SOFTWARE LICENSE",
    }
    up := strings.ToUpper(lic)
    for _, kw := range copyleftLicenses {
        if strings.Contains(up, kw) {
            return true
        }
    }
    return false
}

// -------------------- 2) findFile + removeCaretTilde --------------------

// findFile searches recursively for 'target'
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

func removeCaretTilde(ver string) string {
    ver = strings.TrimSpace(ver)
    return strings.TrimLeft(ver, "^~")
}

// -------------------- 3) Multi-line fallback logic for Node & Python  --------------------

// parseLicenseLine tries to see if line has a known license substring
func parseLicenseLine(line string) string {
    // You can expand this list if needed
    known := []string{
        "MIT","ISC","BSD","APACHE","ARTISTIC","ZLIB","WTFPL","CDDL","UNLICENSE",
        "EUPL","MPL","CC0","LGPL","AGPL","BSD-2-CLAUSE","BSD-3-CLAUSE","X11",
    }
    up := strings.ToUpper(line)
    for _, kw := range known {
        if strings.Contains(up, kw) {
            return kw
        }
    }
    return ""
}

// fallbackNpmLicenseMultiLine fetches npmjs.com/package/<pkgName>,
// scans line by line for "license", then up to 10 lines after for known keywords.
func fallbackNpmLicenseMultiLine(pkgName string) string {
    url := "https://www.npmjs.com/package/" + pkgName
    resp, err := http.Get(url)
    if err != nil || resp.StatusCode != 200 {
        return ""
    }
    defer resp.Body.Close()

    var lines []string
    scanner := bufio.NewScanner(resp.Body)
    for scanner.Scan() {
        lines = append(lines, scanner.Text())
    }
    if scanner.Err() != nil {
        return ""
    }
    for i := 0; i < len(lines); i++ {
        lowerLine := strings.ToLower(lines[i])
        if strings.Contains(lowerLine, "license") {
            // same line
            lic := parseLicenseLine(lines[i])
            if lic != "" {
                return lic
            }
            // next 10 lines
            for j := i+1; j < len(lines) && j <= i+10; j++ {
                lic2 := parseLicenseLine(lines[j])
                if lic2 != "" {
                    return lic2
                }
            }
        }
    }
    return ""
}

// fallbackPyPiLicenseMultiLine does the same for pypi.org/project/<pkgName>
func fallbackPyPiLicenseMultiLine(pkgName string) string {
    url := "https://pypi.org/project/" + pkgName
    resp, err := http.Get(url)
    if err != nil || resp.StatusCode != 200 {
        return ""
    }
    defer resp.Body.Close()

    var lines []string
    scanner := bufio.NewScanner(resp.Body)
    for scanner.Scan() {
        lines = append(lines, scanner.Text())
    }
    if scanner.Err() != nil {
        return ""
    }
    for i := 0; i < len(lines); i++ {
        lowerLine := strings.ToLower(lines[i])
        if strings.Contains(lowerLine, "license") {
            lic := parseLicenseLine(lines[i])
            if lic != "" {
                return lic
            }
            for j := i+1; j < len(lines) && j <= i+10; j++ {
                lic2 := parseLicenseLine(lines[j])
                if lic2 != "" {
                    return lic2
                }
            }
        }
    }
    return ""
}

// -------------------- 4) Node Logic --------------------

type NodeDependency struct {
    Name string
    Version string
    License string
    Details string
    Copyleft bool
    Transitive []*NodeDependency
    Language string
}

func parseNodeDependencies(path string) ([]*NodeDependency, error) {
    raw, err := os.ReadFile(path)
    if err!=nil{return nil,err}
    var pkg map[string]interface{}
    if e:=json.Unmarshal(raw,&pkg); e!=nil{
        return nil,e
    }
    deps, _ := pkg["dependencies"].(map[string]interface{})
    if deps==nil{
        return nil, fmt.Errorf("no dependencies found in package.json")
    }
    visited := map[string]bool{}
    var results []*NodeDependency
    for nm, ver := range deps {
        str,_ := ver.(string)
        nd,e:= resolveNodeDependency(nm, removeCaretTilde(str), visited)
        if e==nil && nd!=nil{
            results=append(results, nd)
        }
    }
    return results,nil
}

func resolveNodeDependency(pkgName, version string, visited map[string]bool)(*NodeDependency,error){
    key:= pkgName+"@"+version
    if visited[key]{return nil,nil}
    visited[key]=true

    regURL:= "https://registry.npmjs.org/"+pkgName
    resp,err:= http.Get(regURL)
    if err!=nil{return nil,err}
    defer resp.Body.Close()

    var data map[string]interface{}
    if e:= json.NewDecoder(resp.Body).Decode(&data); e!=nil{
        return nil,e
    }
    if version=="" {
        if dist,ok:= data["dist-tags"].(map[string]interface{}); ok {
            if lat,ok:= dist["latest"].(string);ok{
                version=lat
            }
        }
    }
    lic:="Unknown"
    var trans []*NodeDependency

    if vs, ok:= data["versions"].(map[string]interface{}); ok{
        if verData, ok:= vs[version].(map[string]interface{}); ok {
            lic = findNpmLicense(verData)
            if deps,ok:= verData["dependencies"].(map[string]interface{}); ok {
                for subName, subVer := range deps {
                    sv,_:= subVer.(string)
                    ch,e2:= resolveNodeDependency(subName, removeCaretTilde(sv), visited)
                    if e2==nil && ch!=nil{
                        trans=append(trans,ch)
                    }
                }
            }
        }
    }
    if lic=="Unknown" {
        fb:= fallbackNpmLicenseMultiLine(pkgName)
        if fb!="" {
            lic= fb
        }
    }
    return &NodeDependency{
        Name: pkgName,
        Version: version,
        License: lic,
        Details: "https://www.npmjs.com/package/"+pkgName,
        Copyleft: isCopyleft(lic),
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

// -------------------- 5) Python Logic with the same fallback --------------------

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
    f,err:= os.Open(path)
    if err!=nil{return nil,err}
    defer f.Close()

    reqs, err:= parseRequirements(f)
    if err!=nil{return nil,err}
    var results []*PythonDependency
    var wg sync.WaitGroup
    depChan:= make(chan PythonDependency)
    errChan:= make(chan error)

    for _, r:= range reqs {
        wg.Add(1)
        go func(nm,vr string){
            defer wg.Done()
            d,e:= resolvePythonDependency(nm,vr,map[string]bool{})
            if e!=nil{errChan<-e;return}
            if d!=nil{depChan<-*d}
        }(r.name, r.version)
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

func resolvePythonDependency(pkgName,version string, visited map[string]bool)(*PythonDependency,error){
    key:= pkgName+"@"+version
    if visited[key]{return nil,nil}
    visited[key]= true

    pyURL:= "https://pypi.org/pypi/"+pkgName+"/json"
    resp,err:= http.Get(pyURL)
    if err!=nil{return nil,err}
    defer resp.Body.Close()

    if resp.StatusCode!=200{
        return nil,fmt.Errorf("PyPI status:%d",resp.StatusCode)
    }
    var data map[string]interface{}
    if e:= json.NewDecoder(resp.Body).Decode(&data); e!=nil{
        return nil,e
    }
    info,_:= data["info"].(map[string]interface{})
    if info==nil{
        return nil, fmt.Errorf("info missing for %s", pkgName)
    }
    if version==""{
        if v2,ok:= info["version"].(string); ok {
            version=v2
        }
    }
    lic:="Unknown"
    if l,ok:= info["license"].(string); ok && l!="" {
        lic= l
    }
    // fallback if unknown
    if lic=="Unknown" {
        fb:= fallbackPyPiLicenseMultiLine(pkgName)
        if fb!=""{
            lic= fb
        }
    }

    return &PythonDependency{
        Name: pkgName,
        Version: version,
        License: lic,
        Details: "https://pypi.org/pypi/"+pkgName+"/json",
        Copyleft: isCopyleft(lic),
        Language:"python",
    },nil
}

// parseRequirements: "package==1.2.3" or "package>=1.2.3"
type requirement struct{name, version string}
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
            name:strings.TrimSpace(p[0]),
            version:strings.TrimSpace(p[1]),
        })
    }
    return out,nil
}

// -------------------- 6) Flatten + HTML expansions for table + tree --------------------

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
        out = append(out, FlatDep{
            Name:nd.Name,Version: nd.Version,License: nd.License,
            Details: nd.Details,Language: nd.Language,Parent: parent,
        })
        if len(nd.Transitive)>0 {
            out = append(out, flattenNodeAll(nd.Transitive, nd.Name)...)
        }
    }
    return out
}

// Python flatten
func flattenPyAll(pds []*PythonDependency, parent string) []FlatDep {
    var out []FlatDep
    for _, pd := range pds {
        out = append(out, FlatDep{
            Name:pd.Name,Version: pd.Version,License: pd.License,
            Details:pd.Details,Language:pd.Language,Parent: parent,
        })
        if len(pd.Transitive)>0{
            out = append(out, flattenPyAll(pd.Transitive, pd.Name)...)
        }
    }
    return out
}

func buildNodeTreeHTML(nd *NodeDependency) string {
    summary := fmt.Sprintf("%s@%s (License: %s)", nd.Name, nd.Version, nd.License)
    var sb strings.Builder
    sb.WriteString("<details>\n<summary>")
    sb.WriteString(template.HTMLEscapeString(summary))
    sb.WriteString("</summary>\n")

    if len(nd.Transitive)>0 {
        sb.WriteString("<ul>\n")
        for _,ch:= range nd.Transitive {
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
    if len(nodes)==0 {
        return "<p>No Node dependencies found.</p>"
    }
    var sb strings.Builder
    for _, nd := range nodes {
        sb.WriteString(buildNodeTreeHTML(nd))
    }
    return sb.String()
}

func buildPythonTreeHTML(pd *PythonDependency) string {
    summary := fmt.Sprintf("%s@%s (License: %s)", pd.Name, pd.Version, pd.License)
    var sb strings.Builder
    sb.WriteString("<details>\n<summary>")
    sb.WriteString(template.HTMLEscapeString(summary))
    sb.WriteString("</summary>\n")

    if len(pd.Transitive)>0 {
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
    for _, pd := range py {
        sb.WriteString(buildPythonTreeHTML(pd))
    }
    return sb.String()
}

// -------------------- 7) The HTML Template --------------------
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
  <th>Name</th>
  <th>Version</th>
  <th>License</th>
  <th>Parent</th>
  <th>Language</th>
  <th>Details</th>
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

// -------------------- 8) main: build report --------------------
func main(){
    // Node parse
    nf := findFile(".", "package.json")
    var nodeDeps []*NodeDependency
    if nf != "" {
        nd,err := parseNodeDependencies(nf)
        if err==nil{
            nodeDeps= nd
        } else {
            log.Println("Node parse error:", err)
        }
    }

    // Python parse
    pf := findFile(".", "requirements.txt")
    if pf==""{
        pf= findFile(".", "requirement.txt")
    }
    var pyDeps []*PythonDependency
    if pf != "" {
        pd,err := parsePythonDependencies(pf)
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

    // Count copyleft
    copyleftCount:=0
    for _,dep:= range allDeps {
        if isCopyleft(dep.License){
            copyleftCount++
        }
    }
    summary:= fmt.Sprintf("%d direct Node.js deps, %d direct Python deps, copyleft:%d",
        len(nodeDeps), len(pyDeps), copyleftCount)

    // Build expansions
    nodeHTML := buildNodeTreesHTML(nodeDeps)
    pyHTML   := buildPythonTreesHTML(pyDeps)

    // Template data
    data := struct{
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

    // Render
    tmpl, e := template.New("report").Funcs(template.FuncMap{
        "isCopyleft": isCopyleft,
    }).Parse(reportTemplate)
    if e!=nil{
        log.Println("Template parse error:", e)
        os.Exit(1)
    }
    out, e2 := os.Create("dependency-license-report.html")
    if e2!=nil{
        log.Println("Create file error:", e2)
        os.Exit(1)
    }
    defer out.Close()

    if e3:= tmpl.Execute(out, data); e3!=nil{
        log.Println("Template exec error:", e3)
        os.Exit(1)
    }

    fmt.Println("dependency-license-report.html generated!")
    fmt.Println("- Node & Python both do multi-line fallback if license=Unknown, scanning up to 10 lines after 'license'.")
    fmt.Println("- parseLicenseLine includes MIT,ISC,BSD,Apache, etc. Add more if needed.")
    fmt.Println("- Copyleft detection still checks for GPL, AGPL, MPL, etc.")
}
