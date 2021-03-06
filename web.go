package web

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	l4g "github.com/steelseries/log4go"
)

type ResponseWriter interface {
	Header() http.Header
	WriteHeader(status int)
	Write(data []byte) (n int, err error)
	Close()
}

type Context struct {
	Request *http.Request
	Params  map[string]string
	Server  *Server
	ResponseWriter
}

func (ctx *Context) WriteString(content string) {
	ctx.ResponseWriter.Write([]byte(content))
}

func (ctx *Context) Abort(status int, body string) {
	ctx.ResponseWriter.WriteHeader(status)
	ctx.ResponseWriter.Write([]byte(body))
}

func (ctx *Context) Redirect(status int, url_ string) {
	ctx.ResponseWriter.Header().Set("Location", url_)
	ctx.ResponseWriter.WriteHeader(status)
	ctx.ResponseWriter.Write([]byte("Redirecting to: " + url_))
}

func (ctx *Context) NotModified() {
	ctx.ResponseWriter.WriteHeader(304)
}

func (ctx *Context) NotFound(message string) {
	ctx.ResponseWriter.WriteHeader(404)
	ctx.ResponseWriter.Write([]byte(message))
}

//Sets the content type by extension, as defined in the mime package.
//For example, ctx.ContentType("json") sets the content-type to "application/json"
func (ctx *Context) ContentType(ext string) {
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	ctype := mime.TypeByExtension(ext)
	if ctype != "" {
		ctx.Header().Set("Content-Type", ctype)
	}
}

func (ctx *Context) SetHeader(hdr string, val string, unique bool) {
	if unique {
		ctx.Header().Set(hdr, val)
	} else {
		ctx.Header().Add(hdr, val)
	}
}

//Sets a cookie -- duration is the amount of time in seconds. 0 = forever
func (ctx *Context) SetCookie(name string, value string, age int64) {
	var utctime time.Time
	if age == 0 {
		// 2^31 - 1 seconds (roughly 2038)
		utctime = time.Unix(2147483647, 0)
	} else {
		utctime = time.Unix(time.Now().Unix()+age, 0)
	}
	cookie := fmt.Sprintf("%s=%s; expires=%s", name, value, webTime(utctime))
	ctx.SetHeader("Set-Cookie", cookie, false)
}

func getCookieSig(key string, val []byte, timestamp string) string {
	hm := hmac.New(sha1.New, []byte(key))

	hm.Write(val)
	hm.Write([]byte(timestamp))

	hex := fmt.Sprintf("%02x", hm.Sum(nil))
	return hex
}

func (ctx *Context) SetSecureCookie(name string, val string, age int64) {
	//base64 encode the val
	if len(ctx.Server.Config.CookieSecret) == 0 {
		l4g.Debug("Secret Key for secure cookies has not been set. Please assign a cookie secret to web.Config.CookieSecret.\r\n")
		return
	}
	var buf bytes.Buffer
	encoder := base64.NewEncoder(base64.StdEncoding, &buf)
	encoder.Write([]byte(val))
	encoder.Close()
	vs := buf.String()
	vb := buf.Bytes()
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	sig := getCookieSig(ctx.Server.Config.CookieSecret, vb, timestamp)
	cookie := strings.Join([]string{vs, timestamp, sig}, "|")
	ctx.SetCookie(name, cookie, age)
}

func (ctx *Context) GetSecureCookie(name string) (string, bool) {
	for _, cookie := range ctx.Request.Cookies() {
		if cookie.Name != name {
			continue
		}

		parts := strings.SplitN(cookie.Value, "|", 3)

		val := parts[0]
		timestamp := parts[1]
		sig := parts[2]

		if getCookieSig(ctx.Server.Config.CookieSecret, []byte(val), timestamp) != sig {
			return "", false
		}

		ts, _ := strconv.ParseInt(timestamp, 0, 64)

		if time.Now().Unix()-31*86400 > ts {
			return "", false
		}

		buf := bytes.NewBufferString(val)
		encoder := base64.NewDecoder(base64.StdEncoding, buf)

		res, _ := ioutil.ReadAll(encoder)
		return string(res), true
	}
	return "", false
}

// small optimization: cache the context type instead of repeteadly calling reflect.Typeof
var contextType reflect.Type

var exeFile string

// default
func defaultStaticDir() string {
	root, _ := path.Split(exeFile)
	return path.Join(root, "static")
}

