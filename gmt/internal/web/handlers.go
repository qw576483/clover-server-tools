package web

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"gmt/internal/auth"
	"gmt/internal/gameclient"
	"gmt/internal/modules"
	"gmt/internal/resp"
	"gmt/internal/store"
)

// ---- 登录 ----

func (s *Server) pageLogin(c *gin.Context) {
	if auth.Current(c) != nil {
		c.Redirect(302, "/")
		return
	}
	s.render(c, "login.html", nil)
}

// apiCaptcha 生成一张登录验证码（算式的 PNG 图片）。
func (s *Server) apiCaptcha(c *gin.Context) {
	png, cookie, err := auth.CaptchaImage(s.conf.Server.Secret)
	if err != nil {
		c.String(http.StatusInternalServerError, "验证码生成失败")
		return
	}
	auth.SetCaptchaCookie(c, cookie)
	c.Header("Cache-Control", "no-store, no-cache, must-revalidate")
	c.Data(http.StatusOK, "image/png", png)
}

func (s *Server) apiLogin(c *gin.Context) {
	var body struct {
		Name     string `json:"name"`
		Password string `json:"password"`
		Captcha  string `json:"captcha"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(200, resp.Fail("请求格式错误"))
		return
	}
	// 先验验证码再比对口令：一道最便宜的闸放在最前面，口令的散列计算不让它白白跑。
	cookie, _ := c.Cookie(auth.CaptchaCookie)
	ok := auth.VerifyCaptcha(s.conf.Server.Secret, cookie, body.Captcha)
	auth.ClearCaptcha(c) // 无论对错都作废，避免一个答案被反复使用
	if !ok {
		c.JSON(200, resp.Fail("验证码错误或已过期"))
		return
	}
	acc, err := auth.Login(s.store, s.conf.Server.Secret, body.Name, body.Password, c.ClientIP())
	if err != nil {
		c.JSON(200, resp.Fail(err.Error()))
		return
	}
	auth.SetCookie(c, s.conf.Server.Secret, acc.ID)
	c.JSON(200, resp.OK(gin.H{"name": acc.Name}))
}

func (s *Server) apiLogout(c *gin.Context) {
	auth.ClearCookie(c)
	c.JSON(200, resp.OK(nil))
}

// ---- 概览 ----

func (s *Server) pageDashboard(c *gin.Context) {
	s.render(c, "dashboard.html", gin.H{"Title": "概览"})
}

func (s *Server) apiDashboard(c *gin.Context) {
	servers, err := store.All[*store.Server](s.store, store.Servers)
	if err != nil {
		c.JSON(200, resp.Err(err))
		return
	}
	machines, err := store.All[*store.Machine](s.store, store.Machines)
	if err != nil {
		c.JSON(200, resp.Err(err))
		return
	}
	bans, err := store.All[*store.Ban](s.store, store.Bans)
	if err != nil {
		c.JSON(200, resp.Err(err))
		return
	}
	gifts, err := store.All[*store.GiftBatch](s.store, store.Gifts)
	if err != nil {
		c.JSON(200, resp.Err(err))
		return
	}
	audits, err := store.All[*store.Audit](s.store, store.Audits)
	if err != nil {
		c.JSON(200, resp.Err(err))
		return
	}

	activeBans := 0
	for _, b := range bans {
		if b.Active {
			activeBans++
		}
	}
	// 今日 0 点：按后台所在时区算，避免 UTC 与本地差一天导致「今日操作数」对不上。
	now := time.Now().In(s.loc)
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, s.loc).Unix()
	todayOps := 0
	for _, a := range audits {
		if a.Time >= midnight {
			todayOps++
		}
	}

	recent := make([]gin.H, 0, 12)
	for i := len(audits) - 1; i >= 0 && len(recent) < 12; i-- {
		a := audits[i]
		recent = append(recent, gin.H{
			"time":    time.Unix(a.Time, 0).In(s.loc).Format("01-02 15:04:05"),
			"account": a.Account,
			"type":    a.Type,
			"sub":     a.SubType,
			"info":    a.Info,
		})
	}

	c.JSON(200, resp.OK(gin.H{
		"stats": gin.H{
			"servers":     len(servers),
			"machines":    len(machines),
			"active_bans": activeBans,
			"gifts":       len(gifts),
			"today_ops":   todayOps,
		},
		"nodes":  s.game.Health(c.Request.Context()),
		"recent": recent,
	}))
}

// ---- 玩家查询 ----

func (s *Server) pagePlayer(c *gin.Context) {
	s.render(c, "player.html", gin.H{"Title": "玩家查询", "Servers": s.serverOptions()})
}

func (s *Server) apiPlayerSearch(c *gin.Context) {
	var body struct {
		ServerID int64  `json:"server_id"`
		Kind     string `json:"kind"`
		Keyword  string `json:"keyword"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(200, resp.Fail("请求格式错误"))
		return
	}
	if strings.TrimSpace(body.Keyword) == "" {
		c.JSON(200, resp.Fail("请输入查询关键字"))
		return
	}
	if body.Kind == "" {
		body.Kind = "player_id"
	}
	p, err := s.game.SearchPlayer(c.Request.Context(), body.ServerID, body.Kind, strings.TrimSpace(body.Keyword))
	if err != nil {
		c.JSON(200, resp.Err(err))
		return
	}
	s.audit(c, "player", "search", fmt.Sprintf("server=%d kind=%s keyword=%s", body.ServerID, body.Kind, body.Keyword))
	c.JSON(200, resp.OK(p))
}

