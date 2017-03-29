package dotweb

import (
	"fmt"
	"github.com/devfeel/dotweb/framework/convert"
	"github.com/devfeel/dotweb/framework/exception"
	"github.com/devfeel/dotweb/framework/json"
	"github.com/devfeel/dotweb/framework/log"
	"github.com/devfeel/dotweb/session"
	"net/http"
	"strings"
	"sync"
	"time"

	"compress/gzip"
	"github.com/devfeel/dotweb/config"
	"github.com/devfeel/dotweb/routers"
	"golang.org/x/net/websocket"
	"io"
	"net/url"
)

const (
	DefaultGzipLevel = 9
	gzipScheme       = "gzip"
)

type (
	//HttpModule定义
	HttpModule struct {
		//响应请求时作为 HTTP 执行管线链中的第一个事件发生
		OnBeginRequest func(*HttpContext)
		//响应请求时作为 HTTP 执行管线链中的最后一个事件发生。
		OnEndRequest func(*HttpContext)
	}

	//HttpServer定义
	HttpServer struct {
		router         Router
		DotApp         *DotWeb
		sessionManager *session.SessionManager
		lock_session   *sync.RWMutex
		pool           *pool
		ServerConfig   *config.ServerConfig
		SessionConfig  *config.SessionConfig
		binder         Binder
		render         Renderer
		offline        bool
	}

	//pool定义
	pool struct {
		response sync.Pool
		context  sync.Pool
	}
)

// Handle is a function that can be registered to a route to handle HTTP
// requests. Like http.HandlerFunc, but has a third parameter for the values of
// wildcards (variables).
type HttpHandle func(*HttpContext)

func NewHttpServer() *HttpServer {
	server := &HttpServer{
		pool: &pool{
			response: sync.Pool{
				New: func() interface{} {
					return &Response{}
				},
			},
			context: sync.Pool{
				New: func() interface{} {
					return &HttpContext{}
				},
			},
		},
		ServerConfig:  config.NewServerConfig(),
		SessionConfig: config.NewSessionConfig(),
		lock_session:  new(sync.RWMutex),
		binder:        newBinder(),
	}
	//设置router
	server.router = NewRouter(server)
	return server
}

//ServeHTTP make sure request can be handled correctly
func (server *HttpServer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	//针对websocket与调试信息特殊处理
	if checkIsWebSocketRequest(req) {
		http.DefaultServeMux.ServeHTTP(w, req)
	} else {
		//设置header信息
		w.Header().Set(HeaderServer, DefaultServerName)
		//处理维护
		if server.IsOffline() {
			server.DotApp.OfflineServer.ServeHTTP(w, req)
		} else {
			server.Router().ServeHTTP(w, req)
		}
	}
}

//IsOffline check server is set offline state
func (server *HttpServer) IsOffline() bool {
	return server.offline
}

//SetOffline set server offline config
func (server *HttpServer) SetOffline(offline bool, offlineText string, offlineUrl string) {
	server.offline = offline
}

//set session store config
func (server *HttpServer) SetSessionConfig(storeConfig *session.StoreConfig) {
	//sync session config
	server.SessionConfig.Timeout = storeConfig.Maxlifetime
	server.SessionConfig.SessionMode = storeConfig.StoreName
	server.SessionConfig.ServerIP = storeConfig.ServerIP
}

//init session manager
func (server *HttpServer) InitSessionManager() {
	storeConfig := new(session.StoreConfig)
	storeConfig.Maxlifetime = server.SessionConfig.Timeout
	storeConfig.StoreName = server.SessionConfig.SessionMode
	storeConfig.ServerIP = server.SessionConfig.ServerIP

	if server.sessionManager == nil {
		//设置Session
		server.lock_session.Lock()
		if manager, err := session.NewDefaultSessionManager(storeConfig); err != nil {
			//panic error with create session manager
			panic(err.Error())
		} else {
			server.sessionManager = manager
		}
		server.lock_session.Unlock()
	}
}

/*
* 关联当前HttpServer实例对应的DotServer实例
 */
func (server *HttpServer) setDotApp(dotApp *DotWeb) {
	server.DotApp = dotApp
}

//get session manager in current httpserver
func (server *HttpServer) GetSessionManager() *session.SessionManager {
	if !server.SessionConfig.EnabledSession {
		return nil
	}
	return server.sessionManager
}