func init() {
	contextType = reflect.TypeOf(Context{})
	//find the location of the exe file
	arg0 := path.Clean(os.Args[0])
	wd, _ := os.Getwd()
	if strings.HasPrefix(arg0, "/") {
		exeFile = arg0
	} else {
		//TODO for robustness, search each directory in $PATH
		exeFile = path.Join(wd, arg0)
	}
}

type route struct {
	r       string
	cr      *regexp.Regexp
	method  string
	handler reflect.Value
}

func (s *Server) addRoute(r string, method string, handler interface{}) {
	cr, err := regexp.Compile(r)
	if err != nil {
		l4g.Error("Error in route regex %q\r\n", r)
		return
	}

	if fv, ok := handler.(reflect.Value); ok {
		s.routes = append(s.routes, route{r, cr, method, fv})
	} else {
		fv := reflect.ValueOf(handler)
		s.routes = append(s.routes, route{r, cr, method, fv})
	}
}

type handlerFunction struct {
	r              string
	cr             *regexp.Regexp
	handler        http.Handler
	reflectHandler reflect.Value
}

func (s *Server) addHandlerFunction(r string, handler http.Handler) {
	cr, err := regexp.Compile(r)
	if err != nil {
		l4g.Error("Error in handler function regex %q\r\n", r)
		return
	}

	fv := reflect.ValueOf(handler)

	s.handlerFunctions = append(s.handlerFunctions, handlerFunction{r: r, cr: cr, handler: handler, reflectHandler: fv})
}

type responseWriter struct {
	http.ResponseWriter
}

func (c *responseWriter) Close() {
	rwc, buf, _ := c.ResponseWriter.(http.Hijacker).Hijack()
	if buf != nil {
		buf.Flush()
	}

	if rwc != nil {
		rwc.Close()
	}
}

func (s *Server) ServeHTTP(c http.ResponseWriter, req *http.Request) {
	w := responseWriter{c}
	s.routeHandler(req, &w)
}

//Calls a function with recover block
func (s *Server) safelyCall(function reflect.Value, args []reflect.Value) (resp []reflect.Value, e interface{}) {
	defer func() {
		if err := recover(); err != nil {
			if !s.Config.RecoverPanic {
				// go back to panic
				panic(err)
			} else {
				e = err
				resp = nil
				l4g.Error("Handler crashed with error: %v\r\n", err)
				for i := 1; ; i += 1 {
					_, file, line, ok := runtime.Caller(i)
					if !ok {
						break
					}
					l4g.Debug("%s:%d\r\n", file, line)
				}
			}
		}
	}()
	return function.Call(args), nil
}

//should the context be passed to the handler?
func requiresContext(handlerType reflect.Type) bool {
	//if the method doesn't take arguments, no
	if handlerType.NumIn() == 0 {
		return false
	}

	//if the first argument is not a pointer, no
	a0 := handlerType.In(0)
	if a0.Kind() != reflect.Ptr {
		return false
	}
	//if the first argument is a context, yes
	if a0.Elem() == contextType {
		return true
	}

	return false
}