// ---- 发邮件 ----

func (s *Server) pageMail(c *gin.Context) {
	s.render(c, "mail.html", gin.H{"Title": "邮件管理", "Servers": s.serverOptions()})
}

func (s *Server) apiMailSend(c *gin.Context) {
	body := modules.Values{}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(200, resp.Fail("请求格式错误"))
		return
	}
	req := gameclient.MailRequest{
		ServerID: body.Int64("server_id"),
		All:      body.Bool("all"),
		MinLevel: body.Int("min_level"),
		Targets:  body.Int64Slice("targets"),
		Title:    strings.TrimSpace(body.Str("title")),
		Content:  strings.TrimSpace(body.Str("content")),
		Items:    parseItems(body.Str("items")),
	}
	if req.Title == "" {
		c.JSON(200, resp.Fail("邮件标题不能为空"))
		return
	}
	if !req.All && len(req.Targets) == 0 {
		c.JSON(200, resp.Fail("个人邮件必须填至少一个玩家 ID"))
		return
	}
	if err := s.game.SendMail(c.Request.Context(), req); err != nil {
		c.JSON(200, resp.Err(err))
		return
	}
	target := "全区服"
	if !req.All {
		target = fmt.Sprintf("%d 个玩家", len(req.Targets))
	}
	s.audit(c, "mail", "send", fmt.Sprintf("server=%d %s title=%s", req.ServerID, target, req.Title))
	c.JSON(200, resp.OK(nil))
}

// parseItems 解析附件文本，支持「1001:10,2002:1」和「1001*10」两种写法。
//
// 运营同学常年用手抄的道具串，多支持一种分隔符的成本远小于来回沟通的成本。
func parseItems(text string) []gameclient.Item {
	out := []gameclient.Item{}
	for _, part := range strings.FieldsFunc(text, func(r rune) bool {
		return r == ',' || r == '，' || r == ';' || r == '\n' || r == ' '
	}) {
		id, num := int64(0), int64(1)
		if i := strings.IndexAny(part, ":*xX"); i > 0 {
			id, _ = strconv.ParseInt(strings.TrimSpace(part[:i]), 10, 64)
			num, _ = strconv.ParseInt(strings.TrimSpace(part[i+1:]), 10, 64)
		} else {
			id, _ = strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		}
		if id > 0 && num > 0 {
			out = append(out, gameclient.Item{ConfigID: id, Num: num})
		}
	}
	return out
}

// ---- 通用模块 ----

func (s *Server) pageModule(c *gin.Context) {
	m, ok := modules.ByKey(c.Param("key"))
	if !ok {
		c.String(404, "模块不存在")
		return
	}
	s.render(c, "list.html", gin.H{"Title": m.Name, "Module": m, "Key": m.Key})
}

func (s *Server) apiModuleSchema(c *gin.Context) {
	m, ok := modules.ByKey(c.Param("key"))
	if !ok {
		c.JSON(200, resp.Fail("模块不存在"))
		return
	}
	// 动态下拉在这里现算：模块注册时数据还不存在，不能在那时把选项快照死。
	// 复制一份再填：模块对象是全局共享的，就地改它会和并发请求互相踩。
	out := *m
	out.Form = append([]modules.Field(nil), m.Form...)
	ctx := s.newCtx(c, m)
	for i := range out.Form {
		if out.Form[i].OptionsFrom != nil {
			out.Form[i].Options = out.Form[i].OptionsFrom(ctx)
		}
	}
	c.JSON(200, resp.OK(out))
}

