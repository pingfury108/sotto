package config

import "testing"

func TestSplitList(t *testing.T) {
	got := SplitList("伏羲, sotto、deploy.sh；再试|换行\n最后")
	want := []string{"伏羲", "sotto", "deploy.sh", "再试", "换行", "最后"}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 项 %q，want %q（全部 %v）", i, got[i], want[i], got)
		}
	}
	if SplitList("   ") != nil {
		t.Fatal("空白应返回 nil")
	}
}