// Iterates through all handler functions and routes defined, attempting to serve content
func (s *Server) routeHandler(req *http.Request, w ResponseWriter) {
	requestPath := req.URL.Path
	ctx := Context{req, map[string]string{}, s, w}

	//log the request
	var logEntry bytes.Buffer
	fmt.Fprintf(&logEntry, "\033[32;1m%s %s\033[0m", req.Method, requestPath)

	//ignore errors from ParseForm because it's usually harmless.
	req.ParseForm()
	if len(req.Form) > 0 {
		for k, v := range req.Form {
			ctx.Params[k] = v[0]
		}
		fmt.Fprintf(&logEntry, "\n\033[37;1mParams: %v\033[0m\n", ctx.Params)
	}

	l4g.Debug("%s\r\n", logEntry.String())

	//set some default headers
	ctx.SetHeader("Server", "web.go", true)
	tm := time.Now().UTC()
	ctx.SetHeader("Date", webTime(tm), true)

	//try to serve a static file
	staticDir := s.Config.StaticDir
	if staticDir == "" {
		staticDir = defaultStaticDir()
	}
	staticFile := path.Join(staticDir, requestPath)
	if fileExists(staticFile) && (req.Method == "GET" || req.Method == "HEAD") {
		http.ServeFile(&ctx, req, staticFile)
		return
	}

	//Set the default content-type
	ctx.SetHeader("Content-Type", "text/html; charset=utf-8", true)

	// Iterate defined handler functions
	for i := 0; i < len(s.handlerFunctions); i++ {
		handlerFunction := s.handlerFunctions[i]
		cr := handlerFunction.cr

		if !cr.MatchString(requestPath) {
			continue
		}
		match := cr.FindStringSubmatch(requestPath)

		if len(match[0]) != len(requestPath) {
			continue
		}

		var args []reflect.Value
		args = append(args, reflect.ValueOf(w).Elem().Field(0)) // First argument to ServeHTTP should be the http.ResponseWriter from the ResponseWriter object passed in
		args = append(args, reflect.ValueOf(req))               // Second argument to ServeHTTP should be the request object passed in

		ret, err := s.safelyCall(handlerFunction.reflectHandler.MethodByName("ServeHTTP"), args)
		if err != nil {
			//l4g.Debug("there was an error or panic while calling the handler\r\n");
			ctx.Abort(500, "Server Error")
		}
		if len(ret) == 0 {
			return
		}

		sval := ret[0]

		var content []byte

		if sval.Kind() == reflect.String {
			content = []byte(sval.String())
		} else if sval.Kind() == reflect.Slice && sval.Type().Elem().Kind() == reflect.Uint8 {
			content = sval.Interface().([]byte)
		}
		ctx.SetHeader("Content-Length", strconv.Itoa(len(content)), true)

		if len(ret) > 1 {
			// Read status code if provided by handler
			scval := ret[1]
			if scval.Kind() == reflect.Int {
				statusCode := int(scval.Int())
				if statusCode != 200 {
					ctx.Abort(statusCode, string(content))
					return
				}
			}
		}

		ctx.Write(content)
		return
	}

	// Iterate defined routes
	for i := 0; i < len(s.routes); i++ {
		route := s.routes[i]
		cr := route.cr
		//if the methods don't match, skip this handler (except HEAD can be used in place of GET)
		if req.Method != route.method && !(req.Method == "HEAD" && route.method == "GET") {
			continue
		}

		if !cr.MatchString(requestPath) {
			continue
		}
		match := cr.FindStringSubmatch(requestPath)

		if len(match[0]) != len(requestPath) {
			continue
		}

		var args []reflect.Value
		handlerType := route.handler.Type()
		if requiresContext(handlerType) {
			args = append(args, reflect.ValueOf(&ctx))
		}
		for _, arg := range match[1:] {
			args = append(args, reflect.ValueOf(arg))
		}

		ret, err := s.safelyCall(route.handler, args)
		if err != nil {
			//there was an error or panic while calling the handler
			ctx.Abort(500, "Server Error")
		}
		if len(ret) == 0 {
			return
		}

		sval := ret[0]

		var content []byte

		if sval.Kind() == reflect.String {
			content = []byte(sval.String())
		} else if sval.Kind() == reflect.Slice && sval.Type().Elem().Kind() == reflect.Uint8 {
			content = sval.Interface().([]byte)
		}
		ctx.SetHeader("Content-Length", strconv.Itoa(len(content)), true)

		if len(ret) > 1 {
			// Read status code if provided by handler
			scval := ret[1]
			if scval.Kind() == reflect.Int {
				statusCode := int(scval.Int())
				if statusCode != 200 {
					ctx.Abort(statusCode, string(content))
					return
				}
			}
		}

		ctx.Write(content)
		return
	}

	//try to serve index.html || index.htm
	if indexPath := path.Join(path.Join(staticDir, requestPath), "index.html"); fileExists(indexPath) {
		http.ServeFile(&ctx, ctx.Request, indexPath)
		return
	}

	if indexPath := path.Join(path.Join(staticDir, requestPath), "index.htm"); fileExists(indexPath) {
		http.ServeFile(&ctx, ctx.Request, indexPath)
		return
	}

	ctx.Abort(404, "Page not found")
}

var Config = &ServerConfig{
	RecoverPanic: true,
}

var mainServer = NewServer()

type Server struct {
	Config           *ServerConfig
	routes           []route
	handlerFunctions []handlerFunction
	Env              map[string]interface{}
	//save the listener so it can be closed
	l net.Listener
}

func NewServer() *Server {
	return &Server{
		Config: Config,
		Env:    map[string]interface{}{},
	}
}

func (s *Server) initServer() {
	if s.Config == nil {
		s.Config = &ServerConfig{}
	}
}

func (s *Server) runHelper() (mux *http.ServeMux) {
	s.initServer()

	mux = http.NewServeMux()
	mux.Handle("/", s)

	return
}

