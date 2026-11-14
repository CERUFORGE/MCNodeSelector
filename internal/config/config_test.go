package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStripComments 验证 JSONC 注释剥离器：字符串内 // 不得误删、转义正确
func TestStripComments(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"行注释", "{ // 注释\n\"a\": 1 }", "{ \n\"a\": 1 }"},
		{"块注释", "{ /* 块\n注释 */ \"a\": 1 }", "{ \n  \"a\": 1 }"},
		{"字符串内斜杠", "{\"token\": \"abc//def\", // 尾注\n\"b\": 2}", "{\"token\": \"abc//def\", \n\"b\": 2}"},
		{"转义引号", "{\"a\": \"x\\\"/*y*/\"}", "{\"a\": \"x\\\"/*y*/\"}"},
		{"字符串内块注释样式", "{\"a\": \"/*不是注释*/\"}", "{\"a\": \"/*不是注释*/\"}"},
		{"无注释", "{\"a\": 1}", "{\"a\": 1}"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := string(StripComments([]byte(c.in)))
			if got != c.want {
				t.Errorf("StripComments(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestLoadFirstRun 文件不存在时生成模板并返回 ErrFirstRun
func TestLoadFirstRun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.jsonc")
	_, err := Load(path)
	if err != ErrFirstRun {
		t.Fatalf("期望 ErrFirstRun，得到 %v", err)
	}
	// 模板应可被再次加载并通过校验
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("模板加载失败：%v", err)
	}
	if len(cfg.Nodes) != 1 || cfg.Nodes[0].Name != "示例节点-请修改" {
		t.Fatalf("模板节点解析异常：%+v", cfg.Nodes)
	}
	// 模板剥离注释后必须是合法 JSON
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("模板文件未生成：%v", err)
	}
}

// TestLoadBOM 兼容 Windows 记事本 / PowerShell 写入的 UTF-8 BOM
func TestLoadBOM(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.jsonc")
	body := []byte(Template)
	withBOM := append([]byte{0xEF, 0xBB, 0xBF}, body...)
	if err := os.WriteFile(path, withBOM, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("带 BOM 的配置应可加载，实际：%v", err)
	}
}

// TestAgentPassthrough 默认开启；显式 false 时关闭
func TestAgentPassthrough(t *testing.T) {
	var m Multipath
	if !m.AgentPassthroughOn() {
		t.Error("agent_passthrough 默认应为 true")
	}
	f := false
	m.AgentPassthrough = &f
	if m.AgentPassthroughOn() {
		t.Error("显式 false 应返回 false")
	}
}

// TestValidate 节点级中文校验
func TestValidate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.jsonc")
	if err := os.WriteFile(path, []byte(Template), 0o644); err != nil {
		t.Fatal(err)
	}
	base, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	// name 重复
	dup := *base
	dup.Nodes = append(dup.Nodes, base.Nodes[0])
	if err := Validate(&dup); err == nil {
		t.Error("重复 name 应报错")
	}

	// 端口越界
	bad := *base
	bad.Nodes = []Node{{Name: "x", ServerAddr: "a.com", ServerPort: 70000, RemotePort: 25565}}
	if err := Validate(&bad); err == nil {
		t.Error("server_port 越界应报错")
	}

	// 权重全零
	zero := *base
	zero.Scoring.Weights = Weights{}
	if err := Validate(&zero); err == nil {
		t.Error("权重全零应报错")
	}
}

// TestLoadSyntaxError 尾随逗号等语法错误给中文提示
func TestLoadSyntaxError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.jsonc")
	bad := "{\n  \"nodes\": [],\n}\n" // 尾随逗号
	if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("语法错误应报错")
	}
	if want := "尾随逗号"; !containsStr(err.Error(), want) {
		t.Errorf("错误信息应包含 %q，实际：%v", want, err)
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && indexStr(s, sub) >= 0)
}

func indexStr(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