//get router interface in server
func (server *HttpServer) Router() Router {
	return server.router
}

//get binder interface in server
func (server *HttpServer) Binder() Binder {
	return server.binder
}

//get renderer interface in server
func (server *HttpServer) Renderer() Renderer {
	return server.render
}

//set custom renderer in server
func (server *HttpServer) SetRenderer(r Renderer) {
	server.render = r
}

//set EnabledAutoHEAD true or false
func (server *HttpServer) SetEnabledAutoHEAD(autoHEAD bool) {
	server.ServerConfig.EnabledAutoHEAD = autoHEAD
}

type LogJson struct {
	RequestUrl string
	HttpHeader string
	HttpBody   string
}

//wrap HttpHandle to httprouter.Handle
func (server *HttpServer) wrapRouterHandle(handle HttpHandle, isHijack bool) routers.Handle {
	return func(w http.ResponseWriter, r *http.Request, params routers.Params) {
		//get from pool
		res := server.pool.response.Get().(*Response)
		res.Reset(w)
		httpCtx := server.pool.context.Get().(*HttpContext)
		httpCtx.Reset(res, r, server, params)

		//gzip
		if server.ServerConfig.EnabledGzip {
			gw, err := gzip.NewWriterLevel(w, DefaultGzipLevel)
			if err != nil {
				panic("use gzip error -> " + err.Error())
			}
			grw := &gzipResponseWriter{Writer: gw, ResponseWriter: w}
			res.Reset(grw)
			httpCtx.SetHeader(HeaderContentEncoding, gzipScheme)
		}
		//增加状态计数
		GlobalState.AddRequestCount(1)

		//session
		//if exists client-sessionid, use it
		//if not exists client-sessionid, new one
		if server.SessionConfig.EnabledSession {
			sessionId, err := server.GetSessionManager().GetClientSessionID(r)
			if err == nil && sessionId != "" {
				httpCtx.SessionID = sessionId
			} else {
				httpCtx.SessionID = server.GetSessionManager().NewSessionID()
				cookie := http.Cookie{
					Name:  server.sessionManager.CookieName,
					Value: url.QueryEscape(httpCtx.SessionID),
					Path:  "/",
				}
				httpCtx.SetCookie(cookie)
			}

		}

		//hijack处理
		if isHijack {
			_, hijack_err := httpCtx.Hijack()
			if hijack_err != nil {
				//输出内容
				httpCtx.Response.WriteHeader(http.StatusInternalServerError)
				httpCtx.Response.Header().Set(HeaderContentType, CharsetUTF8)
				httpCtx.WriteString(hijack_err.Error())
				return
			}
		}

		startTime := time.Now()
		defer func() {
			var errmsg string
			if err := recover(); err != nil {
				errmsg = exception.CatchError("httpserver::RouterHandle", LogTarget_HttpServer, err)

				//默认异常处理
				if server.DotApp.ExceptionHandler != nil {
					server.DotApp.ExceptionHandler(httpCtx, err)
				}

				//记录访问日志
				headinfo := fmt.Sprintln(httpCtx.Response.Header)
				logJson := LogJson{
					RequestUrl: httpCtx.Request.RequestURI,
					HttpHeader: headinfo,
					HttpBody:   errmsg,
				}
				logString := jsonutil.GetJsonString(logJson)
				logger.Log(logString, LogTarget_HttpServer, LogLevel_Error)

				//增加错误计数
				GlobalState.AddErrorCount(1)
			}
			timetaken := int64(time.Now().Sub(startTime) / time.Millisecond)
			//HttpServer Logging
			logger.Log(httpCtx.Url()+" "+logContext(httpCtx, timetaken), LogTarget_HttpRequest, LogLevel_Debug)

			if server.ServerConfig.EnabledGzip {
				var w io.Writer
				w = res.Writer().(*gzipResponseWriter).Writer
				w.(*gzip.Writer).Close()
			}
			// Return to pool
			server.pool.response.Put(res)
			//release context
			httpCtx.release()
			server.pool.context.Put(httpCtx)
		}()

		//处理前置Module集合
		for _, module := range server.DotApp.Modules {
			if module.OnBeginRequest != nil {
				module.OnBeginRequest(httpCtx)
			}
		}

		//处理用户handle
		//if already set HttpContext.End,ignore user handler - fixed issue #5
		if !httpCtx.IsEnd() {
			handle(httpCtx)
		}

		//处理后置Module集合
		for _, module := range server.DotApp.Modules {
			if module.OnEndRequest != nil {
				module.OnEndRequest(httpCtx)
			}
		}

	}
}

