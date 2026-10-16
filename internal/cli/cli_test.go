package cli

import (
	"testing"
)

// TestParseFlagAnywhere 锁死「flag 可写在任意位置」这条契约（agent 与人最容易踩的地方）。
func TestParseFlagAnywhere(t *testing.T) {
	cases := []struct {
		argv    []string
		cmd     string
		rest    []string
		cid     string
		prompt  string
		dry     bool
		wantErr bool
	}{
		{[]string{"chat", "--cid", "6ad16e02581cc70756919ade", "你好"}, "chat",
			[]string{"chat", "你好"}, "6ad16e02581cc70756919ade", "", false, false},
		{[]string{"chat", "你好", "--cid=6ad16e02581cc70756919ade"}, "chat",
			[]string{"chat", "你好"}, "6ad16e02581cc70756919ade", "", false, false},
		{[]string{"--cred", "x.json", "conv", "delete", "6ad16e02581cc70756919ade", "--yes"}, "conv",
			[]string{"conv", "delete", "6ad16e02581cc70756919ade"}, "", "", false, false},
		{[]string{"chat", "-p", "-"}, "chat", []string{"chat"}, "", "-", false, false},
		{[]string{"chat", "--dry-run", "你好"}, "chat", []string{"chat", "你好"}, "", "", true, false},
		{[]string{"chat", "--cid", "x", "--cred"}, "", nil, "", "", false, true}, // --cred 缺取值必须报错
		{[]string{"chat", "--nope"}, "", nil, "", "", false, true},               // 未知 flag 必须直接报错
		{[]string{}, "", nil, "", "", false, false},
	}
	for _, c := range cases {
		fs, f := newFlagSet("t")
		name, err := parse(fs, c.argv, f)
		if c.wantErr {
			if err == nil {
				t.Errorf("%v 应该报错", c.argv)
			}
			continue
		}
		if err != nil {
			t.Errorf("%v 意外报错: %v", c.argv, err)
			continue
		}
		if name != c.cmd {
			t.Errorf("%v: 命令 = %q，期望 %q", c.argv, name, c.cmd)
		}
		if len(f.rest) != len(c.rest) {
			t.Errorf("%v: rest = %v，期望 %v", c.argv, f.rest, c.rest)
		}
		if f.CID != c.cid || f.Prompt != c.prompt || f.DryRun != c.dry {
			t.Errorf("%v: cid=%q prompt=%q dry=%v", c.argv, f.CID, f.Prompt, f.DryRun)
		}
	}
}

// TestParseRepeatableParam 覆盖 --param 重复与 k=v 校验。
func TestParseRepeatableParam(t *testing.T) {
	fs, f := newFlagSet("t")
	if _, err := parse(fs, []string{"conv", "history", "abc", "--param", "page=2", "--param", "size=10"}, f); err != nil {
		t.Fatal(err)
	}
	if len(f.Params) != 2 || f.Params["page"] != "2" {
		t.Errorf("params = %v", map[string]string(f.Params))
	}
	fs, f = newFlagSet("t")
	if _, err := parse(fs, []string{"conv", "history", "abc", "--param", "novalue"}, f); err == nil {
		t.Errorf("--param 缺 = 应报错")
	}
}

// TestResolveAgent 覆盖别名展开与「--ratio 即生图」这条约定。
func TestResolveAgent(t *testing.T) {
	for alias, want := range map[string]string{"main": "65940acff94777010aa6b796",
		"drawing": "65a232c082ff90a2ad2f15e2", "read": "658a7988b8a9a98d38725745"} {
		got, err := resolveAgent(alias, "")
		if err != nil || got != want {
			t.Errorf("%s ⇒ %q %v", alias, got, err)
		}
	}
	if got, _ := resolveAgent("main", "1:1"); got != "65a232c082ff90a2ad2f15e2" {
		t.Errorf("给了 --ratio 应自动切到画图 agent，得到 %q", got)
	}
	if got, _ := resolveAgent("drawing", "16:9"); got != "65a232c082ff90a2ad2f15e2" {
		t.Errorf("显式 --agent 不该被 ratio 覆盖")
	}
	if _, err := resolveAgent("nope", ""); err == nil {
		t.Errorf("未知 agent 必须报错")
	}
	raw := "6ad16e02581cc70756919ade"
	if got, err := resolveAgent(raw, ""); err != nil || got != raw {
		t.Errorf("裸 assistant_id 应直通: %q %v", got, err)
	}
}

// TestValidRatio 覆盖比例白名单（上游对非法值静默降级 1:1，所以 CLI 侧必须拦住）。
func TestValidRatio(t *testing.T) {
	for _, ok := range []string{"1:1", "3:4", "4:3", "9:16", "16:9"} {
		if !validRatio(ok) {
			t.Errorf("%s 应有效", ok)
		}
	}
	for _, bad := range []string{"21:9", "1:2", "", "auto"} {
		if validRatio(bad) {
			t.Errorf("%q 应被拒", bad)
		}
	}
}

// TestCreditsOf 覆盖余额输出（V13 定稿：不折算，原样返回 left_score）。
func TestCreditsOf(t *testing.T) {
	got := creditsOf(map[string]any{"left_score": "330.00"})
	if got["left_score"] != "330.00" {
		t.Errorf("left_score = %v", got["left_score"])
	}
	if _, has := got["credits"]; has {
		t.Errorf("不应再输出折算后的 credits")
	}
	if v, ok := creditsOf(nil)["left_score"]; !ok || v != nil {
		t.Errorf("访客账号应返回 nil 余额且不报错")
	}
}

// TestCommandsHaveUsage 防止新增命令时漏写 usage/schema（两张表同源）。
func TestCommandsCovered(t *testing.T) {
	names := map[string]bool{}
	for _, c := range commands() {
		if c.Usage == "" || c.Run == nil {
			t.Errorf("命令 %q 缺 usage/run", c.Name)
		}
		names[c.Name] = true
	}
	for _, want := range []string{"chat", "user", "models", "agents", "auth", "conv", "raw", "schema", "help"} {
		if !names[want] {
			t.Errorf("命令表缺 %q", want)
		}
	}
}
