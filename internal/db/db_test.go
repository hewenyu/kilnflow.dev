package db

import "testing"

func TestLoadMigrations(t *testing.T) {
	list, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) == 0 {
		t.Fatal("没有找到迁移文件")
	}
	for i := 1; i < len(list); i++ {
		if list[i].version <= list[i-1].version {
			t.Errorf("迁移顺序不对：%s 在 %s 之后", list[i].name, list[i-1].name)
		}
	}
}