//Runs the web application and serves http requests
func (s *Server) Run(addr string) {
	mux := s.runHelper()

	l4g.Debug("web.go serving %s\r\n", addr)

	l, err := net.Listen("tcp", addr)
	if err != nil {
		l4g.Error("ListenAndServe: %v\r\n", err)
		return
	}
	s.l = l
	err = http.Serve(s.l, mux)
	s.l.Close()
}

func (s *Server) RunTLS(addr string, tlsConf *tls.Config) {
	mux := s.runHelper()

	l4g.Debug("web.go serving %s\r\n", addr)

	l, err := tls.Listen("tcp", addr, tlsConf)
	if err != nil {
		l4g.Error("ListenAndServe: %v\r\n", err)
		return
	}
	s.l = l
	err = http.Serve(s.l, mux)
	s.l.Close()
}

//Runs the web application and serves http requests
func Run(addr string) {
	mainServer.Run(addr)
}

func RunTLS(addr string, tlsConf *tls.Config) {
	mainServer.RunTLS(addr, tlsConf)
}

//Stops the web server
func (s *Server) Close() {
	if s.l != nil {
		s.l.Close()
	}
}

//Stops the web server
func Close() {
	mainServer.Close()
}

func (s *Server) RunScgi(addr string) {
	s.initServer()
	l4g.Debug("web.go serving scgi %s\r\n", addr)
	s.listenAndServeScgi(addr)
}

//Runs the web application and serves scgi requests
func RunScgi(addr string) {
	mainServer.RunScgi(addr)
}

//Runs the web application and serves scgi requests for this Server object.
func (s *Server) RunFcgi(addr string) {
	s.initServer()
	l4g.Debug("web.go serving fcgi %s\r\n", addr)
	s.listenAndServeFcgi(addr)
}

//Runs the web application by serving fastcgi requests
func RunFcgi(addr string) {
	mainServer.RunFcgi(addr)
}

//Adds a handler for the 'GET' http method.
func (s *Server) Get(route string, handler interface{}) {
	s.addRoute(route, "GET", handler)
}

//Adds a handler for the 'POST' http method.
func (s *Server) Post(route string, handler interface{}) {
	s.addRoute(route, "POST", handler)
}

//Adds a handler for the 'PUT' http method.
func (s *Server) Put(route string, handler interface{}) {
	s.addRoute(route, "PUT", handler)
}

//Adds a handler for the 'OPTIONS' http method.
func (s *Server) Options(route string, handler interface{}) {
	s.addRoute(route, "OPTIONS", handler)
}

//Adds a handler for the 'DELETE' http method.
func (s *Server) Delete(route string, handler interface{}) {
	s.addRoute(route, "DELETE", handler)
}

//Adds a handler for the 'GET' http method.
func Get(route string, handler interface{}) {
	mainServer.Get(route, handler)
}

//Adds a handler for the 'POST' http method.
func Post(route string, handler interface{}) {
	mainServer.addRoute(route, "POST", handler)
}

//Adds a handler for the 'PUT' http method.
func Put(route string, handler interface{}) {
	mainServer.addRoute(route, "PUT", handler)
}

//Adds a handler for the 'OPTIONS' http method.
func Options(route string, handler interface{}) {
	mainServer.addRoute(route, "OPTIONS", handler)
}

//Adds a handler for the 'DELETE' http method.
func Delete(route string, handler interface{}) {
	mainServer.addRoute(route, "DELETE", handler)
}

//Adds a generic handler function
func HandlerFunc(route string, handler http.Handler) {
	mainServer.addHandlerFunction(route, handler)
}

type ServerConfig struct {
	StaticDir    string
	Addr         string
	Port         int
	CookieSecret string
	RecoverPanic bool
}

func webTime(t time.Time) string {
	ftime := t.Format(time.RFC1123)
	if strings.HasSuffix(ftime, "UTC") {
		ftime = ftime[0:len(ftime)-3] + "GMT"
	}
	return ftime
}

func dirExists(dir string) bool {
	d, e := os.Stat(dir)
	switch {
	case e != nil:
		return false
	case !d.IsDir():
		return false
	}

	return true
}

func fileExists(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil {
		return false
	}

	return !info.IsDir()
}

func Urlencode(data map[string]string) string {
	var buf bytes.Buffer
	for k, v := range data {
		buf.WriteString(url.QueryEscape(k))
		buf.WriteByte('=')
		buf.WriteString(url.QueryEscape(v))
		buf.WriteByte('&')
	}
	s := buf.String()
	return s[0 : len(s)-1]
}
