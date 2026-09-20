// Package web 是 gmt 的 HTTP 层：路由、鉴权、模板渲染。
//
// 这里只做三件事——把请求交给正确的模块、挡住没权限的人、把数据渲染成页面。
// 任何业务判断都不在这一层，否则又会退化成「什么都写在 controller 里」。
package web

import (
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"gmt/internal/auth"
	"gmt/internal/conf"
	"gmt/internal/gameclient"
	"gmt/internal/modules"
	"gmt/internal/resp"
	"gmt/internal/store"
)

// Server 聚合了 HTTP 层需要的全部依赖。
type Server struct {
	conf  *conf.Config
	store *store.Client
	game  gameclient.Client
	tmpl  *template.Template
	loc   *time.Location
}

// New 构造 HTTP 层。
func New(cfg *conf.Config, s *store.Client, g gameclient.Client) (*Server, error) {
	loc, err := time.LoadLocation(cfg.Site.Timezone)
	if err != nil {
		loc = time.Local
	}
	tmpl, err := loadTemplates("web/views")
	if err != nil {
		return nil, err
	}
	return &Server{conf: cfg, store: s, game: g, tmpl: tmpl, loc: loc}, nil
}

// loadTemplates 递归加载模板。
//
// 约定：文件名即模板名（用 / 分隔），另外任何文件里都可以用 {{define "xxx"}}
// 定义公共片段（head/sidebar/footer）。这样新增页面只要丢一个 html 进去。
func loadTemplates(dir string) (*template.Template, error) {
	t := template.New("gmt")
	funcs := template.FuncMap{
		"fmtTime": func(unix int64) string {
			if unix <= 0 {
				return "-"
			}
			return time.Unix(unix, 0).Format("2006-01-02 15:04:05")
		},
		"hasPrefix": strings.HasPrefix,
	}
	t = t.Funcs(funcs)
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".html") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(filepath.ToSlash(path), dir+"/")
		if _, err := t.New(name).Parse(string(b)); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// Engine 装配路由。
func (s *Server) Engine() *gin.Engine {
	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())
	r.Static("/static", "web/static")
	r.Use(auth.Middleware(s.store, s.conf.Server.Secret))

	// 免登录区（验证码图片必须在登录前能取到）
	r.GET("/login", s.pageLogin)
	r.GET("/api/captcha", s.apiCaptcha)
	r.POST("/api/login", s.apiLogin)

	need := r.Group("")
	need.Use(s.requireLogin)

	need.POST("/api/logout", s.apiLogout)

	need.GET("/", s.pageDashboard)
	need.GET("/api/dashboard", s.apiDashboard)

	need.GET("/player", s.requirePerm("/player"), s.pagePlayer)
	need.POST("/api/player/search", s.requirePerm("/player"), s.apiPlayerSearch)

	need.GET("/mail", s.requirePerm("/mail"), s.pageMail)
	need.POST("/api/mail/send", s.requirePerm("/mail"), s.apiMailSend)

	// 通用模块：页面
	need.GET("/m/:key", s.requireModulePerm, s.pageModule)
	// 通用模块：数据接口。写操作额外要求模块可写。
	api := need.Group("/api/m/:key")
	api.Use(s.requireModulePerm)
	api.GET("/schema", s.apiModuleSchema)
	api.POST("/list", s.apiModuleList)
	api.POST("/save", s.requireWritable, s.apiModuleSave)
	api.POST("/delete", s.requireWritable, s.apiModuleDelete)
	api.POST("/action", s.requireWritable, s.apiModuleAction)

	return r
}

// requireLogin 拦截未登录请求：页面跳登录页，接口返回 401 让前端统一处理。
func (s *Server) requireLogin(c *gin.Context) {
	if auth.Current(c) != nil {
		return
	}
	if strings.HasPrefix(c.Request.URL.Path, "/api/") {
		c.AbortWithStatusJSON(http.StatusOK, resp.NoLogin())
		return
	}
	c.Abort()
	c.Redirect(http.StatusFound, "/login")
}

