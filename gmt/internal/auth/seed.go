package auth

import "github.com/qw576483/clover-server-tools/gmt/internal/store"

// EnsureBase 保证「至少有一个能登录的超级管理员」。
//
// 只在缺东西时补写，已有数据一律不动——避免每次启动把别人改过的口令重置掉。
// 初始口令 admin123，登录后请立刻修改。
func EnsureBase(s *store.Client, secret string) error {
	roles, err := store.All[*store.Role](s, store.Roles)
	if err != nil {
		return err
	}
	hasSuper := false
	for _, r := range roles {
		if r.ID == SuperRoleID {
			hasSuper = true
			break
		}
	}
	if !hasSuper {
		// 超管角色必须是 ID=1：IsSuper 就是按这个 ID 判定的，
		// 不能让它在「角色表非空但没有 1」的情况下缺失。
		if _, err := store.Put[*store.Role](s, store.Roles, &store.Role{
			ID:     SuperRoleID,
			Name:   "超级管理员",
			Remark: "拥有全部权限",
		}); err != nil {
			return err
		}
	}

	accounts, err := store.All[*store.Account](s, store.Accounts)
	if err != nil {
		return err
	}
	for _, a := range accounts {
		if a.Name == "admin" {
			return nil
		}
	}
	_, err = store.Put[*store.Account](s, store.Accounts, &store.Account{
		Name:      "admin",
		Nick:      "超级管理员",
		RoleID:    SuperRoleID,
		Status:    1,
		PassHash:  Hash(secret, "admin", "admin123"),
		CreatedAt: store.Now(),
	})
	return err
}
