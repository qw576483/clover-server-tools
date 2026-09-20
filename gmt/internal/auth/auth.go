// Package auth 负责后台自己的登录、会话与菜单级权限。
//
// 权限模型刻意做得**很薄**：权限项 = 菜单的 URL。
// 一个角色能看到哪些菜单，它就能访问哪些地址（页面与它的数据接口共享同一前缀）。
// 之所以不再单独维护一份「动作 → 权限」的映射：多一份映射就多一处会与代码脱节的地方，
// 而菜单本身就是运营每次都要维护的东西，权限跟着它走最不容易漂。
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/qw576483/clover-server-tools/gmt/internal/store"
)

const (
	cookieName  = "gmt_sid"
	sessionTTL  = 12 * time.Hour
	ctxAccount  = "gmt_account"
	SuperRoleID = int64(1)
)

// Hash 计算口令摘要。
//
// 这里用「配置 secret 作 pepper + 账号名作 salt」的 sha256，
// 目的是**不引入额外依赖**、且配置文件被拷走时口令不会直接被彩虹表命中。
// 若将来要对公网部署，换成 bcrypt/argon2 只需改这一个函数 + 重新初始化口令。
func Hash(secret, name, password string) string {
	sum := sha256.Sum256([]byte(secret + ":" + name + ":" + password))
	return fmt.Sprintf("%x", sum)
}

// Login 校验账号口令，成功则更新登录信息并返回账号。
func Login(s *store.Client, secret, name, password, ip string) (*store.Account, error) {
	name = strings.TrimSpace(name)
	if name == "" || password == "" {
		return nil, fmt.Errorf("账号与密码不能为空")
	}
	if n, ok := attempts[ip]; ok && n >= 5 {
		return nil, fmt.Errorf("失败次数过多，请稍后再试")
	}
	accounts, err := store.All[*store.Account](s, store.Accounts)
	if err != nil {
		return nil, err
	}
	var target *store.Account
	for i := range accounts {
		if accounts[i].Name == name {
			target = accounts[i]
			break
		}
	}
	if target == nil || target.PassHash != Hash(secret, name, password) {
		attempts[ip]++
		return nil, fmt.Errorf("账号或密码错误")
	}
	if target.Status == 0 {
		return nil, fmt.Errorf("该账号已被禁用")
	}
	target.LastLoginAt = store.Now()
	target.LastLoginIP = ip
	if _, err := store.Put[*store.Account](s, store.Accounts, target); err != nil {
		return nil, err
	}
	delete(attempts, ip)
	return target, nil
}

// attempts 是进程内的登录失败计数（按 IP）。
//
// 放内存而不是库里：后台通常单进程，重启即清零是可以接受的——
// 它防的是「脚本短时间猛试」，不是持久化审计。
var attempts = map[string]int{}

// Session 生成会话 cookie 值：账号ID.过期时间.签名。
func Session(secret string, id int64) string {
	exp := time.Now().Add(sessionTTL).Unix()
	raw := fmt.Sprintf("%d.%d", id, exp)
	return raw + "." + sign(secret, raw)
}

func sign(secret, raw string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(raw))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Verify 校验会话串，返回账号 ID。
func Verify(secret, value string) (int64, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return 0, false
	}
	if !hmac.Equal([]byte(sign(secret, parts[0]+"."+parts[1])), []byte(parts[2])) {
		return 0, false
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return 0, false
	}
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// Middleware 解析会话并把账号注入 gin 上下文。
//
// 注意：这里**不做拦截**，只做解析。是否允许访问由 requireLogin / requirePerm 决定——
// 登录页本身也要能被访问，把拦截硬编码进解析会导致「没登录就 401 循环」。
func Middleware(s *store.Client, secret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if v, err := c.Cookie(cookieName); err == nil {
			if id, ok := Verify(secret, v); ok {
				if acc, found, err := store.Get[*store.Account](s, store.Accounts, id); err == nil && found && acc.Status == 1 {
					c.Set(ctxAccount, acc)
				}
			}
		}
		c.Next()
	}
}

// SetCookie 下发会话 cookie。
func SetCookie(c *gin.Context, secret string, id int64) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     cookieName,
		Value:    Session(secret, id),
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearCookie 清除会话 cookie。
func ClearCookie(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
}

// Current 返回当前登录账号，未登录返回 nil。
func Current(c *gin.Context) *store.Account {
	v, ok := c.Get(ctxAccount)
	if !ok {
		return nil
	}
	a, _ := v.(*store.Account)
	return a
}

// IsSuper 判断是否为超管（超管放行一切）。
func IsSuper(a *store.Account) bool { return a != nil && a.RoleID == SuperRoleID }

// CanAccess 判断账号能否访问某个地址。
//
// 规则：超管全放行；否则该账号角色的菜单里必须有一条 URL 是请求路径的前缀。
// 用前缀而不是全等，是因为页面 `/m/server` 会带出 `/m/server/xxx` 这类子路径。
func CanAccess(s *store.Client, a *store.Account, path string) bool {
	if a == nil {
		return false
	}
	if IsSuper(a) {
		return true
	}
	role, ok, err := store.Get[*store.Role](s, store.Roles, a.RoleID)
	if err != nil || !ok {
		return false
	}
	allowed := map[int64]bool{}
	for _, id := range role.MenuIDs {
		allowed[id] = true
	}
	menus, err := store.All[*store.Menu](s, store.Menus)
	if err != nil {
		return false
	}
	for _, m := range menus {
		if !allowed[m.ID] || m.URL == "" {
			continue
		}
		if path == m.URL || strings.HasPrefix(path, m.URL+"/") {
			return true
		}
	}
	return false
}

// VisibleMenus 返回该账号可见的菜单（已排序）。
func VisibleMenus(s *store.Client, a *store.Account) []*store.Menu {
	all, err := store.All[*store.Menu](s, store.Menus)
	if err != nil {
		return nil
	}
	if a == nil {
		return nil
	}
	if !IsSuper(a) {
		role, ok, err := store.Get[*store.Role](s, store.Roles, a.RoleID)
		if err != nil || !ok {
			return nil
		}
		allowed := map[int64]bool{}
		for _, id := range role.MenuIDs {
			allowed[id] = true
		}
		kept := make([]*store.Menu, 0, len(all))
		for _, m := range all {
			if allowed[m.ID] {
				kept = append(kept, m)
			}
		}
		all = kept
	}
	sortMenus(all)
	return all
}

func sortMenus(ms []*store.Menu) {
	// 简单插入排序：菜单量级几十条，不值得引入 sort 包的复杂比较器。
	for i := 1; i < len(ms); i++ {
		for j := i; j > 0 && lessMenu(ms[j], ms[j-1]); j-- {
			ms[j], ms[j-1] = ms[j-1], ms[j]
		}
	}
}

func lessMenu(a, b *store.Menu) bool {
	if a.ParentID != b.ParentID {
		return a.ParentID < b.ParentID
	}
	return a.OrderNo < b.OrderNo
}
