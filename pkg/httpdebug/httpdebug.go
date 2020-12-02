package httpdebug

import (
	"fmt"
	"html/template"
	"net/http"
	"toy-rpc/pkg"
	"toy-rpc/pkg/service"
)

const debugText = `<html>
	<body>
	<title>RPC Services</title>
	{{range .}}
	<hr>
	Service {{.Name}}
	<hr>
		<table>
		<th align=center>Method</th><th align=center>Calls</th>
		{{range $name, $mtype := .Method}}
			<tr>
			<td align=left font=fixed>{{$name}}({{$mtype.ArgType}}, {{$mtype.ReplyType}}) error</td>
			<td align=center>{{$mtype.NumCalls}}</td>
			</tr>
		{{end}}
		</table>
	{{end}}
	</body>
	</html>`

var debug = template.Must(template.New("RPC debug").Parse(debugText))

type DebugHTTP struct {
	*pkg.Server
}

type debugService struct {
	Name   string
	Method map[string]*service.MethodType
}

func (server DebugHTTP) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	var services []debugService
	server.ServiceMap.Range(func(namei, svci interface{}) bool {
		svc := svci.(*service.Service)
		services = append(services, debugService{
			Name:   namei.(string),
			Method: svc.Method,
		})
		return true
	})
	err := debug.Execute(w, services)
	if err != nil {
		_, _ = fmt.Fprintln(w, "rpc: error executing template:", err.Error())
	}
}