func (s *Server) apiModuleList(c *gin.Context) {
	m, ok := modules.ByKey(c.Param("key"))
	if !ok {
		c.JSON(200, resp.Fail("模块不存在"))
		return
	}
	if m.List == nil {
		c.JSON(200, resp.Fail("该模块不支持列表"))
		return
	}
	q := modules.Query{}
	if err := c.ShouldBindJSON(&q); err != nil {
		c.JSON(200, resp.Fail("请求格式错误"))
		return
	}
	if q.Size <= 0 {
		q.Size = 20
	}
	rows, total, err := m.List(s.newCtx(c, m), q)
	if err != nil {
		c.JSON(200, resp.Err(err))
		return
	}
	c.JSON(200, resp.OK(resp.List{Rows: rows, Total: total, Page: q.Page, Size: q.Size}))
}

func (s *Server) apiModuleSave(c *gin.Context) {
	m, ok := modules.ByKey(c.Param("key"))
	if !ok || m.Save == nil {
		c.JSON(200, resp.Fail("该模块不支持保存"))
		return
	}
	v := modules.Values{}
	if err := c.ShouldBindJSON(&v); err != nil {
		c.JSON(200, resp.Fail("请求格式错误"))
		return
	}
	if err := m.Save(s.newCtx(c, m), v); err != nil {
		c.JSON(200, resp.Err(err))
		return
	}
	c.JSON(200, resp.OK(nil))
}

func (s *Server) apiModuleDelete(c *gin.Context) {
	m, ok := modules.ByKey(c.Param("key"))
	if !ok || m.Delete == nil {
		c.JSON(200, resp.Fail("该模块不支持删除"))
		return
	}
	v := modules.Values{}
	if err := c.ShouldBindJSON(&v); err != nil {
		c.JSON(200, resp.Fail("请求格式错误"))
		return
	}
	if err := m.Delete(s.newCtx(c, m), v.Int64("id")); err != nil {
		c.JSON(200, resp.Err(err))
		return
	}
	c.JSON(200, resp.OK(nil))
}

func (s *Server) apiModuleAction(c *gin.Context) {
	m, ok := modules.ByKey(c.Param("key"))
	if !ok || m.Do == nil {
		c.JSON(200, resp.Fail("该模块没有自定义操作"))
		return
	}
	var body struct {
		Action string         `json:"action"`
		ID     int64          `json:"id"`
		Form   modules.Values `json:"form"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(200, resp.Fail("请求格式错误"))
		return
	}
	data, err := m.Do(s.newCtx(c, m), body.Action, body.ID, body.Form)
	if err != nil {
		c.JSON(200, resp.Err(err))
		return
	}
	c.JSON(200, resp.OK(data))
}

// ---- 辅助 ----

// newCtx 组装模块回调需要的上下文。
func (s *Server) newCtx(c *gin.Context, m *modules.Module) *modules.Ctx {
	ctx := &modules.Ctx{
		Gin:    c,
		Store:  s.store,
		Game:   s.game,
		User:   auth.Current(c),
		Secret: s.conf.Server.Secret,
	}
	if m != nil {
		ctx.Type = m.Type
	}
	return ctx
}

// audit 记录非通用模块（玩家查询、发邮件）的操作日志。
func (s *Server) audit(c *gin.Context, typ, sub, info string) {
	name := ""
	if u := auth.Current(c); u != nil {
		name = u.Name
	}
	_, _ = store.Put[*store.Audit](s.store, store.Audits, &store.Audit{
		Time: store.Now(), Account: name, IP: c.ClientIP(), Type: typ, SubType: sub, Info: info,
	})
}

// serverOptions 返回「区服下拉」数据。
//
// 展示用数据：读不出来就让下拉空着（页面上会提示没有可选的服），
// 不把整页变成错误页。
func (s *Server) serverOptions() []gin.H {
	servers, err := store.All[*store.Server](s.store, store.Servers)
	if err != nil {
		return nil
	}
	out := make([]gin.H, 0, len(servers)+1)
	for _, sv := range servers {
		label := fmt.Sprintf("%d - %s", sv.ServerID, sv.Name)
		if sv.Zone != "" {
			label = fmt.Sprintf("%s（%s）", label, sv.Zone)
		}
		out = append(out, gin.H{"id": sv.ServerID, "name": label})
	}
	return out
}
