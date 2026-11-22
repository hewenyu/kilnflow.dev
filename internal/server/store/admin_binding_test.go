package store

import "testing"

func TestAdminBinding(t *testing.T) {
	t.Parallel()
	db := newDB(t)
	ctx := t.Context()
	b, err := GetAdminBinding(ctx, db)
	if err != nil || b != nil {
		t.Fatalf("还没有绑定：%+v %v", b, err)
	}
	if ok, err := SetAdminTokenLogin(ctx, db, false); err != nil || ok {
		t.Fatalf("没有绑定时不能关闭 token 登录：%v %v", ok, err)
	}
	if err := PutAdminBinding(ctx, db, 42, "alice", "admin"); err != nil {
		t.Fatal(err)
	}
	b, err = GetAdminBinding(ctx, db)
	if err != nil || b == nil || b.GitHubID != 42 || b.GitHubLogin != "alice" || !b.TokenLogin || b.BoundBy != "admin" {
		t.Fatalf("绑定后：%+v %v", b, err)
	}
	if ok, err := SetAdminTokenLogin(ctx, db, false); err != nil || !ok {
		t.Fatalf("关闭 token 登录：%v %v", ok, err)
	}
	// 更换绑定的账号：token 登录的开关保持不变
	if err := PutAdminBinding(ctx, db, 43, "alice2", "alice"); err != nil {
		t.Fatal(err)
	}
	b, _ = GetAdminBinding(ctx, db)
	if b.GitHubID != 43 || b.GitHubLogin != "alice2" || b.TokenLogin || b.BoundBy != "alice" {
		t.Fatalf("更换绑定后：%+v", b)
	}
	if ok, err := DeleteAdminBinding(ctx, db); err != nil || !ok {
		t.Fatalf("解除绑定：%v %v", ok, err)
	}
	if ok, err := DeleteAdminBinding(ctx, db); err != nil || ok {
		t.Fatalf("重复解除绑定：%v %v", ok, err)
	}
	if b, _ := GetAdminBinding(ctx, db); b != nil {
		t.Fatalf("解除后仍有绑定：%+v", b)
	}
}