//wrap fileHandler to httprouter.Handle
func (server *HttpServer) wrapFileHandle(fileHandler http.Handler) routers.Handle {
	return func(w http.ResponseWriter, r *http.Request, params routers.Params) {
		//增加状态计数
		GlobalState.AddRequestCount(1)
		startTime := time.Now()
		r.URL.Path = params.ByName("filepath")
		fileHandler.ServeHTTP(w, r)
		timetaken := int64(time.Now().Sub(startTime) / time.Millisecond)
		//HttpServer Logging
		logger.Log(r.URL.String()+" "+logRequest(r, timetaken), LogTarget_HttpRequest, LogLevel_Debug)
	}
}

//wrap HttpHandle to websocket.Handle
func (server *HttpServer) wrapWebSocketHandle(handle HttpHandle) websocket.Handler {
	return func(ws *websocket.Conn) {
		//get from pool
		httpCtx := server.pool.context.Get().(*HttpContext)
		httpCtx.Reset(nil, ws.Request(), server, nil)
		httpCtx.WebSocket = &WebSocket{
			Conn: ws,
		}
		httpCtx.IsWebSocket = true

		startTime := time.Now()
		defer func() {
			var errmsg string
			if err := recover(); err != nil {
				errmsg = exception.CatchError("httpserver::WebsocketHandle", LogTarget_HttpServer, err)

				//记录访问日志
				headinfo := fmt.Sprintln(httpCtx.WebSocket.Conn.Request().Header)
				logJson := LogJson{
					RequestUrl: httpCtx.WebSocket.Conn.Request().RequestURI,
					HttpHeader: headinfo,
					HttpBody:   errmsg,
				}
				logString := jsonutil.GetJsonString(logJson)
				logger.Log(logString, LogTarget_HttpServer, LogLevel_Error)

				//增加错误计数
				GlobalState.AddErrorCount(1)
			}
			timetaken := int64(time.Now().Sub(startTime) / time.Millisecond)
			//HttpServer Logging
			logger.Log(httpCtx.Url()+" "+logContext(httpCtx, timetaken), LogTarget_HttpRequest, LogLevel_Debug)

			// Return to pool
			server.pool.context.Put(httpCtx)
		}()

		handle(httpCtx)

		//增加状态计数
		GlobalState.AddRequestCount(1)
	}
}

//get default log string
func logContext(ctx *HttpContext, timetaken int64) string {
	var reqbytelen, resbytelen, method, proto, status, userip string
	if ctx != nil {
		if !ctx.IsWebSocket {
			reqbytelen = convert.Int642String(ctx.Request.ContentLength)
			resbytelen = convert.Int642String(ctx.Response.Size)
			method = ctx.Request.Method
			proto = ctx.Request.Proto
			status = convert.Int2String(ctx.Response.Status)
			userip = ctx.RemoteIP()
		} else {
			reqbytelen = convert.Int642String(ctx.Request.ContentLength)
			resbytelen = "0"
			method = ctx.Request.Method
			proto = ctx.Request.Proto
			status = "0"
			userip = ctx.RemoteIP()
		}
	}

	log := method + " "
	log += userip + " "
	log += proto + " "
	log += status + " "
	log += reqbytelen + " "
	log += resbytelen + " "
	log += convert.Int642String(timetaken)

	return log
}

func logRequest(req *http.Request, timetaken int64) string {
	var reqbytelen, resbytelen, method, proto, status, userip string
	reqbytelen = convert.Int642String(req.ContentLength)
	resbytelen = ""
	method = req.Method
	proto = req.Proto
	status = "200"
	userip = req.RemoteAddr

	log := method + " "
	log += userip + " "
	log += proto + " "
	log += status + " "
	log += reqbytelen + " "
	log += resbytelen + " "
	log += convert.Int642String(timetaken)

	return log
}

//check request is the websocket request
func checkIsWebSocketRequest(req *http.Request) bool {
	if req.Header.Get("Connection") == "Upgrade" {
		return true
	}
	return false
}

//check request is startwith /debug/
func checkIsDebugRequest(req *http.Request) bool {
	if strings.Index(req.RequestURI, "/debug/") == 0 {
		return true
	}
	return false
}
