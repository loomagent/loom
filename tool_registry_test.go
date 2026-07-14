package loom

import (
	"context"
	"strings"
	"testing"
)

type registryTestTool struct {
	info *ToolInfo
}

func (t *registryTestTool) Info(context.Context) (*ToolInfo, error) { return t.info, nil }
func (*registryTestTool) Invoke(context.Context, string) (string, error) {
	return "", nil
}

func TestValidateToolName(t *testing.T) {
	for _, name := range []string{"a", "calculator", "web_search", "tool42", "tool_2", strings.Repeat("a", maxToolNameLength)} {
		if err := ValidateToolName(name); err != nil {
			t.Errorf("ValidateToolName(%q): %v", name, err)
		}
	}
	for _, name := range []string{"", " web_search", "web_search ", "web-search", "web.search", "WebSearch", "1st_tool", "_private", "网页搜索", "name/with/slash", strings.Repeat("a", maxToolNameLength+1)} {
		if err := ValidateToolName(name); err == nil {
			t.Errorf("ValidateToolName(%q) unexpectedly succeeded", name)
		}
	}
}

func TestToolRegistryRejectsDuplicateName(t *testing.T) {
	newNamedTool := func() Tool {
		return NewTool(MustToolContract[NoArguments]("duplicate"), "test", func(context.Context, NoArguments) (string, error) {
			return "ok", nil
		})
	}
	registry := &ToolRegistry{}
	if err := registry.Register(newNamedTool()); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(newNamedTool()); err == nil || !strings.Contains(err.Error(), "已注册") {
		t.Fatalf("duplicate registration error = %v", err)
	}
	infos, err := registry.InfoList(context.Background())
	if err != nil || len(infos) != 1 {
		t.Fatalf("registry contents = %#v, err = %v", infos, err)
	}
}

func TestToolRegistryValidatesCustomToolNameAndIdentity(t *testing.T) {
	invalid := &registryTestTool{info: &ToolInfo{Name: "invalid.name"}}
	if err := NewToolRegistry().Register(invalid); err == nil || !strings.Contains(err.Error(), "名称") {
		t.Fatalf("invalid custom tool error = %v", err)
	}

	mutable := &registryTestTool{info: &ToolInfo{Name: "stable_name"}}
	registry := NewToolRegistry(mutable)
	mutable.info.Name = "changed_name"
	if _, err := registry.InfoList(context.Background()); err == nil || !strings.Contains(err.Error(), "注册后变为") {
		t.Fatalf("changed identity error = %v", err)
	}
}