// requireModulePerm 校验当前账号能否访问 :key 这个模块。
func (s *Server) requireModulePerm(c *gin.Context) {
	user := auth.Current(c)
	if user == nil {
		c.AbortWithStatusJSON(http.StatusOK, resp.NoLogin())
		return
	}
	if !auth.CanAccess(s.store, user, "/m/"+c.Param("key")) {
		s.fail(c, resp.NoPerm())
	}
}

// requirePerm 给「自定义页面」及其数据接口加菜单权限校验。
//
// 通用模块的接口有 :key，可以交给 requireModulePerm 按模块查权限；
// 自定义页面（玩家查询、发邮件）没有 key，只能按页面路径校验——
// 少了这一步，任何登录用户都能直接 POST 接口绕过菜单权限。
func (s *Server) requirePerm(path string) gin.HandlerFunc {
	return func(c *gin.Context) {
		user := auth.Current(c)
		if user == nil {
			c.AbortWithStatusJSON(http.StatusOK, resp.NoLogin())
			return
		}
		if !auth.CanAccess(s.store, user, path) {
			s.fail(c, resp.NoPerm())
		}
	}
}

// requireWritable 只读模块不允许写。
func (s *Server) requireWritable(c *gin.Context) {
	m, ok := modules.ByKey(c.Param("key"))
	if !ok {
		s.fail(c, resp.Fail("模块不存在"))
		return
	}
	if !m.ReadOnly {
		return
	}
	s.fail(c, resp.Fail("该模块只读"))
}

// fail 中断并输出失败响应。页面与接口用同一种输出，前端只需要判断 code。
func (s *Server) fail(c *gin.Context, r resp.Result) {
	c.AbortWithStatusJSON(http.StatusOK, r)
}

// ---- 渲染 ----

// MenuNode 是给模板用的菜单节点。
type MenuNode struct {
	*store.Menu
	Children []*MenuNode
	Active   bool
	Open     bool
}

func (s *Server) render(c *gin.Context, name string, data gin.H) {
	if data == nil {
		data = gin.H{}
	}
	user := auth.Current(c)
	data["Site"] = s.conf.Site
	data["User"] = user
	data["Menus"] = s.menuTree(user, c.Request.URL.Path)
	data["Path"] = c.Request.URL.Path
	data["Driver"] = s.game.Driver()
	c.Header("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(c.Writer, name, data); err != nil {
		// 模板错误在页面上直接暴露：后台使用者就是开发者，藏起来只会更难查。
		_, _ = c.Writer.WriteString("<pre>template error: " + err.Error() + "</pre>")
	}
}

// menuTree 把扁平菜单按 ParentID 组装成树，并标记当前所在项。
func (s *Server) menuTree(user *store.Account, path string) []*MenuNode {
	flat := auth.VisibleMenus(s.store, user)
	byID := map[int64]*MenuNode{}
	roots := make([]*MenuNode, 0, len(flat))
	for _, m := range flat {
		byID[m.ID] = &MenuNode{Menu: m}
	}

	for _, m := range flat {
		n := byID[m.ID]
		if p, ok := byID[m.ParentID]; ok && m.ParentID != 0 {
			p.Children = append(p.Children, n)
			continue
		}
		roots = append(roots, n)
	}
	var walk func(ns []*MenuNode) bool
	walk = func(ns []*MenuNode) bool {
		hit := false
		for _, n := range ns {
			self := n.URL != "" && n.URL[0] != '#' && (path == n.URL || strings.HasPrefix(path, n.URL+"/"))
			child := walk(n.Children)
			if self {
				hit = true
			}
			n.Active = self
			n.Open = self || child
			if child {
				hit = true
			}
		}
		return hit
	}
	walk(roots)
	return roots
}
